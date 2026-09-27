// Package collect coordinates the JSONL store and the search index. It owns the
// rule that every stored line is identified by its file and byte offset, which
// makes indexing idempotent and lets startup resume from the last indexed
// position.
package collect

import (
	"strconv"
	"time"

	"logrift.dev/server/internal/entry"
	"logrift.dev/server/internal/index"
	"logrift.dev/server/internal/store"
)

const batchSize = 500

// Collector writes entries to the store and index and keeps the index position
// up to date.
type Collector struct {
	store *store.Store
	index *index.Index
}

// New returns a collector over st and ix.
func New(st *store.Store, ix *index.Index) *Collector {
	return &Collector{store: st, index: ix}
}

// Write persists and indexes entries. It returns an error if either step fails;
// entries already appended to the store are indexed on the next CatchUp.
func (c *Collector) Write(entries []entry.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	items := make([]index.Item, 0, len(entries))
	last := make(map[string]int64)
	for _, e := range entries {
		raw, err := e.Marshal()
		if err != nil {
			return err
		}
		pos, err := c.store.AppendRaw(e.Time, raw)
		if err != nil {
			return err
		}
		items = append(items, index.Item{ID: docID(pos), Entry: e, Raw: raw})
		last[pos.File] = pos.End
	}
	if err := c.index.Batch(items); err != nil {
		return err
	}
	offsets := c.index.Offsets()
	for name, end := range last {
		offsets[name] = end
	}
	return c.index.SetOffsets(offsets)
}

// CatchUp indexes any stored lines beyond the recorded offsets. Because doc IDs
// are stable, re-indexing a line that is already present simply overwrites it.
func (c *Collector) CatchUp() (int, error) {
	offsets := c.index.Offsets()
	added := 0
	items := make([]index.Item, 0, batchSize)

	flush := func() error {
		if len(items) == 0 {
			return nil
		}
		if err := c.index.Batch(items); err != nil {
			return err
		}
		added += len(items)
		items = items[:0]
		return nil
	}

	end, err := c.store.Scan(offsets, func(pos store.Position, e entry.Entry, raw []byte) error {
		items = append(items, index.Item{ID: docID(pos), Entry: e, Raw: raw})
		if len(items) >= batchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return added, err
	}
	if err := flush(); err != nil {
		return added, err
	}
	return added, c.index.SetOffsets(end)
}

// Prune enforces retention on both the JSONL files and the index. It returns the
// number of index documents deleted.
func (c *Collector) Prune(retention time.Duration) (int, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-retention)
	if err := c.store.Prune(retention); err != nil {
		return 0, err
	}
	deleted, err := c.index.DeleteBefore(cutoff)
	if err != nil {
		return deleted, err
	}
	return deleted, c.dropMissingOffsets()
}

// dropMissingOffsets removes offset entries for day files no longer on disk.
func (c *Collector) dropMissingOffsets() error {
	files, err := c.store.Files()
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(files))
	for _, name := range files {
		present[name] = true
	}
	offsets := c.index.Offsets()
	next := make(map[string]int64, len(offsets))
	for name, end := range offsets {
		if present[name] {
			next[name] = end
		}
	}
	return c.index.SetOffsets(next)
}

func docID(pos store.Position) string {
	return pos.File + ":" + strconv.FormatInt(pos.Start, 10)
}
