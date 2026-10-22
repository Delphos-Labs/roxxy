package commands

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/cli"
	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// getExcludedPlatforms returns a map of excluded platforms from config
// Returns empty map if not configured (all platforms are pulled by default)
func getExcludedPlatforms() map[string]bool {
	excluded := make(map[string]bool)

	// Get from config (defaults to empty if not set)
	globalConfig := config.GetGlobalConfig()
	if globalConfig != nil && len(globalConfig.ExcludedPlatforms) > 0 {
		for _, platform := range globalConfig.ExcludedPlatforms {
			excluded[platform] = true
		}
	}

	return excluded
}

// pullDockerImage pulls a Docker image from a registry into roxxy's cache
// Supports multiple platforms for multi-arch images
// If tag is "latest" and not in airgapped mode, checks for newer version after 24 hours or if force is true
// caCertPath is optional - if provided, the CA certificate will be used to verify TLS connections
// Returns an error instead of calling os.Exit
func pullDockerImage(cfg Config, imageSpec string, platforms []string, force bool, caCertPath string) error {
	// Parse image spec: repo:tag
	var repoName, tag string
	if strings.Contains(imageSpec, ":") {
		parts := strings.SplitN(imageSpec, ":", 2)
		repoName = parts[0]
		tag = parts[1]
	} else {
		return fmt.Errorf("image specification must include tag: %s (expected repo:tag)", imageSpec)
	}

	// Determine registry URL from repo name and remove registry prefix from repoName
	registryURL, actualRepoName := determineRegistryURLAndRepo(repoName)
	repoName = actualRepoName
	baseURL := strings.TrimSuffix(registryURL, "/")

	// Normalize repo name (e.g., "golang" -> "library/golang")
	// Only do this for Docker Hub (not for other registries like quay.io, ghcr.io)
	if registryURL == "https://registry-1.docker.io" && !strings.Contains(repoName, "/") {
		repoName = "library/" + repoName
	}

	// Check if we should verify tag freshness
	// For "latest" tags: check if >24 hours old or if force is requested
	// For other tags: only check if force is requested
	shouldCheckTag := false
	if !isAirgapped(cfg) {
		cachedDigest, cachedTime := getCachedTagDigest(cfg.CacheDir, repoName, tag, registryURL)
		if cachedDigest != "" {
			if force {
				// Force always checks registry
				shouldCheckTag = true
				fmt.Printf("Force checking for newer '%s' tag (cached: %s, age: %s)...\n",
					tag, cachedDigest[:12], formatDuration(time.Since(cachedTime)))
			} else if tag == "latest" && time.Since(cachedTime) > 24*time.Hour {
				// For "latest" tags, check if >24 hours old
				shouldCheckTag = true
				fmt.Printf("Checking for newer 'latest' tag (cached: %s, age: %s)...\n",
					cachedDigest[:12], formatDuration(time.Since(cachedTime)))
			} else if tag == "latest" {
				// "latest" tag is recent, skip registry check
				fmt.Printf("Using cached 'latest' tag (age: %s, use --force to check for updates)\n",
					formatDuration(time.Since(cachedTime)))
			} else {
				// Non-latest tag, use cached version (unless forced)
				fmt.Printf("Using cached '%s' tag (age: %s, use --force to check for updates)\n",
					tag, formatDuration(time.Since(cachedTime)))
			}
		}
	}

	fmt.Printf("Pulling %s:%s from %s...\n", repoName, tag, baseURL)
	if len(platforms) > 0 {
		fmt.Printf("Platforms: %s\n", strings.Join(platforms, ", "))
	}

	// Create HTTP client with optional CA certificate for TLS verification
	transport := &http.Transport{
		Proxy: config.GetUpstreamProxyFunc(),
	}

	// Configure TLS if CA certificate is provided
	if caCertPath != "" {
		caCertPEM, err := os.ReadFile(caCertPath)
		if err != nil {
			fmt.Printf("Warning: Failed to read CA certificate from %s: %v\n", caCertPath, err)
			fmt.Printf("  Continuing without custom CA certificate (TLS verification may fail)\n")
		} else {
			caCertPool := x509.NewCertPool()
			block, _ := pem.Decode(caCertPEM)
			if block != nil {
				caCert, err := x509.ParseCertificate(block.Bytes)
				if err == nil {
					caCertPool.AddCert(caCert)
					transport.TLSClientConfig = &tls.Config{
						RootCAs: caCertPool,
					}
					fmt.Printf("Using CA certificate from %s for TLS verification\n", caCertPath)
				} else {
					fmt.Printf("Warning: Failed to parse CA certificate: %v\n", err)
				}
			} else {
				fmt.Printf("Warning: Failed to decode CA certificate PEM\n")
			}
		}
	}

	client := &http.Client{
		Timeout:   30 * time.Minute, // Long timeout for large blobs
		Transport: transport,
	}

	var authToken string

	// If checking for newer tag, do HEAD request first to get current digest
	var currentDigest string
	if shouldCheckTag {
		manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", baseURL, repoName, tag)
		req, err := http.NewRequest("HEAD", manifestURL, nil)
		if err == nil {
			req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.index.v1+json")

			// Get token if needed (will be set by fetchWithAuth if needed)
			resp, err := client.Do(req)
			if err == nil {
				defer util.DeferClose(resp.Body)
				// Handle auth challenge
				if resp.StatusCode == http.StatusUnauthorized {
					wwwAuth := resp.Header.Get("WWW-Authenticate")
					if wwwAuth != "" {
						token, err := getDockerTokenFromChallenge(wwwAuth, fmt.Sprintf("/v2/%s/manifests/%s", repoName, tag))
						if err == nil {
							authToken = token
							req.Header.Set("Authorization", "Bearer "+authToken)
							resp, err = client.Do(req)
							if err == nil {
								defer util.DeferClose(resp.Body)
							}
						}
					}
				}
				if resp.StatusCode == http.StatusOK {
					currentDigest = resp.Header.Get("Docker-Content-Digest")
					cachedDigest, _ := getCachedTagDigest(cfg.CacheDir, repoName, tag, registryURL)
					if currentDigest != "" && currentDigest == cachedDigest {
						fmt.Printf("'%s' tag is up to date (digest: %s)\n", tag, currentDigest[:12])
						return nil // No need to pull, already have the latest
					} else if currentDigest != "" {
						fmt.Printf("Found newer '%s' tag (old: %s, new: %s)\n",
							tag, cachedDigest[:12], currentDigest[:12])
					}
				}
			}
		}
	}

	// Fetch manifest
	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", baseURL, repoName, tag)
	manifestData, contentType, err := fetchWithAuth(client, manifestURL, &authToken, repoName, fmt.Sprintf("/v2/%s/manifests/%s", repoName, tag))
	if err != nil {
		return fmt.Errorf("fetching manifest: %w", err)
	}

	// Parse manifest to determine if it's an index or single-platform
	var manifestJSON map[string]interface{}
	if err := json.Unmarshal(manifestData, &manifestJSON); err != nil {
		return fmt.Errorf("parsing manifest: %w", err)
	}

	// Check if this is a multi-platform index
	manifests, hasManifests := manifestJSON["manifests"].([]interface{})
	_, hasLayers := manifestJSON["layers"].([]interface{})

	var platformManifests []map[string]interface{}

	if hasManifests && len(manifests) > 0 {
		// Multi-platform index - resolve to platform-specific manifests
		fmt.Printf("Found multi-platform image index with %d platforms\n", len(manifests))

		// Get excluded platforms from config
		excludedPlatforms := getExcludedPlatforms()

		// Helper function to check if a platform should be excluded
		isPlatformExcluded := func(platformOS, platformArch, platformVariant string) bool {
			// Build platform string (e.g., "linux/arm/v6" or "linux/amd64")
			platformStr := platformOS + "/" + platformArch
			if platformVariant != "" {
				platformStr += "/" + platformVariant
			}
			return excludedPlatforms[platformStr]
		}

		// If no platforms specified, default to all platforms (excluding configured ones)
		if len(platforms) == 0 {
			if len(excludedPlatforms) > 0 {
				excludedList := make([]string, 0, len(excludedPlatforms))
				for platform := range excludedPlatforms {
					excludedList = append(excludedList, platform)
				}
				fmt.Printf("No platforms specified, pulling all platforms (excluding: %s)\n",
					strings.Join(excludedList, ", "))
			} else {
				fmt.Printf("No platforms specified, pulling all platforms\n")
			}
			for _, m := range manifests {
				manifestEntry, ok := m.(map[string]interface{})
				if !ok {
					continue
				}
				platformObj, ok := manifestEntry["platform"].(map[string]interface{})
				if !ok {
					continue
				}
				platformOS, _ := platformObj["os"].(string)
				platformArch, _ := platformObj["architecture"].(string)
				platformVariant, _ := platformObj["variant"].(string)

				// Skip excluded platforms
				if isPlatformExcluded(platformOS, platformArch, platformVariant) {
					fmt.Printf("  Skipping excluded platform: %s/%s", platformOS, platformArch)
					if platformVariant != "" {
						fmt.Printf("/%s", platformVariant)
					}
					fmt.Printf("\n")
					continue
				}

				// Log which platform is being included
				platformStr := platformOS + "/" + platformArch
				if platformVariant != "" {
					platformStr += "/" + platformVariant
				}
				fmt.Printf("  Including platform: %s\n", platformStr)
				platformManifests = append(platformManifests, manifestEntry)
			}
		} else {
			// Filter by requested platforms (but still exclude unwanted ones)
			for _, m := range manifests {
				manifestEntry, ok := m.(map[string]interface{})
				if !ok {
					continue
				}
				platformObj, ok := manifestEntry["platform"].(map[string]interface{})
				if !ok {
					continue
				}
				platformOS, _ := platformObj["os"].(string)
				platformArch, _ := platformObj["architecture"].(string)
				platformVariant, _ := platformObj["variant"].(string)
				platformStr := platformOS + "/" + platformArch
				if platformVariant != "" {
					platformStr += "/" + platformVariant
				}

				// Skip excluded platforms even if explicitly requested
				if isPlatformExcluded(platformOS, platformArch, platformVariant) {
					fmt.Printf("  Skipping excluded platform: %s (even though requested)\n", platformStr)
					continue
				}

				for _, requestedPlatform := range platforms {
					// Parse requested platform (format: "os/arch" or just "arch")
					requestedOS, requestedArch := parsePlatform(requestedPlatform)
					if (requestedOS == "" || requestedOS == platformOS) && requestedArch == platformArch {
						platformManifests = append(platformManifests, manifestEntry)
						fmt.Printf("  Selected platform: %s\n", platformStr)
						break
					}
				}
			}
		}

		if len(platformManifests) == 0 {
			return fmt.Errorf("no matching platforms found")
		}

		// Store the index manifest
		manifestDigest := calculateDigest(manifestData)
		if err := storeManifestInCache(cfg.CacheDir, repoName, tag, manifestData, contentType, registryURL); err != nil {
			return fmt.Errorf("storing index manifest: %w", err)
		}
		if err := storeManifestInCache(cfg.CacheDir, repoName, manifestDigest, manifestData, contentType, registryURL); err != nil {
			return fmt.Errorf("storing index manifest by digest: %w", err)
		}
		fmt.Printf("Stored index manifest\n")

		// Fetch and store platform-specific manifests
		for _, platformManifest := range platformManifests {
			digest, ok := platformManifest["digest"].(string)
			if !ok || digest == "" {
				fmt.Printf("Warning: Skipping manifest entry without digest\n")
				continue
			}
			fmt.Printf("Fetching platform manifest %s...\n", digest[:12])

			platformManifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", baseURL, repoName, digest)
			platformManifestData, platformContentType, err := fetchWithAuth(client, platformManifestURL, &authToken, repoName, fmt.Sprintf("/v2/%s/manifests/%s", repoName, digest))
			if err != nil {
				fmt.Printf("Error fetching platform manifest: %v\n", err)
				continue
			}

			if err := storeManifestInCache(cfg.CacheDir, repoName, digest, platformManifestData, platformContentType, registryURL); err != nil {
				fmt.Printf("Error storing platform manifest: %v\n", err)
				continue
			}

			// Extract and download blobs for this platform
			configDigest, layerDigests, err := cli.ExtractBlobDigestsFromManifest(platformManifestData)
			if err != nil {
				fmt.Printf("Error extracting blob digests: %v\n", err)
				continue
			}

			allBlobs := append([]string{configDigest}, layerDigests...)
			if err := pullBlobs(client, cfg.CacheDir, baseURL, repoName, allBlobs, &authToken); err != nil {
				fmt.Printf("Error pulling blobs: %v\n", err)
				continue
			}
		}
	} else if hasLayers {
		// Single-platform manifest
		fmt.Printf("Found single-platform manifest\n")

		// Store manifest
		manifestDigest := calculateDigest(manifestData)
		if err := storeManifestInCache(cfg.CacheDir, repoName, tag, manifestData, contentType, registryURL); err != nil {
			return fmt.Errorf("storing manifest: %w", err)
		}
		if err := storeManifestInCache(cfg.CacheDir, repoName, manifestDigest, manifestData, contentType, registryURL); err != nil {
			return fmt.Errorf("storing manifest by digest: %w", err)
		}
		fmt.Printf("Stored manifest\n")

		// Extract and download blobs
		configDigest, layerDigests, err := cli.ExtractBlobDigestsFromManifest(manifestData)
		if err != nil {
			return fmt.Errorf("extracting blob digests: %w", err)
		}

		allBlobs := append([]string{configDigest}, layerDigests...)
		if err := pullBlobs(client, cfg.CacheDir, baseURL, repoName, allBlobs, &authToken); err != nil {
			return fmt.Errorf("pulling blobs: %w", err)
		}
	} else {
		return fmt.Errorf("unknown manifest format")
	}

	fmt.Printf("Successfully pulled %s:%s into roxxy cache\n", repoName, tag)
	return nil
}

