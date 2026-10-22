package api

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// handleDockerRepositories handles the /api/v1/docker/repositories endpoint
func (a *API) handleDockerRepositories(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	repos, err := a.listDockerRepositories(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error listing repositories: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]interface{}{
		"repositories": repos,
	})
}

// handleDockerRepositoryDetail handles the /api/v1/docker/repositories/{repo} endpoint
// Also handles /api/v1/docker/repositories/{repo}/tags/{tag} for delete
func (a *API) handleDockerRepositoryDetail(w http.ResponseWriter, r *http.Request) {
	// Extract path after /api/v1/docker/repositories/
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/docker/repositories/")
	if path == "" {
		http.Error(w, "Repository name required", http.StatusBadRequest)
		return
	}

	// Check if this is a tag-specific operation (has /tags/ in path)
	if strings.Contains(path, "/tags/") {
		a.handleDockerTagDelete(w, r)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check if detailed=true query param is set
	detailed := r.URL.Query().Get("detailed") == "true"

	if detailed {
		// Get detailed tag information
		tagDetails, err := a.getDockerRepositoryTagsWithDetails(path)
		if err != nil {
			http.Error(w, fmt.Sprintf("Error getting tags: %v", err), http.StatusInternalServerError)
			return
		}

		writeJSON(w, map[string]interface{}{
			"repository": path,
			"tags":       tagDetails,
		})
		return
	}

	// Get simple tag list
	tags, err := a.getDockerRepositoryTags(r, path)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error getting tags: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]interface{}{
		"repository": path,
		"tags":       tags,
	})
}

// getDockerImageCount returns the number of Docker images in the cache
func (a *API) getDockerImageCount() int {
	repos, err := a.listDockerRepositories(nil)
	if err != nil {
		return 0
	}
	return len(repos)
}

