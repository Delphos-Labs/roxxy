package packages

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// CachedFileInfo contains information about a cached file
type CachedFileInfo struct {
	Path         string
	Name         string
	Version      string
	Architecture string
	IsIndex      bool
	URLPath      string
}

// LoadCachedFileInfo loads metadata for a cached file
// Returns nil if metadata doesn't exist or is invalid
func LoadCachedFileInfo(cachePath string) *CachedFileInfo {
	metaPath := cachePath + ".meta"
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		return nil
	}

	var metadata Metadata
	if err := json.Unmarshal(metaData, &metadata); err != nil {
		return nil
	}

	return &CachedFileInfo{
		Path:         cachePath,
		Name:         metadata.Name,
		Version:      metadata.Version,
		Architecture: metadata.Architecture,
		IsIndex:      metadata.IsIndex,
		URLPath:      metadata.URLPath,
	}
}

// IsPackageFile determines if a cached file is a package file (not an index file)
// Uses the IsIndex field from metadata, with fallback to URL path checking
func IsPackageFile(cachePath string) bool {
	info := LoadCachedFileInfo(cachePath)
	if info == nil {
		return false
	}

	// If IsIndex is explicitly set, use it
	if info.IsIndex {
		return false
	}

	// If not an index file, it's a package file
	return true
}

// WalkPackageFiles walks the cache directory and calls fn for each package file (not index files)
func WalkPackageFiles(typeDir string, fn func(info *CachedFileInfo) error) error {
	return filepath.WalkDir(typeDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		// Skip .meta files
		if filepath.Ext(path) == ".meta" {
			return nil
		}

		info := LoadCachedFileInfo(path)
		if info == nil {
			return nil // No metadata, skip
		}

		// Only process package files (not index files)
		if info.IsIndex {
			return nil
		}

		return fn(info)
	})
}
