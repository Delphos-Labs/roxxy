package registry

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// referrersPathRegex matches /v2/<name>/referrers/<sha256-digest>
var referrersPathRegex = regexp.MustCompile(`^/v2/(.+)/referrers/(sha256:[a-f0-9]+)$`)

// tokenCacheEntry stores a cached token with expiration
type tokenCacheEntry struct {
	token     string
	expiresAt time.Time
	scope     string
}

// progressWriter logs download progress periodically
type progressWriter struct {
	w           io.Writer
	hash        string
	total       int64
	written     int64
	lastLog     int64
	logInterval int64 // Log every X bytes (default 100MB)
	debug       bool  // Enable debug logging
}

func (pw *progressWriter) Write(p []byte) (n int, err error) {
	n, err = pw.w.Write(p)
	if err != nil {
		log.Printf("  [docker] ERROR writing to client for %s: %v (wrote %d bytes before error)", pw.hash, err, pw.written)
		return n, err
	}
	pw.written += int64(n)

	// Only log progress if debug mode is enabled
	if !pw.debug {
		return n, err
	}

	// Log every 100MB for summary (debug mode only)
	if pw.logInterval == 0 {
		pw.logInterval = 100 * 1024 * 1024 // 100MB
	}

	if pw.written-pw.lastLog >= pw.logInterval {
		pct := float64(pw.written) / float64(pw.total) * 100
		log.Printf("  [docker] *** Progress %s: %.1f%% (%d/%d bytes) ***", pw.hash, pct, pw.written, pw.total)
		pw.lastLog = pw.written
	}

	return n, err
}

// SBOMManagerInterface is an interface for SBOM tracking
type SBOMManagerInterface interface {
	GetSession(ip string) interface{}
	AddArtifact(ip string, artifact interface{})
}

// DockerRegistryProxy handles Docker registry proxying with our own filesystem storage
// FileMover is a function type for moving files atomically
type FileMover func(src, dst string) error

type DockerRegistryProxy struct {
	upstreamURL    string // e.g., "https://registry-1.docker.io"
	registryName   string // Normalized registry name for override lookups (e.g., "docker.io", "ghcr.io")
	statsTracker   *cache.StatsTracker
	sbomManager    SBOMManagerInterface        // Optional SBOM session manager
	tokenCache     map[string]*tokenCacheEntry // Key: scope, Value: token entry
	tokenMutex     sync.RWMutex                // Protects tokenCache
	hostnames      []string                    // Hostnames this proxy handles (for localhost detection)
	localOnlyPaths []string                    // Repository path prefixes that should only check local storage (e.g., ["delphos/"])
	manifestDir    string                      // Directory for storing manifests (our own storage)
	blobDir        string                      // Directory for storing blobs (our own storage, separate from other cache)
	tempDir        string                      // Directory for temporary files during uploads
	cacheDir       string                      // Base cache directory (for refcount operations)
	tagDigestMap   map[string]string           // Key: "repo/tag", Value: digest
	tagDigestMutex sync.RWMutex                // Protects tagDigestMap
	uploadMap      map[string]string           // Key: upload UUID, Value: temp file path for streaming upload
	uploadMutex    sync.RWMutex                // Protects uploadMap
	airgapProxy    bool                        // Cache all files with no size limits
	airgapped      bool                        // No external access, return 404 for missing artifacts
	proxyPushes    bool                        // If true, proxy push operations to upstream; if false, store locally
	debug          bool                        // Enable debug logging
	fileMover      FileMover                   // Function to move files atomically (defaults to os.Rename)
	overrideStore  *OverrideStore              // Optional image override store (shared across all proxies)
	// authorizeRepo, when non-nil, gates per-repository read access for the
	// current request: catalog listings are filtered to authorized repos, and a
	// cross-repo blob mount is only honored when its source repo is authorized.
	//
	// Contract: nil means allow-all and is load-bearing — it preserves the
	// roxxy proxy's prior behavior byte-for-byte (the proxy gates access in its
	// own middleware, not here). Only archifau, an internet-facing multi-tenant
	// origin, injects a real namespace ACL via SetAuthorizer. Do not change the
	// nil default to deny-all without auditing every construction site.
	authorizeRepo func(ctx context.Context, repo string) bool
}

// NewDockerRegistryProxy creates a new Docker registry proxy
func NewDockerRegistryProxy(upstreamURL string, statsTracker *cache.StatsTracker, hostnames []string, airgapProxy bool, airgapped bool, proxyPushes bool, localOnlyPaths []string, cacheDir string, sbomManager SBOMManagerInterface, debug bool) *DockerRegistryProxy {
	manifestDir := cacheDir + "/docker-manifests"
	blobDir := cacheDir + "/docker-blobs"
	tempDir := cacheDir + "/tmp"

	// Ensure directories exist
	if err := os.MkdirAll(manifestDir, 0755); err != nil {
		log.Printf("Warning: Failed to create manifest directory %s: %v", manifestDir, err)
	}
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		log.Printf("Warning: Failed to create blob directory %s: %v", blobDir, err)
	}
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		log.Printf("Warning: Failed to create temp directory %s: %v", tempDir, err)
	}

	return &DockerRegistryProxy{
		upstreamURL:    upstreamURL,
		registryName:   normalizeRegistryURL(upstreamURL),
		statsTracker:   statsTracker,
		sbomManager:    sbomManager,
		tokenCache:     make(map[string]*tokenCacheEntry),
		hostnames:      hostnames,
		localOnlyPaths: localOnlyPaths,
		manifestDir:    manifestDir,
		blobDir:        blobDir,
		tempDir:        tempDir,
		cacheDir:       cacheDir,
		tagDigestMap:   make(map[string]string),
		uploadMap:      make(map[string]string),
		airgapProxy:    airgapProxy,
		airgapped:      airgapped,
		proxyPushes:    proxyPushes,
		debug:          debug,
		fileMover:      os.Rename,
	}
}

// SetFileMover allows overriding the file mover function (for testing)
func (d *DockerRegistryProxy) SetFileMover(mover FileMover) {
	d.fileMover = mover
}

// SetOverrideStore sets the image override store for this proxy.
func (d *DockerRegistryProxy) SetOverrideStore(store *OverrideStore) {
	d.overrideStore = store
}

// SetAuthorizer installs a per-repository read-authorization callback. When
// set, catalog listings are filtered to repos the callback approves and a
// cross-repo blob mount is only honored when the source (from=) repo is
// approved. Left unset (the default), the proxy authorizes nothing here —
// callers like the roxxy proxy that gate access elsewhere are unaffected.
func (d *DockerRegistryProxy) SetAuthorizer(fn func(ctx context.Context, repo string) bool) {
	d.authorizeRepo = fn
}

// applyOverride checks if the requested image has an override and rewrites
// the request path if so. Returns the (possibly rewritten) path and whether
// an override was applied.
//
// Only applies to manifest requests by tag (not digest). Blob requests don't
// need rewriting because blobs are stored in a content-addressable store
// indexed by digest, not by repository name.
//
// For example, if there's an override "docker:27-dind" -> "delphos/docker-dind:27",
// a request for /v2/library/docker/manifests/27-dind will be rewritten to
// /v2/delphos/docker-dind/manifests/27.
func (d *DockerRegistryProxy) applyOverride(path string) (string, bool) {
	if d.overrideStore == nil {
		return path, false
	}

	// Only apply overrides to manifest requests
	segIdx := strings.Index(path, "/manifests/")
	if segIdx == -1 {
		return path, false
	}

	prefix := "/v2/"
	if !strings.HasPrefix(path, prefix) {
		return path, false
	}

	repoName := path[len(prefix):segIdx]
	reference := path[segIdx+len("/manifests/"):]

	// Don't override digest-based manifest requests — only tag-based
	if strings.HasPrefix(reference, "sha256:") {
		return path, false
	}

	// Look up "repo:tag", prefixed with registry name for non-Docker Hub
	// registries to avoid namespace collisions (e.g., "ghcr.io/astral-sh/uv:0.9.6").
	// Docker Hub images don't get a prefix since their references are stored
	// without one (e.g., "node:22-slim" not "docker.io/node:22-slim").
	lookupRef := repoName + ":" + reference
	if d.registryName != "" && d.registryName != "docker.io" {
		lookupRef = d.registryName + "/" + lookupRef
	}
	target, ok := d.overrideStore.Lookup(lookupRef)
	if !ok {
		return path, false
	}

	// Parse the target reference into repo and tag
	var targetRepo, targetRef string
	if idx := strings.LastIndex(target, ":"); idx != -1 {
		targetRepo = target[:idx]
		targetRef = target[idx+1:]
	} else {
		targetRepo = target
		targetRef = reference // Keep the original tag if target has no tag
	}

	newPath := prefix + targetRepo + "/manifests/" + targetRef
	log.Printf("[override] Rewriting %s -> %s (override: %s -> %s)", path, newPath, lookupRef, target)
	return newPath, true
}