// determineRegistryURLAndRepo determines the registry URL and actual repository name
// Returns (registryURL, actualRepoName)
// Examples:
//   - "quay.io/jetstack/cert-manager-controller" -> ("https://quay.io", "jetstack/cert-manager-controller")
//   - "ghcr.io/org/repo" -> ("https://ghcr.io", "org/repo")
//   - "library/golang" -> ("https://registry-1.docker.io", "library/golang")
func determineRegistryURLAndRepo(repoName string) (string, string) {
	// Check if repo name contains a registry (e.g., "ghcr.io/org/repo")
	parts := strings.Split(repoName, "/")
	if len(parts) > 1 {
		firstPart := parts[0]
		// Check if first part looks like a registry (contains a dot)
		if strings.Contains(firstPart, ".") {
			// Registry found - return registry URL and repo name without registry prefix
			registryURL := "https://" + firstPart
			actualRepoName := strings.Join(parts[1:], "/")
			return registryURL, actualRepoName
		}
	}
	// Default to Docker Hub - repo name stays as-is
	return "https://registry-1.docker.io", repoName
}

// parsePlatform parses a platform string (e.g., "linux/amd64" or "amd64")
func parsePlatform(platform string) (os, arch string) {
	if strings.Contains(platform, "/") {
		parts := strings.Split(platform, "/")
		return parts[0], parts[1]
	}
	// If no OS specified, default to linux
	return "linux", platform
}

