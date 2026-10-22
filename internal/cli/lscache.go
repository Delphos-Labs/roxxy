package cli

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/packages"
	"github.com/Delphos-Labs/roxxy/internal/packages/index"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// Config holds configuration for the lscache command
type Config struct {
	CacheDir     string
	StatsTracker *cache.StatsTracker
}

// Run executes the lscache command with the given arguments
func Run(cfg Config, args []string) {
	var hostname string
	var treeView bool
	var dockerMode bool
	var packagesMode bool
	var imageName string
	var showAll bool
	var sortBy string
	var pmType string
	var deleteImage string
	var fromTarball string
	var platform string
	var longMode bool

	fs := flag.NewFlagSet("lscache", flag.ExitOnError)
	fs.StringVar(&hostname, "hostname", "", "Filter by hostname (optional)")
	fs.BoolVar(&treeView, "tree", false, "Display as compact directory tree")
	fs.BoolVar(&dockerMode, "docker", false, "List Docker registry images")
	fs.BoolVar(&packagesMode, "packages", false, "List package manager cache (APK, APT, NPM, PyPI, Go)")
	fs.StringVar(&pmType, "type", "", "Filter by package type: apk, apt, npm, pypi, go (requires --packages)")
	fs.StringVar(&imageName, "image", "", "Show layers for specific Docker image (requires --docker)")
	fs.BoolVar(&showAll, "all", false, "Show all repositories including those with no tags")
	fs.StringVar(&sortBy, "sort", "host", "Sort by: host (raw/docker), name (packages), time, size, accessed (default: host for raw/docker, name for packages)")
	fs.StringVar(&deleteImage, "delete", "", "Delete Docker image from cache (format: repo:tag or repo@digest, requires --docker)")
	fs.StringVar(&fromTarball, "from-tarball", "", "Read from tarball instead of cache directory (requires --docker)")
	fs.StringVar(&platform, "platform", "", "Platform to use for multi-platform images (format: os/arch, e.g., linux/amd64, linux/arm64)")
	fs.BoolVar(&longMode, "l", false, "Long format: show image size and last-write time (requires --docker)")
	debug := fs.Bool("debug", false, "Enable debug logging")
	util.Logerr(fs.Parse(args))

	// Handle package manager mode
	if packagesMode {
		// For packages, default sort is "name" if not specified
		packageSortBy := sortBy
		if packageSortBy == "host" {
			packageSortBy = "name" // Default for packages
		}

		// Load index data to enrich cache metadata with Description and License
		// Match by URL: cache metadata has URLPath, index data has URL
		indexDataByURL := loadIndexDataByURL(cfg.CacheDir, pmType)

		packages.ListCache(filepath.Join(cfg.CacheDir, "packages"), pmType, packageSortBy, indexDataByURL)
		return
	}

	// Handle Docker registry mode
	if dockerMode {
		if fromTarball != "" {
			// List images from tarball
			if imageName != "" {
				showDockerImageLayersFromTarball(fromTarball, imageName, platform, *debug)
			} else {
				listDockerImagesFromTarball(fromTarball, showAll, *debug)
			}
			return
		}

		if deleteImage != "" {
			deleteDockerImage(deleteImage)
			return
		}
		if imageName != "" {
			showDockerImageLayers(cfg.CacheDir, imageName, platform)
		} else {
			listDockerImages(cfg.CacheDir, showAll, longMode)
		}
		return
	}

	// If hostname wasn't provided as a flag, check if it's a positional argument
	if hostname == "" && len(fs.Args()) > 0 {
		hostname = fs.Args()[0]
	}

	// Collect all cache entries
	entries, err := ListCacheEntries(cfg, hostname)
	if err != nil {
		log.Fatalf("Error listing cache: %v", err)
	}

	if len(entries) == 0 {
		if hostname != "" {
			fmt.Printf("No cached entries found for hostname: %s\n", hostname)
		} else {
			fmt.Println("No cached entries found")
		}
		return
	}

	// Sort entries based on flag
	switch sortBy {
	case "size":
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Size > entries[j].Size // Largest first
		})
	case "accessed":
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].AccessCount > entries[j].AccessCount // Most accessed first
		})
	default: // "host" or anything else
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].Hostname != entries[j].Hostname {
				return entries[i].Hostname < entries[j].Hostname
			}
			return entries[i].Path < entries[j].Path
		})
	}

	if treeView {
		displayTreeView(entries)
	} else {
		displayListView(entries)
	}
}

// CacheEntry represents a cached URL entry
type CacheEntry struct {
	Hostname      string
	Path          string
	Size          int64
	ContentType   string
	CachedAt      time.Time
	ContentHash   string
	AccessCount   int
	LastAccessed  time.Time
	FetchDuration time.Duration
}

// ListCacheEntries walks the cache directory and collects all metadata entries
// Exported for use by web UI
func ListCacheEntries(cfg Config, filterHostname string) ([]CacheEntry, error) {
	var entries []CacheEntry
	urlCacheDir := filepath.Join(cfg.CacheDir, "url")

	// Ensure the url cache directory exists (it may not exist if no URLs have been cached yet)
	if _, err := os.Stat(urlCacheDir); os.IsNotExist(err) {
		// Directory doesn't exist yet, return empty list
		return entries, nil
	}

	// Walk the cache directory structure: cache/url/hostname/12/34/hash
	err := filepath.Walk(urlCacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Parse the path to extract hostname
		// Expected: cache/url/hostname/12/34/hash
		relPath, err := filepath.Rel(urlCacheDir, path)
		if err != nil {
			return nil
		}

		parts := strings.Split(relPath, string(filepath.Separator))
		if len(parts) < 4 {
			return nil // Invalid structure
		}

		entryHostname := parts[0]
		if filterHostname != "" && entryHostname != filterHostname {
			return nil // Skip if hostname doesn't match filter
		}

		// Read metadata file
		metadataBytes, err := os.ReadFile(path)
		if err != nil {
			return nil // Skip files we can't read
		}

		var metadata cache.Metadata
		if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
			return nil // Skip invalid JSON
		}

		// Parse URL to extract path
		parsedURL, err := url.Parse(metadata.URL)
		if err != nil {
			return nil
		}

		// Get access stats for this content hash
		accessCount := 0
		lastAccessed := time.Time{}
		if cfg.StatsTracker != nil {
			accessCount, lastAccessed = cfg.StatsTracker.GetAccessCount(metadata.ContentHash)
		}

		entries = append(entries, CacheEntry{
			Hostname:      entryHostname,
			Path:          parsedURL.Path,
			Size:          metadata.Size,
			ContentType:   metadata.ContentType,
			CachedAt:      metadata.CachedAt,
			ContentHash:   metadata.ContentHash,
			AccessCount:   accessCount,
			LastAccessed:  lastAccessed,
			FetchDuration: metadata.FetchDuration,
		})

		return nil
	})

	return entries, err
}

