// Package server exposes logrift's HTTP surface: a per-project JSON ingest
// endpoint, an admin API for managing projects, a search/query API, and the
// embedded web UI.
package server

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"logrift.dev/server/internal/config"
	"logrift.dev/server/internal/entry"
	"logrift.dev/server/internal/index"
	"logrift.dev/server/internal/manager"
	"logrift.dev/server/internal/project"
)

//go:embed index.html
var assets embed.FS

const allProjects = "all"

// Options configures the HTTP server.
type Options struct {
	AdminKey string
	// Settings holds the runtime configuration; defaults are used when nil.
	Settings *config.Store
}

// Server authenticates ingests per project and serves the admin UI/API.
type Server struct {
	manager  *manager.Manager
	adminKey string
	settings *config.Store
	log      *slog.Logger
}

// New returns a Server backed by m.
func New(m *manager.Manager, opts Options, logger *slog.Logger) *Server {
	if opts.Settings == nil {
		opts.Settings = config.NewStore(config.Defaults())
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{manager: m, adminKey: opts.AdminKey, settings: opts.Settings, log: logger}
}

func (s *Server) cfg() config.Config { return s.settings.Get() }

// Handler builds the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/logs", s.handleIngest)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PATCH /api/settings", s.handleUpdateSettings)
	mux.HandleFunc("GET /api/projects", s.handleProjects)
	mux.HandleFunc("POST /api/projects", s.handleCreateProject)
	mux.HandleFunc("PATCH /api/projects/{name}", s.handleUpdateProject)
	mux.HandleFunc("POST /api/projects/{name}/rotate", s.handleRotateProject)
	mux.HandleFunc("DELETE /api/projects/{name}", s.handleDeleteProject)
	mux.HandleFunc("GET /api/projects/{name}/days", s.handleProjectDays)
	mux.HandleFunc("GET /api/projects/{name}/archive", s.handleArchive)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /{$}", s.handleIndex)
	return mux
}

// restartRequired lists the settings that only take effect after a restart.
var restartRequired = []string{"addr", "data_dir", "reindex"}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":         s.cfg(),
		"restart_required": restartRequired,
		"config_path":      s.settings.Path(),
	})
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unable to read body"})
		return
	}
	var payload struct {
		Addr                *string `json:"addr"`
		DataDir             *string `json:"data_dir"`
		Reindex             *bool   `json:"reindex"`
		CompressAfterDays   *int    `json:"compress_after_days"`
		CompressIntervalMin *int    `json:"compress_interval_min"`
		MaxBodyKB           *int    `json:"max_body_kb"`
		MaxResults          *int    `json:"max_results"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	updated, err := s.settings.Update(func(c *config.Config) {
		if payload.Addr != nil {
			c.Addr = strings.TrimSpace(*payload.Addr)
		}
		if payload.DataDir != nil {
			c.DataDir = strings.TrimSpace(*payload.DataDir)
		}
		if payload.Reindex != nil {
			c.Reindex = *payload.Reindex
		}
		if payload.CompressAfterDays != nil {
			c.CompressAfterDays = *payload.CompressAfterDays
		}
		if payload.CompressIntervalMin != nil {
			c.CompressIntervalMin = *payload.CompressIntervalMin
		}
		if payload.MaxBodyKB != nil {
			c.MaxBodyKB = *payload.MaxBodyKB
		}
		if payload.MaxResults != nil {
			c.MaxResults = *payload.MaxResults
		}
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":         updated,
		"restart_required": restartRequired,
		"config_path":      s.settings.Path(),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	project, ok := s.manager.Authenticate(bearer(r))
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid project API key"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg().MaxBodyBytes()))
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

	collector, ok := s.manager.Collector(project.Name)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "project is not available"})
		return
	}
	if err := collector.Write(entries); err != nil {
		s.log.Error("failed to persist log entries", "project", project.Name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unable to persist entries"})
		return
	}
	s.manager.Touch(project.Name)
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": len(entries), "project": project.Name})
}

type hit struct {
	Project string `json:"project"`
	entry.Entry
	Raw string `json:"raw"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	query, name, err := s.searchQuery(r)
	if errors.Is(err, project.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	var hits []hit
	var total uint64
	if name == allProjects {
		hits, total = s.searchAll(query)
	} else {
		hits, total, err = s.searchOne(name, query)
		if errors.Is(err, project.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		if err != nil {
			s.log.Error("search failed", "project", name, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "search failed"})
			return
		}
	}

	if hits == nil {
		hits = []hit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":   total,
		"count":   len(hits),
		"limit":   query.Limit,
		"offset":  query.Offset,
		"project": name,
		"entries": hits,
	})
}

