package registry

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Override represents a single image override: when a client requests Source,
// serve Target instead. This is like a CNAME for OCI image references.
type Override struct {
	Source string `json:"source"` // e.g., "docker:27-dind"
	Target string `json:"target"` // e.g., "delphos/docker-dind:27"
}

// OverrideStore manages image overrides backed by a JSON file on disk.
// Thread-safe for concurrent access.
type OverrideStore struct {
	mu        sync.RWMutex
	overrides map[string]string // source -> target
	filePath  string
}

// NewOverrideStore creates or loads an override store from the given cache directory.
func NewOverrideStore(cacheDir string) *OverrideStore {
	s := &OverrideStore{
		overrides: make(map[string]string),
		filePath:  filepath.Join(cacheDir, "docker-overrides.json"),
	}
	if err := s.load(); err != nil {
		log.Printf("[overrides] Warning: failed to load overrides from %s: %v", s.filePath, err)
	} else if len(s.overrides) > 0 {
		log.Printf("[overrides] Loaded %d image override(s) from %s", len(s.overrides), s.filePath)
	}
	return s
}

// normalizeImageRef normalizes a Docker image reference for consistent lookup.
// "library/nginx:latest" and "nginx:latest" and "nginx" should all match.
func normalizeImageRef(ref string) string {
	// Split off the tag/digest
	var name, tagOrDigest string
	if idx := strings.Index(ref, "@"); idx != -1 {
		// Has digest — don't normalize further, overrides don't apply to digest refs
		return ref
	}
	if idx := strings.LastIndex(ref, ":"); idx != -1 {
		name = ref[:idx]
		tagOrDigest = ref[idx:]
	} else {
		name = ref
		tagOrDigest = "" // no tag specified
	}

	// Strip "library/" prefix for Docker Hub official images
	// "library/nginx" -> "nginx"
	name = strings.TrimPrefix(name, "library/")

	return name + tagOrDigest
}

// Lookup checks if there is an override for the given image reference.
// Returns (target, true) if an override exists, ("", false) otherwise.
// The reference is normalized before lookup.
func (s *OverrideStore) Lookup(source string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	normalized := normalizeImageRef(source)
	target, ok := s.overrides[normalized]
	return target, ok
}

// Set creates or updates an override.
func (s *OverrideStore) Set(source, target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	normalized := normalizeImageRef(source)
	s.overrides[normalized] = target
	return s.save()
}

// Delete removes an override. Returns true if the override existed.
func (s *OverrideStore) Delete(source string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	normalized := normalizeImageRef(source)
	if _, ok := s.overrides[normalized]; !ok {
		return false, nil
	}
	delete(s.overrides, normalized)
	return true, s.save()
}

// List returns all overrides.
func (s *OverrideStore) List() []Override {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Override, 0, len(s.overrides))
	for source, target := range s.overrides {
		result = append(result, Override{Source: source, Target: target})
	}
	return result
}

func (s *OverrideStore) load() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No file yet, start empty
		}
		return err
	}
	var overrides []Override
	if err := json.Unmarshal(data, &overrides); err != nil {
		return fmt.Errorf("invalid overrides file: %w", err)
	}
	for _, o := range overrides {
		s.overrides[normalizeImageRef(o.Source)] = o.Target
	}
	return nil
}

func (s *OverrideStore) save() error {
	overrides := make([]Override, 0, len(s.overrides))
	for source, target := range s.overrides {
		overrides = append(overrides, Override{Source: source, Target: target})
	}
	data, err := json.MarshalIndent(overrides, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(s.filePath, data, 0644)
}