// ServeHTTP handles HTTP requests for Docker registry API
func (d *DockerRegistryProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Docker Registry API v2 endpoints:
	// - /v2/ - API version check
	// - /v2/<name>/manifests/<reference> - Get manifest
	// - /v2/<name>/blobs/<digest> - Get blob
	// - /v2/<name>/tags/list - List tags
	// - /v2/_catalog - List repositories (may not be supported)

	path := r.URL.Path
	// Reduced verbosity: only log errors and important operations, not every request

	// Apply image overrides for GET/HEAD requests (pull path).
	// This rewrites the path so all downstream logic (cache lookup, upstream fetch)
	// transparently uses the override target.
	overrideApplied := false
	if r.Method == "GET" || r.Method == "HEAD" {
		if newPath, overridden := d.applyOverride(path); overridden {
			path = newPath
			r.URL.Path = newPath
			overrideApplied = true
		}
	}

	// Handle /v2/ API version check
	// Docker Registry API v2 spec requires 200 OK with empty JSON object {}
	if path == "/v2/" || path == "/v2" {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("{}")); err != nil {
			log.Printf("Error writing response: %v", err)
		}
		return
	}

	// Canonicalize bare names to library/: /v2/<bare>/... → /v2/library/<bare>/...
	// transparently, before any downstream logic looks at the path. The push
	// side stores under library/<bare> and the pull side reads from there, so
	// `redis` and `library/redis` are one entry. Internal rewrite rather than
	// a 308 because some clients (helm OCI, raw HTTP) don't follow redirects
	// on GET reliably.
	//
	// Only applied when roxxy is the destination (airgapped, airgap-proxy, or
	// the path is configured local-only). For requests we're going to proxy
	// upstream the client's name is authoritative — registry.k8s.io's `kubectl`
	// repo is a real bare-name repo, and rewriting it to `library/kubectl`
	// would 404 against the real upstream.
	if rewritten := canonicalizeBareDockerHub(path); rewritten != "" {
		repoName := d.extractRepoNameFromPath(path)
		if d.airgapped || d.airgapProxy || d.isLocalOnlyPath(repoName) {
			path = rewritten
			r.URL.Path = rewritten
		}
	}

	// Handle blob upload flow first (POST/PATCH/PUT to /blobs/uploads/)
	if strings.Contains(path, "/blobs/uploads/") {
		// Extract repository name to check if it's a local-only path
		repoName := d.extractRepoNameFromPath(path)
		isLocalOnly := d.isLocalOnlyPath(repoName)

		// In airgapped mode or for local-only paths, always store locally (can't proxy to upstream)
		if d.airgapped || isLocalOnly {
			// Reduced verbosity: only log on errors
			d.handleBlobUpload(w, r)
			return
		}
		if d.proxyPushes {
			// Proxy blob upload to upstream
			// Reduced verbosity: only log on errors
			d.proxyToUpstream(w, r)
			return
		}
		// Store blob upload locally
		d.handleBlobUpload(w, r)
		return
	}

	// Handle different request methods
	if r.Method == "PUT" || r.Method == "POST" || r.Method == "PATCH" {
		// Extract repository name to check if it's a local-only path
		repoName := d.extractRepoNameFromPath(path)
		isLocalOnly := d.isLocalOnlyPath(repoName)

		// In airgapped mode or for local-only paths, always store locally (can't proxy to upstream)
		if d.airgapped || isLocalOnly {
			// Reduced verbosity: only log on errors
			// Fall through to local storage below
		} else if d.proxyPushes {
			// Proxy push to upstream
			// Reduced verbosity: only log on errors
			d.proxyToUpstream(w, r)
			return
		}

		// For PUT/POST/PATCH (push operations), store in our filesystem storage
		// Store based on path (manifest or blob)
		if strings.Contains(path, "/manifests/") {
			// Manifests are small JSON documents - buffer them
			body, err := io.ReadAll(r.Body)
			if err != nil {
				log.Printf("  [docker] ERROR: Failed to read manifest body for %s: %v", path, err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := d.storeManifest(r, body, r.Header); err != nil {
				log.Printf("  [docker] ERROR: Failed to store manifest for %s: %v", path, err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else if strings.Contains(path, "/blobs/") {
			// Direct blob storage (by digest) - stream to disk
			if err := d.storeBlobStreaming(r, r.Body, r.Header); err != nil {
				log.Printf("  [docker] ERROR: Failed to store blob for %s: %v", path, err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		// Return success
		w.WriteHeader(http.StatusCreated)
		return
	}

	// Handle DELETE requests for manifests
	if r.Method == "DELETE" && strings.Contains(path, "/manifests/") {
		d.handleDeleteManifest(w, r)
		return
	}

	// For GET/HEAD, check our filesystem storage first for manifests and blobs
	if r.Method == "GET" || r.Method == "HEAD" {
		// Handle /v2/_catalog requests - return repositories from local storage
		if path == "/v2/_catalog" {
			// In airgapped mode, try cached catalog first (seeded by airgap-proxy).
			// Skipped when an authorizer is set: the cached blob is unfiltered, so
			// we must take the scan path below where per-repo filtering applies.
			if d.airgapped && d.authorizeRepo == nil {
				registryID := normalizeRegistryURL(d.upstreamURL)
				catalogPath := filepath.Join(d.manifestDir, registryID, "_catalog.json")
				if catalogData, err := os.ReadFile(catalogPath); err == nil {
					log.Printf("  [docker] Catalog request (airgapped): returning cached catalog")
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
					if _, err := w.Write(catalogData); err != nil {
						log.Printf("Error writing catalog response: %v", err)
					}
					return
				}
			}
			// Scan manifest directories for current state
			repos := d.getRepositoriesFromStorage()
			sort.Strings(repos)

			// Filter to the caller's authorized repos so the catalog can't
			// enumerate other tenants' namespaces. Filter before pagination so
			// page sizes reflect the authorized view.
			if d.authorizeRepo != nil {
				total := len(repos)
				authorized := repos[:0]
				for _, repo := range repos {
					if d.authorizeRepo(r.Context(), repo) {
						authorized = append(authorized, repo)
					}
				}
				repos = authorized
				// High-volume, low-severity: log only the aggregate so a
				// misconfiguration that strips every repo is distinguishable
				// from a legitimately-empty namespace (count vs. total).
				if dropped := total - len(repos); dropped > 0 {
					log.Printf("  [docker] Catalog filtered by authz: %d of %d repositories visible to caller", len(repos), total)
				}
			}

			// Apply pagination (n=page size, last=last item from previous page)
			repos = paginateStringSlice(repos, r.URL.Query())

			log.Printf("  [docker] Catalog request: %d repositories", len(repos))
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			// Add Link header for next page if pagination was applied and there may be more
			if nStr := r.URL.Query().Get("n"); nStr != "" {
				if n, err := strconv.Atoi(nStr); err == nil && len(repos) == n {
					lastItem := repos[len(repos)-1]
					w.Header().Set("Link", fmt.Sprintf(`</v2/_catalog?n=%d&last=%s>; rel="next"`, n, url.QueryEscape(lastItem)))
				}
			}
			response := struct {
				Repositories []string `json:"repositories"`
			}{
				Repositories: repos,
			}
			if err := json.NewEncoder(w).Encode(response); err != nil {
				log.Printf("Error encoding JSON response: %v", err)
			}
			return
		}

		// Handle tags/list requests - check local storage first for shadow repo or in airgapped mode
		if strings.HasSuffix(path, "/tags/list") {
			repoName := d.extractRepoNameFromPath(path)
			if repoName != "" && (d.isLocalOnlyPath(repoName) || d.airgapped) {
				// For shadow repo (local-only paths) or in airgapped mode, check local storage first
				// Try cached tags/list response first (from airgapProxy mode)
				registryID := normalizeRegistryURL(d.upstreamURL)
				safeRepo := util.SafeRepoName(repoName)
				tagsListPath := filepath.Join(d.manifestDir, registryID, safeRepo, "tags-list.json")
				if tagsListData, err := os.ReadFile(tagsListPath); err == nil {
					// Return cached tags/list
					log.Printf("  [docker] Tags list for %s (airgapped): returning cached tags/list", repoName)
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
					if _, err := w.Write(tagsListData); err != nil {
						log.Printf("Error writing response: %v", err)
					}
					return
				}
				// Fall back to scanning manifest files
				tags := d.getTagsFromStorage(repoName)
				sort.Strings(tags)

				// Apply pagination
				tags = paginateStringSlice(tags, r.URL.Query())

				log.Printf("  [docker] Tags list for %s (local-only/airgapped): %d tags", repoName, len(tags))
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
				// Add Link header for next page if pagination was applied
				if nStr := r.URL.Query().Get("n"); nStr != "" {
					if n, err := strconv.Atoi(nStr); err == nil && len(tags) == n {
						lastItem := tags[len(tags)-1]
						w.Header().Set("Link", fmt.Sprintf(`</v2/%s/tags/list?n=%d&last=%s>; rel="next"`, repoName, n, url.QueryEscape(lastItem)))
					}
				}
				response := struct {
					Name string   `json:"name"`
					Tags []string `json:"tags"`
				}{
					Name: repoName,
					Tags: tags,
				}
				if err := json.NewEncoder(w).Encode(response); err != nil {
					log.Printf("Error encoding JSON response: %v", err)
				}
				return
			}
			// For other repositories in non-airgapped mode, fall through to query upstream
		}

		// Handle referrers API: GET /v2/<name>/referrers/<digest>
		// Returns an OCI image index of manifests that reference the given digest
		if referrersMatch := referrersPathRegex.FindStringSubmatch(path); referrersMatch != nil {
			d.handleReferrers(w, r, referrersMatch[1], referrersMatch[2])
			return
		}

		// For localhost requests, check our storage first, then upstream if not found
		// Check both the Host header AND the RemoteAddr to detect localhost connections
		// When Docker uses a proxy, the Host header is the registry hostname, but RemoteAddr is localhost
		isLocalhost := util.IsLocalhostRequest(r)

		// Check if this is a manifest request by tag (not digest)
		// This helps us understand what we're storing
		isManifestRequest := strings.Contains(path, "/manifests/")
		if isManifestRequest {
			parts := strings.Split(path, "/")
			if len(parts) >= 5 {
				reference := parts[len(parts)-1]
				isTagRequest := !strings.HasPrefix(reference, "sha256:")
				acceptHeader := r.Header.Get("Accept")
				if isTagRequest {
					log.Printf("  [docker] Manifest request by tag: %s (Accept: %s)", reference, acceptHeader)
				} else {
					log.Printf("  [docker] Manifest request by digest: %s (Accept: %s)", reference[:19], acceptHeader)
				}
			}
		}

		// For manifest requests, ONLY check our filesystem storage
		// Manifests are stored in our filesystem
		if isManifestRequest {
			manifest, contentType, err := d.getManifestFromStorage(path)
			if err != nil && overrideApplied {
				// Override target may have been pushed to a different registry namespace
				// (e.g., pushed via localhost but requested through docker.io).
				// Search other registry namespaces for the overridden manifest.
				manifest, contentType, err = d.getManifestFromAnyRegistry(path)
				if err != nil {
					log.Printf("  [docker] Override manifest lookup failed across all registries for path %s: %v", path, err)
				}
			} else if err != nil {
				log.Printf("  [docker] Manifest lookup failed for path %s: %v", path, err)
			}
			if err == nil && manifest != nil {
				// Calculate digest for logging
				hash := sha256.Sum256(manifest)
				digest := "sha256:" + hex.EncodeToString(hash[:])

				// Parse manifest to show layer info for platform manifests
				var manifestCheck struct {
					MediaType string `json:"mediaType"`
					Layers    []struct {
						Digest string `json:"digest"`
					} `json:"layers"`
				}
				if err := json.Unmarshal(manifest, &manifestCheck); err == nil {
					if len(manifestCheck.Layers) > 0 {
						log.Printf("  [docker] Cache hit from manifest storage: %s (mediaType: %s, %d layers, first: %s)",
							digest[:12], manifestCheck.MediaType, len(manifestCheck.Layers), manifestCheck.Layers[0].Digest[:12])
					} else {
						log.Printf("  [docker] Cache hit from manifest storage: %s (mediaType: %s)", digest[:12], manifestCheck.MediaType)
					}
				} else {
					log.Printf("  [docker] Cache hit from manifest storage: %s", digest[:12])
				}

				w.Header().Set("Content-Type", contentType)
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(manifest)))
				w.Header().Set("Docker-Content-Digest", digest)

				// Track manifest for SBOM
				if r.Method == "GET" {
					d.trackDockerManifest(r, path, manifest, digest)
				}

				if r.Method == "HEAD" {
					w.WriteHeader(http.StatusOK)
				} else {
					w.WriteHeader(http.StatusOK)
					if _, err := w.Write(manifest); err != nil {
						log.Printf("Error writing response: %v", err)
					}
				}
				return
			}
			// Manifest not in our storage - will fetch from upstream below
		} else {
			// For blobs, check our filesystem storage first
			// Path format: /v2/<name>/blobs/<digest>
			parts := strings.Split(path, "/")
			if len(parts) >= 5 {
				digest := parts[len(parts)-1]
				if blobData, err := d.getBlobFromStorage(digest); err == nil {
					log.Printf("  [docker] Cache hit from blob storage: %s", digest[:12])

					// Track blob for SBOM
					if r.Method == "GET" {
						d.trackDockerBlob(r, path, digest, int64(len(blobData)))
					}

					if r.Method == "HEAD" {
						setBlobResponseHeaders(w, digest, int64(len(blobData)), nil)
					} else {
						// Handle Range requests
						rangeHeader := r.Header.Get("Range")
						if rangeHeader != "" {
							// Parse Range header (e.g., "bytes=0-1023" or "bytes=1024-")
							d.handleRangeRequest(w, r, blobData, digest, rangeHeader)
						} else {
							// Full content
							setBlobResponseHeaders(w, digest, int64(len(blobData)), nil)
							if _, err := w.Write(blobData); err != nil {
								log.Printf("Error writing response: %v", err)
							}
						}
					}
					return
				}
			}
			// Blob not in our storage - will fetch from upstream below
		}

		// Not found in our storage
		// For GET/HEAD requests (pulls):
		// - If path is in local_only_paths, return 404 (no upstream fetch)
		// - Otherwise, fetch from upstream (proxy_pushes setting doesn't affect pulls)
		// Check if this repository path is in local-only paths first
		repoName := d.extractRepoNameFromPath(path)
		log.Printf("  [docker] Extracted repo name from path %s: %q", path, repoName)
		if d.isLocalOnlyPath(repoName) {
			log.Printf("  [docker] Cache miss for local-only path %s, returning 404 (no upstream fetch)", repoName)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if isLocalhost && (r.Method == "GET" || r.Method == "HEAD") {
			log.Printf("  [docker] Localhost %s request (registry mirror mode) - checking upstream", r.Method)
		}

		// For GET/HEAD requests to other registries, fetch from upstream
		if d.airgapped {
			log.Printf("  [docker] Cache miss in airgapped mode, returning 404")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// If upstreamURL is empty, this is a local-only registry (no upstream)
		if d.upstreamURL == "" {
			log.Printf("  [docker] Cache miss in local-only registry, returning 404")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		log.Printf("  [docker] Cache miss, fetching from upstream: %s", d.upstreamURL)
		d.fetchAndStore(w, r)
		return
	}

	// For DELETE and other methods
	// In airgapped mode, always handle locally (can't proxy to upstream)
	if d.airgapped {
		log.Printf("  [docker] %s request - handling locally (airgapped mode)", r.Method)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// If proxyPushes is false, we should handle locally (return 404 or appropriate response)
	// If proxyPushes is true, proxy to upstream
	if !d.proxyPushes {
		log.Printf("  [docker] %s request (proxy_pushes=false) - returning 404 for local handling", r.Method)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	log.Printf("  [docker] Proxying %s to upstream: %s", r.Method, d.upstreamURL)
	d.proxyToUpstream(w, r)
}

// storeInFilesystemStorage stores the resource in our filesystem storage
func (d *DockerRegistryProxy) storeInFilesystemStorage(r *http.Request, body []byte, headers http.Header) error {
	path := r.URL.Path

	// Determine if this is a manifest or blob
	if strings.Contains(path, "/manifests/") {
		return d.storeManifest(r, body, headers)
	} else if strings.Contains(path, "/blobs/") {
		return d.storeBlob(r, body, headers)
	} else if strings.HasSuffix(path, "/tags/list") {
		// Store tags/list response in airgapProxy mode (for later use in airgapped mode)
		if d.airgapProxy {
			return d.storeTagsList(r, body)
		}
		// In non-airgapProxy mode, tags/list responses are ephemeral (derived from manifest files)
		return nil
	} else if path == "/v2/_catalog" {
		// Store _catalog response in airgapProxy mode (for later use in airgapped mode)
		if d.airgapProxy {
			return d.storeCatalog(r, body)
		}
		// In non-airgapProxy mode, _catalog responses are ephemeral (derived from manifest files)
		return nil
	}

	// Other endpoints don't need to be stored
	return nil
}

// fetchAndStore fetches from upstream and stores in local registry
func (d *DockerRegistryProxy) fetchAndStore(w http.ResponseWriter, r *http.Request) {
	// Fetch from upstream
	upstreamURL := util.BuildUpstreamURL(d.upstreamURL, r.URL.Path, r.URL.RawQuery)

	req, err := http.NewRequest(r.Method, upstreamURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Copy headers
	for k, v := range r.Header {
		req.Header[k] = v
	}

	// Use configurable proxy for upstream requests (supports proxy chaining)
	// Use longer timeout for large blobs (up to 30 minutes)
	client := &http.Client{
		Timeout: 30 * time.Minute,
		Transport: &http.Transport{
			Proxy:                 config.GetUpstreamProxyFunc(),
			ResponseHeaderTimeout: 5 * time.Minute,
			IdleConnTimeout:       10 * time.Minute,
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("  [docker] Error fetching from upstream: %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer util.DeferClose(resp.Body)

	log.Printf("  [docker] Upstream response: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))

	// Handle 401 Unauthorized
	// For paths in local_only_paths, convert 401 to 404 (doesn't exist upstream)
	// For other paths, try to authenticate and fetch
	if resp.StatusCode == http.StatusUnauthorized {
		// Check if this is a local-only path - if so, return 404 (doesn't exist upstream)
		repoName := d.extractRepoNameFromPath(r.URL.Path)
		if d.isLocalOnlyPath(repoName) {
			log.Printf("  [docker] 401 for local-only path %s, returning 404 (doesn't exist upstream)", repoName)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// For non-local-only paths, try to authenticate
		// Check if we already have an Authorization header (client provided credentials)
		if r.Header.Get("Authorization") != "" {
			log.Printf("  [docker] 401 with client credentials - passing through")
			// Client provided credentials but still got 401 - pass through the response
		} else {
			// No client credentials - try to get token automatically
			token, err := d.getTokenFromChallenge(resp.Header.Get("WWW-Authenticate"), r.URL.Path)
			if err != nil {
				log.Printf("  [docker] Failed to get token: %v", err)
				// Pass through the 401 response
			} else if token != "" {
				log.Printf("  [docker] Got token, retrying request")
				// Close the original 401 response body
				util.Logerr(resp.Body.Close())

				// Retry the request with the token
				// Create a new request (can't reuse after Do())
				retryURL := util.BuildUpstreamURL(d.upstreamURL, r.URL.Path, r.URL.RawQuery)
				retryReq, err := http.NewRequest(r.Method, retryURL, nil)
				if err != nil {
					log.Printf("  [docker] Error creating retry request: %v", err)
					// Return 401 - can't retry
					w.WriteHeader(http.StatusUnauthorized)
					return
				}

				// Copy headers from original request
				for k, v := range r.Header {
					retryReq.Header[k] = v
				}
				retryReq.Header.Set("Authorization", "Bearer "+token)

				retryResp, err := client.Do(retryReq)
				if err != nil {
					log.Printf("  [docker] Error retrying with token: %v", err)
					// Return 401 - retry failed
					w.WriteHeader(http.StatusUnauthorized)
					return
				}

				// Use the retry response
				resp = retryResp
				log.Printf("  [docker] Retry response: %d %s", retryResp.StatusCode, http.StatusText(retryResp.StatusCode))
			}
		}
	}

	// For HEAD requests, just copy headers and status, no body
	// But we should track tag-to-digest mapping if this is a manifest HEAD request
	// Note: HEAD is a pull operation, so we always try to authenticate (proxy_pushes doesn't apply)
	if r.Method == "HEAD" {
		log.Printf("  [docker] HEAD request for: %s", r.URL.Path)

		// Track tag-to-digest mapping for manifest HEAD requests
		// This allows us to store manifests by tag even when GET requests use digests
		if strings.Contains(r.URL.Path, "/manifests/") && resp.StatusCode == http.StatusOK {
			digest := resp.Header.Get("Docker-Content-Digest")
			if digest != "" {
				// Parse path to get repo and reference
				parts := strings.Split(r.URL.Path, "/")
				if len(parts) >= 5 {
					repoName := strings.Join(parts[2:len(parts)-2], "/")
					reference := parts[len(parts)-1]
					// Only track if reference is a tag (not a digest)
					if !strings.HasPrefix(reference, "sha256:") {
						key := repoName + "/" + reference
						d.tagDigestMutex.Lock()
						d.tagDigestMap[key] = digest
						d.tagDigestMutex.Unlock()
						log.Printf("  [docker] Tracked tag-to-digest mapping from HEAD: %s -> %s", key, digest[:12])

						// Proactively fetch and store the manifest by digest in the background
						// This ensures we cache manifests even when Docker only does HEAD requests
						go d.fetchAndStoreManifestByDigest(repoName, digest, reference)
					}
				}
			}
		}

		log.Printf("  [docker] HEAD request - returning upstream response (no storage)")
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		return
	}

	// For GET requests, determine if we should use partial caching (for large blobs)
	const maxBufferSize = 100 * 1024 * 1024 // 100MB
	contentLength := resp.ContentLength
	shouldUsePartialCache := contentLength > maxBufferSize
	path := r.URL.Path

	// Extract digest from path for blobs (e.g., /v2/repo/blobs/sha256:abc...)
	var expectedHash string
	if strings.Contains(path, "/blobs/") {
		parts := strings.Split(path, "/")
		if len(parts) >= 5 && strings.HasPrefix(parts[len(parts)-1], "sha256:") {
			expectedHash = parts[len(parts)-1]
		}
	}

	if shouldUsePartialCache && resp.StatusCode >= 200 && resp.StatusCode < 300 && expectedHash != "" {
		// Use partial caching for large blobs
		log.Printf("  [docker] Using partial cache for large blob: %s (Content-Length: %d bytes)", expectedHash[:19], contentLength)

		// Log what Docker is requesting
		log.Printf("  [docker] Docker %s request headers: Range=%q, Accept=%q", r.Method, r.Header.Get("Range"), r.Header.Get("Accept"))

		// Parse client's Range request if present
		var requestedRange *cache.ByteRange
		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "" {
			start, end, err := util.ParseHTTPRangeHeader(rangeHeader, contentLength)
			if err == nil {
				requestedRange = &cache.ByteRange{Start: start, End: end}
				log.Printf("  [docker] Client requested range: bytes=%d-%d", start, end)
			} else {
				log.Printf("  [docker] Invalid range header: %v", err)
			}
		}

		// Write headers BEFORE streaming starts so Docker can show progress
		// Copy only safe headers from upstream response
		safeHeaders := []string{"Cache-Control", "Last-Modified", "ETag"}
		for _, h := range safeHeaders {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}

		// Stream directly from the already-open authenticated connection
		// Wrap with activity timeout
		activityBody := util.NewActivityReader(resp.Body, 2*time.Minute)
		defer util.DeferClose(activityBody)
		defer util.DeferClose(resp.Body)

		// Create cache file for this blob BEFORE writing headers
		cachePath := d.getBlobPath(expectedHash)
		cacheDir := filepath.Dir(cachePath)
		if err := os.MkdirAll(cacheDir, 0755); err != nil {
			log.Printf("  [docker] Warning: cannot create cache dir: %v", err)
		}

		tempFile, err := os.CreateTemp(cacheDir, ".tmp-*")
		if err != nil {
			log.Printf("  [docker] Warning: cannot create temp file, streaming without cache: %v", err)
			// Stream without caching
			n, err := io.Copy(w, activityBody)
			if err != nil {
				log.Printf("  [docker] Error streaming: %v", err)
			} else {
				log.Printf("  [docker] Streamed %d bytes (no cache)", n)
			}
			return
		}
		tempFileName := tempFile.Name()
		defer util.DeferClose(tempFile)
		defer func() {
			// Clean up temp file only if it still exists
			if tempFileName != "" {
				util.Logerr(os.Remove(tempFileName))
			}
		}()

		// Create a progress logging writer
		progressWriter := &progressWriter{w: w, hash: expectedHash[:12], total: contentLength, debug: d.debug}

		// NOW write headers immediately before streaming (after all setup is done)
		if requestedRange != nil {
			log.Printf("  [docker] Responding with range: bytes %d-%d/%d", requestedRange.Start, requestedRange.End, contentLength)
			setBlobResponseHeaders(w, expectedHash, contentLength, &struct{ start, end int64 }{requestedRange.Start, requestedRange.End})
		} else {
			log.Printf("  [docker] Responding with full content: %d bytes", contentLength)
			setBlobResponseHeaders(w, expectedHash, contentLength, nil)
		}

		// Stream to both client and cache file
		multiWriter := io.MultiWriter(progressWriter, tempFile)
		log.Printf("  [docker] Starting io.Copy for %s", expectedHash[:12])
		bytesWritten, err := io.Copy(multiWriter, activityBody)
		log.Printf("  [docker] io.Copy finished for %s: %d bytes, err=%v", expectedHash[:12], bytesWritten, err)
		if err != nil {
			log.Printf("  [docker] Error streaming: %v", err)
			return
		}

		util.Logerr(tempFile.Close())

		// Atomically move temp file to final location
		if err := os.Rename(tempFileName, cachePath); err != nil {
			log.Printf("  [docker] Warning: failed to cache blob: %v", err)
		} else {
			tempFileName = "" // Mark as renamed
			log.Printf("  [docker] Cached blob: %s (%d bytes)", expectedHash[:12], bytesWritten)
		}

		// Track for SBOM
		d.trackDockerBlob(r, path, expectedHash, bytesWritten)

		log.Printf("  [docker] Blob fetch complete: %s (%d bytes streamed and cached)", expectedHash[:12], bytesWritten)
		return
	}

	// For small files or non-200 responses, use buffered approach
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("  Error reading upstream response: %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Store in filesystem storage if successful (only for GET, not HEAD)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Printf("  [docker] Storing in filesystem storage...")
		if err := d.storeInFilesystemStorage(r, body, resp.Header); err != nil {
			log.Printf("  [docker] Warning: Failed to store in filesystem storage: %v", err)
			// Continue anyway - return the response to client
		} else {
			log.Printf("  [docker] Successfully stored in filesystem storage")
		}

		// Track for SBOM after successful fetch
		path := r.URL.Path
		if strings.Contains(path, "/manifests/") {
			digest := resp.Header.Get("Docker-Content-Digest")
			if digest == "" {
				// Calculate digest if not provided
				hash := sha256.Sum256(body)
				digest = "sha256:" + hex.EncodeToString(hash[:])
			}
			d.trackDockerManifest(r, path, body, digest)
		} else if strings.Contains(path, "/blobs/") {
			parts := strings.Split(path, "/")
			if len(parts) >= 5 {
				digest := parts[len(parts)-1]
				d.trackDockerBlob(r, path, digest, int64(len(body)))
			}
		}
	} else {
		log.Printf("  [docker] Upstream returned %d, not storing", resp.StatusCode)
	}

	// Copy response to client
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(body); err != nil {
		log.Printf("Error writing response: %v", err)
	}
}

// normalizeRegistryURL converts a registry URL to a safe filesystem identifier
func normalizeRegistryURL(upstreamURL string) string {
	// Remove protocol and normalize
	url := strings.TrimPrefix(upstreamURL, "https://")
	url = strings.TrimPrefix(url, "http://")

	// Remove trailing slashes
	url = strings.TrimSuffix(url, "/")

	// Normalize common registry URLs
	url = strings.TrimPrefix(url, "registry-1.docker.io")
	if url == "" || url == "registry-1.docker.io" {
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

// canonicalizeBareDockerHub returns the canonical /v2/library/<name>/... path
// for a request whose <name> segment is bare (single component, no slash).
// Returns "" if the path doesn't need rewriting — already namespaced, not a
// /v2/<name>/<action>/... shape, or one of the no-name endpoints (/v2/,
// /v2/_catalog).
//
// Used as an internal path rewrite: roxxy stores every bare-name push under
// library/ and reads every bare-name pull from library/, so the cache has
// one canonical key regardless of whether the client typed `redis` or
// `library/redis`.
func canonicalizeBareDockerHub(path string) string {
	if !strings.HasPrefix(path, "/v2/") {
		return ""
	}
	rest := path[len("/v2/"):]
	// Find the action segment (manifests/blobs/tags/referrers). Repo name
	// is everything before it.
	var actionIdx int
	for _, action := range []string{"/manifests/", "/blobs/", "/tags/", "/referrers/"} {
		if i := strings.Index(rest, action); i > 0 {
			actionIdx = i
			break
		}
	}
	if actionIdx == 0 {
		return ""
	}
	name := rest[:actionIdx]
	if name == "" || strings.Contains(name, "/") {
		return ""
	}
	return "/v2/library/" + rest
}

func (d *DockerRegistryProxy) extractRepoNameFromPath(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) < 4 {
		return ""
	}

	// For blob upload paths: /v2/<name>/blobs/uploads/<uuid>
	// Find the "blobs" segment and extract everything before it
	if strings.Contains(path, "/blobs/uploads/") {
		blobsIdx := -1
		for i, part := range parts {
			if part == "blobs" && i > 1 { // Skip empty first element and "v2"
				blobsIdx = i
				break
			}
		}
		if blobsIdx > 2 {
			return strings.Join(parts[2:blobsIdx], "/")
		}
	}

	// For other paths: /v2/<name>/manifests/<reference>, /v2/<name>/blobs/<digest>, /v2/<name>/tags/list
	// Repo name is parts[2] to parts[len(parts)-2]
	if len(parts) >= 5 {
		repoName := strings.Join(parts[2:len(parts)-2], "/")
		log.Printf("  [docker] extractRepoNameFromPath: path=%q, parts=%v, len(parts)=%d, parts[2:%d]=%v, repoName=%q",
			path, parts, len(parts), len(parts)-2, parts[2:len(parts)-2], repoName)
		return repoName
	}
	return ""
}

// isLocalOnlyPath checks if a repository path matches any of the local-only path prefixes.
// In airgap-proxy mode every path is treated as local-only so that bare (un-prefixed)
// image references work against non-docker.io hosts where the library/ shim isn't available.
func (d *DockerRegistryProxy) isLocalOnlyPath(repoName string) bool {
	if d.airgapProxy {
		return true
	}
	if len(d.localOnlyPaths) == 0 {
		return false
	}
	for _, prefix := range d.localOnlyPaths {
		// Ensure prefix ends with / for proper matching
		normalizedPrefix := prefix
		if !strings.HasSuffix(normalizedPrefix, "/") {
			normalizedPrefix += "/"
		}
		// Check if repoName starts with the prefix
		if strings.HasPrefix(repoName+"/", normalizedPrefix) {
			return true
		}
	}
	return false
}

// getTagsFromStorage retrieves all tags for a repository from local filesystem storage
func (d *DockerRegistryProxy) getTagsFromStorage(repoName string) []string {
	// Normalize registry URL for filesystem
	registryID := normalizeRegistryURL(d.upstreamURL)

	// Sanitize repoName for filesystem (replace / with _)
	safeRepo := util.SafeRepoName(repoName)

	// Directory structure: docker-manifests/<registry>/<repo>/
	repoDir := filepath.Join(d.manifestDir, registryID, safeRepo)

	// Read directory entries
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		// Directory doesn't exist or can't be read - no local tags
		return nil
	}

	var tags []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		// Skip metadata files (.meta) and digest files (sha256:...)
		if strings.HasSuffix(name, ".meta") {
			continue
		}
		if strings.HasPrefix(name, "sha256:") {
			continue
		}

		// This is a tag file
		tags = append(tags, name)
	}

	return tags
}

// getRepositoriesFromStorage retrieves all repositories from local filesystem storage
func (d *DockerRegistryProxy) getRepositoriesFromStorage() []string {
	// Normalize registry URL for filesystem
	registryID := normalizeRegistryURL(d.upstreamURL)

	// Directory structure: docker-manifests/<registry>/<repo>/
	registryDir := filepath.Join(d.manifestDir, registryID)

	// Read directory entries
	entries, err := os.ReadDir(registryDir)
	if err != nil {
		// Directory doesn't exist or can't be read - no local repositories
		return nil
	}

	var repos []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Directory name is the sanitized repo name (with / replaced by _)
		// Convert back to original format
		safeRepo := entry.Name()
		repoName := util.UnsafeRepoName(safeRepo)
		repos = append(repos, repoName)
	}

	return repos
}

// storeTagsList stores a tags/list response for later use in airgapped mode
func (d *DockerRegistryProxy) storeTagsList(r *http.Request, body []byte) error {
	repoName := d.extractRepoNameFromPath(r.URL.Path)
	if repoName == "" {
		return fmt.Errorf("invalid tags/list path: %s", r.URL.Path)
	}

	// Normalize registry URL for filesystem
	registryID := normalizeRegistryURL(d.upstreamURL)

	// Sanitize repoName for filesystem (replace / with _)
	safeRepo := util.SafeRepoName(repoName)

	// Store in: docker-manifests/<registry>/<repo>/tags-list.json
	tagsListPath := filepath.Join(d.manifestDir, registryID, safeRepo, "tags-list.json")

	// Create directory if needed
	dir := filepath.Dir(tagsListPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create tags list directory: %w", err)
	}

	// Write tags list response
	if err := os.WriteFile(tagsListPath, body, 0644); err != nil {
		return fmt.Errorf("failed to write tags list: %w", err)
	}

	log.Printf("  [docker] Stored tags/list for %s (airgapProxy mode)", repoName)
	return nil
}

// storeCatalog stores a _catalog response for later use in airgapped mode
func (d *DockerRegistryProxy) storeCatalog(r *http.Request, body []byte) error {
	// Normalize registry URL for filesystem
	registryID := normalizeRegistryURL(d.upstreamURL)

	// Store in: docker-manifests/<registry>/_catalog.json
	catalogPath := filepath.Join(d.manifestDir, registryID, "_catalog.json")

	// Create directory if needed
	dir := filepath.Dir(catalogPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create catalog directory: %w", err)
	}

	// Write catalog response
	if err := os.WriteFile(catalogPath, body, 0644); err != nil {
		return fmt.Errorf("failed to write catalog: %w", err)
	}

	log.Printf("  [docker] Stored _catalog (airgapProxy mode)")
	return nil
}

// getManifestPath returns the filesystem path for a manifest
// Includes registry in path to prevent collisions between same repo names from different registries
func (d *DockerRegistryProxy) getManifestPath(repoName, reference string, upstreamURL string) string {
	// Normalize registry URL for filesystem
	registryID := normalizeRegistryURL(upstreamURL)

	// Sanitize repoName for filesystem (replace / with _)
	safeRepo := util.SafeRepoName(repoName)
	// For digests, use the full digest; for tags, use the tag name
	// Structure: docker-manifests/<registry>/<repo>/<reference>
	return fmt.Sprintf("%s/%s/%s/%s", d.manifestDir, registryID, safeRepo, reference)
}

// getManifestFromStorage retrieves a manifest from our filesystem storage
func (d *DockerRegistryProxy) getManifestFromStorage(path string) ([]byte, string, error) {
	// Parse path: /v2/<name>/manifests/<reference>
	parts := strings.Split(path, "/")
	if len(parts) < 5 {
		return nil, "", fmt.Errorf("invalid manifest path: %s", path)
	}

	repoName := strings.Join(parts[2:len(parts)-2], "/")
	reference := parts[len(parts)-1]

	// Use this proxy's upstream URL to construct the path (includes registry)
	manifestPath := d.getManifestPath(repoName, reference, d.upstreamURL)
	log.Printf("  [docker] Looking up manifest: repoName=%s, reference=%s, upstreamURL=%s, path=%s", repoName, reference, d.upstreamURL, manifestPath)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		log.Printf("  [docker] Failed to read manifest file %s: %v", manifestPath, err)
		return nil, "", err
	}

	// Try to read metadata file for content type
	metaPath := manifestPath + ".meta"
	metaData, err := os.ReadFile(metaPath)
	contentType := "application/vnd.docker.distribution.manifest.v2+json" // default
	if err == nil {
		var meta struct {
			ContentType string `json:"content_type"`
		}
		if json.Unmarshal(metaData, &meta) == nil && meta.ContentType != "" {
			contentType = meta.ContentType
		}
	}

	return data, contentType, nil
}

// getManifestFromAnyRegistry searches for a manifest across all registry
// namespace subdirectories. This is used when an override rewrites the request
// to a target that was pushed under a different registry (e.g., pushed via
// localhost but requested through docker.io).
func (d *DockerRegistryProxy) getManifestFromAnyRegistry(path string) ([]byte, string, error) {
	parts := strings.Split(path, "/")
	if len(parts) < 5 {
		return nil, "", fmt.Errorf("invalid manifest path: %s", path)
	}

	repoName := strings.Join(parts[2:len(parts)-2], "/")
	reference := parts[len(parts)-1]
	safeRepo := util.SafeRepoName(repoName)

	// List all registry directories under manifestDir
	entries, err := os.ReadDir(d.manifestDir)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read manifest directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidatePath := filepath.Join(d.manifestDir, entry.Name(), safeRepo, reference)
		data, readErr := os.ReadFile(candidatePath)
		if readErr != nil {
			continue
		}

		// Found it — read content type metadata
		contentType := "application/vnd.docker.distribution.manifest.v2+json"
		metaPath := candidatePath + ".meta"
		if metaData, metaErr := os.ReadFile(metaPath); metaErr == nil {
			var meta struct {
				ContentType string `json:"content_type"`
			}
			if json.Unmarshal(metaData, &meta) == nil && meta.ContentType != "" {
				contentType = meta.ContentType
			}
		}
		log.Printf("[override] Found overridden manifest in registry namespace %q: %s/%s", entry.Name(), safeRepo, reference)
		return data, contentType, nil
	}

	return nil, "", fmt.Errorf("manifest %s/%s not found in any registry namespace", repoName, reference)
}

// storeManifestInStorage writes a manifest file and keeps blob refcounts in
// lockstep with what's on disk.
//
// Invariant (matches audit.AuditRefcounts): each manifest file on disk
// contributes +1 to every blob digest it lists. So:
//
//   - File didn't exist (new) → +1 per new-body blob digest.
//   - File existed with identical body → no-op (writes same bytes back).
//   - File existed with different body → -1 per old-body blob digest,
//     +1 per new-body blob digest. (Net zero for blobs shared by old
//     and new, which is the "retag with overlap" case we want cheap.)
//
// Centralizing the delta here means every call site (tag-file and
// digest-file writes from storeManifest, fetchAndStoreManifestByDigest,
// queryTagsForDigest, etc.) gets the accounting for free. Callers no
// longer need their own Increment loop — and historically that's where
// drift came from: the digest-file write incremented but the tag-file
// write didn't, leaving refcounts under-counted by 1 per fresh pull.
//
// Indexes (no config/layers) are a no-op for blob refcounts; their
// sub-manifest refcounts are still managed in storeManifest's index
// branch since the rule there is "increment exactly once per unique
// index digest" rather than "per manifest file on disk."
func (d *DockerRegistryProxy) storeManifestInStorage(repoName, reference string, body []byte, contentType string, upstreamURL string) error {
	manifestPath := d.getManifestPath(repoName, reference, upstreamURL)

	// Create directory if needed
	dir := filepath.Dir(manifestPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create manifest directory: %w", err)
	}

	// Read any existing body so we can compute the refcount delta.
	// Missing file → first write, oldDigests stays nil → +1 per new blob.
	// Identical body → skipDelta, no inc/dec work.
	// Different body → -1 per old digest, +1 per new digest.
	var oldDigests []string
	skipDelta := false
	if existing, err := os.ReadFile(manifestPath); err == nil {
		if bytes.Equal(existing, body) {
			skipDelta = true
		} else if cfg, layers, perr := d.extractBlobDigestsFromManifest(existing); perr == nil {
			oldDigests = append(oldDigests, cfg)
			oldDigests = append(oldDigests, layers...)
		}
	} else if !os.IsNotExist(err) {
		log.Printf("  [docker] Warning: read existing manifest %s for refcount delta: %v", manifestPath, err)
	}

	if err := os.WriteFile(manifestPath, body, 0644); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}

	if !skipDelta {
		for _, dg := range oldDigests {
			hash := strings.TrimPrefix(dg, "sha256:")
			if _, err := cache.DecrementRefcount(d.cacheDir, hash); err != nil {
				// Partial dec is recoverable by audit; stopping here
				// would skip the corresponding incs and leave the cache
				// MORE drifted.
				log.Printf("  [docker] Warning: dec refcount %s during manifest overwrite: %v", dg[:12], err)
			}
		}
		newConfig, newLayers, perr := d.extractBlobDigestsFromManifest(body)
		if perr == nil {
			newDigests := append([]string{}, newConfig)
			newDigests = append(newDigests, newLayers...)
			for _, dg := range newDigests {
				if dg == "" {
					continue
				}
				hash := strings.TrimPrefix(dg, "sha256:")
				if err := cache.IncrementRefcount(d.cacheDir, hash); err != nil {
					log.Printf("  [docker] Warning: inc refcount %s during manifest write: %v", dg[:12], err)
				}
			}
		}
	}

	// Write metadata (including upstream URL so we know which registry it came from)
	metaPath := manifestPath + ".meta"
	meta := struct {
		ContentType string    `json:"content_type"`
		StoredAt    time.Time `json:"stored_at"`
		UpstreamURL string    `json:"upstream_url,omitempty"` // Registry this manifest came from
	}{
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

// getBlobPath returns the filesystem path for a blob
func (d *DockerRegistryProxy) getBlobPath(digest string) string {
	// Use subdirectories: docker-blobs/sha256/<first 2 chars>/<next 2 chars>/<full digest>
	// This prevents having thousands of files in a single directory, which can cause
	// performance issues on many filesystems. Using 2-level subdirectories provides
	// good distribution (256 * 256 = 65,536 possible directories).
	// d.blobDir is already cacheDir + "/docker-blobs"
	if strings.HasPrefix(digest, "sha256:") {
		hashPart := digest[7:] // Skip "sha256:" prefix
		if len(hashPart) >= 4 {
			return filepath.Join(d.blobDir, "sha256", hashPart[0:2], hashPart[2:4], digest)
		}
	}
	// Fallback for non-sha256 digests or malformed digests
	return filepath.Join(d.blobDir, digest)
}

// truncDigest returns a short, log-safe prefix of a digest. Guards against a
// short attacker-supplied mount= value that would panic a naive digest[:19].
func truncDigest(digest string) string {
	if len(digest) > 19 {
		return digest[:19]
	}
	return digest
}

// isValidRepoName reports whether name is a well-formed lowercase repository
// name safe to use as a mount source: no empty/"."/".." path segments, no
// leading/trailing slash, no uppercase. This blocks traversal-style sources
// like "a/other/../../b" from passing a CanRead prefix match before the read
// check resolves them. Mirrors the file surface's cleanRelPath hygiene.
func isValidRepoName(name string) bool {
	if name == "" || name != strings.ToLower(name) {
		return false
	}
	if name != strings.Trim(name, "/") {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// blobIsMemberOfRepo reports whether digest is reachable from a manifest stored
// in repoName: it matches the manifest's own digest, its config, one of its
// layers, or (for an index) a sub-manifest's config/layers. Because blobs are
// stored in a single global content-addressable store, this membership check —
// not the attacker-controlled from= parameter — is what confines a cross-repo
// mount to blobs the source repo actually contains.
func (d *DockerRegistryProxy) blobIsMemberOfRepo(digest, repoName string) bool {
	registryID := normalizeRegistryURL(d.upstreamURL)
	safeRepo := util.SafeRepoName(repoName)
	repoDir := filepath.Join(d.manifestDir, registryID, safeRepo)

	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".meta") {
			continue
		}
		// A manifest may itself be referenced by digest (a tag named after the
		// manifest's sha256), so the manifest reference can equal mountDigest.
		if entry.Name() == digest {
			return true
		}
		body, readErr := os.ReadFile(filepath.Join(repoDir, entry.Name()))
		if readErr != nil {
			continue
		}
		configDigest, layerDigests, parseErr := d.extractBlobDigestsFromManifest(body)
		if parseErr != nil {
			continue
		}
		if configDigest == digest || slices.Contains(layerDigests, digest) {
			return true
		}
		if slices.Contains(extractSubManifestDigests(body), digest) {
			return true
		}
	}
	return false
}

// setBlobResponseHeaders sets standard headers for blob responses
// This is the single source of truth for blob response headers
func setBlobResponseHeaders(w http.ResponseWriter, digest string, totalSize int64, requestedRange *struct{ start, end int64 }) {
	// Always set these headers
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Accept-Ranges", "bytes")

	if requestedRange != nil {
		// Partial content (206)
		rangeSize := requestedRange.end - requestedRange.start + 1
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", requestedRange.start, requestedRange.end, totalSize))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", rangeSize))
		log.Printf("  [docker] HEADERS: Status=206, Content-Length=%d, Content-Range=bytes %d-%d/%d", rangeSize, requestedRange.start, requestedRange.end, totalSize)
		w.WriteHeader(http.StatusPartialContent)
	} else {
		// Full content (200)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", totalSize))
		log.Printf("  [docker] HEADERS: Status=200, Content-Length=%d, Content-Type=application/octet-stream", totalSize)
		w.WriteHeader(http.StatusOK)
	}
}

// getBlobFromStorage retrieves a blob from our filesystem storage
func (d *DockerRegistryProxy) getBlobFromStorage(digest string) ([]byte, error) {
	blobPath := d.getBlobPath(digest)
	data, err := os.ReadFile(blobPath)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// handleRangeRequest handles HTTP Range requests for blob downloads
func (d *DockerRegistryProxy) handleRangeRequest(w http.ResponseWriter, r *http.Request, data []byte, digest, rangeHeader string) {
	// Parse Range header: "bytes=start-end" or "bytes=start-"
	// We only support simple single-range requests for now
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		// Invalid range, return full content
		setBlobResponseHeaders(w, digest, int64(len(data)), nil)
		if _, err := w.Write(data); err != nil {
			log.Printf("Error writing response: %v", err)
		}
		return
	}

	rangeSpec := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.Split(rangeSpec, "-")
	if len(parts) != 2 {
		// Invalid range format
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
		return
	}

	var start, end int64
	var err error

	// Parse start
	if parts[0] != "" {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || start < 0 || start >= int64(len(data)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			return
		}
	}

	// Parse end
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start || end >= int64(len(data)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			return
		}
	} else {
		// "bytes=start-" means from start to end of file
		end = int64(len(data)) - 1
	}

	// Return partial content
	rangeData := data[start : end+1]
	setBlobResponseHeaders(w, digest, int64(len(data)), &struct{ start, end int64 }{start, end})
	if _, err := w.Write(rangeData); err != nil {
		log.Printf("Error writing response: %v", err)
	}

	log.Printf("  [docker] Served range request: bytes %d-%d/%d (%d bytes)", start, end, len(data), len(rangeData))
}

// storeBlobInStorage stores a blob in our filesystem storage
func (d *DockerRegistryProxy) storeBlobInStorage(digest string, body []byte) error {
	blobPath := d.getBlobPath(digest)

	// Ensure blob directory and all parent subdirectories exist
	// getBlobPath may return a path with subdirectories like docker-blobs/sha256/ab/c1/sha256:...
	blobDir := filepath.Dir(blobPath)
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		return fmt.Errorf("failed to create blob directory %s: %w", blobDir, err)
	}

	// Write blob directly - trust the digest from Docker
	if err := os.WriteFile(blobPath, body, 0644); err != nil {
		return fmt.Errorf("failed to write blob: %w", err)
	}

	return nil
}

// storeBlobInStorageStreaming streams a blob directly to disk without buffering
func (d *DockerRegistryProxy) storeBlobInStorageStreaming(digest string, body io.Reader) error {
	blobPath := d.getBlobPath(digest)

	// Ensure blob directory and all parent subdirectories exist
	blobDir := filepath.Dir(blobPath)
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		return fmt.Errorf("failed to create blob directory %s: %w", blobDir, err)
	}

	// Create temp file in the dedicated temp directory (same filesystem as blobs for atomic rename)
	tempFile, err := os.CreateTemp(d.tempDir, "blob-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file in %s: %w", d.tempDir, err)
	}
	tempPath := tempFile.Name()
	defer util.DeferRemove(tempPath) // Clean up on error (or after successful move)

	// Stream body to temp file with hash verification
	hasher := sha256.New()
	writer := io.MultiWriter(tempFile, hasher)
	if _, err := io.Copy(writer, body); err != nil {
		util.Logerr(tempFile.Close())
		return fmt.Errorf("failed to write blob: %w", err)
	}
	util.Logerr(tempFile.Close())

	// Verify digest
	computedDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if digest != computedDigest {
		return fmt.Errorf("digest mismatch: expected %s, got %s", digest, computedDigest)
	}

	// Atomically move temp file to final location
	if err := d.fileMover(tempPath, blobPath); err != nil {
		return fmt.Errorf("failed to move blob to final location: %w", err)
	}

	return nil
}

// extractBlobDigestsFromManifest parses a manifest JSON and returns all blob digests it references
func (d *DockerRegistryProxy) extractBlobDigestsFromManifest(manifestBody []byte) (string, []string, error) {
	var manifest struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		Config        struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}

	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		return "", nil, fmt.Errorf("failed to parse manifest: %w", err)
	}

	var layerDigests []string
	for _, layer := range manifest.Layers {
		if layer.Digest != "" {
			layerDigests = append(layerDigests, layer.Digest)
		}
	}

	return manifest.Config.Digest, layerDigests, nil
}

// extractSubManifestDigests returns the digests referenced from an index
// manifest's `manifests` array. Empty for image manifests.
func extractSubManifestDigests(body []byte) []string {
	var idx struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil
	}
	if idx.MediaType != "application/vnd.oci.image.index.v1+json" &&
		idx.MediaType != "application/vnd.docker.distribution.manifest.list.v2+json" {
		return nil
	}
	out := make([]string, 0, len(idx.Manifests))
	for _, m := range idx.Manifests {
		if m.Digest != "" {
			out = append(out, m.Digest)
		}
	}
	return out
}

// cascadeDeleteManifest is called when a manifest's refcount drops to 0.
// It reads the by-digest manifest body, decrements every reference it
// holds (blobs for image manifests; sub-manifest digests for index
// manifests, recursing if those hit 0 too), and removes the manifest
// file. The caller should already have removed any by-tag mirrors.
func (d *DockerRegistryProxy) cascadeDeleteManifest(repoName, digest string) {
	digestPath := d.getManifestPath(repoName, digest, d.upstreamURL)
	body, err := os.ReadFile(digestPath)
	if err != nil {
		// Already gone or unreadable — still clean up sidecars if any.
		util.Logerr(os.Remove(digestPath + ".meta"))
		util.Logerr(os.Remove(digestPath + ".metadata"))
		return
	}

	if subs := extractSubManifestDigests(body); len(subs) > 0 {
		// Index manifest: dec each sub; recurse on cascade.
		for _, sub := range subs {
			subHash := strings.TrimPrefix(sub, "sha256:")
			shouldDelete, derr := cache.DecrementManifestRefcount(d.cacheDir, subHash)
			if derr != nil {
				log.Printf("  [docker] Warning: dec sub-manifest %s: %v", sub[:12], derr)
				continue
			}
			if shouldDelete {
				d.cascadeDeleteManifest(repoName, sub)
			}
		}
	} else {
		// Image manifest: dec config + layer blob refs. Blob files are NOT
		// deleted here; orphan blobs (refcount=0) are reclaimed by
		// `docker prune-blobs` / `docker audit` so an immediate re-pull
		// sharing blobs doesn't have to re-fetch them.
		configDigest, layerDigests, perr := d.extractBlobDigestsFromManifest(body)
		if perr != nil {
			log.Printf("  [docker] Warning: parse manifest for cascade %s: %v", digest[:12], perr)
		} else {
			all := append([]string{}, configDigest)
			all = append(all, layerDigests...)
			for _, b := range all {
				if b == "" {
					continue
				}
				blobHash := strings.TrimPrefix(b, "sha256:")
				if _, derr := cache.DecrementRefcount(d.cacheDir, blobHash); derr != nil {
					continue
				}
			}
		}
	}

	// Remove the by-digest manifest file itself.
	if err := os.Remove(digestPath); err == nil {
		log.Printf("  [docker] Cascade deleted manifest (refcount=0): %s/%s", repoName, digest[:12])
	}
	util.Logerr(os.Remove(digestPath + ".meta"))
	util.Logerr(os.Remove(digestPath + ".metadata"))
}

// storeManifest stores a manifest in our filesystem storage
func (d *DockerRegistryProxy) storeManifest(r *http.Request, body []byte, headers http.Header) error {
	// Extract repository name and reference from path
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		return fmt.Errorf("invalid manifest path: %s", r.URL.Path)
	}

	repoName := strings.Join(parts[2:len(parts)-2], "/")
	reference := parts[len(parts)-1]

	// Get content type from headers
	contentType := headers.Get("Content-Type")
	if contentType == "" {
		contentType = "application/vnd.docker.distribution.manifest.v2+json"
	}

	// Calculate digest
	hash := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(hash[:])

	// Check if this is a multi-platform index - if so, we need to ensure platform manifests are stored
	var indexCheck struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform,omitempty"`
		} `json:"manifests"`
	}
	isIndex := false
	if err := json.Unmarshal(body, &indexCheck); err == nil {
		isIndex = indexCheck.MediaType == "application/vnd.oci.image.index.v1+json" ||
			indexCheck.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
	}

	// Store manifest by digest (content-addressable)
	if err := d.storeManifestInStorage(repoName, digest, body, contentType, d.upstreamURL); err != nil {
		return fmt.Errorf("failed to store manifest by digest: %w", err)
	}

	// Extract and store metadata for new manifest (async, non-blocking)
	go func() {
		manifestPath := d.getManifestPath(repoName, digest, d.upstreamURL)
		if err := d.processDockerManifestMetadata(manifestPath); err != nil {
			log.Printf("  [docker] Warning: Failed to extract metadata for %s: %v", digest[:12], err)
		}
	}()

	// Debug: for platform manifests, show first few layer digests to help diagnose mismatches
	if !isIndex {
		var platformManifestCheck struct {
			Layers []struct {
				Digest string `json:"digest"`
			} `json:"layers"`
		}
		if err := json.Unmarshal(body, &platformManifestCheck); err == nil && len(platformManifestCheck.Layers) > 0 {
			log.Printf("  [docker] Stored manifest by digest: %s/%s (digest: %s, %d layers, first layer: %s)",
				repoName, digest[:12], digest[:12], len(platformManifestCheck.Layers), platformManifestCheck.Layers[0].Digest[:12])
		} else {
			log.Printf("  [docker] Stored manifest by digest: %s/%s (digest: %s)", repoName, digest[:12], digest[:12])
		}
	} else {
		log.Printf("  [docker] Stored manifest by digest: %s/%s (digest: %s, index with %d platform manifest(s))",
			repoName, digest[:12], digest[:12], len(indexCheck.Manifests))
		// Debug: show what platform manifests the index references
		for _, m := range indexCheck.Manifests {
			if m.Platform.OS != "unknown" && m.Platform.Architecture != "unknown" && m.Platform.OS != "" && m.Platform.Architecture != "" {
				log.Printf("  [docker] Index references: %s/%s -> %s", m.Platform.OS, m.Platform.Architecture, m.Digest)
			}
		}
	}

	// If this is an index, verify that all referenced platform manifests exist
	// Note: Platform manifests should have been pushed before the index, but we verify anyway
	if isIndex && len(indexCheck.Manifests) > 0 {
		log.Printf("  [docker] Detected multi-platform index with %d platform manifest(s)", len(indexCheck.Manifests))
		d.verifyPlatformManifests(repoName, indexCheck.Manifests)
	}

	digestHash := strings.TrimPrefix(digest, "sha256:")

	// Blob refcounts (config + layers) are managed by storeManifestInStorage
	// — one delta per manifest file on disk, in lockstep with the audit
	// invariant. We don't need to inc here.

	// Index manifests: Inc each sub-manifest digest, but only on first
	// observation of the index digest itself — otherwise a re-pushed
	// index would double-count its children.
	if isIndex && !cache.ManifestRefcountExists(d.cacheDir, digestHash) {
		for _, sub := range extractSubManifestDigests(body) {
			subHash := strings.TrimPrefix(sub, "sha256:")
			if err := cache.IncrementManifestRefcount(d.cacheDir, subHash); err != nil {
				log.Printf("  [docker] Warning: Inc sub-manifest %s: %v", sub[:12], err)
			} else {
				log.Printf("  [docker] Incremented refcount for sub-manifest: %s", sub[:12])
			}
		}
	}

	// If reference is a tag, also store it with the tag name (for easy lookup)
	if !strings.HasPrefix(reference, "sha256:") {
		if err := d.storeManifestInStorage(repoName, reference, body, contentType, d.upstreamURL); err != nil {
			log.Printf("  [docker] Warning: Failed to store manifest with tag: %v", err)
		} else {
			log.Printf("  [docker] Also stored manifest with tag: %s/%s", repoName, reference)
			// Extract and store metadata for tag manifest (async, non-blocking)
			go func() {
				tagManifestPath := d.getManifestPath(repoName, reference, d.upstreamURL)
				if err := d.processDockerManifestMetadata(tagManifestPath); err != nil {
					log.Printf("  [docker] Warning: Failed to extract metadata for tag %s: %v", reference, err)
				}
			}()
		}

		// Track tag-to-digest mapping. Manifest refcount accounting:
		//   - new tag:          Inc(new)
		//   - tag → same digest: no change
		//   - tag → new digest: Dec(old) + cascade if 0, then Inc(new)
		key := repoName + "/" + reference
		d.tagDigestMutex.Lock()
		oldDigest := d.tagDigestMap[key]
		d.tagDigestMap[key] = digest
		d.tagDigestMutex.Unlock()
		if oldDigest != digest {
			if oldDigest != "" {
				oldHash := strings.TrimPrefix(oldDigest, "sha256:")
				shouldDelete, err := cache.DecrementManifestRefcount(d.cacheDir, oldHash)
				if err != nil {
					log.Printf("  [docker] Warning: dec old tag digest %s: %v", oldDigest[:12], err)
				} else if shouldDelete {
					d.cascadeDeleteManifest(repoName, oldDigest)
				}
			}
			if err := cache.IncrementManifestRefcount(d.cacheDir, digestHash); err != nil {
				log.Printf("  [docker] Warning: Inc manifest refcount %s: %v", digest[:12], err)
			} else {
				log.Printf("  [docker] Incremented refcount for manifest: %s", digest[:12])
			}
		}
	} else {
		// Reference is a digest - verify the reference matches the calculated digest
		// This ensures we're storing the correct content
		if reference != digest {
			log.Printf("  [docker] WARNING: Reference digest %s does not match calculated digest %s for %s",
				reference[:12], digest[:12], repoName)
			log.Printf("  [docker] WARNING: This may indicate the manifest was pushed with incorrect content")
		}

		// Check if we have a tag mapping for this digest
		// This handles the case where buildx does HEAD by tag, then GET by digest
		d.tagDigestMutex.RLock()
		foundTag := false
		for tagKey, mappedDigest := range d.tagDigestMap {
			if mappedDigest == digest {
				// Found a tag that maps to this digest - also store by tag
				tagParts := strings.Split(tagKey, "/")
				if len(tagParts) == 2 {
					tagRepo := tagParts[0]
					tagName := tagParts[1]
					if tagRepo == repoName {
						// Same repo - store manifest with tag name too
						if err := d.storeManifestInStorage(repoName, tagName, body, contentType, d.upstreamURL); err != nil {
							log.Printf("  [docker] Warning: Failed to store manifest with tag from mapping: %v", err)
						} else {
							log.Printf("  [docker] Also stored manifest with tag from mapping: %s/%s (digest: %s)", repoName, tagName, digest[:12])
						}
						foundTag = true
						break
					}
				}
			}
		}
		d.tagDigestMutex.RUnlock()

		// If we didn't find a tag mapping, try to query upstream for tags that map to this digest
		// This handles the case where buildx requests directly by digest (e.g., FROM golang:1.25-alpine@sha256:...)
		if !foundTag {
			go d.queryTagsForDigest(repoName, digest, body, contentType)
		}
	}

	return nil
}

// verifyPlatformManifests verifies that platform manifests referenced by an index exist and are valid
func (d *DockerRegistryProxy) verifyPlatformManifests(repoName string, manifests []struct {
	Digest   string `json:"digest"`
	Platform struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform,omitempty"`
}) {
	for _, m := range manifests {
		// Skip attestations/SBOMs
		if m.Platform.OS == "unknown" || m.Platform.Architecture == "unknown" ||
			m.Platform.OS == "" || m.Platform.Architecture == "" {
			continue
		}

		// Check if platform manifest exists
		platformManifestPath := d.getManifestPath(repoName, m.Digest, d.upstreamURL)
		storedData, err := os.ReadFile(platformManifestPath)
		if os.IsNotExist(err) {
			d.logMissingPlatformManifest(repoName, m, platformManifestPath)
			continue
		}
		if err != nil {
			continue // Ignore read errors
		}

		// Verify digest
		d.verifyPlatformManifestDigest(repoName, m, platformManifestPath, storedData)
	}
}

// logMissingPlatformManifest logs an error when a platform manifest is missing
func (d *DockerRegistryProxy) logMissingPlatformManifest(repoName string, m struct {
	Digest   string `json:"digest"`
	Platform struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform,omitempty"`
}, path string) {
	log.Printf("  [docker] ERROR: Platform manifest %s/%s (%s/%s) referenced by index but NOT FOUND in storage",
		repoName, m.Digest[:12], m.Platform.OS, m.Platform.Architecture)
	log.Printf("  [docker] ERROR: Path checked: %s", path)
	log.Printf("  [docker] ERROR: This means the index references a platform manifest that was never stored!")
	log.Printf("  [docker] ERROR: This will cause incorrect layer digests to be shown")
}

// verifyPlatformManifestDigest verifies that a stored platform manifest has the correct digest
func (d *DockerRegistryProxy) verifyPlatformManifestDigest(repoName string, m struct {
	Digest   string `json:"digest"`
	Platform struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform,omitempty"`
}, path string, data []byte) {
	storedHash := sha256.Sum256(data)
	storedDigest := "sha256:" + hex.EncodeToString(storedHash[:])

	if storedDigest != m.Digest {
		log.Printf("  [docker] ERROR: Platform manifest %s/%s digest mismatch! Expected %s, stored file has %s",
			repoName, m.Digest[:12], m.Digest, storedDigest)
		log.Printf("  [docker] ERROR: Path: %s", path)
		log.Printf("  [docker] ERROR: This means an old/corrupted manifest exists at that path")
		log.Printf("  [docker] ERROR: This will cause incorrect layer digests to be shown")
		return
	}

	// Digest matches - check layers
	var platformManifestCheck struct {
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(data, &platformManifestCheck); err == nil && len(platformManifestCheck.Layers) > 0 {
		log.Printf("  [docker] Platform manifest %s/%s verified: digest matches, %d layers, first layer: %s",
			repoName, m.Digest[:12], len(platformManifestCheck.Layers), platformManifestCheck.Layers[0].Digest[:12])
	}
}

// fetchAndStoreManifestByDigest proactively fetches and stores a manifest by digest
// This is called when we see a HEAD request that returns a digest, ensuring we cache
// manifests even when Docker only does HEAD requests (e.g., when image is already up to date)
func (d *DockerRegistryProxy) fetchAndStoreManifestByDigest(repoName, digest, tag string) {
	// Check if we already have this manifest
	manifestPath := d.getManifestPath(repoName, digest, d.upstreamURL)
	if _, err := os.Stat(manifestPath); err == nil {
		// Already cached, no need to fetch
		log.Printf("  [docker] Manifest %s/%s already cached, skipping proactive fetch", repoName, digest[:12])
		return
	}

	// Fetch manifest from upstream
	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", d.upstreamURL, repoName, digest)
	req, err := http.NewRequest("GET", manifestURL, nil)
	if err != nil {
		log.Printf("  [docker] Failed to create manifest fetch request: %v", err)
		return
	}

	// Add Accept headers for different manifest types
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.index.v1+json")

	// Get token if needed (for authenticated registries like ghcr.io)
	d.tokenMutex.RLock()
	var authToken string
	for scope, entry := range d.tokenCache {
		if strings.Contains(scope, repoName) && time.Now().Before(entry.expiresAt) {
			authToken = entry.token
			break
		}
	}
	d.tokenMutex.RUnlock()

	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	// Don't use proxy for upstream requests (avoid loop)
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy: config.GetUpstreamProxyFunc(),
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("  [docker] Failed to fetch manifest %s/%s: %v", repoName, digest[:12], err)
		return
	}
	defer util.DeferClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		log.Printf("  [docker] Manifest fetch returned %d for %s/%s", resp.StatusCode, repoName, digest[:12])
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("  [docker] Failed to read manifest body: %v", err)
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/vnd.docker.distribution.manifest.v2+json"
	}

	// Store manifest by digest
	if err := d.storeManifestInStorage(repoName, digest, body, contentType, d.upstreamURL); err != nil {
		log.Printf("  [docker] Failed to store manifest %s/%s: %v", repoName, digest[:12], err)
		return
	}

	log.Printf("  [docker] Proactively stored manifest %s/%s (from HEAD request for tag: %s)", repoName, digest[:12], tag)

	// Check if this is a multi-platform index - if so, proactively fetch platform manifests
	var index struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
				Variant      string `json:"variant,omitempty"`
			} `json:"platform,omitempty"`
		} `json:"manifests"`
	}

	isIndex := false
	if err := json.Unmarshal(body, &index); err == nil {
		isIndex = index.MediaType == "application/vnd.oci.image.index.v1+json" ||
			index.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json"

		if isIndex {
			// Proactively fetch platform manifests in the background
			// Limit to first 5 platforms to avoid too many requests
			maxPlatforms := 5
			fetched := 0
			for _, manifest := range index.Manifests {
				if fetched >= maxPlatforms {
					break
				}
				// Skip attestations/SBOMs (unknown platform)
				if manifest.Platform.OS == "unknown" || manifest.Platform.Architecture == "unknown" {
					continue
				}
				if manifest.Platform.OS == "" || manifest.Platform.Architecture == "" {
					continue
				}

				// Fetch this platform manifest in the background
				go d.fetchAndStoreManifestByDigest(repoName, manifest.Digest, "")
				fetched++
			}
			if fetched > 0 {
				log.Printf("  [docker] Triggered proactive fetch for %d platform manifest(s) from index", fetched)
			}
		}
	}

	// Also store by tag if we have the tag-to-digest mapping
	key := repoName + "/" + tag
	d.tagDigestMutex.RLock()
	mappedDigest := d.tagDigestMap[key]
	d.tagDigestMutex.RUnlock()

	if mappedDigest == digest {
		// Store manifest with tag name too
		if err := d.storeManifestInStorage(repoName, tag, body, contentType, d.upstreamURL); err != nil {
			log.Printf("  [docker] Warning: Failed to store manifest with tag: %v", err)
		} else {
			log.Printf("  [docker] Also stored manifest with tag: %s/%s", repoName, tag)
		}
	}
}

