//go:build !git
// +build !git

package git

import (
	"fmt"
	"net/http"
)

// GitHTTPServer handles Git Smart HTTP protocol requests
// Stub implementation when git build tag is not present
type GitHTTPServer struct {
	repoManager *RepoManager
}

// NewGitHTTPServer creates a new Git HTTP server
// Stub implementation - returns a server that returns 503 errors
func NewGitHTTPServer(repoManager *RepoManager) *GitHTTPServer {
	return &GitHTTPServer{
		repoManager: repoManager,
	}
}

// ServeHTTP handles Git Smart HTTP protocol requests
// Stub implementation - returns 503 Service Unavailable
func (s *GitHTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "Git support not compiled (build with -tags git to enable)", http.StatusServiceUnavailable)
}

// RepoManager manages artifact repositories
// Stub implementation when git build tag is not present
type RepoManager struct {
	cacheDir string
	repos    map[string]*ArtifactRepo
}

// NewRepoManager creates a new repository manager
func NewRepoManager(cacheDir string) *RepoManager {
	return &RepoManager{
		cacheDir: cacheDir,
		repos:    make(map[string]*ArtifactRepo),
	}
}

// GetRepo gets or creates a repository
// Stub implementation - returns error when git support is not compiled
func (rm *RepoManager) GetRepo(repoName string) (*ArtifactRepo, error) {
	return nil, fmt.Errorf("git support not compiled (build with -tags git to enable)")
}
