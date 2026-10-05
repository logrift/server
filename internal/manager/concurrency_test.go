package manager

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"logrift.dev/server/internal/entry"
	"logrift.dev/server/internal/index"
	"logrift.dev/server/internal/project"
)

// TestConcurrentCreateSameName ensures the registry admits exactly one project
// when many goroutines race to create the same name.
func TestConcurrentCreateSameName(t *testing.T) {
	m, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	const n = 10
	start := make(chan struct{})
	results := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := m.Create("race", "", 0)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	success, exists := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, project.ErrExists):
			exists++
		default:
			t.Fatalf("create: %v", err)
		}
	}
	if success != 1 || exists != n-1 {
		t.Fatalf("success = %d, exists = %d, want 1 and %d", success, exists, n-1)
	}
	if got := len(m.Projects()); got != 1 {
		t.Fatalf("projects = %d, want 1", got)
	}
}

// TestConcurrentManagerAccess exercises the manager's maps, registry and one
// project's collector/index from many goroutines at once.
func TestConcurrentManagerAccess(t *testing.T) {
	m, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	_, key, err := m.Create("svc", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	collector, ok := m.Collector("svc")
	if !ok {
		t.Fatal("collector missing")
	}
	ix, ok := m.Index("svc")
	if !ok {
		t.Fatal("index missing")
	}

	const writers, perWriter = 4, 20
	const readers = 4
	errs := make(chan error, writers+readers)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				e := entry.Entry{Time: time.Now().UTC(), Level: "info", Message: "work " + strconv.Itoa(w)}
				if err := collector.Write([]entry.Entry{e}); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter*2; i++ {
				if _, ok := m.Authenticate(key); !ok {
					errs <- errors.New("authenticate lost the project")
					return
				}
				if _, ok := m.Get("svc"); !ok {
					errs <- errors.New("project missing")
					return
				}
				m.Touch("svc")
				_ = m.Projects()
				if _, err := ix.Search(index.Query{Text: "work", Limit: 5}); err != nil {
					errs <- err
					return
				}
				if _, err := m.Usage("svc"); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent access: %v", err)
	}

	if _, err := m.CatchUp(); err != nil {
		t.Fatal(err)
	}
	count, err := ix.DocCount()
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(writers * perWriter); count != want {
		t.Fatalf("documents = %d, want %d", count, want)
	}
}

// TestConcurrentCreateDeleteDistinctProjects adds and removes runtimes while
// other goroutines read the project list.
func TestConcurrentCreateDeleteDistinctProjects(t *testing.T) {
	m, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	const n = 8
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			name := "p" + strconv.Itoa(i)
			if _, _, err := m.Create(name, "", 0); err != nil {
				errs <- err
				return
			}
			if _, ok := m.Collector(name); !ok {
				errs <- errors.New("collector missing for " + name)
				return
			}
			if err := m.Delete(name); err != nil {
				errs <- err
			}
		}(i)
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 2; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = m.Projects()
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}

	close(start)
	wg.Wait()
	close(stop)
	readers.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("create/delete: %v", err)
	}
	if got := len(m.Projects()); got != 0 {
		t.Fatalf("projects = %d, want 0", got)
	}
}
