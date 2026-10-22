package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Mirror is the local on-disk projection of the upstream archive plus an
// ETag map for conditional refresh. Layout:
//
//	<dir>/files/<logical-path>      // file bytes
//	<dir>/state.json                // {logical-path: {etag, mod_time}}
type Mirror struct {
	dir     string
	mu      sync.Mutex
	state   map[string]Entry
	tracked []string
}

// Entry is the persisted conditional-GET state for one logical path.
type Entry struct {
	ETag    string    `json:"etag"`
	ModTime time.Time `json:"mod_time"`
	Size    int64     `json:"size"`
}

func OpenMirror(dir string) (*Mirror, error) {
	if dir == "" {
		return nil, errors.New("mirror dir is required")
	}
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o755); err != nil {
		return nil, err
	}
	m := &Mirror{dir: dir, state: map[string]Entry{}}
	statePath := filepath.Join(dir, "state.json")
	data, err := os.ReadFile(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &m.state); err != nil {
		return nil, fmt.Errorf("parse state.json: %w", err)
	}
	return m, nil
}

// Dir returns the on-disk root of the mirror.
func (m *Mirror) Dir() string { return m.dir }

// TrackedPaths returns the set of logical paths the refresh loop polls.
func (m *Mirror) TrackedPaths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.tracked))
	copy(out, m.tracked)
	return out
}

// LookupEntry returns the recorded ETag/ModTime for a logical path, or
// the zero value if untracked.
func (m *Mirror) LookupEntry(logicalPath string) Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state[logicalPath]
}

// Read returns the bytes currently stored for a logical path.
func (m *Mirror) Read(logicalPath string) ([]byte, error) {
	abs, err := m.absPath(logicalPath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}

// Write stores bytes and updates the ETag map. Atomic via tmp+rename so
// concurrent readers never see partial files.
func (m *Mirror) Write(logicalPath string, body []byte, etag string, modTime time.Time) error {
	abs, err := m.absPath(logicalPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return err
	}
	m.mu.Lock()
	m.state[logicalPath] = Entry{ETag: etag, ModTime: modTime, Size: int64(len(body))}
	err = m.persistStateLocked()
	m.mu.Unlock()
	return err
}

func (m *Mirror) persistStateLocked() error {
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	statePath := filepath.Join(m.dir, "state.json")
	tmp, err := os.CreateTemp(m.dir, ".state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, statePath)
}

func (m *Mirror) absPath(logicalPath string) (string, error) {
	if logicalPath == "" {
		return "", errors.New("empty path")
	}
	if strings.Contains(logicalPath, "..") {
		return "", fmt.Errorf("invalid path %q", logicalPath)
	}
	return filepath.Join(m.dir, "files", filepath.FromSlash(logicalPath)), nil
}
