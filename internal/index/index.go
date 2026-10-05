// Package index wraps a persistent Bleve index for log entries. The index is
// derived from the JSONL store; a small meta file records how many bytes of each
// day file have been indexed so startup only has to catch up on the tail.
package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/blevesearch/bleve/v2/util"

	"logrift.dev/server/internal/entry"
)

// Version 3 refreshes stored entries with normalized request fields.
const metaVersion = 3

// Query describes a log search.
type Query struct {
	Text              string
	Level             string
	Service           string
	Since             *time.Time
	Until             *time.Time
	Limit             int
	Offset            int
	HideMonitors      bool
	HideScans         bool
	MonitorUserAgents []string
	BotScanPaths      []string
}

// Result is a page of matched entries.
type Result struct {
	Total uint64
	Hits  []entry.Entry
	Raw   []string
}

// Item is one document to index. ID must be stable for a given stored line so
// that re-indexing the tail is idempotent.
type Item struct {
	ID    string
	Entry entry.Entry
	Raw   []byte
}

// Index is a concurrency-safe wrapper around a persistent Bleve index.
type Index struct {
	idx      bleve.Index
	metaPath string

	mu      sync.Mutex
	closed  bool
	offsets map[string]int64
}

// ErrClosed is returned when an index is used after Close.
var ErrClosed = errors.New("index is closed")

type meta struct {
	Version int              `json:"version"`
	Offsets map[string]int64 `json:"offsets"`
}

// Open opens the index at path, creating it if it does not exist. The returned
// bool reports whether the index was newly created.
func Open(path string) (*Index, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, fmt.Errorf("create index parent: %w", err)
	}
	m, err := buildMapping()
	if err != nil {
		return nil, false, err
	}

	created := false
	idx, err := bleve.Open(path)
	if errors.Is(err, bleve.ErrorIndexPathDoesNotExist) || errors.Is(err, bleve.ErrorIndexMetaMissing) {
		idx, err = bleve.New(path, m)
		created = true
	}
	if err != nil {
		return nil, false, fmt.Errorf("open index: %w", err)
	}

	if !created {
		// Install the same field mappings on legacy indexes before catch-up.
		existing := idx.Mapping().(*mapping.IndexMappingImpl)
		doc := existing.TypeMapping["_default"]
		if doc.Properties["request_agent"] == nil || doc.Properties["request_path"] == nil {
			addRequestMappings(doc)
			encoded, mapErr := json.Marshal(existing)
			if mapErr == nil {
				mapErr = idx.SetInternal(util.MappingInternalKey, encoded)
			}
			if mapErr != nil {
				_ = idx.Close()
				return nil, false, fmt.Errorf("update request mapping: %w", mapErr)
			}
		}
	}
	i := &Index{idx: idx, metaPath: metaPath(path), offsets: map[string]int64{}}
	if !created {
		if err := i.loadMeta(); err != nil {
			_ = idx.Close()
			return nil, false, err
		}
	}
	return i, created, nil
}

// Remove deletes the index and its meta file. Used for a full rebuild.
func Remove(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove index: %w", err)
	}
	if err := os.Remove(metaPath(path)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove index meta: %w", err)
	}
	return nil
}

func metaPath(path string) string { return path + ".meta.json" }

// Add indexes one item.
func (i *Index) Add(item Item) error { return i.Batch([]Item{item}) }

// Batch indexes several items in one commit.
func (i *Index) Batch(items []Item) error {
	if len(items) == 0 {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return ErrClosed
	}
	batch := i.idx.NewBatch()
	for _, item := range items {
		if err := batch.Index(item.ID, document(item.Entry, item.Raw)); err != nil {
			return fmt.Errorf("index document: %w", err)
		}
	}
	if err := i.idx.Batch(batch); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}
	return nil
}

