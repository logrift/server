package project

import (
	"path/filepath"
	"testing"
)

func TestCreateAuthenticateAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	registry, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}

	p, key, err := registry.Create("dbmodeller", "primary app", 14)
	if err != nil {
		t.Fatal(err)
	}
	if key == "" || p.KeyPrefix == "" {
		t.Fatalf("missing key material: %+v", p)
	}
	if p.KeyHash == key {
		t.Fatal("key was stored in plain text")
	}
	if got, ok := registry.Authenticate(key); !ok || got.Name != "dbmodeller" {
		t.Fatalf("authenticate failed: %+v %v", got, ok)
	}
	if _, ok := registry.Authenticate("lr_wrong"); ok {
		t.Fatal("wrong key authenticated")
	}

	reopened, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.Authenticate(key); !ok || got.Name != "dbmodeller" {
		t.Fatalf("reopened registry did not authenticate: %+v %v", got, ok)
	}
}

func TestCreateValidation(t *testing.T) {
	registry, _ := OpenRegistry(filepath.Join(t.TempDir(), "projects.json"))
	if _, _, err := registry.Create("Bad Name", "", 0); err != ErrInvalidName {
		t.Fatalf("invalid name err = %v", err)
	}
	if _, _, err := registry.Create("ok-name", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.Create("ok-name", "", 0); err != ErrExists {
		t.Fatalf("duplicate err = %v", err)
	}
}

func TestRotateAndDelete(t *testing.T) {
	registry, _ := OpenRegistry(filepath.Join(t.TempDir(), "projects.json"))
	_, oldKey, _ := registry.Create("api", "", 0)

	_, newKey, err := registry.Rotate("api")
	if err != nil {
		t.Fatal(err)
	}
	if newKey == oldKey {
		t.Fatal("rotate returned the same key")
	}
	if _, ok := registry.Authenticate(oldKey); ok {
		t.Fatal("old key still authenticates after rotate")
	}
	if _, ok := registry.Authenticate(newKey); !ok {
		t.Fatal("new key does not authenticate")
	}

	if err := registry.Delete("api"); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Authenticate(newKey); ok {
		t.Fatal("deleted project still authenticates")
	}
}

func TestUpdateCompressAfterDays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	registry, _ := OpenRegistry(path)
	created, _, err := registry.Create("api", "", 14)
	if err != nil {
		t.Fatal(err)
	}
	if created.CompressAfterDays != 14 {
		t.Fatalf("created compress_after_days = %d, want 14", created.CompressAfterDays)
	}

	updated, err := registry.UpdateCompressAfterDays("api", 0)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CompressAfterDays != 0 {
		t.Fatalf("updated compress_after_days = %d, want 0", updated.CompressAfterDays)
	}
	if _, err := registry.UpdateCompressAfterDays("api", -1); err != ErrInvalidDays {
		t.Fatalf("negative days err = %v, want %v", err, ErrInvalidDays)
	}
	if _, err := registry.UpdateCompressAfterDays("missing", 1); err != ErrNotFound {
		t.Fatalf("missing project err = %v, want %v", err, ErrNotFound)
	}

	reopened, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.Get("api"); !ok || got.CompressAfterDays != 0 {
		t.Fatalf("persisted compress_after_days = %+v", got)
	}
}
