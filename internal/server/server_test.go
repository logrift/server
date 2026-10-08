package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"logrift.dev/server/internal/config"
	"logrift.dev/server/internal/entry"
	"logrift.dev/server/internal/manager"
)

const adminKey = "admin-secret-key"

func newTestServer(t *testing.T) (*httptest.Server, *manager.Manager) {
	t.Helper()
	mgr, err := manager.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	srv := New(mgr, Options{AdminKey: adminKey}, nil)
	return httptest.NewServer(srv.Handler()), mgr
}

func request(t *testing.T, method, url, body, key string, admin bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if admin {
		req.Header.Set("Authorization", "Bearer "+adminKey)
	} else if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func decode(t *testing.T, res *http.Response, out any) {
	t.Helper()
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

func TestIngestRequiresProjectKey(t *testing.T) {
	ts, mgr := newTestServer(t)
	defer ts.Close()
	_, key, err := mgr.Create("dbmodeller", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	if res := request(t, http.MethodPost, ts.URL+"/api/logs", `{"msg":"hi"}`, "", false); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key status = %d, want 401", res.StatusCode)
	}
	res := request(t, http.MethodPost, ts.URL+"/api/logs", `{"msg":"hi"}`, key, false)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("with key status = %d, want 202", res.StatusCode)
	}
	var accepted map[string]any
	decode(t, res, &accepted)
	if accepted["project"] != "dbmodeller" {
		t.Fatalf("project = %v", accepted["project"])
	}
}

func TestSearchRequiresAdminAndLabelsProject(t *testing.T) {
	ts, mgr := newTestServer(t)
	defer ts.Close()
	_, key, _ := mgr.Create("api", "", 0)
	request(t, http.MethodPost, ts.URL+"/api/logs", `[{"level":"error","msg":"boom"}]`, key, false).Body.Close()

	if res := request(t, http.MethodGet, ts.URL+"/api/search", "", "", false); res.StatusCode != http.StatusUnauthorized {
		res.Body.Close()
		t.Fatalf("search without admin = %d, want 401", res.StatusCode)
	}
	res := request(t, http.MethodGet, ts.URL+"/api/search?project=all", "", "", true)
	var result struct {
		Total   int `json:"total"`
		Entries []struct {
			Project string `json:"project"`
			Message string `json:"message"`
		} `json:"entries"`
	}
	decode(t, res, &result)
	if result.Total != 1 || result.Entries[0].Project != "api" || result.Entries[0].Message != "boom" {
		t.Fatalf("search result = %+v", result)
	}
}

func TestProjectLifecycle(t *testing.T) {
	ts, _ := newTestServer(t)
	defer ts.Close()

	// Create
	res := request(t, http.MethodPost, ts.URL+"/api/projects", `{"name":"web","description":"frontend"}`, "", true)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", res.StatusCode)
	}
	var created struct {
		Key string `json:"key"`
	}
	decode(t, res, &created)
	if !strings.HasPrefix(created.Key, "lr_") {
		t.Fatalf("key = %q", created.Key)
	}

	// Duplicate
	if res := request(t, http.MethodPost, ts.URL+"/api/projects", `{"name":"web"}`, "", true); res.StatusCode != http.StatusConflict {
		res.Body.Close()
		t.Fatalf("duplicate status = %d, want 409", res.StatusCode)
	}
	// Invalid name
	if res := request(t, http.MethodPost, ts.URL+"/api/projects", `{"name":"Bad Name"}`, "", true); res.StatusCode != http.StatusBadRequest {
		res.Body.Close()
		t.Fatalf("invalid name status = %d, want 400", res.StatusCode)
	}

	// Listing includes the project
	var listed struct {
		Projects []struct {
			Name      string `json:"name"`
			KeyPrefix string `json:"key_prefix"`
		} `json:"projects"`
	}
	decode(t, request(t, http.MethodGet, ts.URL+"/api/projects", "", "", true), &listed)
	if len(listed.Projects) != 1 || listed.Projects[0].Name != "web" || listed.Projects[0].KeyPrefix == "" {
		t.Fatalf("listed = %+v", listed)
	}

	// Rotate invalidates the old key
	var rotated struct {
		Key string `json:"key"`
	}
	decode(t, request(t, http.MethodPost, ts.URL+"/api/projects/web/rotate", "", "", true), &rotated)
	if rotated.Key == created.Key || !strings.HasPrefix(rotated.Key, "lr_") {
		t.Fatalf("rotated key = %q", rotated.Key)
	}
	if res := request(t, http.MethodPost, ts.URL+"/api/logs", `{"msg":"old"}`, created.Key, false); res.StatusCode != http.StatusUnauthorized {
		res.Body.Close()
		t.Fatalf("old key status = %d, want 401", res.StatusCode)
	}
	if res := request(t, http.MethodPost, ts.URL+"/api/logs", `{"msg":"new"}`, rotated.Key, false); res.StatusCode != http.StatusAccepted {
		res.Body.Close()
		t.Fatalf("new key status = %d, want 202", res.StatusCode)
	}

	// Delete
	if res := request(t, http.MethodDelete, ts.URL+"/api/projects/web", "", "", true); res.StatusCode != http.StatusOK {
		res.Body.Close()
		t.Fatalf("delete status = %d", res.StatusCode)
	}
	if res := request(t, http.MethodGet, ts.URL+"/api/search?project=web", "", "", true); res.StatusCode != http.StatusNotFound {
		res.Body.Close()
		t.Fatalf("search deleted project = %d, want 404", res.StatusCode)
	}
}

