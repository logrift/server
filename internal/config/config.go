// Package config loads logrift settings from a JSON config file. A missing
// file is created with defaults so it can be edited afterwards.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Config holds the runtime configuration for the logrift server.
type Config struct {
	Addr                string `json:"addr"`
	DataDir             string `json:"data_dir"`
	Reindex             bool   `json:"reindex"`
	CompressAfterDays   int    `json:"compress_after_days"`
	CompressIntervalMin int    `json:"compress_interval_min"`
	MaxBodyKB           int    `json:"max_body_kb"`
	MaxResults          int    `json:"max_results"`
}

// Defaults returns the configuration used when a setting is absent.
func Defaults() Config {
	return Config{
		Addr:                "127.0.0.1:8787",
		DataDir:             "./data",
		CompressAfterDays:   14,
		CompressIntervalMin: 60,
		MaxBodyKB:           5120,
		MaxResults:          1000,
	}
}

// Load reads the config file at path. A missing file is written with defaults
// and those defaults are returned. Settings absent from an existing file keep
// their default value; an explicit zero disables the setting where allowed.
func Load(path string) (Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := Save(path, cfg); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.normalize()
	return cfg, nil
}

// normalize replaces missing or nonsensical values with defaults.
func (c *Config) normalize() {
	if c.Addr == "" {
		c.Addr = Defaults().Addr
	}
	if c.DataDir == "" {
		c.DataDir = Defaults().DataDir
	}
	if c.MaxBodyKB <= 0 {
		c.MaxBodyKB = Defaults().MaxBodyKB
	}
	if c.MaxResults <= 0 {
		c.MaxResults = Defaults().MaxResults
	}
	if c.CompressAfterDays < 0 {
		c.CompressAfterDays = 0
	}
	if c.CompressIntervalMin < 0 {
		c.CompressIntervalMin = 0
	}
}

// Validate reports values that cannot be applied at runtime.
func (c Config) Validate() error {
	switch {
	case c.Addr == "":
		return errors.New("addr must not be empty")
	case c.DataDir == "":
		return errors.New("data_dir must not be empty")
	case c.CompressAfterDays < 0:
		return errors.New("compress_after_days must not be negative")
	case c.CompressIntervalMin < 0:
		return errors.New("compress_interval_min must not be negative")
	case c.MaxBodyKB <= 0:
		return errors.New("max_body_kb must be positive")
	case c.MaxResults <= 0:
		return errors.New("max_results must be positive")
	}
	return nil
}

// Store is a concurrency-safe holder of the runtime configuration. Edits made
// through Update are persisted to the config file backing the store.
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// Open loads the config file at path into a store, creating the file with
// defaults when it is missing.
func Open(path string) (*Store, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	return &Store{path: path, cfg: cfg}, nil
}

// NewStore returns an in-memory store that is never persisted to disk.
func NewStore(cfg Config) *Store {
	cfg.normalize()
	return &Store{cfg: cfg}
}

// Get returns the current configuration.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Path returns the config file path, empty for in-memory stores.
func (s *Store) Path() string { return s.path }

// Update applies fn to the configuration, validates the result and persists it
// to the config file. The file is not touched when the store is in-memory.
func (s *Store) Update(fn func(*Config)) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cfg
	fn(&next)
	if err := next.Validate(); err != nil {
		return Config{}, err
	}
	if s.path != "" {
		if err := Save(s.path, next); err != nil {
			return Config{}, err
		}
	}
	s.cfg = next
	return next, nil
}

// Save writes cfg to path.
func Save(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// CompressInterval returns how often the compression pass runs. A non-positive
// value disables the background loop.
func (c Config) CompressInterval() time.Duration {
	return time.Duration(c.CompressIntervalMin) * time.Minute
}

// MaxBodyBytes returns the maximum ingest request size.
func (c Config) MaxBodyBytes() int64 {
	return int64(c.MaxBodyKB) * 1024
}