// displayListView displays entries in a simple list format
func displayListView(entries []CacheEntry) {
	fmt.Printf("%-40s %-12s %-10s %-20s %s\n", "HOSTNAME", "SIZE", "ACCESSED", "LAST ACCESS", "PATH")
	fmt.Println(strings.Repeat("-", 140))

	var totalTimeSaved time.Duration

	for _, entry := range entries {
		sizeStr := formatSize(entry.Size)
		accessStr := fmt.Sprintf("%dx", entry.AccessCount)
		if entry.AccessCount == 0 {
			accessStr = "never"
		}

		lastAccessStr := "never"
		if !entry.LastAccessed.IsZero() {
			lastAccessStr = entry.LastAccessed.Format("2006-01-02 15:04")
		}

		fmt.Printf("%-40s %-12s %-10s %-20s %s\n",
			entry.Hostname,
			sizeStr,
			accessStr,
			lastAccessStr,
			entry.Path)

		// Calculate time saved for this entry (cache hits only)
		if entry.AccessCount > 0 && entry.FetchDuration > 0 {
			// Subtract 1 because the first access was a cache miss (the fetch itself)
			cacheHits := entry.AccessCount - 1
			if cacheHits > 0 {
				timeSaved := entry.FetchDuration * time.Duration(cacheHits)
				totalTimeSaved += timeSaved
			}
		}
	}

	fmt.Printf("\nTotal: %d entries\n", len(entries))
	if totalTimeSaved > 0 {
		// Format time saved nicely
		if totalTimeSaved < time.Minute {
			fmt.Printf("Time saved by cache: %.1fs\n", totalTimeSaved.Seconds())
		} else if totalTimeSaved < time.Hour {
			fmt.Printf("Time saved by cache: %.1fm (%.1fs)\n", totalTimeSaved.Minutes(), totalTimeSaved.Seconds())
		} else {
			fmt.Printf("Time saved by cache: %.1fh (%.1fm)\n", totalTimeSaved.Hours(), totalTimeSaved.Minutes())
		}
	}
}

// displayTreeView displays entries in a compact directory tree format
func displayTreeView(entries []CacheEntry) {
	type treeNode struct {
		name     string
		children map[string]*treeNode
		entries  []CacheEntry
		isDir    bool
	}

	buildTree := func(hostname string, hostEntries []CacheEntry) *treeNode {
		root := &treeNode{name: hostname, children: make(map[string]*treeNode), isDir: true}
		for _, entry := range hostEntries {
			pathParts := strings.Split(strings.Trim(entry.Path, "/"), "/")
			current := root
			for i, part := range pathParts {
				if i == len(pathParts)-1 {
					// File node
					if current.children[part] == nil {
						current.children[part] = &treeNode{
							name:     part,
							children: nil,
							entries:  []CacheEntry{entry},
							isDir:    false,
						}
					} else {
						current.children[part].entries = append(current.children[part].entries, entry)
					}
				} else {
					// Directory node
					if current.children[part] == nil {
						current.children[part] = &treeNode{
							name:     part,
							children: make(map[string]*treeNode),
							isDir:    true,
						}
					}
					current = current.children[part]
				}
			}
		}
		return root
	}

	var printTree func(*treeNode, string, bool)
	printTree = func(node *treeNode, prefix string, isLast bool) {
		connector := "├── "
		if isLast {
			connector = "└── "
		}

		if node.isDir {
			fmt.Printf("%s%s%s/\n", prefix, connector, node.name)
			childPrefix := prefix
			if isLast {
				childPrefix += "    "
			} else {
				childPrefix += "│   "
			}

			// Sort children for consistent output
			keys := make([]string, 0, len(node.children))
			for k := range node.children {
				keys = append(keys, k)
			}
			sort.Strings(keys)

			for i, key := range keys {
				printTree(node.children[key], childPrefix, i == len(keys)-1)
			}
		} else {
			// File node - show first entry's details
			entry := node.entries[0]
			fmt.Printf("%s%s%s [%s, %s]\n",
				prefix, connector, node.name,
				formatSize(entry.Size),
				entry.ContentType)
		}
	}

	// Group entries by hostname
	hostGroups := make(map[string][]CacheEntry)
	for _, entry := range entries {
		hostGroups[entry.Hostname] = append(hostGroups[entry.Hostname], entry)
	}

	// Sort hostnames
	hostnames := make([]string, 0, len(hostGroups))
	for h := range hostGroups {
		hostnames = append(hostnames, h)
	}
	sort.Strings(hostnames)

	// Display tree for each hostname
	for i, hostname := range hostnames {
		if i > 0 {
			fmt.Println()
		}
		tree := buildTree(hostname, hostGroups[hostname])
		printTree(tree, "", true)
	}

	fmt.Printf("\nTotal: %d entries\n", len(entries))
}

// formatSize formats file size in human-readable format
func formatSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}