// storeBlob stores a blob in our filesystem storage
func (d *DockerRegistryProxy) storeBlob(r *http.Request, body []byte, headers http.Header) error {
	// Extract digest from path
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		return fmt.Errorf("invalid blob path: %s", r.URL.Path)
	}
	digest := parts[len(parts)-1]

	// Verify digest matches content (optional, but good practice)
	hash := sha256.Sum256(body)
	expectedDigest := "sha256:" + hex.EncodeToString(hash[:])
	if digest != expectedDigest {
		return fmt.Errorf("digest mismatch: expected %s, got %s", expectedDigest, digest)
	}

	// Store blob in our filesystem storage
	if err := d.storeBlobInStorage(digest, body); err != nil {
		return fmt.Errorf("failed to store blob: %w", err)
	}

	log.Printf("  [docker] Stored blob: %s", digest[:12])
	return nil
}

// storeBlobStreaming streams a blob to disk without buffering
func (d *DockerRegistryProxy) storeBlobStreaming(r *http.Request, body io.Reader, headers http.Header) error {
	// Extract digest from path
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		return fmt.Errorf("invalid blob path: %s", r.URL.Path)
	}
	digest := parts[len(parts)-1]

	// Stream blob to storage with hash verification
	if err := d.storeBlobInStorageStreaming(digest, body); err != nil {
		return fmt.Errorf("failed to store blob: %w", err)
	}

	log.Printf("  [docker] Stored blob (streaming): %s", digest[:12])
	return nil
}