// fetchWithAuth fetches a URL with automatic authentication handling
func fetchWithAuth(client *http.Client, url string, authToken *string, repoName, path string) ([]byte, string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create request: %w", err)
	}

	// Add Accept headers for different manifest types
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.index.v1+json")

	if *authToken != "" {
		req.Header.Set("Authorization", "Bearer "+*authToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch: %w", err)
	}
	defer util.DeferClose(resp.Body)

	// Handle authentication challenge
	if resp.StatusCode == http.StatusUnauthorized {
		wwwAuth := resp.Header.Get("WWW-Authenticate")
		if wwwAuth != "" {
			token, err := getDockerTokenFromChallenge(wwwAuth, path)
			if err != nil {
				return nil, "", fmt.Errorf("failed to get auth token: %w", err)
			}
			*authToken = token
			req.Header.Set("Authorization", "Bearer "+*authToken)
			resp, err = client.Do(req)
			if err != nil {
				return nil, "", fmt.Errorf("failed to retry request: %w", err)
			}
			defer util.DeferClose(resp.Body)
		}
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/vnd.docker.distribution.manifest.v2+json"
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read response: %w", err)
	}

	return body, contentType, nil
}

// pullBlobs downloads blobs from the registry and stores them in cache
func pullBlobs(client *http.Client, cacheDir, baseURL, repoName string, blobDigests []string, authToken *string) error {
	fmt.Printf("Pulling %d blobs...\n", len(blobDigests))
	blobIndex := 0

	for _, digest := range blobDigests {
		blobIndex++
		fmt.Printf("  [%d/%d] Pulling blob %s...", blobIndex, len(blobDigests), digest[:12])

		// Check if blob already exists (check both new and old structure)
		blobPath := util.DockerBlobPathWithFallback(cacheDir, digest)
		if _, err := os.Stat(blobPath); err == nil {
			fmt.Printf(" already cached\n")
			continue
		}

		// Fetch blob
		blobURL := fmt.Sprintf("%s/v2/%s/blobs/%s", baseURL, repoName, digest)
		req, err := http.NewRequest("GET", blobURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create blob request: %w", err)
		}

		if *authToken != "" {
			req.Header.Set("Authorization", "Bearer "+*authToken)
		}

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("failed to fetch blob: %w", err)
		}
		defer util.DeferClose(resp.Body)

		// Handle authentication challenge
		if resp.StatusCode == http.StatusUnauthorized {
			wwwAuth := resp.Header.Get("WWW-Authenticate")
			if wwwAuth != "" {
				token, err := getDockerTokenFromChallenge(wwwAuth, fmt.Sprintf("/v2/%s/blobs/%s", repoName, digest))
				if err != nil {
					return fmt.Errorf("failed to get auth token: %w", err)
				}
				*authToken = token
				req.Header.Set("Authorization", "Bearer "+*authToken)
				resp, err = client.Do(req)
				if err != nil {
					return fmt.Errorf("failed to retry blob request: %w", err)
				}
				defer util.DeferClose(resp.Body)
			}
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
		}

		// Read blob data
		blobData, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read blob: %w", err)
		}

		// Verify digest
		hash := sha256.Sum256(blobData)
		expectedDigest := "sha256:" + hex.EncodeToString(hash[:])
		if digest != expectedDigest {
			return fmt.Errorf("digest mismatch: expected %s, got %s", expectedDigest, digest)
		}

		// Store blob using new subdirectory structure
		// Use DockerBlobPath (not WithFallback) to always use new structure for writing
		blobPath = util.DockerBlobPath(cacheDir, digest)

		// Ensure blob directory and all parent subdirectories exist
		blobDir := filepath.Dir(blobPath)
		if err := os.MkdirAll(blobDir, 0755); err != nil {
			return fmt.Errorf("failed to create blob directory %s: %w", blobDir, err)
		}

		if err := os.WriteFile(blobPath, blobData, 0644); err != nil {
			return fmt.Errorf("failed to write blob: %w", err)
		}

		// Increment refcount
		contentHash := strings.TrimPrefix(digest, "sha256:")
		if err := cache.IncrementRefcount(cacheDir, contentHash); err != nil {
			fmt.Printf("Warning: Failed to increment refcount: %v\n", err)
		}

		fmt.Printf(" done (%s)\n", formatBytes(int64(len(blobData))))
	}

	return nil
}

