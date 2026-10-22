package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"time"
)

// Metadata stores metadata about cached URLs
type Metadata struct {
	ContentHash   string        `json:"content_hash"`   // SHA256 hash of file content
	FetchDuration time.Duration `json:"fetch_duration"` // Time taken to fetch from upstream
	URL           string        `json:"url"`            // Original URL
	ContentType   string        `json:"content_type"`   // Content-Type header
	Size          int64         `json:"size"`           // File size in bytes
	CachedAt      time.Time     `json:"cached_at"`      // When it was cached
}

// GetURLPath returns the URL metadata file path for a path hash
func GetURLPath(cacheDir, hostname, pathHash string) string {
	return filepath.Join(cacheDir, "url",
		hostname,
		pathHash[0:2],
		pathHash[2:4],
		pathHash)
}

// GetRefcountPath returns the refcount file path for a content hash
func GetRefcountPath(cacheDir, contentHash string) string {
	return filepath.Join(cacheDir, "refcount",
		contentHash[0:2],
		contentHash[2:4],
		contentHash)
}

// GetContentPath returns the content file path for a content hash
func GetContentPath(cacheDir, contentHash string) string {
	return filepath.Join(cacheDir, "content",
		contentHash[0:2],
		contentHash[2:4],
		contentHash)
}

// PathHash computes the SHA256 hash of a path and returns it as a hex string
func PathHash(path string) string {
	hash := sha256.Sum256([]byte(path))
	return hex.EncodeToString(hash[:])
}
