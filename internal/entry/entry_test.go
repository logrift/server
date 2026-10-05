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

func TestParseOutOfRangeTimestampFallsBackToNow(t *testing.T) {
	for _, raw := range []string{
		`{"time":270000000000}`,
		`{"ts":100000000000000000}`,
		`{"timestamp":999999999999999}`,
		`{"time":"999999999999"}`,
	} {
		e, err := Parse([]byte(raw))
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		if e.Time.Year() > 9999 {
			t.Fatalf("%s: year = %d, want a JSON-representable time", raw, e.Time.Year())
		}
		if _, err := e.Marshal(); err != nil {
			t.Fatalf("%s: marshal: %v", raw, err)
		}
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

// FuzzParse checks that arbitrary input never panics and that any accepted
// record is canonical: a known level, a set time, and a stable JSON round trip.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		`{"level":"error","time":"2026-09-27T10:00:00Z","msg":"boom","service":"api","status":500}`,
		`{"level":50,"time":1758967200000,"msg":"request failed","duration_ms":12}`,
		`{"timestamp":"2026-09-27T10:00:00.123456789Z","severity":"warning","message":"slow"}`,
		`{"@timestamp":1758967200.5,"lvl":"debug","name":"worker"}`,
		`{"level":"info","attrs":{"path":"/a/b?x=1","user_agent":"UptimeRobot/2.0","nested":{"url":"https://ex.com/x"}},"message":"GET /a/b HTTP/1.1"}`,
		`{"level":{},"time":[],"msg":123}`,
		`{"level":"info"}`,
		`[{"level":"info"}]`,
		`null`,
		`not json`,
		``,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		e, err := Parse(raw)
		if err != nil {
			return
		}
		switch e.Level {
		case "trace", "debug", "info", "warn", "error", "fatal":
		default:
			t.Fatalf("non-canonical level %q from %q", e.Level, raw)
		}
		if e.Time.IsZero() {
			t.Fatalf("zero timestamp from %q", raw)
		}
		// Must not panic on arbitrary attribute shapes or messages.
		e.RequestFields()

		encoded, err := e.Marshal()
		if err != nil {
			t.Fatalf("marshal %+v: %v", e, err)
		}
		again, err := Parse(encoded)
		if err != nil {
			t.Fatalf("reparse %q: %v", encoded, err)
		}
		if again.Level != e.Level || again.Message != e.Message || again.Service != e.Service || !again.Time.Equal(e.Time) {
			t.Fatalf("round trip changed %+v to %+v", e, again)
		}
	})
}