func TestIngestNDJSON(t *testing.T) {
	ts, mgr := newTestServer(t)
	defer ts.Close()
	_, key, _ := mgr.Create("svc", "", 0)

	body := "{\"msg\":\"one\"}\n{\"msg\":\"two\"}\n"
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/logs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var accepted map[string]any
	decode(t, res, &accepted)
	if accepted["accepted"] != float64(2) {
		t.Fatalf("accepted = %v, want 2", accepted["accepted"])
	}
}

func TestSearchEmptyEntriesIsArray(t *testing.T) {
	ts, mgr := newTestServer(t)
	defer ts.Close()
	if _, _, err := mgr.Create("api", "", 0); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"/api/search?project=all&q=nomatch", "/api/search?project=api&q=nomatch"} {
		res := request(t, http.MethodGet, ts.URL+url, "", "", true)
		var result struct {
			Total   int              `json:"total"`
			Entries []map[string]any `json:"entries"`
		}
		decode(t, res, &result)
		if result.Entries == nil {
			t.Fatalf("%s: entries was null, want []", url)
		}
		if result.Total != 0 {
			t.Fatalf("%s: total = %d, want 0", url, result.Total)
		}
	}
}

func TestHealth(t *testing.T) {
	ts, _ := newTestServer(t)
	defer ts.Close()
	res := request(t, http.MethodGet, ts.URL+"/healthz", "", "", false)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", res.StatusCode)
	}
	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestReady(t *testing.T) {
	ts, mgr := newTestServer(t)
	defer ts.Close()
	if _, _, err := mgr.Create("api", "", 0); err != nil {
		t.Fatal(err)
	}
	res := request(t, http.MethodGet, ts.URL+"/readyz", "", "", false)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("ready status = %d, want 200", res.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ready" {
		t.Fatalf("status = %q, want ready", body["status"])
	}
}

func TestSearchNoiseFilters(t *testing.T) {
	ts, mgr := newTestServer(t)
	defer ts.Close()
	for _, name := range []string{"web", "other"} {
		_, key, err := mgr.Create(name, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		res := request(t, http.MethodPost, ts.URL+"/api/logs", `[
			{"time":"2026-10-05T10:00:00Z","msg":"customer","path":"/products"},
			{"time":"2026-10-05T10:01:00Z","msg":"scan","path":"/.env"},
			{"time":"2026-10-05T10:02:00Z","msg":"probe","user_agent":"UptimeRobot/2.0"},
			{"time":"2026-10-05T10:03:00Z","msg":"customer","path":"/checkout"}
		]`, key, false)
		res.Body.Close()
		if res.StatusCode != http.StatusAccepted {
			t.Fatalf("ingest = %d", res.StatusCode)
		}
	}
	for _, tc := range []struct {
		params       string
		total, count int
		message      string
	}{
		{"project=web", 4, 4, "customer"},
		{"project=web&hide_monitors=true", 3, 3, "customer"},
		{"project=web&hide_scans=true", 3, 3, "customer"},
		{"project=web&hide_monitors=true&hide_scans=true&limit=1&offset=1", 2, 1, "customer"},
		{"project=all&hide_monitors=1&hide_scans=1&limit=2&offset=2", 4, 2, "customer"},
		{"project=all&hide_monitors=false&hide_scans=false", 8, 8, "customer"},
		{"project=web&hide_monitors=true&hide_scans=true&q=scan", 0, 0, ""},
	} {
		var result struct {
			Total, Count int
			Entries      []struct{ Message string }
		}
		decode(t, request(t, http.MethodGet, ts.URL+"/api/search?"+tc.params, "", "", true), &result)
		if result.Total != tc.total || result.Count != tc.count {
			t.Fatalf("%s: result = %+v", tc.params, result)
		}
		if len(result.Entries) > 0 && result.Entries[0].Message != tc.message {
			t.Fatalf("%s: wrong entry %+v", tc.params, result)
		}
	}
	for _, param := range []string{"hide_monitors=bad", "hide_scans=bad"} {
		res := request(t, http.MethodGet, ts.URL+"/api/search?"+param, "", "", true)
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d", param, res.StatusCode)
		}
	}
	var stats struct{ Total int }
	decode(t, request(t, http.MethodGet, ts.URL+"/api/stats?project=all", "", "", true), &stats)
	if stats.Total != 8 {
		t.Fatalf("stored total = %d", stats.Total)
	}
}

