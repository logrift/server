package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCreatesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logrift.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != Defaults() {
		t.Fatalf("cfg = %+v, want %+v", cfg, Defaults())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file not created: %v", err)
	}
}

func TestLoadOverridesKeepDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logrift.json")
	if err := os.WriteFile(path, []byte(`{"addr":"127.0.0.1:9999","compress_after_days":0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:9999" || cfg.CompressAfterDays != 0 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.DataDir != Defaults().DataDir || cfg.MaxResults != Defaults().MaxResults {
		t.Fatalf("defaults lost: %+v", cfg)
	}
}

func TestLoadRejectsInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logrift.json")
	if err := os.WriteFile(path, []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for invalid JSON")
	}
}

func TestStoreUpdatePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logrift.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(func(c *Config) {
		c.CompressAfterDays = 3
		c.CompressIntervalMin = 5
	}); err != nil {
		t.Fatal(err)
	}
	if store.Get().CompressAfterDays != 3 || store.Get().CompressIntervalMin != 5 {
		t.Fatalf("store = %+v", store.Get())
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Get().CompressAfterDays != 3 || reopened.Get().CompressIntervalMin != 5 {
		t.Fatalf("reopened = %+v", reopened.Get())
	}

	if _, err := store.Update(func(c *Config) { c.MaxResults = 0 }); err == nil {
		t.Fatal("expected an error for max_results = 0")
	}
	if store.Get().MaxResults != Defaults().MaxResults {
		t.Fatalf("rejected update applied: %+v", store.Get())
	}
}

func TestStoreInMemory(t *testing.T) {
	store := NewStore(Defaults())
	if store.Path() != "" {
		t.Fatalf("path = %q, want empty", store.Path())
	}
	if _, err := store.Update(func(c *Config) { c.CompressAfterDays = 1 }); err != nil {
		t.Fatal(err)
	}
	if store.Get().CompressAfterDays != 1 {
		t.Fatalf("store = %+v", store.Get())
	}
}