// searchQuery parses the search parameters. For the "all" project it fetches
// enough from each index to satisfy the page before merging.
func (s *Server) searchQuery(r *http.Request) (index.Query, string, error) {
	params := r.URL.Query()
	name := strings.TrimSpace(params.Get("project"))
	if name == "" {
		name = allProjects
	}
	if name != allProjects {
		if _, ok := s.manager.Get(name); !ok {
			return index.Query{}, name, project.ErrNotFound
		}
	}
	since, err := parseTimeParam(params.Get("since"))
	if err != nil {
		return index.Query{}, name, errors.New("invalid since: " + err.Error())
	}
	until, err := parseTimeParam(params.Get("until"))
	if err != nil {
		return index.Query{}, name, errors.New("invalid until: " + err.Error())
	}
	limit := clamp(parseInt(params.Get("limit"), 100), 1, s.cfg().MaxResults)
	offset := max(parseInt(params.Get("offset"), 0), 0)
	return index.Query{
		Text:    params.Get("q"),
		Level:   strings.TrimSpace(params.Get("level")),
		Service: strings.TrimSpace(params.Get("service")),
		Since:   since,
		Until:   until,
		Limit:   limit,
		Offset:  offset,
	}, name, nil
}

func (s *Server) searchOne(name string, query index.Query) ([]hit, uint64, error) {
	ix, ok := s.manager.Index(name)
	if !ok {
		return nil, 0, project.ErrNotFound
	}
	res, err := ix.Search(query)
	if err != nil {
		return nil, 0, err
	}
	return labelHits(name, res), res.Total, nil
}

func (s *Server) searchAll(query index.Query) ([]hit, uint64) {
	merged := []hit{}
	var total uint64
	// Each index is asked for enough to fill the page after merging.
	fetch := clamp(query.Limit+query.Offset, 1, s.cfg().MaxResults)
	for _, p := range s.manager.Projects() {
		ix, ok := s.manager.Index(p.Name)
		if !ok {
			continue
		}
		res, err := ix.Search(index.Query{
			Text: query.Text, Level: query.Level, Service: query.Service,
			Since: query.Since, Until: query.Until, Limit: fetch, Offset: 0,
		})
		if err != nil {
			s.log.Warn("search skipped project", "project", p.Name, "error", err)
			continue
		}
		total += res.Total
		merged = append(merged, labelHits(p.Name, res)...)
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Entry.Time.After(merged[j].Entry.Time) })
	return s.page(merged, query), total
}

func labelHits(name string, res index.Result) []hit {
	hits := make([]hit, 0, len(res.Hits))
	for i, e := range res.Hits {
		hits = append(hits, hit{Project: name, Entry: e, Raw: res.Raw[i]})
	}
	return hits
}

func (s *Server) page(hits []hit, query index.Query) []hit {
	if hits == nil {
		return []hit{}
	}
	if query.Offset >= len(hits) {
		return hits[:0]
	}
	hits = hits[query.Offset:]
	if len(hits) > query.Limit {
		hits = hits[:query.Limit]
	}
	return hits
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("project"))
	if name == "" {
		name = allProjects
	}
	if name == allProjects {
		total := uint64(0)
		levels := map[string]uint64{}
		for _, p := range s.manager.Projects() {
			ix, ok := s.manager.Index(p.Name)
			if !ok {
				continue
			}
			t, l := counts(ix)
			total += t
			for k, v := range l {
				levels[k] += v
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"total": total, "levels": levels})
		return
	}
	ix, ok := s.manager.Index(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	total, levels := counts(ix)
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "levels": levels})
}