// listDockerImages lists all images from our filesystem manifest storage
func listDockerImages(cacheDir string, showAll, longMode bool) {
	manifestDir := filepath.Join(cacheDir, "docker-manifests")

	// Scan manifest directory for repositories
	repos := make(map[string]map[string]bool)     // repo -> set of tags
	reposWithOnlyDigests := make(map[string]bool) // Repos that only have digest-only manifests

	err := filepath.Walk(manifestDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		// Skip sidecars: .meta (content-type cache) and .metadata (helm chart
		// metadata written alongside chart manifests). Both are bookkeeping
		// files, not separate images.
		if strings.HasSuffix(path, ".meta") || strings.HasSuffix(path, ".metadata") {
			return nil
		}

		// Path format: docker-manifests/<safe-repo>/<reference>
		// where safe-repo has / replaced with _
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

		// Include registry in repo key to avoid collisions: <registry>/<repo>
		repo := registryID + "/" + util.UnsafeRepoName(safeRepo)

		// If it's a digest, track that this repo has at least one manifest
		if strings.HasPrefix(reference, "sha256:") {
			// Check if this repo directory has any tag files
			repoDir := filepath.Dir(path)
			entries, err := os.ReadDir(repoDir)
			if err == nil {
				hasTag := false
				for _, entry := range entries {
					if !entry.IsDir() && !strings.HasSuffix(entry.Name(), ".meta") && !strings.HasSuffix(entry.Name(), ".metadata") {
						if !strings.HasPrefix(entry.Name(), "sha256:") {
							hasTag = true
							break
						}
					}
				}
				// If no tags found, mark this repo as having only digests
				if !hasTag {
					reposWithOnlyDigests[repo] = true
				}
			}
			return nil
		}

		// It's a tag file
		if repos[repo] == nil {
			repos[repo] = make(map[string]bool)
		}
		repos[repo][reference] = true
		return nil
	})

	if err != nil {
		log.Fatalf("Failed to scan manifest directory: %v", err)
	}

	if len(repos) == 0 {
		fmt.Println("No images found in local cache")
		return
	}

	fmt.Printf("Images in local cache:\n\n")
	repoCount := 0
	taglessCount := 0

	// Sort repos for consistent output
	repoNames := make([]string, 0, len(repos))
	for repo := range repos {
		repoNames = append(repoNames, repo)
	}
	sort.Strings(repoNames)

	for _, repo := range repoNames {
		tags := repos[repo]
		tagList := make([]string, 0, len(tags))
		for tag := range tags {
			tagList = append(tagList, tag)
		}
		sort.Strings(tagList)

		if len(tagList) == 0 {
			taglessCount++
			if showAll {
				displayRepo := formatRepoForDisplay(repo)
				fmt.Printf("  %s: (no tags)\n", displayRepo)
				repoCount++
			}
		} else {
			for _, tag := range tagList {
				platforms, isIndex := getCachedPlatforms(manifestDir, repo, tag)
				// Format repo name for display (hide docker.io prefix for cleaner output)
				displayRepo := formatRepoForDisplay(repo)
				suffix := ""
				switch {
				case len(platforms) > 0:
					suffix = " [" + strings.Join(platforms, ", ") + "]"
				case isIndex:
					suffix = " [multi-platform, platforms not cached]"
				}
				if longMode {
					size, mtime := dockerImageStats(manifestDir, repo, tag)
					fmt.Printf("  %-50s %10s  %s%s\n",
						displayRepo+":"+tag, util.FormatBytes(size),
						mtime.Format("2006-01-02 15:04"), suffix)
				} else {
					fmt.Printf("  %s:%s%s\n", displayRepo, tag, suffix)
				}
			}
			repoCount++
		}
	}

	// If --all is set, also show repos that only have digest-only manifests
	if showAll {
		digestOnlyRepos := make([]string, 0, len(reposWithOnlyDigests))
		for repo := range reposWithOnlyDigests {
			// Only include if not already shown (i.e., has no tags)
			if len(repos[repo]) == 0 {
				digestOnlyRepos = append(digestOnlyRepos, repo)
			}
		}
		sort.Strings(digestOnlyRepos)
		for _, repo := range digestOnlyRepos {
			displayRepo := formatRepoForDisplay(repo)
			fmt.Printf("  %s: (digest-only, no tags)\n", displayRepo)
			repoCount++
		}
	}

	if showAll {
		fmt.Printf("\nTotal: %d repositories\n", repoCount)
	} else {
		fmt.Printf("\nTotal: %d repositories", repoCount)
		if taglessCount > 0 {
			fmt.Printf(" (%d tagless repositories hidden, use --all to show)\n", taglessCount)
		} else {
			fmt.Println()
		}
	}
}

// getCachedPlatforms returns a list of platforms that are cached for a given image tag
// Returns (platforms, isIndex) where isIndex indicates if this is a multi-platform manifest list
// dockerImageStats walks the manifest tree for repo:tag and returns the total
// on-disk size (manifest files + every config + layer blob, plus each
// sub-manifest under an OCI/Docker index) and the most recent mtime across
// every file it touched. Missing blobs are skipped silently — the count is
// "what's actually on disk", not "what the manifest claims" — so a partially-
// pulled multi-arch image still reports the bytes you'd reclaim by deleting
// it. Returns zero size and zero time if the top-level manifest is unreadable.
func dockerImageStats(manifestDir, repo, tag string) (int64, time.Time) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return 0, time.Time{}
	}
	registryID, repoName := parts[0], parts[1]
	safeRepo := util.SafeRepoName(repoName)
	repoDir := filepath.Join(manifestDir, registryID, safeRepo)
	cacheDir := filepath.Dir(manifestDir)

	var total int64
	var latest time.Time
	bump := func(t time.Time) {
		if t.After(latest) {
			latest = t
		}
	}
	addFile := func(path string) (data []byte, ok bool) {
		info, err := os.Stat(path)
		if err != nil {
			return nil, false
		}
		total += info.Size()
		bump(info.ModTime())
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, false
		}
		return b, true
	}

	// Walk the manifest at `path`. If it's an index, recurse into each
	// sub-manifest by digest. For leaf image manifests, stat config + layers.
	// `seen` prevents revisiting a sub-manifest that's referenced twice.
	seen := make(map[string]bool)
	var walk func(path string)
	walk = func(path string) {
		data, ok := addFile(path)
		if !ok {
			return
		}
		// Try as index first.
		var idx struct {
			MediaType string `json:"mediaType"`
			Manifests []struct {
				Digest string `json:"digest"`
			} `json:"manifests"`
		}
		if err := json.Unmarshal(data, &idx); err == nil && len(idx.Manifests) > 0 {
			for _, sub := range idx.Manifests {
				if sub.Digest == "" || seen[sub.Digest] {
					continue
				}
				seen[sub.Digest] = true
				walk(filepath.Join(repoDir, sub.Digest))
			}
			return
		}
		// Leaf manifest: stat config + every layer blob.
		var leaf struct {
			Config struct {
				Digest string `json:"digest"`
			} `json:"config"`
			Layers []struct {
				Digest string `json:"digest"`
			} `json:"layers"`
		}
		if err := json.Unmarshal(data, &leaf); err != nil {
			return
		}
		statBlob := func(digest string) {
			if digest == "" {
				return
			}
			info, err := os.Stat(util.DockerBlobPathWithFallback(cacheDir, digest))
			if err != nil {
				return
			}
			total += info.Size()
			bump(info.ModTime())
		}
		statBlob(leaf.Config.Digest)
		for _, l := range leaf.Layers {
			statBlob(l.Digest)
		}
	}
	walk(filepath.Join(repoDir, tag))
	return total, latest
}

