package collect

import (
	"path/filepath"
	"testing"
	"time"

	"logrift.dev/server/internal/entry"
	"logrift.dev/server/internal/index"
	"logrift.dev/server/internal/store"
)

func fixture(t *testing.T) (*store.Store, *index.Index, *Collector) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	ix, _, err := index.Open(filepath.Join(root, "index"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close(); _ = st.Close() })
	return st, ix, New(st, ix)
}

func entries() []entry.Entry {
	now := time.Now().UTC()
	return []entry.Entry{
		{Time: now, Level: "info", Message: "first"},
		{Time: now, Level: "warn", Message: "second"},
	}
}

func TestWriteThenCatchUpIsIdempotent(t *testing.T) {
	_, ix, c := fixture(t)
	if err := c.Write(entries()); err != nil {
		t.Fatal(err)
	}
	added, err := c.CatchUp()
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Fatalf("catch up re-indexed %d entries, want 0", added)
	}
	count, err := ix.DocCount()
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("documents = %d, want 2", count)
	}
}

func TestCatchUpIndexesUnindexedTail(t *testing.T) {
	st, ix, c := fixture(t)
	if err := c.Write(entries()); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash between appending to the store and indexing it.
	if _, err := st.Append(entry.Entry{Time: time.Now().UTC(), Level: "error", Message: "tail"}); err != nil {
		t.Fatal(err)
	}

	added, err := c.CatchUp()
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("catch up indexed %d, want 1", added)
	}
	count, _ := ix.DocCount()
	if count != 3 {
		t.Fatalf("documents = %d, want 3", count)
	}
}

func TestStableIDsPreventDuplicatesOnRestart(t *testing.T) {
	_, ix, c := fixture(t)
	if err := c.Write(entries()); err != nil {
		t.Fatal(err)
	}
	// Simulate a lost/behind meta file: clearing offsets forces a full replay.
	if err := ix.SetOffsets(map[string]int64{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CatchUp(); err != nil {
		t.Fatal(err)
	}
	count, _ := ix.DocCount()
	if count != 2 {
		t.Fatalf("documents = %d, want 2 (no duplicates)", count)
	}
}