// storeManifestInCache stores a manifest in roxxy's cache structure
func storeManifestInCache(cacheDir, repoName, reference string, manifestData []byte, contentType, upstreamURL string) error {
	// Use the same path structure as DockerRegistryProxy
	registryID := NormalizeRegistryURLForPath(upstreamURL)
	safeRepo := SafeRepoName(repoName)
	manifestDir := filepath.Join(cacheDir, "docker-manifests", registryID, safeRepo)

	// Create directory
	if err := os.MkdirAll(manifestDir, 0755); err != nil {
		return fmt.Errorf("failed to create manifest directory: %w", err)
	}

	// Write manifest
	manifestPath := filepath.Join(manifestDir, reference)
	if err := os.WriteFile(manifestPath, manifestData, 0644); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}

	// Write metadata
	metaPath := manifestPath + ".meta"
	meta := DockerManifestMetadata{
		ContentType: contentType,
		StoredAt:    time.Now(),
		UpstreamURL: upstreamURL,
	}
	metaData, err := json.Marshal(meta)
	if err == nil {
		util.Logerr(os.WriteFile(metaPath, metaData, 0644))
	}

	return nil
}

// isAirgapped checks if roxxy is running in airgapped mode
func isAirgapped(cfg Config) bool {
	if cfg.GlobalConfig == nil {
		return false
	}
	// Check if airgapped flag is set in global config
	// This is a simple check - in practice, airgapped mode is set via command-line flag
	// For CLI commands, we assume not airgapped unless explicitly configured
	return false
}

