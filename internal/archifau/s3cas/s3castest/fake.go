// Package s3castest provides an in-memory s3cas.Store for handler tests.
package s3castest

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/Delphos-Labs/roxxy/internal/archifau/s3cas"
)

// Fake is an in-memory Store with call counters for cache-fill assertions.
type Fake struct {
	mu      sync.Mutex
	cas     map[string][]byte // digest -> content
	meta    map[string][]byte // key -> json
	metaSHA map[string]string // key -> sha256 object metadata

	GetCASCalls     int
	PutCASCalls     int // counts actual writes, not dedup-skipped puts
	GetMetaCalls    int
	PutMetaCalls    int // counts actual writes, not dedup-skipped puts
	DeleteMetaCalls int

	// GetCASErr / GetMetaErr, when set, force a (non-NotFound) failure from
	// the matching reader so tests can exercise upstream-error paths.
	GetCASErr  error
	GetMetaErr error
}

var _ s3cas.Store = (*Fake)(nil)

func NewFake() *Fake {
	return &Fake{
		cas:     map[string][]byte{},
		meta:    map[string][]byte{},
		metaSHA: map[string]string{},
	}
}

func (f *Fake) GetCAS(_ context.Context, digest string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.GetCASCalls++
	if f.GetCASErr != nil {
		return nil, f.GetCASErr
	}
	b, ok := f.cas[digest]
	if !ok {
		return nil, s3cas.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *Fake) PutCAS(_ context.Context, digest string, _ int64, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.cas[digest]; ok {
		return nil // immutable dedup, mirroring Client.PutCAS
	}
	f.PutCASCalls++
	f.cas[digest] = b
	return nil
}

func (f *Fake) GetMeta(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.GetMetaCalls++
	if f.GetMetaErr != nil {
		return nil, f.GetMetaErr
	}
	b, ok := f.meta[key]
	if !ok {
		return nil, s3cas.ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

func (f *Fake) PutMeta(_ context.Context, key string, data []byte, sha256 string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.metaSHA[key] == sha256 && sha256 != "" {
		return nil // unchanged, mirroring Client.PutMeta
	}
	f.PutMetaCalls++
	f.meta[key] = append([]byte(nil), data...)
	f.metaSHA[key] = sha256
	return nil
}

func (f *Fake) DeleteMeta(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DeleteMetaCalls++
	delete(f.meta, key)
	delete(f.metaSHA, key)
	return nil
}

func (f *Fake) ListMeta(_ context.Context, keyPrefix string) ([]string, error) {
	prefix := strings.TrimSuffix(keyPrefix, "/") + "/"
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	for k := range f.meta {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			rest = rest[:i+1] // immediate child "directory"
		}
		seen[rest] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// HasCAS reports whether the digest is stored (test assertion helper).
func (f *Fake) HasCAS(digest string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.cas[digest]
	return ok
}

// SeedCAS stores content directly (test setup helper).
func (f *Fake) SeedCAS(digest string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cas[digest] = append([]byte(nil), content...)
}

// SeedMeta stores a meta object directly (test setup helper).
func (f *Fake) SeedMeta(key string, data []byte, sha256 string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.meta[key] = append([]byte(nil), data...)
	f.metaSHA[key] = sha256
}

// CASCount returns the number of stored CAS objects.
func (f *Fake) CASCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cas)
}

// HasMeta reports whether the meta key exists.
func (f *Fake) HasMeta(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.meta[key]
	return ok
}