// Search runs a query and returns matching entries newest first.
func (i *Index) Search(q Query) (Result, error) {
	req := bleve.NewSearchRequestOptions(buildQuery(q), q.Limit, q.Offset, false)
	req.SortBy([]string{"-time"})
	// Only the stored canonical JSON is needed: hits are rebuilt from it.
	req.Fields = []string{"raw"}

	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return Result{}, ErrClosed
	}
	res, err := i.idx.Search(req)
	i.mu.Unlock()
	if err != nil {
		return Result{}, fmt.Errorf("search: %w", err)
	}

	out := Result{Total: res.Total, Hits: make([]entry.Entry, 0, len(res.Hits)), Raw: make([]string, 0, len(res.Hits))}
	for _, hit := range res.Hits {
		raw, _ := hit.Fields["raw"].(string)
		e, err := entry.Parse([]byte(raw))
		if err != nil {
			continue
		}
		out.Hits = append(out.Hits, e)
		out.Raw = append(out.Raw, raw)
	}
	return out, nil
}

// Count returns the number of entries matching an optional exact level.
func (i *Index) Count(level string) (uint64, error) {
	var q query.Query
	if level == "" {
		q = bleve.NewMatchAllQuery()
	} else {
		term := bleve.NewTermQuery(strings.ToLower(level))
		term.SetField("level")
		q = term
	}
	req := bleve.NewSearchRequestOptions(q, 0, 0, false)

	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return 0, ErrClosed
	}
	res, err := i.idx.Search(req)
	i.mu.Unlock()
	if err != nil {
		return 0, fmt.Errorf("count: %w", err)
	}
	return res.Total, nil
}

// DeleteBefore removes every document older than t and returns how many were
// deleted.
func (i *Index) DeleteBefore(t time.Time) (int, error) {
	rng := bleve.NewDateRangeQuery(time.Unix(0, 0).UTC(), t.UTC())
	rng.SetField("time")
	bool := bleve.NewBooleanQuery()
	bool.AddMust(rng)

	deleted := 0
	for {
		req := bleve.NewSearchRequestOptions(bool, 1000, 0, false)
		i.mu.Lock()
		if i.closed {
			i.mu.Unlock()
			return deleted, ErrClosed
		}
		res, err := i.idx.Search(req)
		if err != nil {
			i.mu.Unlock()
			return deleted, fmt.Errorf("find expired docs: %w", err)
		}
		if len(res.Hits) == 0 {
			i.mu.Unlock()
			return deleted, nil
		}
		batch := i.idx.NewBatch()
		for _, hit := range res.Hits {
			batch.Delete(hit.ID)
		}
		deleted += len(res.Hits)
		err = i.idx.Batch(batch)
		i.mu.Unlock()
		if err != nil {
			return deleted, fmt.Errorf("commit delete: %w", err)
		}
	}
}

// DocCount returns the number of indexed documents.
func (i *Index) DocCount() (uint64, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return 0, ErrClosed
	}
	return i.idx.DocCount()
}

// Offsets returns a copy of the indexed byte offsets per day file.
func (i *Index) Offsets() map[string]int64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make(map[string]int64, len(i.offsets))
	for k, v := range i.offsets {
		out[k] = v
	}
	return out
}

// SetOffsets replaces and persists the indexed byte offsets.
func (i *Index) SetOffsets(offsets map[string]int64) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	next := make(map[string]int64, len(offsets))
	for k, v := range offsets {
		next[k] = v
	}
	i.offsets = next
	return i.saveMetaLocked()
}

// Close releases the index. It is safe to call more than once and serializes
// with in-flight operations.
func (i *Index) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil
	}
	i.closed = true
	return i.idx.Close()
}

func (i *Index) loadMeta() error {
	data, err := os.ReadFile(i.metaPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read index meta: %w", err)
	}
	var m meta
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("parse index meta: %w", err)
	}
	if m.Version != metaVersion {
		// Unknown meta version: fall back to a full tail scan rather than
		// trusting stale offsets.
		return nil
	}
	if m.Offsets != nil {
		i.offsets = m.Offsets
	}
	return nil
}

