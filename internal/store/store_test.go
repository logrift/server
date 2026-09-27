package store

import (
	"os"
	"path/filepath"
	"strings"
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

func TestCompressArchivesOldDays(t *testing.T) {
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

	cutoff := dayStart(time.Now().UTC()).AddDate(0, 0, -7)
	compressed, err := st.Compress(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(compressed) != 1 {
		t.Fatalf("compressed = %v, want one file", compressed)
	}
	oldName := filePrefix + old.Format(dateLayout) + fileSuffix
	if _, err := os.Stat(filepath.Join(dir, oldName)); !os.IsNotExist(err) {
		t.Fatalf("uncompressed %s still exists", oldName)
	}
	if _, err := os.Stat(filepath.Join(dir, oldName+gzSuffix)); err != nil {
		t.Fatalf("compressed archive missing: %v", err)
	}

	days, err := st.Days()
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("days = %+v", days)
	}
	if !days[0].Compressed || days[1].Compressed {
		t.Fatalf("days order/flags = %+v", days)
	}

	messages := replay(t, st, nil)
	if len(messages) != 1 || messages[0] != "recent" {
		t.Fatalf("scan after compress = %v (compressed days must not be indexed)", messages)
	}
	lines := readRange(t, st, old, old, 0, -1)
	if len(lines) != 1 || !strings.Contains(lines[0], "ancient") {
		t.Fatalf("archive lines = %v", lines)
	}
}

func TestCompressMergesIntoExistingArchive(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().UTC().AddDate(0, 0, -30)
	name := filePrefix + old.Format(dateLayout) + fileSuffix
	if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"level":"info","message":"first"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cutoff := dayStart(time.Now().UTC()).AddDate(0, 0, -7)
	if _, err := st.Compress(cutoff); err != nil {
		t.Fatal(err)
	}
	// A backdated append lands in the already-compressed day; a later entry
	// rotates the active file away from it again.
	if _, err := st.Append(entry.Entry{Time: old, Level: "info", Message: "backfill"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(entry.Entry{Time: time.Now().UTC(), Level: "info", Message: "later"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Compress(cutoff); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Fatalf("uncompressed %s still exists", name)
	}

	lines := readRange(t, st, old, old, 0, -1)
	if len(lines) != 2 || !strings.Contains(lines[0], "first") || !strings.Contains(lines[1], "backfill") {
		t.Fatalf("merged archive lines = %v", lines)
	}
}

func TestReadRangeSkipsAndLimits(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	day := time.Now().UTC().AddDate(0, 0, -2)
	for i, msg := range []string{"one", "two", "three", "four"} {
		if _, err := st.Append(entry.Entry{Time: day.Add(time.Duration(i) * time.Minute), Level: "info", Message: msg}); err != nil {
			t.Fatal(err)
		}
	}

	lines := readRange(t, st, day, day, 1, 2)
	if len(lines) != 2 || !strings.Contains(lines[0], "two") || !strings.Contains(lines[1], "three") {
		t.Fatalf("skip/limit lines = %v", lines)
	}
	more, err := st.ReadRange(day, day, 1, 2, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !more {
		t.Fatal("more = false, want true")
	}
	more, err = st.ReadRange(day, day, 3, 2, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if more {
		t.Fatal("more = true at end of range, want false")
	}
}

func readRange(t *testing.T, st *Store, from, to time.Time, skip, limit int) []string {
	t.Helper()
	var lines []string
	if _, err := st.ReadRange(from, to, skip, limit, func(raw []byte) error {
		lines = append(lines, string(raw))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return lines
}