// queryTagsForDigest queries the upstream registry to find which tags map to a given digest
// This is called asynchronously when we store a manifest by digest without a known tag
func (d *DockerRegistryProxy) queryTagsForDigest(repoName, digest string, manifestBody []byte, contentType string) {
	// Query tags list from upstream
	tagsURL := fmt.Sprintf("%s/v2/%s/tags/list", d.upstreamURL, repoName)

	req, err := http.NewRequest("GET", tagsURL, nil)
	if err != nil {
		log.Printf("  [docker] Failed to create tags list request: %v", err)
		return
	}

	// Don't use proxy for upstream requests (avoid loop)
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy: config.GetUpstreamProxyFunc(),
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("  [docker] Failed to query tags list for %s: %v", repoName, err)
		return
	}
	defer util.DeferClose(resp.Body)

	// Handle 401 Unauthorized - try to authenticate and retry
	if resp.StatusCode == http.StatusUnauthorized {
		// Read WWW-Authenticate header before closing body
		wwwAuth := resp.Header.Get("WWW-Authenticate")

		// Try to get token and retry
		tagsPath := fmt.Sprintf("/v2/%s/tags/list", repoName)
		token, err := d.getTokenFromChallenge(wwwAuth, tagsPath)
		if err != nil {
			log.Printf("  [docker] Tags list query returned 401 for %s, failed to get token: %v", repoName, err)
			return
		}

		if token != "" {
			// Close first response before retry
			util.Logerr(resp.Body.Close())

			// Retry with token
			retryReq, err := http.NewRequest("GET", tagsURL, nil)
			if err != nil {
				log.Printf("  [docker] Failed to create retry tags list request: %v", err)
				return
			}
			retryReq.Header.Set("Authorization", "Bearer "+token)

			retryResp, err := client.Do(retryReq)
			if err != nil {
				log.Printf("  [docker] Failed to retry tags list query for %s: %v", repoName, err)
				return
			}

			if retryResp.StatusCode != http.StatusOK {
				util.Logerr(retryResp.Body.Close())
				log.Printf("  [docker] Tags list query returned %d for %s (after retry with token)", retryResp.StatusCode, repoName)
				return
			}

			// Use the retry response (defer will close this at function exit)
			resp = retryResp
		} else {
			log.Printf("  [docker] Tags list query returned 401 for %s (no token obtained)", repoName)
			return
		}
	} else if resp.StatusCode != http.StatusOK {
		log.Printf("  [docker] Tags list query returned %d for %s", resp.StatusCode, repoName)
		return
	}

	// Parse tags list response
	var tagsList struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tagsList); err != nil {
		log.Printf("  [docker] Failed to parse tags list: %v", err)
		return
	}

	// For each tag, check if it maps to our digest
	// We'll do HEAD requests to check digests (but limit to avoid too many requests)
	maxChecks := 10
	checked := 0
	for _, tag := range tagsList.Tags {
		if checked >= maxChecks {
			break
		}
		checked++

		// Check if this tag maps to our digest
		manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", d.upstreamURL, repoName, tag)
		headReq, err := http.NewRequest("HEAD", manifestURL, nil)
		if err != nil {
			continue
		}

		headResp, err := client.Do(headReq)
		if err != nil {
			continue
		}
		util.Logerr(headResp.Body.Close())

		if headResp.StatusCode == http.StatusOK {
			tagDigest := headResp.Header.Get("Docker-Content-Digest")
			if tagDigest == digest {
				// Found a tag that maps to this digest!
				log.Printf("  [docker] Found tag mapping: %s/%s -> %s (queried from upstream)", repoName, tag, digest[:12])

				// Store manifest with tag name (use the same upstream URL as the original manifest)
				if err := d.storeManifestInStorage(repoName, tag, manifestBody, contentType, d.upstreamURL); err != nil {
					log.Printf("  [docker] Warning: Failed to store manifest with discovered tag: %v", err)
				} else {
					log.Printf("  [docker] Stored manifest with discovered tag: %s/%s", repoName, tag)
				}

				// Update tag-to-digest mapping
				key := repoName + "/" + tag
				d.tagDigestMutex.Lock()
				d.tagDigestMap[key] = digest
				d.tagDigestMutex.Unlock()
				return // Found it, we're done
			}
		}
	}

	log.Printf("  [docker] No tag mapping found for digest %s in %s (checked %d tags)", digest[:12], repoName, checked)
}