func (i *Index) saveMetaLocked() error {
	data, err := json.Marshal(meta{Version: metaVersion, Offsets: i.offsets})
	if err != nil {
		return fmt.Errorf("encode index meta: %w", err)
	}
	tmp := i.metaPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write index meta: %w", err)
	}
	if err := os.Rename(tmp, i.metaPath); err != nil {
		return fmt.Errorf("replace index meta: %w", err)
	}
	return nil
}

func buildQuery(q Query) query.Query {
	var musts []query.Query
	if text := strings.TrimSpace(q.Text); text != "" {
		musts = append(musts, bleve.NewMatchQuery(text))
	}
	if q.Level != "" {
		term := bleve.NewTermQuery(strings.ToLower(q.Level))
		term.SetField("level")
		musts = append(musts, term)
	}
	if q.Service != "" {
		term := bleve.NewTermQuery(q.Service)
		term.SetField("service")
		musts = append(musts, term)
	}
	if q.Since != nil || q.Until != nil {
		start := time.Unix(0, 0).UTC()
		end := time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
		if q.Since != nil {
			start = q.Since.UTC()
		}
		if q.Until != nil {
			end = q.Until.UTC()
		}
		inclusive := true
		rng := bleve.NewDateRangeInclusiveQuery(start, end, &inclusive, &inclusive)
		rng.SetField("time")
		musts = append(musts, rng)
	}

	if len(musts) == 0 {
		musts = append(musts, bleve.NewMatchAllQuery())
	}
	boolean := bleve.NewBooleanQuery()
	boolean.AddMust(musts...)
	for _, filter := range []struct {
		field    string
		enabled  bool
		patterns []string
	}{
		{"request_agent", q.HideMonitors, q.MonitorUserAgents},
		{"request_path", q.HideScans, q.BotScanPaths},
	} {
		if !filter.enabled {
			continue
		}
		for _, pattern := range filter.patterns {
			wildcard := bleve.NewWildcardQuery(strings.ToLower(strings.TrimSpace(pattern)))
			wildcard.SetField(filter.field)
			boolean.AddMustNot(wildcard)
		}
	}
	return boolean
}

func document(e entry.Entry, raw []byte) map[string]any {
	agents, paths := e.RequestFields()
	doc := map[string]any{
		"time":          e.Time,
		"level":         e.Level,
		"service":       e.Service,
		"message":       e.Message,
		"raw":           string(raw),
		"request_agent": agents,
		"request_path":  paths,
	}
	for k, v := range e.Attrs {
		doc["attr."+k] = v
	}
	return doc
}

func buildMapping() (mapping.IndexMapping, error) {
	im := bleve.NewIndexMapping()

	doc := bleve.NewDocumentMapping()
	doc.Dynamic = true

	timeField := bleve.NewDateTimeFieldMapping()
	timeField.Store = true
	timeField.DocValues = true
	doc.AddFieldMappingsAt("time", timeField)

	levelField := bleve.NewTextFieldMapping()
	levelField.Store = true
	levelField.DocValues = true
	levelField.Analyzer = "keyword"
	doc.AddFieldMappingsAt("level", levelField)

	serviceField := bleve.NewTextFieldMapping()
	serviceField.Store = true
	serviceField.Analyzer = "keyword"
	doc.AddFieldMappingsAt("service", serviceField)

	messageField := bleve.NewTextFieldMapping()
	messageField.Store = true
	doc.AddFieldMappingsAt("message", messageField)

	rawField := bleve.NewTextFieldMapping()
	rawField.Store = true
	rawField.Index = false
	doc.AddFieldMappingsAt("raw", rawField)
	addRequestMappings(doc)

	im.AddDocumentMapping("_default", doc)
	return im, nil
}

func addRequestMappings(doc *mapping.DocumentMapping) {
	for _, name := range []string{"request_agent", "request_path"} {
		if doc.Properties[name] != nil {
			continue
		}
		field := bleve.NewTextFieldMapping()
		field.Analyzer = "keyword"
		field.IncludeInAll = false
		doc.AddFieldMappingsAt(name, field)
	}
}
