//go:build !git
// +build !git

package git

import (
	"fmt"
	"os"
	"time"
)

// ArtifactRepo represents a Git repository containing artifacts with versioned commits
// Stub implementation when git build tag is not present
type ArtifactRepo struct {
	path string
}

// ArtifactVersion represents a version of artifacts in the repo
type ArtifactVersion struct {
	Tag     string // Version tag (v1, v2, v3, or timestamp/digest)
	Commit  string // Stub - returns empty string
	Created time.Time
}

// CreateArtifactRepo creates a new Git repository from cached artifacts
// Stub implementation - returns error when git support is not compiled
func CreateArtifactRepo(cacheDir, repoName string) (*ArtifactRepo, error) {
	return nil, fmt.Errorf("git support not compiled (build with -tags git to enable)")
}

// AddVersion creates a new versioned commit from artifacts
// Stub implementation - returns error when git support is not compiled
func (ar *ArtifactRepo) AddVersion(versionTag string, artifactPaths []string) (*ArtifactVersion, error) {
	return nil, fmt.Errorf("git support not compiled (build with -tags git to enable)")
}

// RemoveVersion removes a version tag (and optionally the commit)
// Stub implementation - returns error when git support is not compiled
func (ar *ArtifactRepo) RemoveVersion(versionTag string) error {
	return fmt.Errorf("git support not compiled (build with -tags git to enable)")
}

// ListVersions returns all version tags in the repository
// Stub implementation - returns empty list when git support is not compiled
func (ar *ArtifactRepo) ListVersions() ([]ArtifactVersion, error) {
	return []ArtifactVersion{}, fmt.Errorf("git support not compiled (build with -tags git to enable)")
}

// GetRepoPath returns the filesystem path to the repository
func (ar *ArtifactRepo) GetRepoPath() string {
	return ar.path
}

// GetLatestCommit returns the commit hash that HEAD points to
// Stub implementation - returns error when git support is not compiled
func (ar *ArtifactRepo) GetLatestCommit() (string, error) {
	return "", fmt.Errorf("git support not compiled (build with -tags git to enable)")
}

// Cleanup removes the ephemeral repository
func (ar *ArtifactRepo) Cleanup() error {
	if ar.path != "" {
		return os.RemoveAll(ar.path)
	}
	return nil
}