type projectSummary struct {
	Name              string            `json:"name"`
	Description       string            `json:"description,omitempty"`
	KeyPrefix         string            `json:"key_prefix"`
	CompressAfterDays int               `json:"compress_after_days"`
	LogsBytes         int64             `json:"logs_bytes"`
	IndexBytes        int64             `json:"index_bytes"`
	TotalBytes        int64             `json:"total_bytes"`
	CreatedAt         time.Time         `json:"created_at"`
	LastUsedAt        time.Time         `json:"last_used_at,omitempty"`
	Total             uint64            `json:"total"`
	Levels            map[string]uint64 `json:"levels"`
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": s.summaries()})
}

func (s *Server) summaries() []projectSummary {
	projects := s.manager.Projects()
	out := make([]projectSummary, 0, len(projects))
	for _, p := range projects {
		summary := projectSummary{
			Name:              p.Name,
			Description:       p.Description,
			KeyPrefix:         p.KeyPrefix,
			CompressAfterDays: p.CompressAfterDays,
			CreatedAt:         p.CreatedAt,
			LastUsedAt:        p.LastUsedAt,
			Levels:            map[string]uint64{},
		}
		if ix, ok := s.manager.Index(p.Name); ok {
			summary.Total, summary.Levels = counts(ix)
		}
		if usage, err := s.manager.Usage(p.Name); err == nil {
			summary.LogsBytes, summary.IndexBytes, summary.TotalBytes = usage.LogsBytes, usage.IndexBytes, usage.TotalBytes
		}
		out = append(out, summary)
	}
	return out
}