// getCachedTagDigest returns the cached digest and storage time for a tag
func getCachedTagDigest(cacheDir, repoName, tag, registryURL string) (string, time.Time) {
	if cacheDir == "" {
		return "", time.Time{}
	}

	registryID := NormalizeRegistryURLForPath(registryURL)
	safeRepo := SafeRepoName(repoName)
	manifestPath := filepath.Join(cacheDir, "docker-manifests", registryID, safeRepo, tag)

	// Check if manifest exists
	if _, err := os.Stat(manifestPath); err != nil {
		return "", time.Time{}
	}

	// Read manifest to get digest
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", time.Time{}
	}

	// Calculate digest
	hash := sha256.Sum256(manifestData)
	digest := "sha256:" + hex.EncodeToString(hash[:])

	// Read metadata to get storage time
	meta := ReadDockerManifestMetadata(manifestPath)
	if meta != nil {
		return digest, meta.StoredAt
	}

	// Fallback to file modification time
	if info, err := os.Stat(manifestPath); err == nil {
		return digest, info.ModTime()
	}

	return digest, time.Time{}
}

// formatDuration formats a duration in a human-readable way
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0f seconds", d.Seconds())
	} else if d < time.Hour {
		return fmt.Sprintf("%.1f minutes", d.Minutes())
	} else if d < 24*time.Hour {
		return fmt.Sprintf("%.1f hours", d.Hours())
	} else {
		days := d.Hours() / 24
		return fmt.Sprintf("%.1f days", days)
	}
}