// proxyToUpstream proxies write operations directly to upstream (no caching)
func (d *DockerRegistryProxy) proxyToUpstream(w http.ResponseWriter, r *http.Request) {
	upstreamURL := util.BuildUpstreamURL(d.upstreamURL, r.URL.Path, r.URL.RawQuery)

	// Stream request body directly to upstream (no buffering)
	req, err := http.NewRequest(r.Method, upstreamURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Copy headers (including Authorization header from client)
	for k, v := range r.Header {
		// Skip hop-by-hop headers that shouldn't be forwarded
		if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Keep-Alive") ||
			strings.EqualFold(k, "Transfer-Encoding") || strings.EqualFold(k, "Upgrade") ||
			strings.EqualFold(k, "Proxy-Authenticate") || strings.EqualFold(k, "Proxy-Authorization") {
			continue
		}
		req.Header[k] = v
	}

	// Use configurable proxy for upstream requests (supports proxy chaining)
	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			Proxy: config.GetUpstreamProxyFunc(),
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("  Error proxying to upstream: %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer util.DeferClose(resp.Body)

	// For 401 responses, pass through the WWW-Authenticate header so client can handle auth
	// The client (Docker) will handle the authentication flow itself
	if resp.StatusCode == http.StatusUnauthorized {
		log.Printf("  [docker] 401 Unauthorized from upstream - passing through for client to handle auth")
	}

	// Copy response including WWW-Authenticate header
	copyResponse(w, resp)
}

// copyResponse copies an HTTP response to the response writer
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	// Copy headers
	for k, v := range resp.Header {
		// Skip hop-by-hop headers that shouldn't be copied
		if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Keep-Alive") ||
			strings.EqualFold(k, "Transfer-Encoding") || strings.EqualFold(k, "Upgrade") {
			continue
		}
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)

	// Copy body (if present)
	if resp.Body != nil {
		if _, err := io.Copy(w, resp.Body); err != nil {
			log.Printf("  Error copying response body: %v", err)
		}
	}
}

// parseWWWAuthenticate parses a WWW-Authenticate header and extracts realm, service, and scope
func parseWWWAuthenticate(wwwAuth string) (realm, service, scope string, err error) {
	// Format: Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:astral-sh/uv:pull"
	realmRegex := regexp.MustCompile(`realm="([^"]+)"`)
	serviceRegex := regexp.MustCompile(`service="([^"]+)"`)
	scopeRegex := regexp.MustCompile(`scope="([^"]+)"`)

	realmMatch := realmRegex.FindStringSubmatch(wwwAuth)
	if len(realmMatch) > 1 {
		realm = realmMatch[1]
	}

	serviceMatch := serviceRegex.FindStringSubmatch(wwwAuth)
	if len(serviceMatch) > 1 {
		service = serviceMatch[1]
	}

	scopeMatch := scopeRegex.FindStringSubmatch(wwwAuth)
	if len(scopeMatch) > 1 {
		scope = scopeMatch[1]
	}

	if realm == "" {
		return "", "", "", fmt.Errorf("no realm found in WWW-Authenticate header")
	}

	return realm, service, scope, nil
}

// getTokenFromChallenge gets a Bearer token from the token service
func (d *DockerRegistryProxy) getTokenFromChallenge(wwwAuth, path string) (string, error) {
	if wwwAuth == "" {
		return "", fmt.Errorf("no WWW-Authenticate header")
	}

	realm, service, scope, err := parseWWWAuthenticate(wwwAuth)
	if err != nil {
		return "", fmt.Errorf("failed to parse WWW-Authenticate: %v", err)
	}

	// Extract repository scope from path if not provided
	if scope == "" {
		// Path format: /v2/<name>/manifests/<reference> or /v2/<name>/blobs/<digest>
		parts := strings.Split(strings.TrimPrefix(path, "/v2/"), "/")
		if len(parts) >= 2 {
			repoName := strings.Join(parts[:len(parts)-1], "/")
			scope = fmt.Sprintf("repository:%s:pull", repoName)
		} else {
			scope = "repository:*:pull" // Fallback to wildcard
		}
	}

	// Check cache first
	d.tokenMutex.RLock()
	if entry, ok := d.tokenCache[scope]; ok {
		if time.Now().Before(entry.expiresAt) {
			token := entry.token
			d.tokenMutex.RUnlock()
			log.Printf("  [docker] Using cached token for scope: %s", scope)
			return token, nil
		}
		// Token expired, remove from cache
		delete(d.tokenCache, scope)
	}
	d.tokenMutex.RUnlock()

	// Build token URL
	tokenURL := realm
	if !strings.Contains(tokenURL, "?") {
		tokenURL += "?"
	} else {
		tokenURL += "&"
	}
	tokenURL += "service=" + url.QueryEscape(service)
	tokenURL += "&scope=" + url.QueryEscape(scope)

	log.Printf("  [docker] Fetching token from: %s", tokenURL)

	// Fetch token
	req, err := http.NewRequest("GET", tokenURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create token request: %v", err)
	}

	// Parse token URL to check if it's an auth endpoint
	parsedTokenURL, err := url.Parse(tokenURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse token URL: %v", err)
	}

	// Bypass proxy for authentication endpoints (auth.docker.io, ghcr.io/token, etc.)
	// These endpoints should always be accessed directly to avoid authentication issues
	// when going through proxy chains
	authHostname := parsedTokenURL.Hostname()
	isAuthEndpoint := strings.Contains(authHostname, "auth.") ||
		strings.HasSuffix(authHostname, ".docker.io") ||
		strings.Contains(parsedTokenURL.Path, "/token")

	var proxyFunc func(*http.Request) (*url.URL, error)
	if isAuthEndpoint {
		log.Printf("  [docker] Bypassing proxy for auth endpoint: %s", authHostname)
		proxyFunc = nil // Direct connection, no proxy
	} else {
		proxyFunc = config.GetUpstreamProxyFunc()
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: proxyFunc,
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch token: %v", err)
	}
	defer util.DeferClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token service returned %d: %s", resp.StatusCode, string(body))
	}

	// Parse token response
	var tokenResp struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"` // Some registries use this
		ExpiresIn   int    `json:"expires_in"`
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read token response: %v", err)
	}

	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("failed to parse token response: %v", err)
	}

	token := tokenResp.Token
	if token == "" {
		token = tokenResp.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("no token in response")
	}

	// Cache the token (expire 5 minutes before actual expiration, or default to 1 hour)
	expiresIn := tokenResp.ExpiresIn
	if expiresIn == 0 {
		expiresIn = 3600 // Default to 1 hour
	}
	// Expire 5 minutes early to avoid using expired tokens
	if expiresIn > 300 {
		expiresIn -= 300
	}

	entry := &tokenCacheEntry{
		token:     token,
		expiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second),
		scope:     scope,
	}

	d.tokenMutex.Lock()
	d.tokenCache[scope] = entry
	d.tokenMutex.Unlock()

	log.Printf("  [docker] Cached token for scope: %s (expires in %d seconds)", scope, expiresIn)
	return token, nil
}

