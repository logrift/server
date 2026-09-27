// Package config loads logrift settings from the environment.
package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds the runtime configuration for the logrift server.
type Config struct {
	Addr          string
	DataDir       string
	AdminKey      string
	Reindex       bool
	Retention     time.Duration
	MaxBodyBytes  int64
	MaxResults    int
	PruneInterval time.Duration
}

// Load reads configuration from the environment, applying defaults.
func Load() Config {
	return Config{
		Addr:          env("LOGRIFT_ADDR", "127.0.0.1:8787"),
		DataDir:       env("LOGRIFT_DATA_DIR", "./data"),
		AdminKey:      os.Getenv("LOGRIFT_ADMIN_KEY"),
		Reindex:       os.Getenv("LOGRIFT_REINDEX") == "1",
		Retention:     time.Duration(envInt("LOGRIFT_RETENTION_DAYS", 14)) * 24 * time.Hour,
		MaxBodyBytes:  int64(envInt("LOGRIFT_MAX_BODY_KB", 5120)) * 1024,
		MaxResults:    envInt("LOGRIFT_MAX_RESULTS", 1000),
		PruneInterval: time.Duration(envInt("LOGRIFT_PRUNE_INTERVAL_MIN", 60)) * time.Minute,
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