func TestNoiseSignaturesApplyToHistoricalLogsImmediately(t *testing.T) {
	ts, mgr := newTestServer(t)
	defer ts.Close()
	_, key, err := mgr.Create("custom", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	res := request(t, http.MethodPost, ts.URL+"/api/logs", `[
		{"msg":"custom probe","req":{"headers":{"User-Agent":"My-Monitor/2"},"url":"/"}},
		{"msg":"custom scan","path":"/%77p-login.php?test=1"},
		{"msg":"customer","path":"/products"},
		{"msg":"default probe","user_agent":"UptimeRobot/2.0"}
	]`, key, false)
	res.Body.Close()
	check := func(want int) {
		t.Helper()
		for _, project := range []string{"custom", "all"} {
			var result struct{ Total, Count int }
			decode(t, request(t, http.MethodGet, ts.URL+"/api/search?project="+project+"&hide_monitors=true&hide_scans=true&limit=1", "", "", true), &result)
			if result.Total != want || result.Count != 1 {
				t.Fatalf("%s: got %+v, want total %d", project, result, want)
			}
		}
	}
	check(3)
	// Replace defaults after ingest. Nothing is rewritten or reingested.
	res = request(t, http.MethodPatch, ts.URL+"/api/settings", `{"monitor_user_agents":["*MY-MONITOR*"],"bot_scan_paths":["/wp-login.ph?"]}`, "", true)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("save status = %d", res.StatusCode)
	}
	check(2)
	// A rejected edit must not partially apply either category.
	res = request(t, http.MethodPatch, ts.URL+"/api/settings", `{"monitor_user_agents":[],"bot_scan_paths":[""]}`, "", true)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid edit status = %d", res.StatusCode)
	}
	check(2)
	res = request(t, http.MethodPatch, ts.URL+"/api/settings", `{"monitor_user_agents":[],"bot_scan_paths":[]}`, "", true)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("clear status = %d", res.StatusCode)
	}
	check(4)
}

func TestSettingsEndpoints(t *testing.T) {
	ts, _ := newTestServer(t)
	defer ts.Close()

	var payload struct {
		Settings        config.Config `json:"settings"`
		RestartRequired []string      `json:"restart_required"`
	}
	decode(t, request(t, http.MethodGet, ts.URL+"/api/settings", "", "", true), &payload)
	if payload.Settings.CompressAfterDays != 14 || payload.Settings.CompressIntervalMin != 60 {
		t.Fatalf("default settings = %+v", payload.Settings)
	}
	if len(payload.RestartRequired) == 0 {
		t.Fatal("restart_required was empty")
	}
	if res := request(t, http.MethodGet, ts.URL+"/api/settings", "", "", false); res.StatusCode != http.StatusUnauthorized {
		res.Body.Close()
		t.Fatalf("settings without admin = %d, want 401", res.StatusCode)
	}

	res := request(t, http.MethodPatch, ts.URL+"/api/settings", `{"compress_after_days":3,"compress_interval_min":5,"max_results":250}`, "", true)
	decode(t, res, &payload)
	if payload.Settings.CompressAfterDays != 3 || payload.Settings.CompressIntervalMin != 5 || payload.Settings.MaxResults != 250 {
		t.Fatalf("updated settings = %+v", payload.Settings)
	}

	// The configured default applies to projects created afterwards.
	var created struct {
		Project projectSummary `json:"project"`
	}
	decode(t, request(t, http.MethodPost, ts.URL+"/api/projects", `{"name":"later"}`, "", true), &created)
	if created.Project.CompressAfterDays != 3 {
		t.Fatalf("created compress_after_days = %d, want 3", created.Project.CompressAfterDays)
	}

	for _, body := range []string{`{"compress_after_days":-1}`, `{"max_results":0}`, `{"addr":""}`, `{"max_body_kb":-5}`, `{"compress_interval_min":-2}`} {
		if res := request(t, http.MethodPatch, ts.URL+"/api/settings", body, "", true); res.StatusCode != http.StatusBadRequest {
			res.Body.Close()
			t.Fatalf("patch %s status = %d, want 400", body, res.StatusCode)
		}
	}
	decode(t, request(t, http.MethodGet, ts.URL+"/api/settings", "", "", true), &payload)
	if payload.Settings.CompressAfterDays != 3 || payload.Settings.MaxResults != 250 {
		t.Fatalf("settings after rejected patches = %+v", payload.Settings)
	}
}

