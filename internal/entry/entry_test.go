package entry

import (
	"fmt"
	"testing"
	"time"
)

func TestParsePinoShape(t *testing.T) {
	e, err := Parse([]byte(`{"level":50,"time":"2026-09-27T10:00:00Z","msg":"request failed","service":"dbmodeller","status":500}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.Level != "error" {
		t.Errorf("level = %q, want error", e.Level)
	}
	if e.Service != "dbmodeller" {
		t.Errorf("service = %q", e.Service)
	}
	if e.Message != "request failed" {
		t.Errorf("message = %q", e.Message)
	}
	if !e.Time.Equal(time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("time = %v", e.Time)
	}
	if fmt.Sprint(e.Attrs["status"]) != "500" {
		t.Errorf("status attr = %v", e.Attrs["status"])
	}
}

func TestParseLevelAliases(t *testing.T) {
	cases := map[string]string{
		"WARNING":  "warn",
		"err":      "error",
		"critical": "fatal",
		"debug":    "debug",
	}
	for input, want := range cases {
		e, err := Parse([]byte(`{"level":"` + input + `"}`))
		if err != nil {
			t.Fatalf("parse level %q: %v", input, err)
		}
		if e.Level != want {
			t.Errorf("level %q = %q, want %q", input, e.Level, want)
		}
	}
}

func TestParseEpochMillis(t *testing.T) {
	e, err := Parse([]byte(`{"ts":1758967200000,"level":30,"message":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := time.UnixMilli(1758967200000).UTC()
	if !e.Time.Equal(want) {
		t.Errorf("time = %v, want %v", e.Time, want)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	raw, err := Parse([]byte(`{"level":"warn","message":"slow","duration_ms":12,"path":"/app"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := raw.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if again.Level != "warn" || again.Message != "slow" {
		t.Fatalf("round trip lost fields: %+v", again)
	}
}
