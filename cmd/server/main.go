package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
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

	cfg := config.Load()

	adminKey, generated, err := resolveAdminKey(cfg.DataDir, cfg.AdminKey)
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

	if cfg.Retention > 0 {
		if deleted, err := mgr.Prune(cfg.Retention); err != nil {
			logger.Warn("startup prune failed", "error", err)
		} else if deleted > 0 {
			logger.Info("pruned expired index documents", "deleted", deleted)
		}
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
	} else if cfg.AdminKey == "" {
		logger.Info("using stored admin key", "path", filepath.Join(cfg.DataDir, "admin.key"))
	}

	srv := server.New(mgr, server.Options{AdminKey: adminKey, MaxBodyBytes: cfg.MaxBodyBytes, MaxResults: cfg.MaxResults}, logger)
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go pruneLoop(ctx, mgr, cfg, logger)
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

func pruneLoop(ctx context.Context, mgr *manager.Manager, cfg config.Config, logger *slog.Logger) {
	if cfg.Retention <= 0 && cfg.PruneInterval <= 0 {
		return
	}
	ticker := time.NewTicker(cfg.PruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if cfg.Retention > 0 {
				deleted, err := mgr.Prune(cfg.Retention)
				if err != nil {
					logger.Warn("retention prune failed", "error", err)
				} else if deleted > 0 {
					logger.Info("pruned expired index documents", "deleted", deleted)
				}
			}
			if err := mgr.Flush(); err != nil {
				logger.Warn("unable to persist project registry", "error", err)
			}
		}
	}
}

// resolveAdminKey returns the admin key from env if set, otherwise a key stored
// in dataDir (generated on first use).
func resolveAdminKey(dataDir, fromEnv string) (key string, generated bool, err error) {
	if strings.TrimSpace(fromEnv) != "" {
		return strings.TrimSpace(fromEnv), false, nil
	}
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