func TestProjectSettingsAndStorageUsage(t *testing.T) {
	ts, mgr := newTestServer(t)
	defer ts.Close()
	if _, _, err := mgr.Create("web", "", 14); err != nil {
		t.Fatal(err)
	}
	collector, _ := mgr.Collector("web")
	if err := collector.Write([]entry.Entry{{Time: time.Now().UTC(), Level: "info", Message: "hello"}}); err != nil {
		t.Fatal(err)
	}

	var listed struct {
		Projects []projectSummary `json:"projects"`
	}
	decode(t, request(t, http.MethodGet, ts.URL+"/api/projects", "", "", true), &listed)
	if len(listed.Projects) != 1 {
		t.Fatalf("projects = %+v", listed)
	}
	sum := listed.Projects[0]
	if sum.CompressAfterDays != 14 {
		t.Fatalf("compress_after_days = %d, want 14", sum.CompressAfterDays)
	}
	if sum.LogsBytes <= 0 || sum.IndexBytes <= 0 || sum.TotalBytes != sum.LogsBytes+sum.IndexBytes {
		t.Fatalf("usage = logs %d index %d total %d", sum.LogsBytes, sum.IndexBytes, sum.TotalBytes)
	}

	res := request(t, http.MethodPatch, ts.URL+"/api/projects/web", `{"compress_after_days": 3}`, "", true)
	var updated struct {
		Project projectSummary `json:"project"`
	}
	decode(t, res, &updated)
	if updated.Project.CompressAfterDays != 3 {
		t.Fatalf("updated compress_after_days = %d, want 3", updated.Project.CompressAfterDays)
	}

	for _, body := range []string{`{}`, `{"compress_after_days":-1}`} {
		res := request(t, http.MethodPatch, ts.URL+"/api/projects/web", body, "", true)
		if res.StatusCode != http.StatusBadRequest {
			res.Body.Close()
			t.Fatalf("patch %s status = %d, want 400", body, res.StatusCode)
		}
	}
	if res := request(t, http.MethodPatch, ts.URL+"/api/projects/missing", `{"compress_after_days":1}`, "", true); res.StatusCode != http.StatusNotFound {
		res.Body.Close()
		t.Fatalf("patch missing status = %d, want 404", res.StatusCode)
	}
}

func TestDaysAndArchiveEndpoints(t *testing.T) {
	ts, mgr := newTestServer(t)
	defer ts.Close()
	if _, _, err := mgr.Create("web", "", 0); err != nil {
		t.Fatal(err)
	}
	collector, _ := mgr.Collector("web")
	old := time.Now().UTC().AddDate(0, 0, -30)
	if err := collector.Write([]entry.Entry{
		{Time: old, Level: "error", Message: "ancient boom"},
		{Time: time.Now().UTC(), Level: "info", Message: "fresh"},
	}); err != nil {
		t.Fatal(err)
	}

	from, to := old.Format("2006-01-02"), time.Now().UTC().Format("2006-01-02")

	var days struct {
		Days []struct {
			Date       string `json:"date"`
			Bytes      int64  `json:"bytes"`
			Compressed bool   `json:"compressed"`
		} `json:"days"`
	}
	decode(t, request(t, http.MethodGet, ts.URL+"/api/projects/web/days", "", "", true), &days)
	if len(days.Days) != 2 || days.Days[0].Date != from || days.Days[0].Bytes <= 0 {
		t.Fatalf("days = %+v", days)
	}

	archiveURL := fmt.Sprintf("%s/api/projects/web/archive?from=%s&to=%s", ts.URL, from, to)
	var page struct {
		Entries []struct {
			Message string `json:"message"`
		} `json:"entries"`
		More bool `json:"more"`
	}
	decode(t, request(t, http.MethodGet, archiveURL, "", "", true), &page)
	if len(page.Entries) != 2 || page.Entries[0].Message != "ancient boom" || page.Entries[1].Message != "fresh" {
		t.Fatalf("archive page = %+v", page)
	}
	if page.More {
		t.Fatal("more = true, want false")
	}
	decode(t, request(t, http.MethodGet, archiveURL+"&limit=1", "", "", true), &page)
	if len(page.Entries) != 1 || !page.More {
		t.Fatalf("limited archive page = %+v", page)
	}

	res := request(t, http.MethodGet, archiveURL+"&raw=1", "", "", true)
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "ancient boom") || !strings.Contains(string(body), "fresh") {
		t.Fatalf("raw archive = %q", body)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Fatalf("raw content type = %q", ct)
	}

	// Compressed days stay readable through the archive endpoints.
	if _, err := mgr.SetCompressAfterDays("web", 7); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Compress(); err != nil {
		t.Fatal(err)
	}
	var compressed struct {
		Days []struct {
			Compressed bool `json:"compressed"`
		} `json:"days"`
	}
	decode(t, request(t, http.MethodGet, ts.URL+"/api/projects/web/days", "", "", true), &compressed)
	if len(compressed.Days) != 2 || !compressed.Days[0].Compressed || compressed.Days[1].Compressed {
		t.Fatalf("days after compress = %+v", compressed)
	}
	decode(t, request(t, http.MethodGet, archiveURL, "", "", true), &page)
	if len(page.Entries) != 2 {
		t.Fatalf("archive after compress = %+v", page)
	}
}

