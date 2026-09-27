// Package project manages logrift projects and their ingest API keys. Project
// metadata is persisted as JSON; API keys are stored as SHA-256 hashes and are
// shown in plain text only when created or rotated.
package project

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

const (
	fileVersion = 1
	keyPrefix   = "lr_"
)

var (
	// ErrNotFound is returned when a project does not exist.
	ErrNotFound = errors.New("project not found")
	// ErrExists is returned when creating a project whose name is taken.
	ErrExists = errors.New("project already exists")
	// ErrInvalidName is returned for names that are not URL-safe slugs.
	ErrInvalidName = errors.New("project name must match [a-z0-9][a-z0-9_-]{0,63}")
	// ErrInvalidDays is returned for a negative compression age.
	ErrInvalidDays = errors.New("compress_after_days must not be negative")
	nameRE         = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// Project is a named log stream with one ingest API key.
type Project struct {
	Name              string    `json:"name"`
	Description       string    `json:"description,omitempty"`
	KeyHash           string    `json:"key_hash"`
	KeyPrefix         string    `json:"key_prefix"`
	CompressAfterDays int       `json:"compress_after_days"`
	CreatedAt         time.Time `json:"created_at"`
	LastUsedAt        time.Time `json:"last_used_at,omitempty"`
}

type registryFile struct {
	Version  int                 `json:"version"`
	Projects map[string]*Project `json:"projects"`
}

// Registry is a persisted, concurrency-safe set of projects.
type Registry struct {
	mu       sync.Mutex
	path     string
	projects map[string]*Project
	byHash   map[string]string
}

// OpenRegistry loads the registry at path, creating an empty one if needed.
func OpenRegistry(path string) (*Registry, error) {
	r := &Registry{path: path, projects: map[string]*Project{}, byHash: map[string]string{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var file registryFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	if file.Version != fileVersion {
		return nil, fmt.Errorf("unsupported registry version %d", file.Version)
	}
	for name, p := range file.Projects {
		if p == nil {
			continue
		}
		p.Name = name
		r.projects[name] = p
		if p.KeyHash != "" {
			r.byHash[p.KeyHash] = name
		}
	}
	return r, nil
}

// List returns projects sorted by name.
func (r *Registry) List() []Project {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Project, 0, len(r.projects))
	for _, p := range r.projects {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns a project by name.
func (r *Registry) Get(name string) (Project, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.projects[name]
	if !ok {
		return Project{}, false
	}
	return *p, true
}

// Create adds a project and returns its key in plain text. The plain text key is
// not stored and cannot be retrieved again.
func (r *Registry) Create(name, description string, compressAfterDays int) (Project, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !nameRE.MatchString(name) {
		return Project{}, "", ErrInvalidName
	}
	if compressAfterDays < 0 {
		return Project{}, "", ErrInvalidDays
	}
	if _, ok := r.projects[name]; ok {
		return Project{}, "", ErrExists
	}
	key, err := generateKey()
	if err != nil {
		return Project{}, "", err
	}
	p := &Project{
		Name:              name,
		Description:       description,
		KeyHash:           hashKey(key),
		KeyPrefix:         displayPrefix(key),
		CompressAfterDays: compressAfterDays,
		CreatedAt:         time.Now().UTC(),
	}
	r.projects[name] = p
	r.byHash[p.KeyHash] = name
	if err := r.saveLocked(); err != nil {
		delete(r.projects, name)
		delete(r.byHash, p.KeyHash)
		return Project{}, "", err
	}
	return *p, key, nil
}

// UpdateCompressAfterDays changes when a project's logs are compressed and
// dropped from the search index. Zero disables compression.
func (r *Registry) UpdateCompressAfterDays(name string, days int) (Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.projects[name]
	if !ok {
		return Project{}, ErrNotFound
	}
	if days < 0 {
		return Project{}, ErrInvalidDays
	}
	p.CompressAfterDays = days
	if err := r.saveLocked(); err != nil {
		return Project{}, err
	}
	return *p, nil
}

// Rotate replaces a project's key and returns the new key in plain text.
func (r *Registry) Rotate(name string) (Project, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.projects[name]
	if !ok {
		return Project{}, "", ErrNotFound
	}
	key, err := generateKey()
	if err != nil {
		return Project{}, "", err
	}
	delete(r.byHash, p.KeyHash)
	p.KeyHash = hashKey(key)
	p.KeyPrefix = displayPrefix(key)
	r.byHash[p.KeyHash] = name
	if err := r.saveLocked(); err != nil {
		return Project{}, "", err
	}
	return *p, key, nil
}

// Delete removes a project.
func (r *Registry) Delete(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.projects[name]
	if !ok {
		return ErrNotFound
	}
	delete(r.projects, name)
	delete(r.byHash, p.KeyHash)
	return r.saveLocked()
}

// Authenticate returns the project owning key.
func (r *Registry) Authenticate(key string) (Project, bool) {
	if key == "" {
		return Project{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	name, ok := r.byHash[hashKey(key)]
	if !ok {
		return Project{}, false
	}
	p := r.projects[name]
	if p == nil {
		return Project{}, false
	}
	return *p, true
}

// Touch records that a project's key was used.
func (r *Registry) Touch(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.projects[name]; ok {
		p.LastUsedAt = time.Now().UTC()
	}
}

// Flush persists the registry to disk.
func (r *Registry) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saveLocked()
}

func (r *Registry) saveLocked() error {
	data, err := json.MarshalIndent(registryFile{Version: fileVersion, Projects: r.projects}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode registry: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return fmt.Errorf("create registry directory: %w", err)
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("replace registry: %w", err)
	}
	return nil
}

func generateKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return keyPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func displayPrefix(key string) string {
	if len(key) > 11 {
		return key[:11]
	}
	return key
}
