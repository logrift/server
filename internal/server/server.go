// Package server exposes logrift's HTTP surface: a JSON ingest endpoint, a
// search/query API, and the embedded web UI.
package server

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"logrift.dev/server/internal/collect"
	"logrift.dev/server/internal/entry"
	"logrift.dev/server/internal/index"
)

//go:embed index.html
var assets embed.FS

// Options configures the HTTP server.
type Options struct {
	Token        string
	MaxBodyBytes int64
	MaxResults   int
}

// Server coordinates ingestion into the store and index and serves the UI.
type Server struct {
	collector *collect.Collector
	index     *index.Index
	token     string
	maxBody   int64
	maxHits   int
	log       *slog.Logger
}

// New returns a Server writing through collector and searching index.
func New(collector *collect.Collector, ix *index.Index, opts Options, logger *slog.Logger) *Server {
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 5 * 1024 * 1024
	}
	if opts.MaxResults <= 0 {
		opts.MaxResults = 1000
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{collector: collector, index: ix, token: opts.Token, maxBody: opts.MaxBodyBytes, maxHits: opts.MaxResults, log: logger}
}

// Handler builds the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/logs", s.handleIngest)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /{$}", s.handleIndex)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid ingest token"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unable to read body"})
		return
	}

	entries, err := decodeEntries(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(entries) == 0 {
		writeJSON(w, http.StatusOK, map[string]int{"accepted": 0})
		return
	}

	if err := s.collector.Write(entries); err != nil {
		s.log.Error("failed to persist log entries", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unable to persist entries"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"accepted": len(entries)})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	since, err := parseTimeParam(params.Get("since"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid since: " + err.Error()})
		return
	}
	until, err := parseTimeParam(params.Get("until"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid until: " + err.Error()})
		return
	}

	limit := clamp(parseInt(params.Get("limit"), 100), 1, s.maxHits)
	offset := max(parseInt(params.Get("offset"), 0), 0)

	result, err := s.index.Search(index.Query{
		Text:    params.Get("q"),
		Level:   strings.TrimSpace(params.Get("level")),
		Service: strings.TrimSpace(params.Get("service")),
		Since:   since,
		Until:   until,
		Limit:   limit,
		Offset:  offset,
	})
	if err != nil {
		s.log.Error("search failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "search failed"})
		return
	}

	type hit struct {
		entry.Entry
		Raw string `json:"raw"`
	}
	hits := make([]hit, 0, len(result.Hits))
	for n, e := range result.Hits {
		hits = append(hits, hit{Entry: e, Raw: result.Raw[n]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":   result.Total,
		"count":   len(hits),
		"limit":   limit,
		"offset":  offset,
		"entries": hits,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	levels := []string{"trace", "debug", "info", "warn", "error", "fatal"}
	counts := map[string]uint64{}
	for _, level := range levels {
		n, err := s.index.Count(level)
		if err != nil {
			s.log.Error("count failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "count failed"})
			return
		}
		counts[level] = n
	}
	total, err := s.index.Count("")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "count failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "levels": counts})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	page, err := assets.ReadFile("index.html")
	if err != nil {
		http.Error(w, "unable to load UI", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(page)
}

func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
		if equal(strings.TrimPrefix(header, "Bearer "), s.token) {
			return true
		}
	}
	return equal(r.Header.Get("X-Logrift-Token"), s.token)
}

func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// decodeEntries accepts a single JSON object, a JSON array of objects, or a
// stream of newline-delimited JSON objects.
func decodeEntries(body []byte) ([]entry.Entry, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var raws []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &raws); err != nil {
			return nil, errors.New("invalid JSON array")
		}
		entries := make([]entry.Entry, 0, len(raws))
		for _, raw := range raws {
			e, err := entry.Parse(raw)
			if err != nil {
				return nil, errors.New("invalid log entry in array")
			}
			entries = append(entries, e)
		}
		return entries, nil
	}

	dec := json.NewDecoder(strings.NewReader(trimmed))
	var entries []entry.Entry
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, errors.New("invalid JSON log line")
		}
		e, err := entry.Parse(raw)
		if err != nil {
			return nil, errors.New("invalid log entry")
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func parseTimeParam(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if d, err := parseDuration(value); err == nil {
		t := time.Now().UTC().Add(-d)
		return &t, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, value); err == nil {
			t = t.UTC()
			return &t, nil
		}
	}
	return nil, errors.New("expected a duration like 1h or an RFC3339 timestamp")
}

// parseDuration extends time.ParseDuration with a day unit.
func parseDuration(value string) (time.Duration, error) {
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(value, "d"), 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(value)
}

func parseInt(value string, fallback int) int {
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func clamp(n, low, high int) int { return min(max(n, low), high) }

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
