// Package entry defines the canonical log record used by logrift and the
// lenient parser that turns arbitrary JSON log lines into that record.
package entry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Entry is the canonical representation of a single log record.
type Entry struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Service string         `json:"service,omitempty"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

// Parse turns a JSON log line into an Entry. It accepts the common structured
// logging shapes: a "level" string or a numeric (pino) level, a "time",
// "timestamp", "ts" or "@timestamp" field, and "service"/"name"/"app" and
// "message"/"msg" aliases. Any remaining fields become Attrs.
func Parse(raw []byte) (Entry, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		return Entry{}, fmt.Errorf("decode log entry: %w", err)
	}
	return fromFields(fields), nil
}

func fromFields(fields map[string]any) Entry {
	e := Entry{Time: time.Now().UTC(), Attrs: map[string]any{}}
	consumed := map[string]bool{}

	if v, ok := lookup(fields, "time", "timestamp", "ts", "@timestamp"); ok {
		if t, ok := parseTime(v); ok {
			e.Time = t.UTC()
		}
	}
	if v, ok := lookup(fields, "level", "severity", "lvl"); ok {
		consumed[levelKey(fields)] = true
		e.Level = normalizeLevel(v)
	}
	if v, ok := lookup(fields, "service", "name", "app"); ok {
		e.Service = toString(v)
	}
	if v, ok := lookup(fields, "message", "msg"); ok {
		e.Message = toString(v)
	}
	if e.Level == "" {
		e.Level = "info"
	}

	for _, k := range []string{"time", "timestamp", "ts", "@timestamp", "level", "severity", "lvl", "service", "name", "app", "message", "msg"} {
		consumed[k] = true
	}
	if nested, ok := fields["attrs"].(map[string]any); ok {
		for k, v := range nested {
			e.Attrs[k] = v
		}
	}
	consumed["attrs"] = true

	for k, v := range fields {
		if !consumed[k] {
			e.Attrs[k] = v
		}
	}
	if len(e.Attrs) == 0 {
		e.Attrs = nil
	}
	return e
}

// Marshal renders the entry as canonical single-line JSON.
func (e Entry) Marshal() ([]byte, error) {
	return json.Marshal(e)
}

// lookup returns the first present key. Keys are matched case-insensitively so
// that "level" and "Level" both work.
func lookup(fields map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		for k, v := range fields {
			if strings.EqualFold(k, key) {
				return v, true
			}
		}
	}
	return nil, false
}

func levelKey(fields map[string]any) string {
	for k := range fields {
		if strings.EqualFold(k, "level") || strings.EqualFold(k, "severity") || strings.EqualFold(k, "lvl") {
			return k
		}
	}
	return ""
}

func toString(v any) string {
	switch value := v.(type) {
	case nil:
		return ""
	case string:
		return value
	default:
		return fmt.Sprint(value)
	}
}

// normalizeLevel accepts string levels ("warn", "WARNING", "err") and numeric
// pino levels (10 trace ... 60 fatal), returning a lowercase canonical level.
func normalizeLevel(v any) string {
	switch value := v.(type) {
	case json.Number:
		if n, err := value.Int64(); err == nil {
			return levelFromNumber(n)
		}
	case float64:
		return levelFromNumber(int64(value))
	case string:
		return levelFromString(value)
	}
	return "info"
}

func levelFromNumber(n int64) string {
	switch {
	case n >= 60:
		return "fatal"
	case n >= 50:
		return "error"
	case n >= 40:
		return "warn"
	case n >= 30:
		return "info"
	case n >= 20:
		return "debug"
	default:
		return "trace"
	}
}

func levelFromString(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace":
		return "trace"
	case "debug":
		return "debug"
	case "info", "information", "notice", "log":
		return "info"
	case "warn", "warning":
		return "warn"
	case "error", "err":
		return "error"
	case "fatal", "critical", "crit", "panic", "emerg", "alert":
		return "fatal"
	default:
		return "info"
	}
}

// parseTime accepts RFC3339 strings and Unix timestamps in seconds,
// milliseconds, microseconds or nanoseconds. A value that resolves outside the
// range encoding/json can represent is treated as absent so the caller keeps
// its default instead of failing to marshal later.
func parseTime(v any) (time.Time, bool) {
	var t time.Time
	switch value := v.(type) {
	case string:
		parsed, ok := parseTimeString(value)
		if !ok {
			return time.Time{}, false
		}
		t = parsed
	case json.Number:
		n, err := value.Float64()
		if err != nil {
			return time.Time{}, false
		}
		t = parseTimeNumber(n)
	case float64:
		t = parseTimeNumber(value)
	default:
		return time.Time{}, false
	}
	if !jsonTimeRange(t) {
		return time.Time{}, false
	}
	return t, true
}

// jsonTimeRange reports whether t has a year the JSON time encoding accepts;
// encoding/json rejects years outside [0, 9999].
func jsonTimeRange(t time.Time) bool {
	return t.Year() >= 0 && t.Year() <= 9999
}

func parseTimeString(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.000", "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return parseTimeNumber(n), true
	}
	return time.Time{}, false
}

func parseTimeNumber(n float64) time.Time {
	switch {
	case n >= 1e18:
		return time.Unix(0, int64(n))
	case n >= 1e15:
		return time.Unix(0, int64(n)*int64(time.Microsecond))
	case n >= 1e12:
		return time.Unix(0, int64(n)*int64(time.Millisecond))
	default:
		seconds := int64(n)
		nanos := int64((n - float64(seconds)) * float64(time.Second))
		return time.Unix(seconds, nanos)
	}
}
