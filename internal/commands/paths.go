package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/packages"
	"github.com/Delphos-Labs/roxxy/internal/packages/index"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// DockerManifestMetadata represents metadata stored alongside Docker manifests
type DockerManifestMetadata struct {
	ContentType string    `json:"content_type"`
	StoredAt    time.Time `json:"stored_at"`
	UpstreamURL string    `json:"upstream_url,omitempty"`
}

// NormalizeRegistryURLForPath converts a registry URL to a safe filesystem identifier.
// This is used to create directory names in the cache that correspond to registries.
// Examples:
//   - "https://registry-1.docker.io" -> "docker.io"
//   - "https://ghcr.io" -> "ghcr.io"
//   - "https://quay.io" -> "quay.io"
func NormalizeRegistryURLForPath(upstreamURL string) string {
	// Remove protocol and normalize
	url := strings.TrimPrefix(upstreamURL, "https://")
	url = strings.TrimPrefix(url, "http://")

	// Normalize common registry URLs
	if url == "registry-1.docker.io" || url == "" {
		return "docker.io"
	}

	// Remove port if present
	if idx := strings.Index(url, ":"); idx != -1 {
		url = url[:idx]
	}

	// Sanitize for filesystem (replace any remaining problematic chars)
	url = strings.ReplaceAll(url, "/", "_")
	url = strings.ReplaceAll(url, ":", "_")

	return url
}

// NormalizeRepoName normalizes a Docker repository name for display.
// Adds "library/" prefix if the repo name doesn't contain a "/".
// Examples:
//   - "golang" -> "library/golang"
//   - "library/golang" -> "library/golang"
//   - "user/repo" -> "user/repo"
func NormalizeRepoName(repoName string) string {
	if !strings.Contains(repoName, "/") {
		return "library/" + repoName
	}
	return repoName
}

// SafeRepoName is a package-local alias for util.SafeRepoName retained so the
// many in-package callers stay terse. See util.SafeRepoName for the encoding.
func SafeRepoName(repoName string) string { return util.SafeRepoName(repoName) }

// UnsafeRepoName is a package-local alias for util.UnsafeRepoName.
func UnsafeRepoName(safeRepo string) string { return util.UnsafeRepoName(safeRepo) }

// DockerManifestPath constructs the filesystem path for a Docker manifest.
// Structure: <cacheDir>/docker-manifests/<registry>/<safeRepo>/<reference>
func DockerManifestPath(cacheDir, registryID, repoName, reference string) string {
	safeRepo := SafeRepoName(repoName)
	return filepath.Join(cacheDir, "docker-manifests", registryID, safeRepo, reference)
}

// DockerManifestDir constructs the directory path for a Docker repository's manifests.
// Structure: <cacheDir>/docker-manifests/<registry>/<safeRepo>
func DockerManifestDir(cacheDir, registryID, repoName string) string {
	safeRepo := SafeRepoName(repoName)
	return filepath.Join(cacheDir, "docker-manifests", registryID, safeRepo)
}

// ReadDockerManifestMetadata reads metadata from a .meta file next to a Docker manifest.
// Returns nil if the metadata file doesn't exist or can't be read.
func ReadDockerManifestMetadata(manifestPath string) *DockerManifestMetadata {
	metaPath := manifestPath + ".meta"
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil
	}

	var meta DockerManifestMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil
	}

	return &meta
}

// WriteDockerManifestMetadata writes metadata to a .meta file next to a Docker manifest.
func WriteDockerManifestMetadata(manifestPath string, meta *DockerManifestMetadata) error {
	metaPath := manifestPath + ".meta"
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	return os.WriteFile(metaPath, data, 0644)
}

// PackageTypeDir constructs the directory path for a package type's cache.
// Structure: <cacheDir>/packages/<type>
func PackageTypeDir(cacheDir, packageType string) string {
	return filepath.Join(cacheDir, "packages", packageType)
}

// IndexDBDir constructs the directory path for the index database.
// Structure: <cacheDir>/index
func IndexDBDir(cacheDir string) string {
	return filepath.Join(cacheDir, "index")
}

// GetOrCreateIndexDB gets the IndexDB from config or creates a new one if nil.
func GetOrCreateIndexDB(cfg Config) *index.IndexDB {
	if cfg.IndexDB != nil {
		return cfg.IndexDB
	}
	return index.NewIndexDB(IndexDBDir(cfg.CacheDir))
}

// AllPackageTypes returns a list of all supported package types.
func AllPackageTypes() []packages.Type {
	return []packages.Type{
		packages.TypeAPK,
		packages.TypeAPT,
		packages.TypeNPM,
		packages.TypePyPI,
		packages.TypeGo,
	}
}

// IsValidPackageType checks if a package type string is valid.
func IsValidPackageType(pmType string) bool {
	pt := packages.Type(pmType)
	for _, validType := range AllPackageTypes() {
		if pt == validType {
			return true
		}
	}
	return false
}

// GetPackageTypesToParse returns the list of package types to parse.
// If pmType is empty, returns all types. Otherwise validates and returns the single type.
func GetPackageTypesToParse(pmType string) ([]packages.Type, error) {
	if pmType == "" {
		return AllPackageTypes(), nil
	}

	if !IsValidPackageType(pmType) {
		return nil, fmt.Errorf("invalid package type: %s (valid types: apk, apt, npm, pypi, go)", pmType)
	}

	return []packages.Type{packages.Type(pmType)}, nil
}