// listDockerRepositories lists all Docker repositories in the cache
func (a *API) listDockerRepositories(r *http.Request) ([]map[string]interface{}, error) {
	// Check if we're on host (not in Docker) first - if so, always use filesystem
	// On host, /v2/ endpoints aren't available on port 3128, so skip API entirely
	isDocker := false
	if _, err := os.Stat("/app"); err == nil {
		isDocker = true
	}

	// If not in Docker, skip API and go straight to filesystem
	if !isDocker {
		// Skip to filesystem path below
	} else {
		// In Docker - try to use registry API first
		var registryURL string
		if r != nil {
			// Use the request's host/port, falling back to configured port
			// In Docker, if accessing via proxy port, use 443 for registry API
			registryURL = util.GetRegistryURL(r, r.TLS != nil)
		} else {
			// No request context - use default
			registryURL = os.Getenv("LOCAL_REGISTRY_URL")
			if registryURL == "" {
				// Check if in Docker
				if _, err := os.Stat("/app"); err == nil {
					registryURL = "https://localhost:443"
				} else {
					registryURL = util.GetRegistryURL(nil, false)
				}
			}
		}

		// Try the API
		if registryURL != "" {
			// In Docker - try the API
			client := &http.Client{
				Timeout: 1 * time.Second, // Very short timeout
			}
			resp, err := client.Get(registryURL + "/v2/_catalog")
			if err != nil {
				// Fall back to filesystem
			} else {
				defer util.DeferClose(resp.Body)

				if resp.StatusCode == 200 {
					var catalog struct {
						Repositories []string `json:"repositories"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&catalog); err == nil {
						var repos []map[string]interface{}
						for _, repo := range catalog.Repositories {
							// Get tag count - create a dummy request with the same host/port
							dummyReq := &http.Request{
								Host: strings.TrimPrefix(strings.TrimPrefix(registryURL, "https://"), "http://"),
							}
							if strings.HasPrefix(registryURL, "https://") {
								dummyReq.TLS = &tls.ConnectionState{}
							}
							tags, _ := a.getDockerRepositoryTags(dummyReq, repo)
							repos = append(repos, map[string]interface{}{
								"name":      repo,
								"tag_count": len(tags),
							})
						}
						return repos, nil
					}
				}
				// If API call failed, fall through to filesystem
			}
		}
	}

	// Running on host - read directly from filesystem cache
	manifestDir := filepath.Join(a.cacheDir, "docker-manifests")

	// Check if manifest directory exists
	if _, err := os.Stat(manifestDir); os.IsNotExist(err) {
		// Directory doesn't exist - return empty list
		return []map[string]interface{}{}, nil
	}

	repos := make(map[string]map[string]bool) // repo -> set of tags

	err := filepath.Walk(manifestDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		// Skip sidecar files (.meta content-type cache, .metadata helm chart info)
		if strings.HasSuffix(path, ".meta") || strings.HasSuffix(path, ".metadata") {
			return nil
		}

		relPath, err := filepath.Rel(manifestDir, path)
		if err != nil {
			return nil
		}

		parts := strings.Split(relPath, string(filepath.Separator))
		// New structure: <registry>/<repo>/<reference>
		if len(parts) != 3 {
			return nil
		}

		registryID := parts[0]
		safeRepo := parts[1]
		reference := parts[2]

		// Skip digest-only manifests (only count tags)
		if strings.HasPrefix(reference, "sha256:") {
			return nil
		}

		// Include registry in repo key to avoid collisions: <registry>/<repo>
		repo := registryID + "/" + util.UnsafeRepoName(safeRepo)

		// It's a tag file
		if repos[repo] == nil {
			repos[repo] = make(map[string]bool)
		}
		repos[repo][reference] = true
		return nil
	})

	if err != nil {
		return nil, err
	}

	// Calculate size, date, and layer counts for each repo
	var result []map[string]interface{}

	for repo, tags := range repos {
		repoData := map[string]interface{}{
			"name":        repo,
			"tag_count":   len(tags),
			"size":        int64(0),
			"date":        "",
			"slices":      0,
			"license":     "",
			"description": "",
		}

		// Parse repo name to get registry and safe repo path
		parts := strings.Split(repo, "/")
		if len(parts) < 2 {
			result = append(result, repoData)
			continue
		}

		registryID := parts[0]
		safeRepo := util.SafeRepoName(strings.Join(parts[1:], "/"))

		// Find the most recent tag and get its manifest
		var latestTime time.Time
		var totalSize int64
		var maxLayers int
		var repoLicense string
		var repoDescription string

		// First, check all digest manifests in this repo for metadata
		// (metadata is often saved for digest manifests, not tag manifests)
		repoDir := filepath.Join(manifestDir, registryID, safeRepo)
		if entries, err := os.ReadDir(repoDir); err == nil {
			for _, entry := range entries {
				if entry.IsDir() || strings.HasSuffix(entry.Name(), ".meta") || strings.HasSuffix(entry.Name(), ".metadata") {
					continue
				}
				// Check digest manifests for metadata
				if strings.HasPrefix(entry.Name(), "sha256:") {
					digestManifestPath := filepath.Join(repoDir, entry.Name())
					metaPath := digestManifestPath + ".metadata"
					if metaData, err := os.ReadFile(metaPath); err == nil {
						var metadata map[string]string
						if json.Unmarshal(metaData, &metadata) == nil {
							// Use metadata from digest manifests (they're more likely to have it)
							if repoLicense == "" && metadata["license"] != "" {
								repoLicense = metadata["license"]
							}
							if repoDescription == "" && metadata["description"] != "" {
								repoDescription = metadata["description"]
							}
						}
					}
				}
			}
		}

		for tag := range tags {
			// Get manifest path for this tag
			manifestPath := filepath.Join(manifestDir, registryID, safeRepo, tag)
			if info, err := os.Stat(manifestPath); err == nil {
				// Track latest modification time
				if info.ModTime().After(latestTime) {
					latestTime = info.ModTime()
				}

				// Try to read metadata file (first from tag path, then from digest path)
				// Only check if we haven't found metadata yet from digest manifests
				if repoLicense == "" || repoDescription == "" {
					metaPath := manifestPath + ".metadata"
					metaData, err := os.ReadFile(metaPath)
					if err != nil {
						// Metadata not found for tag - try reading tag manifest to get digest
						// and check digest manifest for metadata
						manifestData, manifestErr := os.ReadFile(manifestPath)
						if manifestErr == nil {
							// Calculate digest of tag manifest
							hash := sha256.Sum256(manifestData)
							digest := "sha256:" + hex.EncodeToString(hash[:])
							// Check for metadata on digest manifest
							digestManifestPath := filepath.Join(manifestDir, registryID, safeRepo, digest)
							metaPath = digestManifestPath + ".metadata"
							metaData, err = os.ReadFile(metaPath)
						}
					}

					if err == nil && metaData != nil {
						var metadata map[string]string
						if json.Unmarshal(metaData, &metadata) == nil {
							// Use metadata from the most recent tag (or first one found)
							if repoLicense == "" && metadata["license"] != "" {
								repoLicense = metadata["license"]
							}
							if repoDescription == "" && metadata["description"] != "" {
								repoDescription = metadata["description"]
							}
						} else {
							// Try unmarshaling as struct (for backwards compatibility)
							var metaStruct struct {
								License     string `json:"license"`
								Description string `json:"description"`
							}
							if json.Unmarshal(metaData, &metaStruct) == nil {
								if repoLicense == "" && metaStruct.License != "" {
									repoLicense = metaStruct.License
								}
								if repoDescription == "" && metaStruct.Description != "" {
									repoDescription = metaStruct.Description
								}
							}
						}
					}
				}

				// Read manifest to get layer count and sizes
				manifestData, err := os.ReadFile(manifestPath)
				if err == nil {
					// First check if this is a manifest list (multi-arch)
					var manifestList struct {
						MediaType string `json:"mediaType"`
						Manifests []struct {
							Digest string `json:"digest"`
							Size   int64  `json:"size"`
						} `json:"manifests"`
					}

					isManifestList := false
					if json.Unmarshal(manifestData, &manifestList) == nil {
						isManifestList = manifestList.MediaType == "application/vnd.oci.image.index.v1+json" ||
							manifestList.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
					}

					if isManifestList && len(manifestList.Manifests) > 0 {
						// For manifest lists, try to resolve to platform manifests
						// Try each platform manifest in the list
						foundPlatformManifest := false
						for _, m := range manifestList.Manifests {
							digestManifestPath := filepath.Join(manifestDir, registryID, safeRepo, m.Digest)
							if digestData, err := os.ReadFile(digestManifestPath); err == nil {
								manifestData = digestData
								foundPlatformManifest = true
								break // Use the first one we find
							}
						}
						// If we couldn't find any platform manifests, skip this manifest list
						if !foundPlatformManifest {
							continue
						}
					}

					var manifest struct {
						Layers []struct {
							Digest string `json:"digest"`
							Size   int64  `json:"size"`
						} `json:"layers"`
						Config struct {
							Digest string `json:"digest"`
							Size   int64  `json:"size"`
						} `json:"config"`
						// Also try uppercase for Docker manifest v2 schema 2
						ConfigUpper struct {
							Digest string `json:"Digest"`
							Size   int64  `json:"Size"`
						} `json:"Config"`
						LayersUpper []struct {
							Digest string `json:"Digest"`
							Size   int64  `json:"Size"`
						} `json:"Layers"`
					}

					if json.Unmarshal(manifestData, &manifest) == nil {
						// Count layers
						layerCount := len(manifest.Layers)
						if layerCount == 0 {
							layerCount = len(manifest.LayersUpper)
						}
						if layerCount > maxLayers {
							maxLayers = layerCount
						}

						// Sum up sizes from manifest (sizes are already in the manifest)
						// Try lowercase first, then uppercase
						var repoSize int64
						if manifest.Config.Size > 0 {
							repoSize = manifest.Config.Size
						} else if manifest.ConfigUpper.Size > 0 {
							repoSize = manifest.ConfigUpper.Size
						}

						if len(manifest.Layers) > 0 {
							for _, layer := range manifest.Layers {
								repoSize += layer.Size
							}
						} else if len(manifest.LayersUpper) > 0 {
							for _, layer := range manifest.LayersUpper {
								repoSize += layer.Size
							}
						}

						if repoSize > totalSize {
							totalSize = repoSize
						}
					}
				}
			}
		}

		// Set the calculated values
		repoData["size"] = totalSize
		repoData["slices"] = maxLayers
		if !latestTime.IsZero() {
			repoData["date"] = latestTime.Format("2006-01-02 15:04:05")
		}
		if repoLicense != "" {
			repoData["license"] = repoLicense
		}
		if repoDescription != "" {
			repoData["description"] = repoDescription
		}

		result = append(result, repoData)
	}

	return result, nil
}

// getDockerRepositoryTags gets the tags for a specific Docker repository
func (a *API) getDockerRepositoryTags(r *http.Request, repo string) ([]string, error) {
	// Use the request's hostname and port for registry API calls
	var registryURL string
	if r != nil {
		// Use the request's host/port, falling back to configured port
		registryURL = util.GetRegistryURL(r, r.TLS != nil)
	} else {
		// No request context - check environment or use filesystem
		registryURL = os.Getenv("LOCAL_REGISTRY_URL")
		if registryURL == "" {
			// Check if in Docker
			if _, err := os.Stat("/app"); err == nil {
				registryURL = "https://localhost:443"
			} else {
				// Use configured proxy address
				registryURL = util.GetRegistryURL(nil, false)
			}
		}
	}

	// If we have a registry URL, try to use the API first
	// But on host, skip this since /v2/ endpoints aren't available on port 3128
	if registryURL != "" {
		// Check if we're on host (not in Docker) - if so, skip API and use filesystem
		if _, err := os.Stat("/app"); os.IsNotExist(err) {
			// Skip API, use filesystem
		} else {
			// In Docker - try the API
			client := &http.Client{
				Timeout: 1 * time.Second, // Very short timeout
			}
			resp, err := client.Get(fmt.Sprintf("%s/v2/%s/tags/list", registryURL, repo))
			if err != nil {
				// Fall back to filesystem
			} else {
				defer util.DeferClose(resp.Body)

				if resp.StatusCode == 200 {
					var tagsResp struct {
						Name string   `json:"name"`
						Tags []string `json:"tags"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&tagsResp); err == nil {
						return tagsResp.Tags, nil
					}
				}
				// If API call failed, fall through to filesystem
			}
		}
	}

	// Running on host - read directly from filesystem cache
	manifestDir := filepath.Join(a.cacheDir, "docker-manifests")

	// Parse repo to get registry and repo name
	// Repo format: <registry>/<repo> (e.g., "docker.io/library/alpine")
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid repo format: %s", repo)
	}
	registryID := parts[0]
	repoName := parts[1]
	safeRepo := util.SafeRepoName(repoName)

	repoDir := filepath.Join(manifestDir, registryID, safeRepo)
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		return []string{}, nil // No tags found
	}

	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return nil, err
	}

	var tags []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".meta") || strings.HasSuffix(entry.Name(), ".metadata") {
			continue
		}
		// Only include tag files (not digest-only manifests)
		if !strings.HasPrefix(entry.Name(), "sha256:") {
			tags = append(tags, entry.Name())
		}
	}

	return tags, nil
}

