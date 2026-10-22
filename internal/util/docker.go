package util

import (
	"os"
	"path/filepath"
	"strings"
)

// SafeRepoName encodes a Docker repository name into a single path segment.
// `/` becomes `_`; literal `_` is doubled to `__` so the encoding is reversible.
// Examples:
//
//	library/redis        -> library_redis
//	oliver006/redis_exporter -> oliver006_redis__exporter
//	prom/node_exporter   -> prom_node__exporter
//
// Historically (pre-2026-05) this was a lossy `/` → `_` replace, which
// silently collapsed `a/b_c` and `a/b/c` to the same on-disk directory.
// New entries written by callers using this helper are unambiguous; entries
// created under the lossy scheme remain on disk and round-trip incorrectly
// until they're re-pulled (deliberate — no migration).
func SafeRepoName(repoName string) string {
	s := strings.ReplaceAll(repoName, "_", "__")
	return strings.ReplaceAll(s, "/", "_")
}

// UnsafeRepoName reverses SafeRepoName. Walks left-to-right: `__` becomes a
// literal `_`, lone `_` becomes `/`. Robust against trailing `_`.
func UnsafeRepoName(safeRepo string) string {
	var b strings.Builder
	b.Grow(len(safeRepo))
	for i := 0; i < len(safeRepo); i++ {
		if safeRepo[i] != '_' {
			b.WriteByte(safeRepo[i])
			continue
		}
		if i+1 < len(safeRepo) && safeRepo[i+1] == '_' {
			b.WriteByte('_')
			i++
		} else {
			b.WriteByte('/')
		}
	}
	return b.String()
}

// DockerBlobPath constructs the filesystem path for a Docker blob.
// Structure: <cacheDir>/docker-blobs/sha256/<first 2 chars>/<next 2 chars>/<full digest>
// This prevents having thousands of files in a single directory, which can cause
// performance issues on many filesystems. Using 2-level subdirectories provides
// good distribution (256 * 256 = 65,536 possible directories).
// Example: sha256:abc123... -> docker-blobs/sha256/ab/c1/sha256:abc123...
func DockerBlobPath(cacheDir, digest string) string {
	blobDir := filepath.Join(cacheDir, "docker-blobs")
	// Structure: docker-blobs/sha256/<first 2 chars>/<next 2 chars>/<full digest>
	if strings.HasPrefix(digest, "sha256:") {
		hashPart := digest[7:] // Skip "sha256:" prefix
		if len(hashPart) >= 4 {
			return filepath.Join(blobDir, "sha256", hashPart[0:2], hashPart[2:4], digest)
		}
	}
	// Fallback for non-sha256 digests or malformed digests
	return filepath.Join(blobDir, digest)
}

// DockerBlobPathWithFallback constructs the filesystem path for a Docker blob,
// checking both the new subdirectory structure and the old flat structure for backwards compatibility.
// Returns the path that exists, or the new structure path if neither exists (for writing).
func DockerBlobPathWithFallback(cacheDir, digest string) string {
	newPath := DockerBlobPath(cacheDir, digest)
	// Check if new path exists
	if _, err := os.Stat(newPath); err == nil {
		return newPath
	}
	// Check old flat structure
	oldPath := filepath.Join(cacheDir, "docker-blobs", digest)
	if _, err := os.Stat(oldPath); err == nil {
		return oldPath
	}
	// Neither exists, return new structure (for writing)
	return newPath
}
