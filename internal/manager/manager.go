// Package manager owns the lifecycle of every project: its registry entry, its
// JSONL store, and its persistent Bleve index.
package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"logrift.dev/server/internal/collect"
	"logrift.dev/server/internal/index"
	"logrift.dev/server/internal/project"
	"logrift.dev/server/internal/store"
)

const (
	registryName = "projects.json"
	projectsDir  = "projects"
)

type runtime struct {
	store     *store.Store
	index     *index.Index
	collector *collect.Collector
}

// Manager coordinates project registration and storage.
type Manager struct {
	root     string
	registry *project.Registry

	mu       sync.RWMutex
	runtimes map[string]*runtime
}

// Open loads the registry under root and opens every registered project. When
// reindex is true, each project's index is rebuilt from its JSONL files.
func Open(root string, reindex bool) (*Manager, error) {
	registry, err := project.OpenRegistry(filepath.Join(root, registryName))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, projectsDir), 0o755); err != nil {
		return nil, fmt.Errorf("create projects directory: %w", err)
	}
	m := &Manager{root: root, registry: registry, runtimes: map[string]*runtime{}}
	for _, p := range registry.List() {
		if err := m.openRuntime(p.Name, reindex); err != nil {
			m.Close()
			return nil, fmt.Errorf("open project %q: %w", p.Name, err)
		}
	}
	return m, nil
}

// Create registers a project and returns its plain-text key.
func (m *Manager) Create(name, description string) (project.Project, string, error) {
	p, key, err := m.registry.Create(name, description)
	if err != nil {
		return project.Project{}, "", err
	}
	if err := m.openRuntime(name, false); err != nil {
		_ = m.registry.Delete(name)
		return project.Project{}, "", err
	}
	return p, key, nil
}

// Rotate issues a new key for a project.
func (m *Manager) Rotate(name string) (string, error) {
	_, key, err := m.registry.Rotate(name)
	return key, err
}

// Delete removes a project and its stored data.
func (m *Manager) Delete(name string) error {
	if _, ok := m.registry.Get(name); !ok {
		return project.ErrNotFound
	}
	m.mu.Lock()
	rt := m.runtimes[name]
	delete(m.runtimes, name)
	m.mu.Unlock()
	if rt != nil {
		_ = rt.index.Close()
		_ = rt.store.Close()
	}
	if err := m.registry.Delete(name); err != nil {
		return err
	}
	return os.RemoveAll(m.projectDir(name))
}

// Authenticate returns the project that owns key, if any.
func (m *Manager) Authenticate(key string) (project.Project, bool) {
	return m.registry.Authenticate(key)
}

// Touch records key usage for a project.
func (m *Manager) Touch(name string) { m.registry.Touch(name) }

// Projects lists all registered projects.
func (m *Manager) Projects() []project.Project { return m.registry.List() }

// Get returns a project's metadata.
func (m *Manager) Get(name string) (project.Project, bool) { return m.registry.Get(name) }

// Collector returns the collector for a project.
func (m *Manager) Collector(name string) (*collect.Collector, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rt, ok := m.runtimes[name]
	if !ok {
		return nil, false
	}
	return rt.collector, true
}

// Index returns the search index for a project.
func (m *Manager) Index(name string) (*index.Index, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rt, ok := m.runtimes[name]
	if !ok {
		return nil, false
	}
	return rt.index, true
}

// CatchUp indexes any tail not yet indexed for every project and returns the
// total number of documents added.
func (m *Manager) CatchUp() (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	total := 0
	for name, rt := range m.runtimes {
		added, err := rt.collector.CatchUp()
		if err != nil {
			return total, fmt.Errorf("catch up %q: %w", name, err)
		}
		total += added
	}
	return total, nil
}

// Prune enforces retention on every project and returns documents deleted.
func (m *Manager) Prune(retention time.Duration) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	total := 0
	for name, rt := range m.runtimes {
		deleted, err := rt.collector.Prune(retention)
		if err != nil {
			return total, fmt.Errorf("prune %q: %w", name, err)
		}
		total += deleted
	}
	return total, nil
}

// Flush persists registry metadata.
func (m *Manager) Flush() error { return m.registry.Flush() }

// Close releases every project's index and store.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, rt := range m.runtimes {
		_ = rt.index.Close()
		_ = rt.store.Close()
		delete(m.runtimes, name)
	}
	return nil
}

func (m *Manager) projectDir(name string) string {
	return filepath.Join(m.root, projectsDir, name)
}

func (m *Manager) openRuntime(name string, reindex bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runtimes[name]; ok {
		return nil
	}
	dir := m.projectDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	indexPath := filepath.Join(dir, "index")
	if reindex {
		if err := index.Remove(indexPath); err != nil {
			return err
		}
	}
	st, err := store.Open(dir)
	if err != nil {
		return err
	}
	if _, err := st.Repair(); err != nil {
		_ = st.Close()
		return err
	}
	ix, _, err := index.Open(indexPath)
	if err != nil {
		_ = st.Close()
		return err
	}
	m.runtimes[name] = &runtime{store: st, index: ix, collector: collect.New(st, ix)}
	return nil
}
