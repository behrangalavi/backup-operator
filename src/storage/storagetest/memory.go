// Package storagetest holds the in-memory storage.Storage fake and the
// conformance suite every backend's semantics are checked against. The fake
// is the reference implementation of the Storage contract documented on the
// interface; tests in other packages use it instead of hand-rolled fakes.
package storagetest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"backup-operator/storage"
)

// Memory is a prefix-aware in-memory Storage. Keys are stored with the
// configured pathPrefix applied exactly like the object-store backends do
// (plain concatenation with one separator), so the prefix-bleed class of
// bugs is reproducible without a bucket.
type Memory struct {
	mu         sync.Mutex
	name       string
	pathPrefix string
	objects    map[string][]byte
	modified   map[string]time.Time
	closed     bool
	// Hooks let tests inject failures; nil means "succeed".
	ListErr, GetErr, DeleteErr, UploadErr func(path string) error
	// Now is the clock used for LastModified; defaults to time.Now.
	Now func() time.Time
}

var (
	_ storage.Storage          = (*Memory)(nil)
	_ storage.BatchStorage     = (*Memory)(nil)
	_ storage.DirectoryRemover = (*Memory)(nil)
)

// NewMemory returns an empty fake with the given logical name and no prefix.
func NewMemory(name string) *Memory { return NewMemoryWithPrefix(name, "") }

// NewMemoryWithPrefix returns an empty fake whose keys live under pathPrefix.
func NewMemoryWithPrefix(name, pathPrefix string) *Memory {
	return &Memory{
		name:       name,
		pathPrefix: strings.TrimRight(pathPrefix, "/"),
		objects:    map[string][]byte{},
		modified:   map[string]time.Time{},
		Now:        time.Now,
	}
}

// Seed adds an object by its LOGICAL path (prefix applied internally).
func (m *Memory) Seed(logicalPath string, data []byte) *Memory {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[m.full(logicalPath)] = append([]byte(nil), data...)
	m.modified[m.full(logicalPath)] = m.Now()
	return m
}

// SeedAt is Seed with an explicit LastModified.
func (m *Memory) SeedAt(logicalPath string, data []byte, at time.Time) *Memory {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[m.full(logicalPath)] = append([]byte(nil), data...)
	m.modified[m.full(logicalPath)] = at
	return m
}

// Keys returns the raw stored keys (prefix included), sorted. Lets tests
// assert the on-the-wire layout rather than the logical view.
func (m *Memory) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.objects))
	for k := range m.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Closed reports whether Close was called.
func (m *Memory) Closed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *Memory) full(p string) string {
	p = strings.TrimLeft(p, "/")
	if m.pathPrefix == "" {
		return p
	}
	return m.pathPrefix + "/" + p
}

func (m *Memory) stripPrefix(key string) string {
	if m.pathPrefix == "" {
		return key
	}
	return strings.TrimPrefix(key, m.pathPrefix+"/")
}

func (m *Memory) Name() string { return m.name }

func (m *Memory) Upload(ctx context.Context, p string, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.UploadErr != nil {
		if err := m.UploadErr(p); err != nil {
			return err
		}
	}
	data, err := io.ReadAll(storage.ContextReader(ctx, r))
	if err != nil {
		return fmt.Errorf("memory upload %s: %w", p, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[m.full(p)] = data
	m.modified[m.full(p)] = m.Now()
	return nil
}

func (m *Memory) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.ListErr != nil {
		if err := m.ListErr(prefix); err != nil {
			return nil, err
		}
	}
	full := m.full(prefix)
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []storage.Object
	for k, v := range m.objects {
		if !strings.HasPrefix(k, full) {
			continue
		}
		out = append(out, storage.Object{Path: m.stripPrefix(k), Size: int64(len(v)), LastModified: m.modified[k]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (m *Memory) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.GetErr != nil {
		if err := m.GetErr(p); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[m.full(p)]
	if !ok {
		return nil, fmt.Errorf("memory get %s: %w", p, storage.ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), nil
}

func (m *Memory) Delete(ctx context.Context, p string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.DeleteErr != nil {
		if err := m.DeleteErr(p); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, m.full(p))
	delete(m.modified, m.full(p))
	return nil
}

// RemoveDirectory succeeds when no object lives under the directory, which
// is the only observable "empty directory" an object model has.
func (m *Memory) RemoveDirectory(_ context.Context, p string) error {
	full := strings.TrimRight(m.full(p), "/") + "/"
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.objects {
		if strings.HasPrefix(k, full) {
			return fmt.Errorf("memory rmdir %s: directory not empty", p)
		}
	}
	return nil
}

// WithSession returns the same fake; the fake has no connection to share.
func (m *Memory) WithSession(ctx context.Context) (storage.Storage, func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return m, func() error { return nil }, nil
}

func (m *Memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}
