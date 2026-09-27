package index

import (
	"testing"
	"time"

	"logrift.dev/server/internal/entry"
)

func add(t *testing.T, ix *Index, id string, e entry.Entry) {
	t.Helper()
	raw, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.Add(Item{ID: id, Entry: e, Raw: raw}); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) (*Index, time.Time) {
	t.Helper()
	ix, _, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	add(t, ix, "1", entry.Entry{Time: base, Level: "info", Service: "dbmodeller", Message: "server started"})
	add(t, ix, "2", entry.Entry{Time: base.Add(time.Minute), Level: "error", Service: "dbmodeller", Message: "database connection failed"})
	add(t, ix, "3", entry.Entry{Time: base.Add(2 * time.Minute), Level: "warn", Service: "worker", Message: "slow database query"})
	return ix, base
}

func TestSearchText(t *testing.T) {
	ix, _ := newFixture(t)
	defer ix.Close()

	res, err := ix.Search(Query{Text: "database", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 2 {
		t.Fatalf("total = %d, want 2", res.Total)
	}
}

func TestSearchFilters(t *testing.T) {
	ix, base := newFixture(t)
	defer ix.Close()

	res, err := ix.Search(Query{Level: "error", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || res.Hits[0].Level != "error" {
		t.Fatalf("level filter failed: %+v", res)
	}

	res, err = ix.Search(Query{Service: "worker", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || res.Hits[0].Service != "worker" {
		t.Fatalf("service filter failed: %+v", res)
	}

	since := base.Add(90 * time.Second)
	res, err = ix.Search(Query{Since: &since, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Fatalf("since filter total = %d, want 1", res.Total)
	}
}

func TestSearchNewestFirst(t *testing.T) {
	ix, _ := newFixture(t)
	defer ix.Close()

	res, err := ix.Search(Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 3 {
		t.Fatalf("hits = %d, want 3", len(res.Hits))
	}
	if !res.Hits[0].Time.After(res.Hits[2].Time) {
		t.Fatalf("results not sorted newest first: %v", res.Hits)
	}
}

func TestCount(t *testing.T) {
	ix, _ := newFixture(t)
	defer ix.Close()

	total, err := ix.Count("")
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	errors, err := ix.Count("error")
	if err != nil {
		t.Fatal(err)
	}
	if errors != 1 {
		t.Fatalf("error count = %d, want 1", errors)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/index"
	ix, created, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected a newly created index")
	}
	add(t, ix, "1", entry.Entry{Time: time.Now().UTC(), Level: "info", Message: "persisted"})
	if err := ix.SetOffsets(map[string]int64{"logs-20260927.jsonl": 42}); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, created, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if created {
		t.Fatal("expected to open the existing index")
	}
	res, err := reopened.Search(Query{Text: "persisted", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Fatalf("reopened total = %d, want 1", res.Total)
	}
	if got := reopened.Offsets()["logs-20260927.jsonl"]; got != 42 {
		t.Fatalf("offsets not persisted: %d", got)
	}
}

func TestDeleteBefore(t *testing.T) {
	ix, base := newFixture(t)
	defer ix.Close()

	deleted, err := ix.DeleteBefore(base.Add(90 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	total, err := ix.Count("")
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("remaining = %d, want 1", total)
	}
}
