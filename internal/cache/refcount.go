package cache

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// IncrementRefcount increments the reference count for a content hash
func IncrementRefcount(cacheDir, contentHash string) error {
	refcountPath := GetRefcountPath(cacheDir, contentHash)

	// Ensure directory exists before any file operations
	dir := filepath.Dir(refcountPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create refcount directory %s: %w", dir, err)
	}

	// Read current count
	count := 0
	if data, err := os.ReadFile(refcountPath); err == nil {
		util.LogerrT(fmt.Sscanf(string(data), "%d", &count))
	}

	// Increment and write back
	count++
	return os.WriteFile(refcountPath, []byte(fmt.Sprintf("%d", count)), 0644)
}

// DecrementRefcount decrements the reference count and returns true if count
// reaches 0.
//
// When count hits 0 the refcount file is *kept* at value "0" rather than
// removed, so a quickly-following Increment (e.g. retag with overlapping
// blobs) sees the existing state and just bumps it back to 1 without any
// blob-recovery work. The blob file itself is NOT deleted here — callers
// must not eagerly remove it either. Orphan blobs (refcount=0) are
// reclaimed asynchronously by `roxxy docker prune-blobs` / `docker audit`,
// which is the explicit flush point. This keeps the "delete tag, re-push
// new version sharing some blobs" path zero-copy: the blob stays on disk
// across the refcount=0 window.
func DecrementRefcount(cacheDir, contentHash string) (bool, error) {
	refcountPath := GetRefcountPath(cacheDir, contentHash)

	// Read current count
	count := 0
	if data, err := os.ReadFile(refcountPath); err != nil {
		return false, err
	} else {
		util.LogerrT(fmt.Sscanf(string(data), "%d", &count))
	}

	// Decrement (clamp at 0 — under-decrement leaks are easier to spot at
	// flush time than negative refcounts).
	count--
	if count < 0 {
		count = 0
	}
	if err := os.WriteFile(refcountPath, []byte(fmt.Sprintf("%d", count)), 0644); err != nil {
		return false, err
	}
	return count == 0, nil
}

// GetManifestRefcountPath returns the refcount file path for a manifest digest.
// Lives under refcount/manifest/ so it can't collide with blob refcounts at
// refcount/<aa>/<bb>/<hash> (hex hash chars are 0-9a-f, so the first dir
// segment of a blob refcount can never be "manifest").
func GetManifestRefcountPath(cacheDir, manifestHash string) string {
	return filepath.Join(cacheDir, "refcount", "manifest",
		manifestHash[0:2],
		manifestHash[2:4],
		manifestHash)
}

// IncrementManifestRefcount increments the reference count for a manifest digest.
func IncrementManifestRefcount(cacheDir, manifestHash string) error {
	refcountPath := GetManifestRefcountPath(cacheDir, manifestHash)
	dir := filepath.Dir(refcountPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create manifest refcount directory %s: %w", dir, err)
	}
	count := 0
	if data, err := os.ReadFile(refcountPath); err == nil {
		util.LogerrT(fmt.Sscanf(string(data), "%d", &count))
	}
	count++
	return os.WriteFile(refcountPath, []byte(fmt.Sprintf("%d", count)), 0644)
}

// DecrementManifestRefcount decrements the manifest reference count and
// returns true if the count reaches 0 (caller should cascade-delete the
// manifest body, its blob refs, and any sub-manifest refs).
func DecrementManifestRefcount(cacheDir, manifestHash string) (bool, error) {
	refcountPath := GetManifestRefcountPath(cacheDir, manifestHash)
	count := 0
	data, err := os.ReadFile(refcountPath)
	if err != nil {
		// Missing refcount: treat as 0. Caller cascades; safe because
		// after rebuildManifestRefcounts on startup, every referenced
		// manifest has a refcount file. A missing file here means
		// nothing else references this manifest.
		return true, nil
	}
	util.LogerrT(fmt.Sscanf(string(data), "%d", &count))
	count--
	if count <= 0 {
		util.Logerr(os.Remove(refcountPath))
		return true, nil
	}
	return false, os.WriteFile(refcountPath, []byte(fmt.Sprintf("%d", count)), 0644)
}

// ManifestRefcountExists reports whether a manifest refcount file exists.
// Used as a "have we ever seen this manifest digest before" signal so
// blob-refcount Inc happens exactly once per manifest digest.
func ManifestRefcountExists(cacheDir, manifestHash string) bool {
	_, err := os.Stat(GetManifestRefcountPath(cacheDir, manifestHash))
	return err == nil
}