// handleBlobUpload handles the Docker blob upload flow:
// 1. POST /v2/<name>/blobs/uploads/ - initiate upload, return UUID
// 2. PATCH /v2/<name>/blobs/uploads/<uuid> - upload chunks
// 3. PUT /v2/<name>/blobs/uploads/<uuid>?digest=<digest> - finalize upload
func (d *DockerRegistryProxy) handleBlobUpload(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	parts := strings.Split(path, "/")

	// Extract repo name
	// Handle both /v2/<name>/blobs/uploads/ and /v2/<name>/blobs/blobs/uploads/ (malformed)
	if len(parts) < 4 {
		http.Error(w, "Invalid upload path", http.StatusBadRequest)
		return
	}

	// Find the first "blobs" segment to extract repo name correctly
	// Path format: /v2/<name>/blobs/uploads/... or /v2/<name>/blobs/blobs/uploads/...
	blobsIdx := -1
	for i, part := range parts {
		if part == "blobs" && i > 1 { // Skip empty first element and "v2"
			blobsIdx = i
			break
		}
	}
	if blobsIdx == -1 || blobsIdx < 2 {
		http.Error(w, "Invalid upload path: no blobs segment", http.StatusBadRequest)
		return
	}
	repoName := strings.Join(parts[2:blobsIdx], "/")

	switch r.Method {
	case "POST":
		// Initiate upload: POST /v2/<name>/blobs/uploads/
		if !strings.HasSuffix(path, "/blobs/uploads/") {
			http.Error(w, "Invalid upload initiation path", http.StatusBadRequest)
			return
		}

		// Handle cross-repository blob mount: ?mount=<digest>&from=<repo>
		// If the blob already exists locally (by digest), return 201 immediately.
		if mountDigest := r.URL.Query().Get("mount"); mountDigest != "" {
			from := r.URL.Query().Get("from")
			// Authorize the mount source. Blobs live in a single global
			// content-addressable store keyed only by digest, so reading
			// CanRead(from) alone is not enough: from= is attacker-controlled
			// and proves only that the caller can read *some* repo it names, not
			// that mountDigest belongs to from=. We therefore require BOTH:
			//   1. from= is a well-formed repo name the caller may read, and
			//   2. mountDigest is actually reachable from a manifest stored in
			//      from= (membership check) — so a caller can't name their own
			//      readable repo as from= and mount a foreign tenant's digest.
			// nil authorizer => allow (roxxy gates elsewhere). An unauthorized,
			// malformed, or non-member source falls through to a normal upload.
			mountAllowed := d.authorizeRepo == nil ||
				(isValidRepoName(from) &&
					d.authorizeRepo(r.Context(), from) &&
					d.blobIsMemberOfRepo(mountDigest, from))
			if mountAllowed {
				blobPath := d.getBlobPath(mountDigest)
				if _, err := os.Stat(blobPath); err == nil {
					log.Printf("  [docker] Cross-repo mount: blob %s already exists locally", truncDigest(mountDigest))
					w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", repoName, mountDigest))
					w.Header().Set("Docker-Content-Digest", mountDigest)
					w.WriteHeader(http.StatusCreated)
					return
				}
			} else if d.authorizeRepo != nil {
				// Security-relevant, low-volume: a denied cross-repo mount is a
				// probe against the tenant boundary and must leave a forensic
				// trace (the success path above logs; this path used to be
				// silent, indistinguishable from a benign cache-miss upload).
				log.Printf("  [docker] Cross-repo mount DENIED into %q: mount=%s from=%q not authorized/member", repoName, truncDigest(mountDigest), from)
			}
			// Blob not found locally or mount not authorized — fall through to
			// regular upload.
		}

		// Generate UUID for this upload
		uuidBytes := make([]byte, 16)
		if _, err := rand.Read(uuidBytes); err != nil {
			log.Printf("  [docker] ERROR: Failed to generate upload UUID for %s: %v", repoName, err)
			http.Error(w, "Failed to generate upload UUID", http.StatusInternalServerError)
			return
		}
		uploadUUID := hex.EncodeToString(uuidBytes)

		// Create temp file for streaming upload
		tempFile, err := os.CreateTemp(filepath.Join(d.cacheDir, "tmp"), "upload-"+uploadUUID+"-*.tmp")
		if err != nil {
			log.Printf("  [docker] ERROR: Failed to create temp file for upload %s: %v", uploadUUID[:8], err)
			http.Error(w, "Failed to initialize upload", http.StatusInternalServerError)
			return
		}
		tempPath := tempFile.Name()
		util.Logerr(tempFile.Close())

		// Store temp file path in upload map
		d.uploadMutex.Lock()
		d.uploadMap[uploadUUID] = tempPath
		d.uploadMutex.Unlock()

		// Return Location header with upload UUID
		// Docker expects this to be an absolute path from the registry root
		uploadURL := fmt.Sprintf("/v2/%s/blobs/uploads/%s", repoName, uploadUUID)
		w.Header().Set("Location", uploadURL)
		w.Header().Set("Range", "0-0")
		w.Header().Set("Docker-Upload-UUID", uploadUUID)
		w.WriteHeader(http.StatusAccepted)
		return

	case "PATCH":
		// Upload chunk: PATCH /v2/<name>/blobs/uploads/<uuid>
		if len(parts) < 5 {
			http.Error(w, "Invalid upload chunk path", http.StatusBadRequest)
			return
		}
		uploadUUID := parts[len(parts)-1]

		// Get temp file path
		d.uploadMutex.RLock()
		tempPath, exists := d.uploadMap[uploadUUID]
		d.uploadMutex.RUnlock()

		if !exists {
			log.Printf("  [docker] ERROR: Upload not found for UUID %s (repo: %s)", uploadUUID[:8], repoName)
			http.Error(w, "Upload not found", http.StatusNotFound)
			return
		}

		// Open temp file in append mode and stream chunk data directly
		file, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Printf("  [docker] ERROR: Failed to open temp file for upload UUID %s: %v", uploadUUID[:8], err)
			http.Error(w, "Failed to write chunk", http.StatusInternalServerError)
			return
		}

		bytesWritten, err := io.Copy(file, r.Body)
		util.Logerr(file.Close())

		if err != nil {
			log.Printf("  [docker] ERROR: Failed to write chunk for upload UUID %s: %v", uploadUUID[:8], err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Get current file size for Range header
		fileInfo, err := os.Stat(tempPath)
		if err != nil {
			log.Printf("  [docker] ERROR: Failed to stat temp file for upload UUID %s: %v", uploadUUID[:8], err)
			http.Error(w, "Failed to get upload size", http.StatusInternalServerError)
			return
		}
		newSize := fileInfo.Size()

		// Return progress
		uploadURL := fmt.Sprintf("/v2/%s/blobs/uploads/%s", repoName, uploadUUID)
		w.Header().Set("Location", uploadURL)
		w.Header().Set("Range", fmt.Sprintf("0-%d", newSize-1))
		w.Header().Set("Docker-Upload-UUID", uploadUUID)
		w.WriteHeader(http.StatusNoContent)

		if d.debug {
			log.Printf("  [docker] Upload chunk: %s wrote %d bytes (total: %d)", uploadUUID[:8], bytesWritten, newSize)
		}
		return

	case "PUT":
		// Finalize upload: PUT /v2/<name>/blobs/uploads/<uuid>?digest=<digest>
		if len(parts) < 5 {
			http.Error(w, "Invalid upload finalization path", http.StatusBadRequest)
			return
		}
		uploadUUID := parts[len(parts)-1]

		// Get digest from query parameter
		digest := r.URL.Query().Get("digest")
		if digest == "" {
			log.Printf("  [docker] ERROR: Missing digest parameter for upload UUID %s (repo: %s)", uploadUUID[:8], repoName)
			http.Error(w, "Missing digest parameter", http.StatusBadRequest)
			return
		}

		// Get temp file path from map
		d.uploadMutex.Lock()
		tempPath, exists := d.uploadMap[uploadUUID]
		delete(d.uploadMap, uploadUUID) // Clean up immediately
		d.uploadMutex.Unlock()

		var finalReader io.Reader
		var cleanupFunc func()

		// Check if we have a temp file from chunked upload (POST + PATCH)
		if exists && tempPath != "" {
			// Check temp file size - if empty and PUT has body data, use body instead
			fileInfo, err := os.Stat(tempPath)
			if err != nil {
				log.Printf("  [docker] ERROR: Failed to stat temp file for upload UUID %s (repo: %s): %v", uploadUUID[:8], repoName, err)
				http.Error(w, fmt.Sprintf("Failed to stat temp file: %v", err), http.StatusInternalServerError)
				return
			}

			// Per OCI spec, PUT can include final chunk data - append it to temp file if present
			if r.ContentLength > 0 || r.Header.Get("Transfer-Encoding") == "chunked" {
				// Append PUT body data to temp file
				file, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_APPEND, 0644)
				if err != nil {
					log.Printf("  [docker] ERROR: Failed to open temp file for append for upload UUID %s: %v", uploadUUID[:8], err)
					http.Error(w, "Failed to write final chunk", http.StatusInternalServerError)
					return
				}
				if _, err := io.Copy(file, r.Body); err != nil {
					util.Logerr(file.Close())
					log.Printf("  [docker] ERROR: Failed to append final chunk for upload UUID %s: %v", uploadUUID[:8], err)
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				util.Logerr(file.Close())
			} else if fileInfo.Size() == 0 {
				// Temp file is empty and no body data - error
				util.Logerr(os.Remove(tempPath))
				log.Printf("  [docker] ERROR: Empty temp file and no body data for upload UUID %s (repo: %s)", uploadUUID[:8], repoName)
				http.Error(w, "No upload data provided", http.StatusBadRequest)
				return
			}

			// Open temp file for reading
			tempFile, err := os.Open(tempPath)
			if err != nil {
				log.Printf("  [docker] ERROR: Failed to open temp file for upload UUID %s (repo: %s): %v", uploadUUID[:8], repoName, err)
				http.Error(w, fmt.Sprintf("Failed to open temp file: %v", err), http.StatusInternalServerError)
				return
			}
			finalReader = tempFile
			cleanupFunc = func() {
				util.Logerr(tempFile.Close())
				util.Logerr(os.Remove(tempPath))
			}
		} else {
			// Monolithic upload - data is in request body
			// Check if there's any body data
			if r.ContentLength == 0 && !exists {
				log.Printf("  [docker] ERROR: Upload not found and no body data for UUID %s (repo: %s)", uploadUUID[:8], repoName)
				http.Error(w, "Upload not found and no body data", http.StatusNotFound)
				return
			}
			// For monolithic upload, stream directly from request body
			finalReader = r.Body
			cleanupFunc = func() {}
		}
		defer cleanupFunc()

		// Stream to final blob location with digest verification
		if err := d.storeBlobInStorageStreaming(digest, finalReader); err != nil {
			log.Printf("  [docker] ERROR: Failed to store blob %s for upload UUID %s (repo: %s): %v", digest[:12], uploadUUID[:8], repoName, err)
			http.Error(w, fmt.Sprintf("Failed to store blob: %v", err), http.StatusInternalServerError)
			return
		}

		// Return success with Location header pointing to the blob
		blobURL := fmt.Sprintf("/v2/%s/blobs/%s", repoName, digest)
		w.Header().Set("Location", blobURL)
		w.Header().Set("Content-Length", "0")
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)

		if d.debug {
			log.Printf("  [docker] Upload finalized: %s -> %s", uploadUUID[:8], digest[:12])
		}
		return

	case "GET":
		// Upload status: GET /v2/<name>/blobs/uploads/<uuid>
		// Returns the current status of an in-progress upload (OCI spec)
		if len(parts) < 5 {
			http.Error(w, "Invalid upload status path", http.StatusBadRequest)
			return
		}
		uploadUUID := parts[len(parts)-1]

		// Look up the upload
		d.uploadMutex.RLock()
		tempPath, exists := d.uploadMap[uploadUUID]
		d.uploadMutex.RUnlock()

		if !exists {
			http.Error(w, "Upload not found", http.StatusNotFound)
			return
		}

		// Stat the temp file to get current offset
		fileInfo, err := os.Stat(tempPath)
		if err != nil {
			log.Printf("  [docker] ERROR: Failed to stat temp file for upload UUID %s: %v", uploadUUID[:8], err)
			http.Error(w, "Failed to get upload status", http.StatusInternalServerError)
			return
		}

		offset := fileInfo.Size()
		uploadURL := fmt.Sprintf("/v2/%s/blobs/uploads/%s", repoName, uploadUUID)
		w.Header().Set("Location", uploadURL)
		w.Header().Set("Docker-Upload-UUID", uploadUUID)
		if offset > 0 {
			w.Header().Set("Range", fmt.Sprintf("0-%d", offset-1))
		} else {
			w.Header().Set("Range", "0-0")
		}
		w.WriteHeader(http.StatusNoContent)
		return

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
}

// handleDeleteManifest handles DELETE requests for manifests
func (d *DockerRegistryProxy) handleDeleteManifest(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		http.Error(w, "Invalid manifest path", http.StatusBadRequest)
		return
	}

	repoName := strings.Join(parts[2:len(parts)-2], "/")
	reference := parts[len(parts)-1]

	log.Printf("  [docker] DELETE manifest: %s/%s", repoName, reference)

	// Resolve the digest the caller is unreferencing. For a tag delete,
	// look up the digest the tag points at. For a digest delete, the
	// reference IS the digest.
	var digest string
	if strings.HasPrefix(reference, "sha256:") {
		digest = reference
	} else {
		key := repoName + "/" + reference
		d.tagDigestMutex.RLock()
		digest = d.tagDigestMap[key]
		d.tagDigestMutex.RUnlock()
	}

	// Always remove the by-tag mirror file (if reference is a tag) —
	// that's the one literal file the client asked to delete. The
	// by-digest file stays until refcount hits 0 (cascade handles it).
	if !strings.HasPrefix(reference, "sha256:") {
		manifestPath := d.getManifestPath(repoName, reference, d.upstreamURL)
		if err := os.Remove(manifestPath); err != nil && !os.IsNotExist(err) {
			log.Printf("  [docker] Warning: Failed to delete manifest file %s: %v", manifestPath, err)
		} else if err == nil {
			log.Printf("  [docker] Deleted manifest file: %s/%s", repoName, reference)
		}
		util.Logerr(os.Remove(manifestPath + ".meta"))
		util.Logerr(os.Remove(manifestPath + ".metadata"))
	}

	// Decrement the manifest refcount; cascade-delete if it hits 0.
	if digest != "" {
		digestHash := strings.TrimPrefix(digest, "sha256:")
		shouldDelete, err := cache.DecrementManifestRefcount(d.cacheDir, digestHash)
		if err != nil {
			log.Printf("  [docker] Warning: dec manifest refcount %s: %v", digest[:12], err)
		} else if shouldDelete {
			d.cascadeDeleteManifest(repoName, digest)
		} else {
			log.Printf("  [docker] Decremented refcount for manifest: %s (still referenced)", digest[:12])
		}
	} else {
		// We don't know which digest the tag pointed at (cold cache,
		// tagDigestMap not populated). Fall back to old behavior: read
		// the body and dec its blobs directly, removing the by-digest
		// file too. Better than leaking; matches pre-refcount semantics.
		manifestBody, _, err := d.getManifestFromStorage(r.URL.Path)
		if err == nil {
			configDigest, layerDigests, _ := d.extractBlobDigestsFromManifest(manifestBody)
			for _, b := range append([]string{configDigest}, layerDigests...) {
				if b == "" {
					continue
				}
				bh := strings.TrimPrefix(b, "sha256:")
				if _, derr := cache.DecrementRefcount(d.cacheDir, bh); derr != nil {
					log.Printf("  [docker] Warning: dec blob refcount %s: %v", b[:12], derr)
				}
			}
		}
	}

	// Remove tag-to-digest mapping if it was a tag
	if !strings.HasPrefix(reference, "sha256:") {
		key := repoName + "/" + reference
		d.tagDigestMutex.Lock()
		delete(d.tagDigestMap, key)
		d.tagDigestMutex.Unlock()
		log.Printf("  [docker] Removed tag-to-digest mapping: %s", key)
	}

	// Docker Registry API returns 202 Accepted for successful manifest deletion
	log.Printf("  [docker] Successfully deleted manifest: %s/%s", repoName, reference)
	w.WriteHeader(http.StatusAccepted)
}

