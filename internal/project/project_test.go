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

	p, key, err := registry.Create("dbmodeller", "primary app")
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
	if _, _, err := registry.Create("Bad Name", ""); err != ErrInvalidName {
		t.Fatalf("invalid name err = %v", err)
	}
	if _, _, err := registry.Create("ok-name", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.Create("ok-name", ""); err != ErrExists {
		t.Fatalf("duplicate err = %v", err)
	}
}

func TestRotateAndDelete(t *testing.T) {
	registry, _ := OpenRegistry(filepath.Join(t.TempDir(), "projects.json"))
	_, oldKey, _ := registry.Create("api", "")

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
