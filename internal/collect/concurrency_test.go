package collect

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"logrift.dev/server/internal/entry"
	"logrift.dev/server/internal/index"
)

// TestConcurrentWritesIndexEveryEntry drives the same collector from many
// goroutines, the way concurrent HTTP ingests do, and checks that every stored
// line ends up searchable exactly once.
func TestConcurrentWritesIndexEveryEntry(t *testing.T) {
	_, ix, c := fixture(t)

	const writers, perWriter = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			batch := make([]entry.Entry, 0, perWriter)
			for i := 0; i < perWriter; i++ {
				batch = append(batch, entry.Entry{
					Time:    time.Now().UTC(),
					Level:   "info",
					Message: "concurrent " + strconv.Itoa(w) + "-" + strconv.Itoa(i),
				})
			}
			if err := c.Write(batch); err != nil {
				errs <- err
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("write: %v", err)
	}

	if _, err := c.CatchUp(); err != nil {
		t.Fatal(err)
	}
	const want = writers * perWriter
	count, err := ix.DocCount()
	if err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("documents = %d, want %d", count, want)
	}
	res, err := ix.Search(index.Query{Text: "concurrent", Limit: want})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != want {
		t.Fatalf("search total = %d, want %d", res.Total, want)
	}
}

// TestConcurrentWriteAndSearch runs writers and readers against one index at
// once to exercise its locking under the race detector.
func TestConcurrentWriteAndSearch(t *testing.T) {
	_, ix, c := fixture(t)

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := ix.Search(index.Query{Text: "work", Limit: 10}); err != nil {
					t.Errorf("search: %v", err)
					return
				}
			}
		}()
	}

	const writers, perWriter = 4, 10
	var writeWG sync.WaitGroup
	for w := 0; w < writers; w++ {
		writeWG.Add(1)
		go func(w int) {
			defer writeWG.Done()
			for i := 0; i < perWriter; i++ {
				e := entry.Entry{Time: time.Now().UTC(), Level: "info", Message: "work " + strconv.Itoa(w)}
				if err := c.Write([]entry.Entry{e}); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(w)
	}
	writeWG.Wait()
	close(stop)
	readers.Wait()

	const want = writers * perWriter
	count, err := ix.DocCount()
	if err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("documents = %d, want %d", count, want)
	}
}
