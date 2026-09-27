package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"logrift.dev/server/internal/entry"
)

func replay(t *testing.T, st *Store, offsets map[string]int64) []string {
	t.Helper()
	var messages []string
	if _, err := st.Scan(offsets, func(_ Position, e entry.Entry, _ []byte) error {
		messages = append(messages, e.Message)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return messages
}

func TestAppendAndScanOrdersOldestFirst(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC()
	older := now.AddDate(0, 0, -2)
	for _, e := range []entry.Entry{
		{Time: now, Level: "info", Message: "newer"},
		{Time: older, Level: "warn", Message: "older"},
	} {
		if _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}

	messages := replay(t, st, nil)
	if len(messages) != 2 || messages[0] != "older" || messages[1] != "newer" {
		t.Fatalf("scan order = %v", messages)
	}
}

func TestScanResumesFromOffset(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC()
	if _, err := st.Append(entry.Entry{Time: now, Level: "info", Message: "first"}); err != nil {
		t.Fatal(err)
	}
	end, err := st.Scan(nil, func(Position, entry.Entry, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(entry.Entry{Time: now, Level: "info", Message: "second"}); err != nil {
		t.Fatal(err)
	}

	messages := replay(t, st, end)
	if len(messages) != 1 || messages[0] != "second" {
		t.Fatalf("resumed scan = %v", messages)
	}
}

func TestRepairTruncatesPartialLine(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	name := filePrefix + now.Format(dateLayout) + fileSuffix
	complete := "{\"level\":\"info\",\"message\":\"complete\"}\n"
	content := complete + "{\"level\":\"info\",\"mess"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	removed, err := st.Repair()
	if err != nil {
		t.Fatal(err)
	}
	if removed != int64(len(content)-len(complete)) {
		t.Fatalf("removed = %d, want %d", removed, len(content)-len(complete))
	}
	if _, err := st.Append(entry.Entry{Time: now, Level: "info", Message: "after repair"}); err != nil {
		t.Fatal(err)
	}

	messages := replay(t, st, nil)
	if len(messages) != 2 || messages[0] != "complete" || messages[1] != "after repair" {
		t.Fatalf("messages = %v", messages)
	}
}

func TestPruneRemovesOldDays(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	old := time.Now().UTC().AddDate(0, 0, -30)
	if _, err := st.Append(entry.Entry{Time: old, Level: "info", Message: "ancient"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(entry.Entry{Time: time.Now().UTC(), Level: "info", Message: "recent"}); err != nil {
		t.Fatal(err)
	}

	if err := st.Prune(7 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}

	messages := replay(t, st, nil)
	if len(messages) != 1 || messages[0] != "recent" {
		t.Fatalf("after prune messages = %v", messages)
	}
}
