package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"logrift.dev/server/internal/collect"
	"logrift.dev/server/internal/index"
	"logrift.dev/server/internal/store"
)

func newTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ix, _, err := index.Open(t.TempDir() + "/index")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close(); _ = st.Close() })
	srv := New(collect.New(st, ix), ix, Options{Token: token}, nil)
	return httptest.NewServer(srv.Handler())
}

func TestIngestAndSearch(t *testing.T) {
	ts := newTestServer(t, "")
	defer ts.Close()

	body := `[{"level":"error","service":"dbmodeller","msg":"boom","path":"/app"},{"level":"info","msg":"ok"}]`
	res, err := http.Post(ts.URL+"/api/logs", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("ingest status = %d", res.StatusCode)
	}
	var accepted map[string]int
	if err := json.NewDecoder(res.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	if accepted["accepted"] != 2 {
		t.Fatalf("accepted = %d, want 2", accepted["accepted"])
	}

	search, err := http.Get(ts.URL + "/api/search?level=error")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Total   int `json:"total"`
		Entries []struct {
			Message string `json:"message"`
			Level   string `json:"level"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(search.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || result.Entries[0].Message != "boom" {
		t.Fatalf("search result = %+v", result)
	}
}

func TestIngestRequiresToken(t *testing.T) {
	ts := newTestServer(t, "secret")
	defer ts.Close()

	res, err := http.Post(ts.URL+"/api/logs", "application/json", bytes.NewBufferString(`{"msg":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/logs", bytes.NewBufferString(`{"msg":"hi"}`))
	req.Header.Set("Authorization", "Bearer secret")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("authorized status = %d, want 202", res.StatusCode)
	}
}

func TestIngestNDJSON(t *testing.T) {
	ts := newTestServer(t, "")
	defer ts.Close()

	body := "{\"msg\":\"one\"}\n{\"msg\":\"two\"}\n"
	res, err := http.Post(ts.URL+"/api/logs", "application/x-ndjson", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	var accepted map[string]int
	if err := json.NewDecoder(res.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	if accepted["accepted"] != 2 {
		t.Fatalf("accepted = %d, want 2", accepted["accepted"])
	}
}

func TestHealth(t *testing.T) {
	ts := newTestServer(t, "")
	defer ts.Close()

	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", res.StatusCode)
	}
}
