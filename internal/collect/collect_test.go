package collect

import (
	"path/filepath"
	"strings"
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

func TestCompressArchivesAndUnindexes(t *testing.T) {
	st, ix, c := fixture(t)
	old := time.Now().UTC().AddDate(0, 0, -30)
	for _, e := range []entry.Entry{
		{Time: old, Level: "error", Message: "ancient"},
		{Time: time.Now().UTC(), Level: "info", Message: "recent"},
	} {
		if err := c.Write([]entry.Entry{e}); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := c.Compress(7 * 24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	count, _ := ix.DocCount()
	if count != 1 {
		t.Fatalf("documents = %d, want 1", count)
	}
	res, err := ix.Search(index.Query{Text: "ancient", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 0 {
		t.Fatalf("archived entry still searchable, total = %d", res.Total)
	}

	files, err := st.Files()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.Contains(name, old.Format("20060102")) {
			t.Fatalf("uncompressed day file kept: %v", files)
		}
	}
	for name := range ix.Offsets() {
		if strings.Contains(name, old.Format("20060102")) {
			t.Fatalf("offset for compressed file kept: %v", ix.Offsets())
		}
	}
}
