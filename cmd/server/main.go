package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"logrift.dev/server/internal/config"
	"logrift.dev/server/internal/manager"
	"logrift.dev/server/internal/server"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	configPath := flag.String("config", "logrift.json", "path to the JSON config file")
	flag.Parse()

	cfgs, err := config.Open(*configPath)
	if err != nil {
		logger.Error("unable to load config", "path", *configPath, "error", err)
		os.Exit(1)
	}
	cfg := cfgs.Get()

	adminKey, generated, err := resolveAdminKey(cfg.DataDir)
	if err != nil {
		logger.Error("unable to resolve admin key", "error", err)
		os.Exit(1)
	}

	mgr, err := manager.Open(cfg.DataDir, cfg.Reindex)
	if err != nil {
		logger.Error("unable to open projects", "error", err)
		os.Exit(1)
	}
	defer mgr.Close()

	if deleted, err := mgr.Compress(); err != nil {
		logger.Warn("startup compression failed", "error", err)
	} else if deleted > 0 {
		logger.Info("compressed aged logs", "index_documents_removed", deleted)
	}

	started := time.Now()
	added, err := mgr.CatchUp()
	if err != nil {
		logger.Error("unable to catch up index", "error", err)
		os.Exit(1)
	}
	if err := mgr.Flush(); err != nil {
		logger.Warn("unable to persist project registry", "error", err)
	}
	logger.Info("index ready",
		"data_dir", cfg.DataDir,
		"projects", len(mgr.Projects()),
		"indexed", added,
		"took", time.Since(started).Round(time.Millisecond).String(),
	)
	if generated {
		logger.Warn("generated admin key and stored it", "path", filepath.Join(cfg.DataDir, "admin.key"), "admin_key", adminKey)
	} else {
		logger.Info("using stored admin key", "path", filepath.Join(cfg.DataDir, "admin.key"))
	}

	srv := server.New(mgr, server.Options{AdminKey: adminKey, Settings: cfgs}, logger)
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go compressLoop(ctx, mgr, cfgs, logger)
	go func() {
		logger.Info("logrift listening", "addr", cfg.Addr, "admin_auth", adminKey != "")
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
	if err := mgr.Flush(); err != nil {
		logger.Warn("unable to persist project registry", "error", err)
	}
}

// compressLoop runs the compression pass on the interval from the settings.
// The interval is re-read every round so settings edits apply without a
// restart; a non-positive interval pauses the loop.
func compressLoop(ctx context.Context, mgr *manager.Manager, cfgs *config.Store, logger *slog.Logger) {
	timer := time.NewTimer(intervalOrDefault(cfgs))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			interval := intervalOrDefault(cfgs)
			if cfgs.Get().CompressInterval() > 0 {
				deleted, err := mgr.Compress()
				if err != nil {
					logger.Warn("compression pass failed", "error", err)
				} else if deleted > 0 {
					logger.Info("compressed aged logs", "index_documents_removed", deleted)
				}
				if err := mgr.Flush(); err != nil {
					logger.Warn("unable to persist project registry", "error", err)
				}
			}
			timer.Reset(interval)
		}
	}
}

// intervalOrDefault returns the configured compression interval, polling at
// least once a minute while the pass is disabled so enabling it takes effect
// quickly.
func intervalOrDefault(cfgs *config.Store) time.Duration {
	if interval := cfgs.Get().CompressInterval(); interval > 0 {
		return interval
	}
	return time.Minute
}

// resolveAdminKey returns the admin key stored in dataDir, generating and
// persisting one on first use.
func resolveAdminKey(dataDir string) (key string, generated bool, err error) {
	path := filepath.Join(dataDir, "admin.key")
	if data, err := os.ReadFile(path); err == nil {
		if key := strings.TrimSpace(string(data)); key != "" {
			return key, false, nil
		}
	} else if !os.IsNotExist(err) {
		return "", false, err
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", false, err
	}
	key = "lr_admin_" + base64.RawURLEncoding.EncodeToString(buf)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return "", false, err
	}
	return key, true, nil
}