func getCachedPlatforms(manifestDir, repo, tag string) ([]string, bool) {
	// Repo format is now: <registry>/<repo> (e.g., "docker.io/library/alpine")
	// Path structure is: <manifestDir>/<registry>/<safeRepo>/<tag>
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return nil, false
	}
	registryID := parts[0]
	repoName := parts[1]
	safeRepo := util.SafeRepoName(repoName)
	manifestPath := filepath.Join(manifestDir, registryID, safeRepo, tag)

	// Read the manifest
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, false
	}

	// Check if this is an OCI index (multi-platform)
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

	if err := json.Unmarshal(data, &index); err != nil {
		return nil, false
	}

	// Check if it's an index manifest
	isIndex := index.MediaType == "application/vnd.oci.image.index.v1+json" ||
		index.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json"

	if !isIndex {
		// This is a single-platform manifest - extract platform from config blob
		var singleManifest struct {
			Config struct {
				Digest string `json:"digest"`
			} `json:"config"`
			Layers []struct {
				Digest string `json:"digest"`
			} `json:"layers"`
		}

		// Try to parse as single-platform manifest
		if err := json.Unmarshal(data, &singleManifest); err == nil {
			cacheDir := filepath.Dir(manifestDir)

			allBlobsCached := true

			// Check config blob
			if singleManifest.Config.Digest != "" {
				blobPath := util.DockerBlobPathWithFallback(cacheDir, singleManifest.Config.Digest)
				if _, err := os.Stat(blobPath); err == nil {
					// Read config blob to get platform info
					configData, err := os.ReadFile(blobPath)
					if err == nil {
						var config struct {
							Architecture string `json:"architecture"`
							OS           string `json:"os"`
							Variant      string `json:"variant,omitempty"`
						}
						if err := json.Unmarshal(configData, &config); err == nil {
							// Check layer blobs
							for _, layer := range singleManifest.Layers {
								if layer.Digest != "" {
									blobPath := util.DockerBlobPathWithFallback(cacheDir, layer.Digest)
									if _, err := os.Stat(blobPath); err != nil {
										allBlobsCached = false
										break
									}
								}
							}

							// Only return platform if all blobs are cached
							if allBlobsCached && config.OS != "" && config.Architecture != "" {
								platform := config.OS + "/" + config.Architecture
								if config.Variant != "" {
									platform += "/" + config.Variant
								}
								return []string{platform}, false // false = not an index
							}
						}
					}
				}
			}
		}

		// Not a multi-platform image and couldn't extract platform or blobs not cached
		return nil, false
	}

	// Check which platform manifests exist in our storage
	var cachedPlatforms []string
	for _, manifest := range index.Manifests {
		// Skip manifests with unknown platform (likely attestations, SBOMs, etc.)
		// These are not actual platform-specific image manifests
		if manifest.Platform.OS == "unknown" || manifest.Platform.Architecture == "unknown" {
			continue
		}

		// Skip if OS or Architecture is empty
		if manifest.Platform.OS == "" || manifest.Platform.Architecture == "" {
			continue
		}

		// Check if this platform manifest exists
		platformManifestPath := filepath.Join(manifestDir, registryID, safeRepo, manifest.Digest)
		if _, err := os.Stat(platformManifestPath); err != nil {
			continue // Platform manifest not cached
		}

		// Read the platform manifest to check if its blobs are cached
		platformManifestData, err := os.ReadFile(platformManifestPath)
		if err != nil {
			continue
		}

		// Parse platform manifest to get blob digests
		var platformManifest struct {
			Config struct {
				Digest string `json:"digest"`
			} `json:"config"`
			Layers []struct {
				Digest string `json:"digest"`
			} `json:"layers"`
		}

		if err := json.Unmarshal(platformManifestData, &platformManifest); err != nil {
			continue
		}

		// Check if all required blobs are cached
		// Get blob directory from cache directory
		cacheDir := filepath.Dir(manifestDir) // Go up from docker-manifests to cache

		allBlobsCached := true

		// Check config blob
		if platformManifest.Config.Digest != "" {
			blobPath := util.DockerBlobPathWithFallback(cacheDir, platformManifest.Config.Digest)
			if _, err := os.Stat(blobPath); err != nil {
				allBlobsCached = false
			}
		}

		// Check layer blobs
		if allBlobsCached {
			for _, layer := range platformManifest.Layers {
				if layer.Digest != "" {
					blobPath := util.DockerBlobPathWithFallback(cacheDir, layer.Digest)
					if _, err := os.Stat(blobPath); err != nil {
						allBlobsCached = false
						break
					}
				}
			}
		}

		// Only include platform if all blobs are cached
		if allBlobsCached {
			platform := manifest.Platform.OS + "/" + manifest.Platform.Architecture
			if manifest.Platform.Variant != "" {
				platform += "/" + manifest.Platform.Variant
			}
			cachedPlatforms = append(cachedPlatforms, platform)
		}
	}

	return cachedPlatforms, true
}

// formatRepoForDisplay formats a repo name for display, hiding the docker.io prefix
// since it's the default registry
func formatRepoForDisplay(repo string) string {
	// If repo starts with "docker.io/", remove it for cleaner display
	if strings.HasPrefix(repo, "docker.io/") {
		return strings.TrimPrefix(repo, "docker.io/")
	}
	return repo
}

// deleteDockerImage deletes a Docker image from cache using the registry API
// normalizeDockerRepoName normalizes a Docker repo name by adding the library/ prefix
// if it's a single-segment name (e.g., "golang" -> "library/golang")
func normalizeDockerRepoName(repoName string) string {
	// If repo name doesn't contain a slash, it's likely a library image
	if !strings.Contains(repoName, "/") {
		return "library/" + repoName
	}
	return repoName
}

