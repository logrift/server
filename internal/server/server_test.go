package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	_, key, err := mgr.Create("dbmodeller", "")
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
	_, key, _ := mgr.Create("api", "")
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
	_, key, _ := mgr.Create("svc", "")

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
	if _, _, err := mgr.Create("api", ""); err != nil {
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
}
