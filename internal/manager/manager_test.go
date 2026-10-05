package manager

import (
	"testing"
	"time"

	"logrift.dev/server/internal/entry"
	"logrift.dev/server/internal/index"
)

func TestCreateWriteAndSearch(t *testing.T) {
	m, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	_, key, err := m.Create("dbmodeller", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := m.Authenticate(key)
	if !ok {
		t.Fatal("authenticate failed")
	}

	collector, ok := m.Collector(p.Name)
	if !ok {
		t.Fatal("collector missing")
	}
	if err := collector.Write([]entry.Entry{{Time: time.Now().UTC(), Level: "error", Message: "boom"}}); err != nil {
		t.Fatal(err)
	}

	ix, ok := m.Index(p.Name)
	if !ok {
		t.Fatal("index missing")
	}
	res, err := ix.Search(index.Query{Text: "boom", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Fatalf("search total = %d, want 1", res.Total)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	root := t.TempDir()
	m, err := Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := m.Create("api", "", 0)
	collector, _ := m.Collector("api")
	if err := collector.Write([]entry.Entry{{Time: time.Now().UTC(), Level: "info", Message: "persisted"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	added, err := reopened.CatchUp()
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Fatalf("restart re-indexed %d entries, want 0", added)
	}
	if _, ok := reopened.Authenticate(key); !ok {
		t.Fatal("project key not persisted")
	}
	ix, ok := reopened.Index("api")
	if !ok {
		t.Fatal("project index missing after reopen")
	}
	res, err := ix.Search(index.Query{Text: "persisted", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Fatalf("persisted search total = %d, want 1", res.Total)
	}
}

func TestDeleteRemovesData(t *testing.T) {
	m, _ := Open(t.TempDir(), false)
	defer m.Close()
	_, key, _ := m.Create("gone", "", 0)
	if err := m.Delete("gone"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Authenticate(key); ok {
		t.Fatal("deleted project still authenticates")
	}
	if _, ok := m.Collector("gone"); ok {
		t.Fatal("deleted project still has a runtime")
	}
}