func deleteDockerImage(imageSpec string) {
	// Parse image spec: repo:tag or repo@digest
	var repoName, reference string
	if strings.Contains(imageSpec, "@") {
		// Digest format: repo@sha256:...
		parts := strings.SplitN(imageSpec, "@", 2)
		repoName = parts[0]
		reference = parts[1]
	} else if strings.Contains(imageSpec, ":") {
		// Tag format: repo:tag
		parts := strings.SplitN(imageSpec, ":", 2)
		repoName = parts[0]
		reference = parts[1]
	} else {
		log.Fatalf("Invalid image specification: %s (expected repo:tag or repo@digest)", imageSpec)
	}

	// Normalize repo name (e.g., "golang" -> "library/golang")
	normalizedRepo := normalizeDockerRepoName(repoName)

	// Use registry API to delete the manifest
	// When running inside container, use localhost:443 (SNI routing port)
	// When running from host, port 443 is not available, so we delete directly from filesystem
	// Check if we're in a container by checking if /app exists
	if _, err := os.Stat("/app"); err == nil {
		// Running in Docker - use registry API
		// Use GetRegistryURL to get the correct URL (uses request host if available, or configured port)
		registryURL := util.GetRegistryURL(nil, true)
		manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", registryURL, normalizedRepo, reference)

		req, err := http.NewRequest("DELETE", manifestURL, nil)
		if err != nil {
			log.Fatalf("Failed to create DELETE request: %v", err)
		}

		// Use roxxy's CA certificate for TLS verification
		certsDir := config.GetDefaultCertsDir()
		certPath := filepath.Join(certsDir, "ca-cert.pem")

		var tlsConfig *tls.Config

		// Try to read and use roxxy's CA certificate
		caCertPEM, err := os.ReadFile(certPath)
		if err == nil {
			// Parse PEM block
			block, _ := pem.Decode(caCertPEM)
			if block != nil {
				// Parse certificate
				caCert, err := x509.ParseCertificate(block.Bytes)
				if err == nil {
					// Create cert pool with roxxy's CA
					caCertPool := x509.NewCertPool()
					caCertPool.AddCert(caCert)
					tlsConfig = &tls.Config{
						RootCAs: caCertPool,
					}
				}
			}
		}

		// Fallback to InsecureSkipVerify if CA cert not found (for backwards compatibility)
		// This should only happen if certs haven't been generated yet
		if tlsConfig == nil {
			log.Printf("Warning: CA certificate not found at %s, using InsecureSkipVerify for localhost", certPath)
			log.Printf("  Run 'roxxy certs generate' to create the CA certificate")
			tlsConfig = &tls.Config{
				InsecureSkipVerify: true,
			}
		}

		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
			},
			Timeout: 30 * time.Second,
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Fatalf("Failed to delete image: %v", err)
		}
		defer util.DeferClose(resp.Body)

		switch resp.StatusCode {
		case http.StatusAccepted:
			// Show normalized name if it was different
			if normalizedRepo != repoName {
				fmt.Printf("Successfully deleted image: %s (normalized from %s)\n", normalizedRepo+":"+reference, imageSpec)
			} else {
				fmt.Printf("Successfully deleted image: %s\n", imageSpec)
			}
			return
		case http.StatusNotFound:
			fmt.Printf("Image not found in cache: %s\n", imageSpec)
			os.Exit(1)
		default:
			body, _ := io.ReadAll(resp.Body)
			log.Fatalf("Failed to delete image: %d %s", resp.StatusCode, string(body))
		}
	}

	// Running on host - delete directly from filesystem
	// Get cache directory from environment or use default
	cacheDir := os.Getenv("CACHE_DIR")
	if cacheDir == "" {
		// Try to get from config
		configFile := config.FindConfigFile(false)
		globalConfig, err := config.LoadGlobalConfig(configFile)
		if err == nil {
			cacheDir = globalConfig.CacheDir
		}
	}
	if cacheDir == "" {
		log.Fatalf("Cannot determine cache directory. Set CACHE_DIR environment variable or configure cache_dir in config.yaml")
	}

	manifestDir := filepath.Join(cacheDir, "docker-manifests")

	// Extract registry and repo from normalizedRepo (which might include registry)
	registryID, actualRepoName := extractRegistryAndRepoFromFullName(normalizedRepo)
	safeRepo := util.SafeRepoName(actualRepoName)

	// Try to find the manifest - search more broadly to handle malformed paths
	// Walk through registry directories
	var manifestPath string
	var candidatePaths []string // Collect potential matches for better error messages

	err := filepath.Walk(manifestDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".meta") || strings.HasSuffix(path, ".metadata") {
			return nil
		}

		relPath, err := filepath.Rel(manifestDir, path)
		if err != nil {
			return nil
		}

		parts := strings.Split(relPath, string(filepath.Separator))
		if len(parts) == 3 {
			// Format: <registry>/<repo>/<reference>
			registryPart := parts[0]
			repoPart := parts[1]
			refPart := parts[2]

			// Check if reference matches (tag or digest)
			refMatches := refPart == reference || strings.HasPrefix(refPart, reference)
			if !refMatches {
				return nil
			}

			// Check if this matches the expected path (normal case)
			registryMatch := registryID == "" || registryPart == registryID
			if registryMatch && repoPart == safeRepo {
				manifestPath = path
				return filepath.SkipAll // Found exact match
			}

			// Check for malformed paths where registry is duplicated in repo part
			// Example: ghcr.io/ghcr.io_cloudnative-pg_cloudnative-pg/1.22.0
			// This handles old images cached before path normalization was fixed
			if registryID != "" && registryPart == registryID && strings.HasPrefix(repoPart, registryID+"_") {
				// Repo part has duplicate registry prefix: "ghcr.io_cloudnative-pg_cloudnative-pg"
				// Remove the duplicate registry prefix to get the actual repo
				expectedRepo := strings.TrimPrefix(repoPart, registryID+"_")
				if expectedRepo == safeRepo {
					// Found malformed path - prefer this if we haven't found a normal one
					if manifestPath == "" {
						manifestPath = path
					}
					return filepath.SkipAll
				}
			}

			// Also search more broadly: if repo part contains the expected repo name
			// This helps find malformed paths even if registry doesn't match exactly
			if strings.Contains(repoPart, safeRepo) {
				// Check if this looks like a match by comparing the last part of the repo
				// For "ghcr.io/ghcr.io_cloudnative-pg_cloudnative-pg", we want to match
				// when searching for "ghcr.io/cloudnative-pg/cloudnative-pg"
				repoParts := strings.Split(repoPart, "_")
				expectedParts := strings.Split(safeRepo, "_")
				if len(repoParts) >= len(expectedParts) {
					// Check if the last N parts match
					lastParts := repoParts[len(repoParts)-len(expectedParts):]
					if strings.Join(lastParts, "_") == safeRepo {
						candidatePaths = append(candidatePaths, relPath)
						// If we haven't found a match yet, use this as a candidate
						if manifestPath == "" {
							manifestPath = path
						}
					}
				}
			}
		}
		return nil
	})

	if manifestPath == "" || err != nil {
		if len(candidatePaths) > 0 {
			fmt.Printf("Image not found with exact path, but found similar paths:\n")
			for _, p := range candidatePaths {
				fmt.Printf("  %s\n", p)
			}
			fmt.Printf("\nTry using the exact path shown above, or check for typos in the image name.\n")
		} else {
			fmt.Printf("Image not found in cache: %s\n", imageSpec)
		}
		os.Exit(1)
	}

	if _, err := cache.RemoveManifest(cacheDir, manifestPath); err != nil {
		log.Fatalf("Failed to delete manifest: %v", err)
	}
	fmt.Printf("Successfully deleted image: %s (run 'docker prune-blobs' to reclaim now-unreferenced blob storage)\n", imageSpec)
}