func TestArchiveValidatesDates(t *testing.T) {
	ts, mgr := newTestServer(t)
	defer ts.Close()
	if _, _, err := mgr.Create("web", "", 0); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{
		"/api/projects/web/archive?from=not-a-date",
		"/api/projects/web/archive?from=2026-02-30",
		"/api/projects/web/archive?from=2026-09-02&to=2026-09-01",
	} {
		if res := request(t, http.MethodGet, ts.URL+url, "", "", true); res.StatusCode != http.StatusBadRequest {
			res.Body.Close()
			t.Fatalf("%s status = %d, want 400", url, res.StatusCode)
		}
	}
	if res := request(t, http.MethodGet, ts.URL+"/api/projects/missing/archive", "", "", true); res.StatusCode != http.StatusNotFound {
		res.Body.Close()
		t.Fatalf("missing project status = %d, want 404", res.StatusCode)
	}
}

func TestAccessLogMiddleware(t *testing.T) {
	mgr, err := manager.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	// Create the access log project manually (normally done at startup).
	if _, _, err := mgr.Create("logrift", "", 0); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.AccessLogProject = "logrift"
	srv := New(mgr, Options{AdminKey: adminKey, Settings: config.NewStore(cfg)}, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Make a request that will be logged.
	res := request(t, http.MethodGet, ts.URL+"/api/stats", "", "", true)
	res.Body.Close()

	// Search the access log project for the entry.
	ix, ok := mgr.Index("logrift")
	if !ok {
		t.Fatal("access log index not found")
	}
	total, err := ix.DocCount()
	if err != nil {
		t.Fatal(err)
	}
	if total < 1 {
		t.Fatalf("access log entries = %d, want >= 1", total)
	}
}

func TestAccessLogMiddlewareExcludesHealthProbes(t *testing.T) {
	mgr, err := manager.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	if _, _, err := mgr.Create("logrift", "", 0); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.AccessLogProject = "logrift"
	srv := New(mgr, Options{AdminKey: adminKey, Settings: config.NewStore(cfg)}, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Hit health and ready endpoints.
	request(t, http.MethodGet, ts.URL+"/healthz", "", "", false).Body.Close()
	request(t, http.MethodGet, ts.URL+"/readyz", "", "", false).Body.Close()

	ix, ok := mgr.Index("logrift")
	if !ok {
		t.Fatal("access log index not found")
	}
	count, err := ix.DocCount()
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("health probes logged %d entries, want 0", count)
	}
}

func TestAccessLogMiddlewareDisabledWhenEmpty(t *testing.T) {
	mgr, err := manager.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	cfg := config.Defaults()
	cfg.AccessLogProject = ""
	srv := New(mgr, Options{AdminKey: adminKey, Settings: config.NewStore(cfg)}, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Make a request — should not panic or create a project.
	request(t, http.MethodGet, ts.URL+"/api/stats", "", "", true).Body.Close()

	if _, ok := mgr.Get("logrift"); ok {
		t.Fatal("logrift project should not exist when access log is disabled")
	}
}
