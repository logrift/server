// Package store persists log entries as newline-delimited JSON files, one file
// per UTC day, and scans them for indexing. The files are the durable source of
// truth; the search index is derived from them.
package store

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"logrift.dev/server/internal/entry"
)

const (
	filePrefix = "logs-"
	fileSuffix = ".jsonl"
	dateLayout = "20060102"
)

// Position locates a stored line: the day file, the byte offset where the line
// starts, and the byte offset just past its trailing newline.
type Position struct {
	File  string
	Start int64
	End   int64
}

// Store appends entries to daily JSONL files under a directory.
type Store struct {
	dir string

	mu   sync.Mutex
	file *os.File
	date string
	size int64
}

// Open creates the directory if needed and returns a store ready for appends.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir returns the directory backing the store.
func (s *Store) Dir() string { return s.dir }

// Append writes an entry to the day file selected by its timestamp.
func (s *Store) Append(e entry.Entry) (Position, error) {
	raw, err := e.Marshal()
	if err != nil {
		return Position{}, err
	}
	return s.AppendRaw(e.Time.UTC(), raw)
}

// AppendRaw writes a pre-encoded canonical JSON line for time t.
func (s *Store) AppendRaw(t time.Time, raw []byte) (Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	date := t.UTC().Format(dateLayout)
	if s.file == nil || s.date != date {
		if err := s.rotateLocked(date); err != nil {
			return Position{}, err
		}
	}
	start := s.size
	line := make([]byte, 0, len(raw)+1)
	line = append(line, raw...)
	line = append(line, '\n')
	n, err := s.file.Write(line)
	s.size += int64(n)
	if err != nil {
		return Position{}, fmt.Errorf("write log line: %w", err)
	}
	return Position{File: filePrefix + date + fileSuffix, Start: start, End: s.size}, nil
}

func (s *Store) rotateLocked(date string) error {
	if s.file != nil {
		_ = s.file.Sync()
		_ = s.file.Close()
		s.file = nil
	}
	path := filepath.Join(s.dir, filePrefix+date+fileSuffix)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("stat log file: %w", err)
	}
	s.file, s.date, s.size = file, date, info.Size()
	return nil
}

// Scan streams stored entries oldest first. For each file it starts at
// offsets[name] (clamped to the file size), which lets callers index only the
// tail written since a previous run. It returns the end offset of every file it
// read, so callers can persist their new position.
func (s *Store) Scan(offsets map[string]int64, fn func(Position, entry.Entry, []byte) error) (map[string]int64, error) {
	files, err := s.days()
	if err != nil {
		return nil, err
	}
	end := make(map[string]int64, len(files))
	for _, path := range files {
		name := filepath.Base(path)
		size, err := scanFile(path, name, offsets[name], fn)
		if err != nil {
			return end, err
		}
		end[name] = size
	}
	return end, nil
}

func scanFile(path, name string, start int64, fn func(Position, entry.Entry, []byte) error) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	if start < 0 || start > info.Size() {
		start = info.Size()
	}
	if start > 0 {
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			return start, fmt.Errorf("seek %s: %w", path, err)
		}
	}

	reader := bufio.NewReaderSize(file, 64*1024)
	offset := start
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			end := offset + int64(len(line))
			raw := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
			if len(raw) > 0 {
				if e, perr := entry.Parse(raw); perr == nil {
					rawCopy := make([]byte, len(raw))
					copy(rawCopy, raw)
					if ferr := fn(Position{File: name, Start: offset, End: end}, e, rawCopy); ferr != nil {
						return offset, ferr
					}
				}
			}
			offset = end
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return offset, fmt.Errorf("read %s: %w", path, err)
		}
	}
	return offset, nil
}

// Files returns the base names of the day files currently on disk.
func (s *Store) Files() ([]string, error) {
	files, err := s.days()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for _, path := range files {
		names = append(names, filepath.Base(path))
	}
	return names, nil
}

// Repair truncates a partial trailing line left behind by an unclean shutdown,
// so the next append does not concatenate onto it. It must run before appends.
// It returns the number of bytes discarded.
func (s *Store) Repair() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		_ = s.file.Close()
		s.file, s.date, s.size = nil, "", 0
	}
	files, err := s.days()
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, nil
	}
	// Only the most recently written file can end mid-line.
	return truncatePartialLine(files[len(files)-1])
}

func truncatePartialLine(path string) (int64, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	size := info.Size()
	if size == 0 {
		return 0, nil
	}

	last := make([]byte, 1)
	if _, err := file.ReadAt(last, size-1); err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	if last[0] == '\n' {
		return 0, nil
	}

	const chunk = 64 * 1024
	for pos := size; pos > 0; {
		start := pos - chunk
		if start < 0 {
			start = 0
		}
		block := make([]byte, pos-start)
		if _, err := file.ReadAt(block, start); err != nil {
			return 0, fmt.Errorf("read %s: %w", path, err)
		}
		if i := bytes.LastIndexByte(block, '\n'); i >= 0 {
			cut := start + int64(i) + 1
			if err := file.Truncate(cut); err != nil {
				return 0, fmt.Errorf("truncate %s: %w", path, err)
			}
			return size - cut, nil
		}
		pos = start
	}

	// No newline anywhere: the whole file is a single partial line.
	if err := file.Truncate(0); err != nil {
		return 0, fmt.Errorf("truncate %s: %w", path, err)
	}
	return size, nil
}

// Prune deletes day files older than the retention window. A retention of zero
// keeps everything.
func (s *Store) Prune(retention time.Duration) error {
	if retention <= 0 {
		return nil
	}
	cutoff := time.Now().UTC().Add(-retention)
	files, err := s.days()
	if err != nil {
		return err
	}
	for _, path := range files {
		date, ok := dayFromName(filepath.Base(path))
		if !ok {
			continue
		}
		if date.Before(cutoff) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("prune %s: %w", path, err)
			}
		}
	}
	return nil
}

// Close flushes and closes the active file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file, s.date, s.size = nil, "", 0
	return err
}

func (s *Store) days() ([]string, error) {
	dirEntries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read data directory: %w", err)
	}
	var files []string
	for _, de := range dirEntries {
		name := de.Name()
		if de.IsDir() || !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
			continue
		}
		files = append(files, filepath.Join(s.dir, name))
	}
	sort.Strings(files)
	return files, nil
}

func dayFromName(name string) (time.Time, bool) {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileSuffix)
	t, err := time.Parse(dateLayout, trimmed)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