// extractRegistryAndRepoFromFullName extracts registry and repo from a full repo name
// This is used when we have a full image name like "ghcr.io/user/repo"
// Examples:
//
//	"golang" -> ("docker.io", "library/golang")
//	"library/golang" -> ("docker.io", "library/golang")
//	"ghcr.io/cloudnative-pg/cloudnative-pg" -> ("ghcr.io", "cloudnative-pg/cloudnative-pg")
func extractRegistryAndRepoFromFullName(fullRepoName string) (registryID, repoName string) {
	// Check if repo name contains a registry (has a dot and slash, like "ghcr.io/...")
	parts := strings.SplitN(fullRepoName, "/", 2)
	if len(parts) == 2 && strings.Contains(parts[0], ".") {
		// First part looks like a registry (contains a dot)
		registryID = parts[0]
		repoName = parts[1]
		return
	}

	// Default to docker.io
	registryID = "docker.io"
	repoName = fullRepoName

	// Normalize repo name (e.g., "golang" -> "library/golang")
	if !strings.Contains(repoName, "/") {
		repoName = "library/" + repoName
	}

	return
}

// showDockerImageLayers shows the layers for a specific Docker image
func showDockerImageLayers(cacheDir string, imageName string, platform string) {
	// Read manifest directly from filesystem storage
	manifestDir := filepath.Join(cacheDir, "docker-manifests")

	// Parse image name and tag
	parts := strings.Split(imageName, ":")
	repo := parts[0]
	tag := "latest"
	if len(parts) > 1 {
		tag = parts[1]
	}

	// Extract registry and repo (same logic as tarball.go extractRegistryAndRepo)
	registryID, actualRepoName := extractRegistryAndRepoFromFullName(repo)

	// Sanitize repo name for filesystem (replace / with _)
	safeRepo := util.SafeRepoName(actualRepoName)

	// Construct manifest path: docker-manifests/<registry>/<safeRepo>/<tag>
	manifestPath := filepath.Join(manifestDir, registryID, safeRepo, tag)

	// Read manifest from filesystem
	bodyBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		log.Fatalf("Failed to read manifest for %s:%s: %v", repo, tag, err)
	}

	// Calculate digest
	hash := sha256.Sum256(bodyBytes)
	digest := "sha256:" + hex.EncodeToString(hash[:])

	fmt.Fprintf(os.Stderr, "DEBUG: Index manifest digest: %s\n", digest)
	fmt.Fprintf(os.Stderr, "DEBUG: Index manifest size: %d bytes\n", len(bodyBytes))

	// First check if this is a manifest index (multi-platform)
	var indexCheck struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
	}

	if err := json.Unmarshal(bodyBytes, &indexCheck); err == nil {
		// If it's a manifest index, find the matching platform manifest
		if indexCheck.MediaType == "application/vnd.oci.image.index.v1+json" ||
			indexCheck.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
			if len(indexCheck.Manifests) > 0 {
				// Debug: show ALL manifests in index (including attestations)
				fmt.Fprintf(os.Stderr, "DEBUG: Index contains %d total manifest(s):\n", len(indexCheck.Manifests))
				for i, m := range indexCheck.Manifests {
					osStr := m.Platform.OS
					archStr := m.Platform.Architecture
					if osStr == "" {
						osStr = "(empty)"
					}
					if archStr == "" {
						archStr = "(empty)"
					}
					fmt.Fprintf(os.Stderr, "  [%d] %s/%s -> %s\n", i, osStr, archStr, m.Digest)
				}

				// Collect available platforms for display
				var availablePlatforms []string
				for _, m := range indexCheck.Manifests {
					// Skip attestations/SBOMs (unknown platform)
					if m.Platform.OS == "unknown" || m.Platform.Architecture == "unknown" {
						continue
					}
					if m.Platform.OS == "" || m.Platform.Architecture == "" {
						continue
					}
					platformStr := fmt.Sprintf("%s/%s", m.Platform.OS, m.Platform.Architecture)
					availablePlatforms = append(availablePlatforms, platformStr)
				}

				// Parse platform if specified (format: "os/arch")
				var targetOS, targetArch string
				if platform != "" {
					platformParts := strings.Split(platform, "/")
					if len(platformParts) == 2 {
						targetOS = platformParts[0]
						targetArch = platformParts[1]
					}
				}

				// If no platform specified, try to match current CPU architecture
				if targetOS == "" && targetArch == "" {
					// Map Go's runtime architecture to Docker's architecture names
					currentOS := runtime.GOOS
					currentArch := runtime.GOARCH

					// Docker uses "linux" for Linux, "darwin" for macOS, "windows" for Windows
					// For architecture, Docker uses: amd64, arm64, arm, etc.
					// Go uses: amd64, arm64, arm, 386, etc.
					if currentOS == "darwin" {
						currentOS = "linux" // Docker images on macOS are typically linux-based
					}
					// Architecture names are mostly the same, but handle edge cases
					if currentArch == "386" {
						currentArch = "i386"
					}

					targetOS = currentOS
					targetArch = currentArch
				}

				// Find matching platform manifest
				var selectedDigest string
				var selectedPlatform string

				// Debug: show all platform manifests in the index
				fmt.Fprintf(os.Stderr, "DEBUG: Index contains %d platform manifest(s):\n", len(indexCheck.Manifests))
				for i, m := range indexCheck.Manifests {
					if m.Platform.OS != "unknown" && m.Platform.Architecture != "unknown" && m.Platform.OS != "" && m.Platform.Architecture != "" {
						fmt.Fprintf(os.Stderr, "  [%d] %s/%s -> %s\n", i, m.Platform.OS, m.Platform.Architecture, m.Digest)
					}
				}

				for _, m := range indexCheck.Manifests {
					// Skip attestations/SBOMs (unknown platform)
					if m.Platform.OS == "unknown" || m.Platform.Architecture == "unknown" {
						continue
					}
					if m.Platform.OS == "" || m.Platform.Architecture == "" {
						continue
					}

					// Try to match the target platform
					if targetOS != "" && targetArch != "" {
						if m.Platform.OS == targetOS && m.Platform.Architecture == targetArch {
							selectedDigest = m.Digest
							selectedPlatform = fmt.Sprintf("%s/%s", m.Platform.OS, m.Platform.Architecture)
							fmt.Fprintf(os.Stderr, "DEBUG: Selected platform manifest: %s/%s -> %s\n", targetOS, targetArch, selectedDigest)
							break
						}
					}
				}

				// Fallback: prefer linux/amd64, then first available
				if selectedDigest == "" {
					for _, m := range indexCheck.Manifests {
						if m.Platform.Architecture == "amd64" && m.Platform.OS == "linux" {
							selectedDigest = m.Digest
							selectedPlatform = fmt.Sprintf("%s/%s", m.Platform.OS, m.Platform.Architecture)
							break
						}
					}
				}
				if selectedDigest == "" && len(indexCheck.Manifests) > 0 {
					// Get first non-attestation manifest
					for _, m := range indexCheck.Manifests {
						if m.Platform.OS != "unknown" && m.Platform.Architecture != "unknown" && m.Platform.OS != "" && m.Platform.Architecture != "" {
							selectedDigest = m.Digest
							selectedPlatform = fmt.Sprintf("%s/%s", m.Platform.OS, m.Platform.Architecture)
							break
						}
					}
				}

				if selectedDigest != "" {
					// Show available platforms
					if len(availablePlatforms) > 1 {
						fmt.Printf("Multi-platform image, available architectures: %s\n", strings.Join(availablePlatforms, ", "))
						fmt.Printf("Showing %s manifest (use --platform to select a different architecture)\n\n", selectedPlatform)
					} else {
						fmt.Printf("Multi-platform image, showing %s manifest\n\n", selectedPlatform)
					}

					// Debug: show which platform manifest digest we're about to read
					fmt.Fprintf(os.Stderr, "DEBUG: Reading platform manifest for %s/%s: %s\n", selectedPlatform, repo, selectedDigest)

					// Read the platform-specific manifest from filesystem
					// Path: docker-manifests/<registry>/<safeRepo>/<digest>
					platformManifestPath := filepath.Join(manifestDir, registryID, safeRepo, selectedDigest)
					bodyBytes, err = os.ReadFile(platformManifestPath)
					if err != nil {
						log.Fatalf("Failed to read platform manifest at %s: %v", platformManifestPath, err)
					}

					// Debug: show file size and first few bytes to verify we got the right file
					fmt.Fprintf(os.Stderr, "DEBUG: Read platform manifest: %d bytes from %s\n", len(bodyBytes), platformManifestPath)

					// Verify the digest of the platform manifest matches what we expect
					platformHash := sha256.Sum256(bodyBytes)
					platformDigest := "sha256:" + hex.EncodeToString(platformHash[:])
					fmt.Fprintf(os.Stderr, "DEBUG: Platform manifest digest check:\n")
					fmt.Fprintf(os.Stderr, "  Expected (from index): %s\n", selectedDigest)
					fmt.Fprintf(os.Stderr, "  Actual (file content):  %s\n", platformDigest)

					if platformDigest != selectedDigest {
						fmt.Fprintf(os.Stderr, "ERROR: Platform manifest digest mismatch!\n")
						fmt.Fprintf(os.Stderr, "  File path: %s\n", platformManifestPath)
						fmt.Fprintf(os.Stderr, "\nThis indicates the platform-specific manifest stored in roxxy's cache\n")
						fmt.Fprintf(os.Stderr, "does not match what the index manifest references. This can happen if:\n")
						fmt.Fprintf(os.Stderr, "  1. The manifest was stored incorrectly during push\n")
						fmt.Fprintf(os.Stderr, "  2. An old/corrupted manifest exists at that path\n")
						fmt.Fprintf(os.Stderr, "  3. The manifest was overwritten with incorrect data\n")
						fmt.Fprintf(os.Stderr, "\nThe layer digests shown below may be incorrect.\n\n")
					} else {
						// Digest matches - parse and show first few layer digests for comparison
						var platformManifestCheck struct {
							MediaType string `json:"mediaType"`
							Config    struct {
								Digest string `json:"digest"`
							} `json:"config"`
							Layers []struct {
								Digest string `json:"digest"`
							} `json:"layers"`
						}
						if err := json.Unmarshal(bodyBytes, &platformManifestCheck); err == nil {
							fmt.Fprintf(os.Stderr, "DEBUG: Platform manifest parsed successfully:\n")
							fmt.Fprintf(os.Stderr, "  MediaType: %s\n", platformManifestCheck.MediaType)
							fmt.Fprintf(os.Stderr, "  Config: %s\n", platformManifestCheck.Config.Digest)
							fmt.Fprintf(os.Stderr, "  Layers: %d\n", len(platformManifestCheck.Layers))
							if len(platformManifestCheck.Layers) > 0 {
								fmt.Fprintf(os.Stderr, "  First layer: %s\n", platformManifestCheck.Layers[0].Digest)
								if len(platformManifestCheck.Layers) > 1 {
									fmt.Fprintf(os.Stderr, "  Second layer: %s\n", platformManifestCheck.Layers[1].Digest)
								}
							}
							// Check if this looks like a platform manifest (has layers) vs an index (has manifests)
							if len(platformManifestCheck.Layers) == 0 && platformManifestCheck.MediaType != "" {
								// This might be an index, not a platform manifest
								fmt.Fprintf(os.Stderr, "WARNING: Platform manifest path contains an index manifest instead!\n")
								fmt.Fprintf(os.Stderr, "  MediaType: %s\n", platformManifestCheck.MediaType)
								fmt.Fprintf(os.Stderr, "  This explains why layer digests don't match.\n\n")
							}
						} else {
							fmt.Fprintf(os.Stderr, "DEBUG: Failed to parse platform manifest JSON: %v\n", err)
						}
					}
				} else if targetOS != "" && targetArch != "" {
					// Platform was specified but not found
					log.Fatalf("Platform %s/%s not found in index. Available platforms: %s", targetOS, targetArch, strings.Join(availablePlatforms, ", "))
				}
			}
		}
	}

	var manifest struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		Config        struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"config"`
		Layers []struct {
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
			MediaType string `json:"mediaType"`
		} `json:"layers"`
	}

	if err := json.Unmarshal(bodyBytes, &manifest); err != nil {
		log.Fatalf("Failed to parse manifest: %v", err)
	}

	fmt.Printf("Image: %s:%s\n", repo, tag)
	fmt.Printf("Manifest: %s\n", digest)
	fmt.Printf("Schema: v%d (%s)\n\n", manifest.SchemaVersion, manifest.MediaType)

	fmt.Printf("Config:\n")
	fmt.Printf("  %s (%s)\n\n", manifest.Config.Digest, formatSize(manifest.Config.Size))

	fmt.Printf("Layers (%d):\n", len(manifest.Layers))
	totalSize := int64(0)
	for i, layer := range manifest.Layers {
		fmt.Printf("  %2d. %s (%s)\n", i+1, layer.Digest, formatSize(layer.Size))
		totalSize += layer.Size
	}

	fmt.Printf("\nTotal layer size: %s\n", formatSize(totalSize))
}

// loadIndexDataByURL loads index packages and creates a map by URL for lookup
func loadIndexDataByURL(cacheDir string, filterType string) map[string]packages.IndexPackageData {
	indexDataByURL := make(map[string]packages.IndexPackageData)

	// Load index data for types we're displaying
	// Note: Go index files (.info, /list) don't contain Description/License, so skip them
	typesToLoad := []packages.Type{packages.TypeAPK, packages.TypeAPT, packages.TypeNPM, packages.TypePyPI}
	if filterType != "" && filterType != "go" {
		typesToLoad = []packages.Type{packages.Type(filterType)}
	} else if filterType == "go" {
		// Go index files don't have useful metadata, return empty map
		return indexDataByURL
	}

	indexDir := filepath.Join(cacheDir, "index")
	indexDB := index.NewIndexDB(indexDir)

	for _, pmType := range typesToLoad {
		pkgs, err := indexDB.GetPackages(pmType)
		if err == nil {
			for _, pkg := range pkgs {
				// Use URL as the key for lookup
				if pkg.URL != "" {
					indexDataByURL[pkg.URL] = packages.IndexPackageData{
						Description:  pkg.Description,
						License:      pkg.License,
						Name:         pkg.Name,
						Version:      pkg.Version,
						Architecture: pkg.Architecture,
					}
				}
			}
		}
	}

	return indexDataByURL
}

// listDockerImagesFromTarball lists Docker images from a tarball
func listDockerImagesFromTarball(tarballPath string, showAll bool, debug bool) {
	reader, err := OpenTarballWithDebug(tarballPath, debug)
	if err != nil {
		log.Fatalf("Failed to open tarball: %v", err)
	}
	defer util.DeferClose(reader)

	// Get all manifest paths (streaming)
	manifestPaths, err := reader.ListManifests()
	if err != nil {
		log.Fatalf("Failed to list manifests: %v", err)
	}

	// Build repository map from manifest paths
	repos := make(map[string]map[string]bool) // repo -> set of tags

	for _, path := range manifestPaths {
		// Path format: docker-manifests/<registry>/<safe-repo>/<reference>
		relPath := strings.TrimPrefix(path, "docker-manifests/")
		parts := strings.Split(relPath, "/")
		if len(parts) != 3 {
			continue
		}

		registryID := parts[0]
		safeRepo := parts[1]
		reference := parts[2]

		// Skip digest-only manifests unless showAll is true
		if strings.HasPrefix(reference, "sha256:") {
			if !showAll {
				continue
			}
		}

		// Convert safe-repo back to normal format
		repoName := util.UnsafeRepoName(safeRepo)
		// Include registry in repo key: <registry>/<repo>
		repo := registryID + "/" + repoName

		if repos[repo] == nil {
			repos[repo] = make(map[string]bool)
		}

		// Only add tags (not digests) to the tag list
		if !strings.HasPrefix(reference, "sha256:") {
			repos[repo][reference] = true
		}
	}

	if len(repos) == 0 {
		fmt.Println("No images found in tarball")
		return
	}

	fmt.Printf("Images in tarball:\n\n")

	// Sort repos for consistent output
	repoNames := make([]string, 0, len(repos))
	for repo := range repos {
		repoNames = append(repoNames, repo)
	}
	sort.Strings(repoNames)

	for _, repo := range repoNames {
		tags := repos[repo]
		if len(tags) == 0 && !showAll {
			continue
		}

		fmt.Printf("%s\n", repo)
		if len(tags) > 0 {
			tagList := make([]string, 0, len(tags))
			for tag := range tags {
				tagList = append(tagList, tag)
			}
			sort.Strings(tagList)
			for _, tag := range tagList {
				fmt.Printf("  └─ %s\n", tag)
			}
		}
		fmt.Println()
	}
}

// showDockerImageLayersFromTarball shows layers for an image from a tarball
func showDockerImageLayersFromTarball(tarballPath, imageName, platform string, debug bool) {
	reader, err := OpenTarballWithDebug(tarballPath, debug)
	if err != nil {
		log.Fatalf("Failed to open tarball: %v", err)
	}
	defer util.DeferClose(reader)

	// Parse image name: repo:tag or repo@digest
	var repoName, reference string
	if strings.Contains(imageName, "@") {
		parts := strings.SplitN(imageName, "@", 2)
		repoName = parts[0]
		reference = parts[1]
	} else if strings.Contains(imageName, ":") {
		parts := strings.SplitN(imageName, ":", 2)
		repoName = parts[0]
		reference = parts[1]
	} else {
		log.Fatalf("Invalid image specification: %s (expected repo:tag or repo@digest)", imageName)
	}

	// Find manifest and get blob digests
	var manifestData []byte
	var err2 error
	if strings.HasPrefix(reference, "sha256:") {
		manifestData, _, _, err2 = reader.FindManifestByDigest(repoName, reference)
	} else {
		manifestData, _, _, err2 = reader.FindManifestByRepoTag(repoName, reference, platform)
	}
	if err2 != nil {
		log.Fatalf("Failed to find manifest: %v", err2)
	}

	// Parse manifest to get config and layer digests separately
	configDigest, layerDigests, err := ExtractBlobDigestsFromManifest(manifestData)
	if err != nil {
		log.Fatalf("Failed to parse manifest: %v", err)
	}

	fmt.Printf("Image: %s\n", imageName)
	fmt.Printf("Config: %s\n", configDigest)
	fmt.Printf("Layers (%d):\n", len(layerDigests))
	for i, digest := range layerDigests {
		// Check if blob exists in tarball
		exists, err := reader.HasBlob(digest)
		if err != nil {
			log.Printf("Warning: Failed to check blob %s: %v", digest, err)
			exists = false
		}
		status := "✓"
		if !exists {
			status = "✗"
		}
		fmt.Printf("  %d. %s %s\n", i+1, status, digest)
	}
}