func (s *Server) summary(name string) (projectSummary, bool) {
	for _, sum := range s.summaries() {
		if sum.Name == name {
			return sum, true
		}
	}
	return projectSummary{}, false
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unable to read body"})
		return
	}
	var payload struct {
		Name              string `json:"name"`
		Description       string `json:"description"`
		CompressAfterDays *int   `json:"compress_after_days"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	compressAfterDays := s.cfg().CompressAfterDays
	if payload.CompressAfterDays != nil {
		compressAfterDays = *payload.CompressAfterDays
	}
	p, key, err := s.manager.Create(strings.TrimSpace(payload.Name), strings.TrimSpace(payload.Description), compressAfterDays)
	if errors.Is(err, project.ErrInvalidName) || errors.Is(err, project.ErrInvalidDays) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, project.ErrExists) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		s.log.Error("create project failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unable to create project"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"project": p, "key": key})
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	if _, ok := s.manager.Get(name); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unable to read body"})
		return
	}
	var payload struct {
		CompressAfterDays *int `json:"compress_after_days"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if payload.CompressAfterDays == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "compress_after_days is required"})
		return
	}
	if _, err := s.manager.SetCompressAfterDays(name, *payload.CompressAfterDays); errors.Is(err, project.ErrInvalidDays) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	} else if err != nil {
		s.log.Error("update project failed", "project", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unable to update project"})
		return
	}
	if sum, ok := s.summary(name); ok {
		writeJSON(w, http.StatusOK, map[string]any{"project": sum})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": true, "project": name})
}

func (s *Server) handleProjectDays(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	if _, ok := s.manager.Get(name); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	days, err := s.manager.Days(name)
	if err != nil {
		s.log.Error("list days failed", "project", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unable to list stored days"})
		return
	}
	type day struct {
		Date       string `json:"date"`
		Name       string `json:"name"`
		Bytes      int64  `json:"bytes"`
		Compressed bool   `json:"compressed"`
	}
	out := make([]day, 0, len(days))
	for _, d := range days {
		out = append(out, day{
			Date:       d.Date.Format("2006-01-02"),
			Name:       d.Name,
			Bytes:      d.Bytes,
			Compressed: d.Compressed,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": name, "days": out})
}

// handleArchive serves stored log lines for a date range. Without raw=1 it
// returns a JSON page of parsed entries for the viewer; with raw=1 it streams
// the plain JSONL as a download.
func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	if _, ok := s.manager.Get(name); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	params := r.URL.Query()
	from, err := parseDayParam(params.Get("from"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid from: " + err.Error()})
		return
	}
	to, err := parseDayParam(params.Get("to"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid to: " + err.Error()})
		return
	}
	now := time.Now().UTC()
	if from == nil {
		t := now.AddDate(0, 0, -7)
		from = &t
	}
	if to == nil {
		to = &now
	}
	if to.Before(*from) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "to must not be before from"})
		return
	}

	if params.Get("raw") == "1" {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("attachment; filename=%q", name+"-"+from.Format("20060102")+"-"+to.Format("20060102")+".jsonl"))
		_, err := s.manager.ReadRange(name, *from, *to, 0, -1, func(raw []byte) error {
			if _, werr := w.Write(raw); werr != nil {
				return werr
			}
			_, werr := w.Write([]byte("\n"))
			return werr
		})
		if err != nil {
			s.log.Error("archive download failed", "project", name, "error", err)
		}
		return
	}

	skip := max(parseInt(params.Get("offset"), 0), 0)
	limit := clamp(parseInt(params.Get("limit"), 100), 1, s.cfg().MaxResults)
	entries := make([]hit, 0, limit)
	more, err := s.manager.ReadRange(name, *from, *to, skip, limit, func(raw []byte) error {
		e, perr := entry.Parse(raw)
		if perr != nil {
			return nil
		}
		line := string(raw)
		entries = append(entries, hit{Project: name, Entry: e, Raw: line})
		return nil
	})
	if err != nil {
		s.log.Error("archive read failed", "project", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unable to read archive"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project": name,
		"from":    from.Format("2006-01-02"),
		"to":      to.Format("2006-01-02"),
		"count":   len(entries),
		"offset":  skip,
		"limit":   limit,
		"more":    more,
		"entries": entries,
	})
}

func (s *Server) handleRotateProject(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	key, err := s.manager.Rotate(r.PathValue("name"))
	if errors.Is(err, project.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	if err != nil {
		s.log.Error("rotate project failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unable to rotate key"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": key})
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	if err := s.manager.Delete(name); errors.Is(err, project.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	} else if err != nil {
		s.log.Error("delete project failed", "project", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unable to delete project"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "project": name})
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	page, err := assets.ReadFile("index.html")
	if err != nil {
		http.Error(w, "unable to load UI", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(page)
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.adminKey == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "admin key is not configured"})
		return false
	}
	key := r.Header.Get("X-Logrift-Admin")
	if key == "" {
		key = bearer(r)
	}
	if subtle.ConstantTimeCompare([]byte(key), []byte(s.adminKey)) == 1 {
		return true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid admin key"})
	return false
}

func bearer(r *http.Request) string {
	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	}
	return r.Header.Get("X-Logrift-Token")
}

func counts(ix *index.Index) (uint64, map[string]uint64) {
	levels := map[string]uint64{}
	for _, level := range []string{"trace", "debug", "info", "warn", "error", "fatal"} {
		if n, err := ix.Count(level); err == nil {
			levels[level] = n
		}
	}
	total, err := ix.Count("")
	if err != nil {
		total = 0
	}
	return total, levels
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

// parseDayParam parses a YYYY-MM-DD date into the start of that UTC day. An
// empty value yields nil.
func parseDayParam(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", value)
	if err != nil {
		return nil, errors.New("expected a date like 2026-09-01")
	}
	t = t.UTC()
	return &t, nil
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
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