// DockerTagDetail contains detailed information about a Docker tag
type DockerTagDetail struct {
	Tag       string `json:"tag"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Date      string `json:"date"`
	Layers    int    `json:"layers"`
	MediaType string `json:"media_type,omitempty"`
}

// getDockerRepositoryTagsWithDetails gets detailed tag information for a repository
func (a *API) getDockerRepositoryTagsWithDetails(repo string) ([]DockerTagDetail, error) {
	manifestDir := filepath.Join(a.cacheDir, "docker-manifests")

	// Parse repo to get registry and repo name
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid repo format: %s", repo)
	}
	registryID := parts[0]
	repoName := parts[1]
	safeRepo := util.SafeRepoName(repoName)

	repoDir := filepath.Join(manifestDir, registryID, safeRepo)
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		return []DockerTagDetail{}, nil
	}

	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return nil, err
	}

	var details []DockerTagDetail
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".meta") || strings.HasSuffix(entry.Name(), ".metadata") {
			continue
		}
		// Skip digest-only manifests
		if strings.HasPrefix(entry.Name(), "sha256:") {
			continue
		}

		tag := entry.Name()
		manifestPath := filepath.Join(repoDir, tag)

		detail := DockerTagDetail{Tag: tag}

		// Get file info for date
		if info, err := os.Stat(manifestPath); err == nil {
			detail.Date = info.ModTime().Format("2006-01-02 15:04:05")
		}

		// Read manifest to get digest, size, and layers
		manifestData, err := os.ReadFile(manifestPath)
		if err != nil {
			details = append(details, detail)
			continue
		}

		// Calculate digest
		hash := sha256.Sum256(manifestData)
		detail.Digest = "sha256:" + hex.EncodeToString(hash[:])

		// Parse manifest
		var manifest struct {
			MediaType string `json:"mediaType"`
			Layers    []struct {
				Digest string `json:"digest"`
				Size   int64  `json:"size"`
			} `json:"layers"`
			Config struct {
				Size int64 `json:"size"`
			} `json:"config"`
			Manifests []struct {
				Digest string `json:"digest"`
				Size   int64  `json:"size"`
			} `json:"manifests"`
		}

		if json.Unmarshal(manifestData, &manifest) == nil {
			detail.MediaType = manifest.MediaType

			// Check if this is a manifest list
			isManifestList := manifest.MediaType == "application/vnd.oci.image.index.v1+json" ||
				manifest.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json"

			if isManifestList && len(manifest.Manifests) > 0 {
				// Try to get details from first platform manifest
				for _, m := range manifest.Manifests {
					platformManifestPath := filepath.Join(repoDir, m.Digest)
					if platformData, err := os.ReadFile(platformManifestPath); err == nil {
						var platformManifest struct {
							Layers []struct {
								Size int64 `json:"size"`
							} `json:"layers"`
							Config struct {
								Size int64 `json:"size"`
							} `json:"config"`
						}
						if json.Unmarshal(platformData, &platformManifest) == nil {
							detail.Layers = len(platformManifest.Layers)
							detail.Size = platformManifest.Config.Size
							for _, layer := range platformManifest.Layers {
								detail.Size += layer.Size
							}
						}
						break
					}
				}
			} else {
				// Regular manifest
				detail.Layers = len(manifest.Layers)
				detail.Size = manifest.Config.Size
				for _, layer := range manifest.Layers {
					detail.Size += layer.Size
				}
			}
		}

		details = append(details, detail)
	}

	return details, nil
}

// handleDockerTagDelete handles DELETE /api/v1/docker/repositories/{repo}/tags/{tag}
func (a *API) handleDockerTagDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract repo and tag from path: /api/v1/docker/repositories/{repo}/tags/{tag}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/docker/repositories/")
	tagsIdx := strings.Index(path, "/tags/")
	if tagsIdx == -1 {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	repo := path[:tagsIdx]
	tag := path[tagsIdx+6:] // len("/tags/") == 6

	if repo == "" || tag == "" {
		http.Error(w, "Repository and tag required", http.StatusBadRequest)
		return
	}

	if err := a.deleteDockerTag(repo, tag); err != nil {
		http.Error(w, fmt.Sprintf("Error deleting tag: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// deleteDockerTag deletes a specific Docker tag and decrements blob refcounts
func (a *API) deleteDockerTag(repo, tag string) error {
	manifestDir := filepath.Join(a.cacheDir, "docker-manifests")

	// Parse repo to get registry and repo name
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid repo format: %s", repo)
	}
	registryID := parts[0]
	repoName := parts[1]
	safeRepo := util.SafeRepoName(repoName)

	manifestPath := filepath.Join(manifestDir, registryID, safeRepo, tag)

	// Check if manifest exists
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		return fmt.Errorf("tag not found: %s:%s", repo, tag)
	}

	if _, err := cache.RemoveManifest(a.cacheDir, manifestPath); err != nil {
		return err
	}
	return nil
}

// handleDockerBulkDelete handles POST /api/v1/docker/bulk-delete
func (a *API) handleDockerBulkDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Items []struct {
			Repo string `json:"repo"`
			Tag  string `json:"tag"`
		} `json:"items"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if len(req.Items) == 0 {
		http.Error(w, "No items provided", http.StatusBadRequest)
		return
	}

	var deleted []map[string]string
	var failed []map[string]string

	for _, item := range req.Items {
		if err := a.deleteDockerTag(item.Repo, item.Tag); err != nil {
			failed = append(failed, map[string]string{
				"repo":  item.Repo,
				"tag":   item.Tag,
				"error": err.Error(),
			})
		} else {
			deleted = append(deleted, map[string]string{
				"repo": item.Repo,
				"tag":  item.Tag,
			})
		}
	}

	writeJSON(w, map[string]interface{}{
		"deleted": deleted,
		"failed":  failed,
	})
}
