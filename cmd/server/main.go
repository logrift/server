package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"logrift.dev/server/internal/collect"
	"logrift.dev/server/internal/config"
	"logrift.dev/server/internal/index"
	"logrift.dev/server/internal/server"
	"logrift.dev/server/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg := config.Load()

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		logger.Error("unable to open data directory", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	if repaired, err := st.Repair(); err != nil {
		logger.Error("unable to repair data files", "error", err)
		os.Exit(1)
	} else if repaired > 0 {
		logger.Warn("discarded partial trailing log line", "bytes", repaired)
	}

	if cfg.Reindex {
		logger.Info("rebuilding index", "index_dir", cfg.IndexDir)
		if err := index.Remove(cfg.IndexDir); err != nil {
			logger.Error("unable to remove index", "error", err)
			os.Exit(1)
		}
	}

	ix, created, err := index.Open(cfg.IndexDir)
	if err != nil {
		logger.Error("unable to open index", "error", err)
		os.Exit(1)
	}
	defer ix.Close()

	collector := collect.New(st, ix)

	if cfg.Retention > 0 {
		if deleted, err := collector.Prune(cfg.Retention); err != nil {
			logger.Warn("startup prune failed", "error", err)
		} else if deleted > 0 {
			logger.Info("pruned expired index documents", "deleted", deleted)
		}
	}

	started := time.Now()
	added, err := collector.CatchUp()
	if err != nil {
		logger.Error("unable to catch up index", "error", err)
		os.Exit(1)
	}
	documents, _ := ix.DocCount()
	logger.Info("index ready",
		"index_dir", cfg.IndexDir,
		"created", created,
		"indexed", added,
		"documents", documents,
		"took", time.Since(started).Round(time.Millisecond).String(),
	)

	srv := server.New(collector, ix, server.Options{Token: cfg.Token, MaxBodyBytes: cfg.MaxBodyBytes, MaxResults: cfg.MaxResults}, logger)
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go pruneLoop(ctx, collector, cfg, logger)
	go func() {
		logger.Info("logrift listening", "addr", cfg.Addr, "auth", cfg.Token != "")
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
}

func pruneLoop(ctx context.Context, collector *collect.Collector, cfg config.Config, logger *slog.Logger) {
	if cfg.Retention <= 0 || cfg.PruneInterval <= 0 {
		return
	}
	ticker := time.NewTicker(cfg.PruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deleted, err := collector.Prune(cfg.Retention)
			if err != nil {
				logger.Warn("retention prune failed", "error", err)
			} else if deleted > 0 {
				logger.Info("pruned expired index documents", "deleted", deleted)
			}
		}
	}
}