// processDockerManifestMetadata extracts and stores metadata from a Docker manifest
// This is called automatically when new manifests are stored
func (d *DockerRegistryProxy) processDockerManifestMetadata(manifestPath string) error {
	// Check if metadata already exists
	metaPath := manifestPath + ".metadata"
	if _, err := os.Stat(metaPath); err == nil {
		// Metadata already exists, skip
		return nil
	}

	// Read manifest
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest: %w", err)
	}

	// Parse manifest to get config digest and annotations
	var manifest struct {
		MediaType string `json:"mediaType"`
		Config    struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}

	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	metadata := make(map[string]string)

	// First, check manifest annotations (OCI standard)
	if manifest.Annotations != nil {
		// OCI standard annotation keys
		if license, ok := manifest.Annotations["org.opencontainers.image.licenses"]; ok && license != "" {
			metadata["license"] = license
		}
		if desc, ok := manifest.Annotations["org.opencontainers.image.description"]; ok && desc != "" {
			metadata["description"] = desc
		}
		if title, ok := manifest.Annotations["org.opencontainers.image.title"]; ok && title != "" {
			metadata["title"] = title
		}
		if doc, ok := manifest.Annotations["org.opencontainers.image.documentation"]; ok && doc != "" {
			metadata["documentation"] = doc
		}

		// Also check legacy annotation keys
		if license, ok := manifest.Annotations["license"]; ok && license != "" && metadata["license"] == "" {
			metadata["license"] = license
		}
		if desc, ok := manifest.Annotations["description"]; ok && desc != "" && metadata["description"] == "" {
			metadata["description"] = desc
		}
	}

	// Then, check config blob labels (Docker format)
	if manifest.Config.Digest != "" {
		configBlobPath := util.DockerBlobPathWithFallback(d.cacheDir, manifest.Config.Digest)
		configData, err := os.ReadFile(configBlobPath)
		if err != nil {
			// Config blob might not be cached yet, that's okay
			// We'll process it later when the blob is available
		} else {
			var config struct {
				Config struct {
					Labels map[string]string `json:"Labels,omitempty"`
				} `json:"config,omitempty"`
			}

			if err := json.Unmarshal(configData, &config); err == nil {
				if config.Config.Labels != nil {
					// Check standard OCI label keys
					if license, ok := config.Config.Labels["org.opencontainers.image.licenses"]; ok && license != "" && metadata["license"] == "" {
						metadata["license"] = license
					}
					if desc, ok := config.Config.Labels["org.opencontainers.image.description"]; ok && desc != "" && metadata["description"] == "" {
						metadata["description"] = desc
					}
					if title, ok := config.Config.Labels["org.opencontainers.image.title"]; ok && title != "" && metadata["title"] == "" {
						metadata["title"] = title
					}
					if doc, ok := config.Config.Labels["org.opencontainers.image.documentation"]; ok && doc != "" && metadata["documentation"] == "" {
						metadata["documentation"] = doc
					}

					// Check legacy label keys
					if license, ok := config.Config.Labels["license"]; ok && license != "" && metadata["license"] == "" {
						metadata["license"] = license
					}
					if desc, ok := config.Config.Labels["description"]; ok && desc != "" && metadata["description"] == "" {
						metadata["description"] = desc
					}
				}
			}
		}
	}

	// Only save if we found any metadata
	if len(metadata) == 0 {
		return nil // No metadata found, skip saving
	}

	// Save metadata to .metadata file (already a map[string]string)
	metaJSON, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	if err := os.WriteFile(metaPath, metaJSON, 0644); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}

	return nil
}

// paginateStringSlice applies OCI pagination (n, last) to a sorted string slice.
// n = max items to return, last = last item from previous page (start after it).
func paginateStringSlice(items []string, query url.Values) []string {
	last := query.Get("last")
	if last != "" {
		// Find the position after 'last' using binary search on sorted slice
		idx := sort.SearchStrings(items, last)
		// Move past the 'last' item itself
		if idx < len(items) && items[idx] == last {
			idx++
		}
		items = items[idx:]
	}

	if nStr := query.Get("n"); nStr != "" {
		if n, err := strconv.Atoi(nStr); err == nil && n > 0 && n < len(items) {
			items = items[:n]
		}
	}

	return items
}

// handleReferrers implements GET /v2/<name>/referrers/<digest>
// Returns an OCI image index containing manifests that reference the given digest via subject.digest.
func (d *DockerRegistryProxy) handleReferrers(w http.ResponseWriter, r *http.Request, repoName, digest string) {
	registryID := normalizeRegistryURL(d.upstreamURL)
	safeRepo := util.SafeRepoName(repoName)
	repoDir := filepath.Join(d.manifestDir, registryID, safeRepo)

	entries, err := os.ReadDir(repoDir)
	if err != nil {
		// No manifests at all — return empty index
		d.writeReferrersIndex(w, nil, "")
		return
	}

	// Optional artifactType filter
	artifactTypeFilter := r.URL.Query().Get("artifactType")

	type ociDescriptor struct {
		MediaType    string `json:"mediaType"`
		Digest       string `json:"digest"`
		Size         int64  `json:"size"`
		ArtifactType string `json:"artifactType,omitempty"`
	}

	var referrers []ociDescriptor

	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".meta") {
			continue
		}
		// Only check digest-addressed manifests (sha256:...) to avoid duplicates with tags
		if !strings.HasPrefix(entry.Name(), "sha256:") {
			continue
		}

		manifestPath := filepath.Join(repoDir, entry.Name())
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}

		// Parse manifest to check for subject field
		var manifest struct {
			MediaType    string `json:"mediaType"`
			ArtifactType string `json:"artifactType"`
			Subject      *struct {
				Digest string `json:"digest"`
			} `json:"subject"`
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			continue
		}

		// Skip manifests without a subject or that don't reference our digest
		if manifest.Subject == nil || manifest.Subject.Digest != digest {
			continue
		}

		// Apply artifactType filter if specified
		if artifactTypeFilter != "" && manifest.ArtifactType != artifactTypeFilter {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		referrers = append(referrers, ociDescriptor{
			MediaType:    manifest.MediaType,
			Digest:       entry.Name(),
			Size:         info.Size(),
			ArtifactType: manifest.ArtifactType,
		})
	}

	d.writeReferrersIndex(w, referrers, artifactTypeFilter)
}

// writeReferrersIndex writes an OCI image index response for the referrers API.
func (d *DockerRegistryProxy) writeReferrersIndex(w http.ResponseWriter, manifests interface{}, artifactTypeFilter string) {
	if manifests == nil {
		manifests = []struct{}{}
	}

	response := struct {
		SchemaVersion int         `json:"schemaVersion"`
		MediaType     string      `json:"mediaType"`
		Manifests     interface{} `json:"manifests"`
	}{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     manifests,
	}

	w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if artifactTypeFilter != "" {
		w.Header().Set("OCI-Filters-Applied", "artifactType")
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Error encoding referrers response: %v", err)
	}
}
