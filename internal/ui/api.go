package ui

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/cli"
	"github.com/Delphos-Labs/roxxy/internal/packages"
	"github.com/Delphos-Labs/roxxy/internal/packages/index"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// countingWriter wraps an io.Writer and tallies bytes successfully
// written. Used by handleExport to learn the precise current tar-file
// position without trusting hand-rolled `currentOffset += 512` math
// (which silently miscounts under PAX extended headers — see comment
// in handleExport for details).
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// normalizeDebianVersion strips the epoch prefix from a Debian package version.
// Debian versions can have an epoch prefix like "1:3.6.0-7.1" where "1:" is the epoch.
// The .deb filename may omit the epoch, but the Packages file includes it.
// Returns the version without the epoch prefix for comparison.
func normalizeDebianVersion(version string) string {
	// Debian epoch format: "epoch:version" where epoch is a number
	// Example: "1:3.6.0-7.1" -> "3.6.0-7.1"
	parts := strings.SplitN(version, ":", 2)
	if len(parts) == 2 {
		// Check if first part is numeric (epoch)
		if len(parts[0]) > 0 {
			hasDigits := false
			for _, r := range parts[0] {
				if r >= '0' && r <= '9' {
					hasDigits = true
					break
				}
			}
			if hasDigits {
				// This looks like an epoch, return the version part
				return parts[1]
			}
		}
	}
	// No epoch found, return as-is
	return version
}

// API provides REST API endpoints for the web UI
type API struct {
	cacheDir     string
	certsDir     string
	statsTracker *cache.StatsTracker
	indexDB      IndexDBInterface // Optional index database for parsing index files
}

// IndexDBInterface is an interface for the index database
type IndexDBInterface interface {
	ParseCachedIndexFiles(cacheDir string, pmType packages.Type) (int, error)
	GetPackages(pmType packages.Type) ([]index.IndexPackage, error)
}

// NewAPI creates a new API handler
func NewAPI(cacheDir string, statsTracker *cache.StatsTracker, indexDB interface{}) *API {
	var db IndexDBInterface
	if idx, ok := indexDB.(IndexDBInterface); ok {
		db = idx
	}
	return &API{
		cacheDir:     cacheDir,
		statsTracker: statsTracker,
		indexDB:      db,
	}
}

// SetCertsDir sets the certificates directory for the API
func (a *API) SetCertsDir(certsDir string) {
	a.certsDir = certsDir
}

// RegisterRoutes registers API routes with the given mux
func (a *API) RegisterRoutes(mux *http.ServeMux) {
	// Stats endpoints
	mux.HandleFunc("/api/v1/stats/summary", a.corsMiddleware(a.handleStatsSummary))
	mux.HandleFunc("/api/v1/stats/access", a.corsMiddleware(a.handleAccessStats))

	// Build info endpoint
	mux.HandleFunc("/api/v1/buildinfo", a.corsMiddleware(a.handleBuildInfo))

	// Package endpoints (register more specific routes first)
	mux.HandleFunc("/api/v1/packages/go/", a.corsMiddleware(a.handleGoMetadata))
	mux.HandleFunc("/api/v1/packages/apk/", a.corsMiddleware(a.handleAPKMetadata))
	mux.HandleFunc("/api/v1/packages/pypi/", a.corsMiddleware(a.handlePyPIMetadata))
	mux.HandleFunc("/api/v1/packages/npm/", a.corsMiddleware(a.handleNPMTarballMetadata))
	mux.HandleFunc("/api/v1/packages/groups", a.corsMiddleware(a.handlePackageGroups))
	mux.HandleFunc("/api/v1/packages", a.corsMiddleware(a.handlePackages))
	mux.HandleFunc("/api/v1/packages/", a.corsMiddleware(a.handlePackageDetail))

	// Docker registry endpoints
	mux.HandleFunc("/api/v1/docker/repositories", a.corsMiddleware(a.handleDockerRepositories))
	mux.HandleFunc("/api/v1/docker/repositories/", a.corsMiddleware(a.handleDockerRepositoryDetail))

	// URL cache endpoints
	mux.HandleFunc("/api/v1/cache/url", a.corsMiddleware(a.handleURLCache))

	// Management endpoints
	mux.HandleFunc("/api/v1/cache/flush", a.corsMiddleware(a.handleFlushCache))
	mux.HandleFunc("/api/v1/index/parse", a.corsMiddleware(a.handleParseIndexes))

	// Export endpoints (register more specific routes first)
	mux.HandleFunc("/api/v1/export/docker", a.handleExportDocker)
	mux.HandleFunc("/api/v1/export/packages", a.handleExportPackages)
	mux.HandleFunc("/api/v1/export/raw", a.handleExportRaw)
	mux.HandleFunc("/api/v1/export", a.handleExportFull)

	// Certificate endpoint
	mux.HandleFunc("/api/v1/certs/ca", a.corsMiddleware(a.handleCACertificate))
}

// corsMiddleware adds CORS headers to API responses
func (a *API) corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "3600")

		// Handle preflight requests
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Call the next handler
		next(w, r)
	}
}

// GetBuildTime returns the build time (set from main package)
var GetBuildTime = func() string { return "unknown" }

// SetBuildTime sets the build time (called from main package)
func SetBuildTime(buildTime string) {
	GetBuildTime = func() string { return buildTime }
}

func (a *API) handleBuildInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, map[string]interface{}{
		"build_time": GetBuildTime(),
	})
}

// StatsSummaryResponse represents the dashboard overview
type StatsSummaryResponse struct {
	TotalCacheSize     int64          `json:"total_cache_size"`
	PackageCount       int            `json:"package_count"`
	PackageCountByType map[string]int `json:"package_count_by_type"`
	DockerImageCount   int            `json:"docker_image_count"`
	TotalTimeSaved     string         `json:"total_time_saved"`
	CacheHitRate       float64        `json:"cache_hit_rate"`
	URLCacheCount      int            `json:"url_cache_count"`
}

func (a *API) handleStatsSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Return early with defaults if cacheDir is not set
	if a.cacheDir == "" {
		log.Printf("Warning: cacheDir is empty in handleStatsSummary")
		response := StatsSummaryResponse{
			TotalCacheSize:     0,
			PackageCount:       0,
			PackageCountByType: make(map[string]int),
			DockerImageCount:   0,
			TotalTimeSaved:     "0s",
			CacheHitRate:       0,
			URLCacheCount:      0,
		}
		writeJSON(w, response)
		return
	}

	cfg := cli.Config{
		CacheDir:     a.cacheDir,
		StatsTracker: a.statsTracker,
	}

	// Collect package statistics (optimized - only count, don't load full details)
	packageCountByType := make(map[string]int)
	totalPackageSize := int64(0)

	// Fast path: just count cached packages without loading index metadata
	cacheRoot := filepath.Join(a.cacheDir, "packages")
	pmTypes := []packages.Type{packages.TypeAPK, packages.TypeAPT, packages.TypeNPM, packages.TypePyPI, packages.TypeGo}

	for _, pmType := range pmTypes {
		typeDir := filepath.Join(cacheRoot, string(pmType))
		if _, err := os.Stat(typeDir); os.IsNotExist(err) {
			continue
		}

		// Quick count by scanning directory structure
		count, size, err := a.countPackagesByType(typeDir, pmType)
		if err != nil {
			log.Printf("Error counting packages for type %s: %v", pmType, err)
			continue
		}
		packageCountByType[string(pmType)] = count
		totalPackageSize += size
	}

	// Collect URL cache statistics
	urlEntries, err := listCacheEntries(cfg, "")
	if err != nil {
		log.Printf("Error listing URL cache in handleStatsSummary: %v", err)
		urlEntries = []cli.CacheEntry{}
	}

	totalURLCacheSize := int64(0)
	for _, entry := range urlEntries {
		totalURLCacheSize += entry.Size
	}

	// Get Docker image count
	dockerCount := a.getDockerImageCount()

	// Calculate time saved and cache hit rate by scanning metadata files
	// This is reasonably fast since we're just reading small .meta files
	var totalTimeSaved time.Duration
	var totalAccessCount int
	var cacheHits int

	if a.statsTracker != nil {
		// Get all stats from the tracker
		statsMap := a.statsTracker.GetAllStats()

		// Build a map of stats by key for quick lookup
		// Stats keys are like "pypi:requests@2.31.0"
		statsByKey := make(map[string]*cache.Stats)
		for key, stat := range statsMap {
			if stat.AccessCount > 1 {
				statsByKey[key] = stat
			}
		}

		// Quick scan: read metadata files to get fetch durations
		// We'll match metadata with stats by constructing the key from package info
		for _, pmType := range pmTypes {
			typeDir := filepath.Join(cacheRoot, string(pmType))
			if _, err := os.Stat(typeDir); os.IsNotExist(err) {
				continue
			}

			// Walk metadata files (much faster than walking all files)
			// Use WalkDir instead of Walk for better performance (no stat calls)
			if err := filepath.WalkDir(typeDir, func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}

				if !strings.HasSuffix(path, ".meta") {
					return nil
				}

				// Read metadata
				metaData, err := os.ReadFile(path)
				if err != nil {
					return nil
				}

				var metadata struct {
					Type          string        `json:"type"`
					Name          string        `json:"name"`
					Version       string        `json:"version"`
					Architecture  string        `json:"architecture"`
					FetchDuration time.Duration `json:"fetch_duration"`
					AccessCount   int           `json:"access_count"`
					IsIndex       bool          `json:"is_index"`
				}
				if err := json.Unmarshal(metaData, &metadata); err != nil {
					return nil
				}

				// Skip index files
				if metadata.IsIndex {
					return nil
				}

				// Build stats key to match with stats tracker
				// Use strings.Builder for better performance in loops
				var keyBuilder strings.Builder
				keyBuilder.WriteString(metadata.Type)
				keyBuilder.WriteString(":")
				keyBuilder.WriteString(metadata.Name)
				keyBuilder.WriteString("@")
				keyBuilder.WriteString(metadata.Version)
				if metadata.Architecture != "" {
					keyBuilder.WriteString(":")
					keyBuilder.WriteString(metadata.Architecture)
				}
				key := keyBuilder.String()

				// Check if we have stats for this package
				if stat, exists := statsByKey[key]; exists {
					if stat.AccessCount > 1 && metadata.FetchDuration > 0 {
						// Time saved = fetch duration * (access count - 1)
						// The -1 is because the first access was the fetch itself
						timeSaved := metadata.FetchDuration * time.Duration(stat.AccessCount-1)
						totalTimeSaved += timeSaved
						cacheHits += stat.AccessCount - 1
					}
					totalAccessCount += stat.AccessCount
				} else if metadata.AccessCount > 1 && metadata.FetchDuration > 0 {
					// Fallback: use access count from metadata if stats tracker doesn't have it
					timeSaved := metadata.FetchDuration * time.Duration(metadata.AccessCount-1)
					totalTimeSaved += timeSaved
					cacheHits += metadata.AccessCount - 1
					totalAccessCount += metadata.AccessCount
				}

				return nil
			}); err != nil {
				log.Printf("Error walking directory %s: %v", typeDir, err)
			}
		}
	}

	// Calculate cache hit rate
	cacheHitRate := 0.0
	if totalAccessCount > 0 && cacheHits > 0 {
		cacheHitRate = float64(cacheHits) / float64(totalAccessCount) * 100
	}

	// Calculate total package count
	totalPackageCount := 0
	for _, count := range packageCountByType {
		totalPackageCount += count
	}

	response := StatsSummaryResponse{
		TotalCacheSize:     totalPackageSize + totalURLCacheSize,
		PackageCount:       totalPackageCount,
		PackageCountByType: packageCountByType,
		DockerImageCount:   dockerCount,
		TotalTimeSaved:     formatDuration(totalTimeSaved),
		CacheHitRate:       cacheHitRate,
		URLCacheCount:      len(urlEntries),
	}

	// Ensure package_count_by_type is never nil
	if response.PackageCountByType == nil {
		response.PackageCountByType = make(map[string]int)
	}

	writeJSON(w, response)
}

// PackageEntry represents a package in the API response
type PackageEntry struct {
	ID            string    `json:"id"`
	Type          string    `json:"type"`
	Name          string    `json:"name"`
	Version       string    `json:"version"`
	Architecture  string    `json:"architecture"`
	Group         string    `json:"group,omitempty"` // Component/group (e.g., "main", "security")
	Size          int64     `json:"size"`
	CachedAt      time.Time `json:"cached_at"`
	FetchDuration int64     `json:"fetch_duration_ms"`
	AccessCount   int       `json:"access_count"`
	LastAccessed  time.Time `json:"last_accessed"`
	Cached        bool      `json:"cached"`       // True if the actual package file is cached
	Description   string    `json:"description"`  // Package description (from index)
	Dependencies  []string  `json:"dependencies"` // Package dependencies (from index)
	URL           string    `json:"url"`          // Package download URL (from index)
}

func (a *API) handlePackages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse query parameters
	typeFilter := r.URL.Query().Get("type")
	search := r.URL.Query().Get("search")
	sortBy := r.URL.Query().Get("sort")
	showUncached := r.URL.Query().Get("show_uncached") == "true"

	entries, err := a.listPackages(typeFilter, search, showUncached)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error listing packages: %v", err), http.StatusInternalServerError)
		return
	}

	// Sort entries - default to "name" if not specified
	if sortBy == "" {
		sortBy = "name"
	}
	entries = sortPackages(entries, sortBy)

	// Return all packages (no pagination for hierarchical tree view)
	response := map[string]interface{}{
		"packages": entries,
		"total":    len(entries),
	}

	writeJSON(w, response)
}

// isGoSubPackage returns true if candidate is a sub-package of mainModule
// A sub-package is any package whose name starts with mainModule + "/"
// e.g., "github.com/bytedance/sonic/loader" is a sub-package of "github.com/bytedance/sonic"
func isGoSubPackage(mainModule, candidate string) bool {
	if mainModule == "" || candidate == "" {
		return false
	}
	// A sub-package must start with mainModule + "/"
	prefix := mainModule + "/"
	return strings.HasPrefix(candidate, prefix)
}

// findGoMainModules returns a set of main module names (packages that are not sub-packages of any other)
func findGoMainModules(entries []PackageEntry) map[string]bool {
	mainModules := make(map[string]bool)

	// First pass: collect all Go package names
	goPackages := []string{}
	for _, entry := range entries {
		if entry.Type == string(packages.TypeGo) && entry.Name != "" {
			goPackages = append(goPackages, entry.Name)
		}
	}

	// Second pass: for each package, check if it's a sub-package of any other
	for _, pkgName := range goPackages {
		isSubPackage := false
		for _, otherPkgName := range goPackages {
			if pkgName != otherPkgName && isGoSubPackage(otherPkgName, pkgName) {
				isSubPackage = true
				break
			}
		}
		if !isSubPackage {
			mainModules[pkgName] = true
		}
	}

	return mainModules
}

func (a *API) handlePackageGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	typeFilter := r.URL.Query().Get("type")
	groupFilter := r.URL.Query().Get("group") // e.g., "debian:bookworm" or "debian:bookworm:main"
	showUncached := r.URL.Query().Get("show_uncached") == "true"
	search := r.URL.Query().Get("search")

	// If group filter is provided, return packages for that group
	if groupFilter != "" {
		entries, err := a.listPackages(typeFilter, search, showUncached)
		if err != nil {
			http.Error(w, fmt.Sprintf("Error listing packages: %v", err), http.StatusInternalServerError)
			return
		}

		// Filter by group
		var filtered []PackageEntry

		// For Go packages, find main modules (exclude sub-packages)
		mainModules := findGoMainModules(entries)

		for _, entry := range entries {
			// Normalize empty group to "default" for comparison
			entryGroup := entry.Group
			if entryGroup == "" {
				entryGroup = "default"
			}

			// Check group match FIRST - this ensures packages matching the group are included
			// even if they would normally be filtered as sub-packages
			// For Go: filter "github.com/microsoft" should match "github.com/microsoft/go-winio"
			// For APT: filter "debian:bookworm" should match "debian:bookworm:main"
			groupMatches := false

			// Determine separator based on package type
			separator := ":"
			if entry.Type == string(packages.TypeGo) {
				separator = "/"
			}

			if strings.Contains(entryGroup, separator) {
				// Entry has hierarchical group - check if it starts with filter
				// If filter also has separator, check for exact prefix match
				// If filter has no separator, check if entry starts with filter + separator
				if strings.Contains(groupFilter, separator) {
					// Both hierarchical: check if entry starts with filter (with separator)
					// e.g., "github.com/microsoft/go-winio" starts with "github.com/microsoft/"
					// e.g., "debian:bookworm:main" starts with "debian:bookworm:"
					if strings.HasPrefix(entryGroup, groupFilter+separator) || entryGroup == groupFilter {
						groupMatches = true
					}
				} else {
					// Filter is flat but entry is hierarchical: check if entry starts with filter + separator
					// e.g., "gopkg.in/yaml.v3" starts with "gopkg.in/"
					// e.g., "debian:bookworm:main" starts with "debian:"
					if strings.HasPrefix(entryGroup, groupFilter+separator) {
						groupMatches = true
					}
				}
			} else {
				// Both flat: exact match
				groupMatches = entryGroup == groupFilter
			}

			// If group matches, include it (even if it's a sub-package, because it's explicitly requested)
			if groupMatches {
				filtered = append(filtered, entry)
				continue
			}

			// If group doesn't match, apply sub-package filtering for Go packages
			// For Go packages, skip sub-packages (they should only appear in metadata)
			if entry.Type == string(packages.TypeGo) && entry.Name != "" {
				isSubPackage := false
				for mainModule := range mainModules {
					if isGoSubPackage(mainModule, entry.Name) {
						isSubPackage = true
						break
					}
				}
				if isSubPackage {
					continue
				}
			}

			// If we get here, the entry doesn't match the group filter, so skip it
			continue
		}

		// Sort filtered packages by name
		filtered = sortPackages(filtered, "name")

		log.Printf("[api] Filtered packages for group '%s': %d packages (from %d total)", groupFilter, len(filtered), len(entries))
		writeJSON(w, map[string]interface{}{
			"packages": filtered,
			"total":    len(filtered),
			"group":    groupFilter,
		})
		return
	}

	// Otherwise, return group hierarchy structure
	groups, err := a.getPackageGroups(typeFilter, showUncached)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error getting groups: %v", err), http.StatusInternalServerError)
		return
	}

	// Calculate total package count from groups structure
	// This helper function recursively sums counts from the groups structure
	var aggregateCountsFromGroups func(level map[string]interface{}) int
	aggregateCountsFromGroups = func(level map[string]interface{}) int {
		totalCount := 0
		for _, value := range level {
			if valueMap, ok := value.(map[string]interface{}); ok {
				// Check if this node has the new structure (groups/packages)
				packagesMap, hasPackages := valueMap["packages"].(map[string]interface{})
				groupsMap, hasGroups := valueMap["groups"].(map[string]interface{})

				if hasPackages || hasGroups {
					// New structure: aggregate from packages and groups
					packageCount := 0
					if hasPackages {
						for _, pkgValue := range packagesMap {
							if pkgMap, ok := pkgValue.(map[string]interface{}); ok {
								// Check if this package has sub-packages
								if subPackagesMap, hasSubPackages := pkgMap["packages"].(map[string]interface{}); hasSubPackages && len(subPackagesMap) > 0 {
									// Package has sub-packages - recursively count them
									subCount := aggregateCountsFromGroups(subPackagesMap)
									// The package count should include both itself and its sub-packages
									if pkgCount, ok := pkgMap["count"].(int); ok {
										packageCount += pkgCount + subCount
									} else {
										packageCount += subCount
									}
								} else {
									// Regular package without sub-packages
									if count, ok := pkgMap["count"].(int); ok {
										packageCount += count
									}
								}
							}
						}
					}
					groupCount := 0
					if hasGroups {
						groupCount = aggregateCountsFromGroups(groupsMap)
					}
					totalCount += packageCount + groupCount
				} else {
					// Old structure: check if this is a leaf node (package with count)
					if count, hasCount := valueMap["count"].(int); hasCount {
						// Check if this is a leaf (only count) or intermediate (has other keys)
						if len(valueMap) == 1 {
							// Leaf node: package with count only
							totalCount += count
						} else {
							// Intermediate node: recursively aggregate
							subCount := aggregateCountsFromGroups(valueMap)
							if count > subCount {
								subCount = count
							}
							totalCount += subCount
						}
					} else {
						// No count yet, recursively aggregate
						subCount := aggregateCountsFromGroups(valueMap)
						totalCount += subCount
					}
				}
			}
		}
		return totalCount
	}

	totalCount := 0
	for _, typeData := range groups {
		if typeMap, ok := typeData.(map[string]interface{}); ok {
			// Aggregate counts from this type's groups
			typeCount := aggregateCountsFromGroups(typeMap)
			totalCount += typeCount
		}
	}

	writeJSON(w, map[string]interface{}{
		"groups": groups,
		"total":  totalCount,
	})
}

func (a *API) getPackageGroups(typeFilter string, showUncached bool) (map[string]interface{}, error) {
	cacheRoot := filepath.Join(a.cacheDir, "packages")
	pmTypes := []packages.Type{packages.TypeAPK, packages.TypeAPT, packages.TypeNPM, packages.TypePyPI, packages.TypeGo}

	result := make(map[string]interface{})

	for _, pmType := range pmTypes {
		if typeFilter != "" && string(pmType) != typeFilter {
			continue
		}

		typeGroups := make(map[string]interface{})

		// Build a set of cached packages if we need to filter by cached status
		cachedPackageKeys := make(map[string]bool)
		if !showUncached {
			// Get cached packages to filter counts
			typeDir := filepath.Join(cacheRoot, string(pmType))
			if _, err := os.Stat(typeDir); err == nil {
				cachedEntries := listPackagesByType(typeDir, pmType, "")
				for _, entry := range cachedEntries {
					key := fmt.Sprintf("%s:%s@%s", entry.Type, entry.Name, entry.Version)
					if entry.Architecture != "" {
						key = fmt.Sprintf("%s:%s@%s:%s", entry.Type, entry.Name, entry.Version, entry.Architecture)
					}
					cachedPackageKeys[key] = true
					// Debug: log yaml packages
					if pmType == packages.TypeGo && strings.Contains(entry.Name, "yaml") {
						log.Printf("[getPackageGroups] Added cached package key: %s (name: %s, version: %s)", key, entry.Name, entry.Version)
					}
				}
				if pmType == packages.TypeGo {
					log.Printf("[getPackageGroups] Built %d cached package keys for Go (total entries: %d)", len(cachedPackageKeys), len(cachedEntries))
				}
			}
		}

		// Get groups from index database
		if a.indexDB != nil {
			indexPkgs, err := a.indexDB.GetPackages(pmType)
			if err == nil && len(indexPkgs) > 0 {
				if pmType == packages.TypeAPT {
					// Build hierarchical structure for APT
					hierarchy := make(map[string]interface{})
					for _, pkg := range indexPkgs {
						// Skip if not cached and showUncached is false
						if !showUncached {
							key := fmt.Sprintf("%s:%s@%s", pkg.Type, pkg.Name, pkg.Version)
							if pkg.Architecture != "" {
								key = fmt.Sprintf("%s:%s@%s:%s", pkg.Type, pkg.Name, pkg.Version, pkg.Architecture)
							}
							if !cachedPackageKeys[key] {
								continue
							}
						}
						// APT v2 parser uses / separator: basePath/distribution/component
						if pkg.Group != "" && strings.Contains(pkg.Group, "/") {
							parts := strings.Split(pkg.Group, "/")
							if len(parts) >= 3 {
								basePath := parts[0]
								distribution := parts[1]
								component := parts[2]

								// Initialize base path if needed
								if hierarchy[basePath] == nil {
									hierarchy[basePath] = make(map[string]interface{})
								}
								baseMap := hierarchy[basePath].(map[string]interface{})

								// Initialize distribution if needed
								if baseMap[distribution] == nil {
									baseMap[distribution] = make(map[string]interface{})
								}
								distMap := baseMap[distribution].(map[string]interface{})

								// Initialize component if needed
								if distMap[component] == nil {
									distMap[component] = map[string]interface{}{
										"count": 0,
									}
								}
								compMap := distMap[component].(map[string]interface{})
								currentCount := compMap["count"].(int)
								compMap["count"] = currentCount + 1
							}
						} else if pkg.Group != "" {
							// Skip if not cached and showUncached is false
							if !showUncached {
								key := fmt.Sprintf("%s:%s@%s", pkg.Type, pkg.Name, pkg.Version)
								if pkg.Architecture != "" {
									key = fmt.Sprintf("%s:%s@%s:%s", pkg.Type, pkg.Name, pkg.Version, pkg.Architecture)
								}
								if !cachedPackageKeys[key] {
									continue
								}
							}
							// Flat group structure
							if typeGroups[pkg.Group] == nil {
								typeGroups[pkg.Group] = map[string]interface{}{
									"count": 0,
								}
							}
							groupMap := typeGroups[pkg.Group].(map[string]interface{})
							currentCount := groupMap["count"].(int)
							groupMap["count"] = currentCount + 1
						} else {
							// Skip if not cached and showUncached is false
							if !showUncached {
								key := fmt.Sprintf("%s:%s@%s", pkg.Type, pkg.Name, pkg.Version)
								if pkg.Architecture != "" {
									key = fmt.Sprintf("%s:%s@%s:%s", pkg.Type, pkg.Name, pkg.Version, pkg.Architecture)
								}
								if !cachedPackageKeys[key] {
									continue
								}
							}
							// Default group
							if typeGroups["default"] == nil {
								typeGroups["default"] = map[string]interface{}{
									"count": 0,
								}
							}
							defaultMap := typeGroups["default"].(map[string]interface{})
							currentCount := defaultMap["count"].(int)
							defaultMap["count"] = currentCount + 1
						}
					}

					// Aggregate counts up the hierarchy
					if len(hierarchy) > 0 {
						// Calculate distribution counts (sum of components)
						for _, baseData := range hierarchy {
							baseMap := baseData.(map[string]interface{})
							for _, distData := range baseMap {
								distMap := distData.(map[string]interface{})
								distCount := 0
								for _, compData := range distMap {
									if compMap, ok := compData.(map[string]interface{}); ok {
										if count, ok := compMap["count"].(int); ok {
											distCount += count
										}
									} else {
										// Not a component, skip
										continue
									}
								}
								distMap["count"] = distCount
							}

							// Calculate base path counts (sum of distributions)
							baseCount := 0
							for _, distData := range baseMap {
								if distMap, ok := distData.(map[string]interface{}); ok {
									if count, ok := distMap["count"].(int); ok {
										baseCount += count
									}
								}
							}
							baseMap["count"] = baseCount
						}

						typeGroups["hierarchy"] = hierarchy
					}
				} else if pmType == packages.TypeGo {
					// Build hierarchical structure for Go packages
					// Track packages by full name to ensure each appears only once
					addedPackages := make(map[string]bool) // package name -> already added
					hierarchy := make(map[string]interface{})

					for _, pkg := range indexPkgs {
						// Skip if not cached and showUncached is false
						if !showUncached {
							key := fmt.Sprintf("%s:%s@%s", pkg.Type, pkg.Name, pkg.Version)
							if pkg.Architecture != "" {
								key = fmt.Sprintf("%s:%s@%s:%s", pkg.Type, pkg.Name, pkg.Version, pkg.Architecture)
							}
							if !cachedPackageKeys[key] {
								continue
							}
						}

						// Skip if we've already added this package (by name, regardless of group path)
						if addedPackages[pkg.Name] {
							log.Printf("[getPackageGroups] Skipping duplicate Go package: %s", pkg.Name)
							continue
						}
						addedPackages[pkg.Name] = true

						// Use the group path (module path without /vN) to build hierarchy
						groupPath := pkg.Group
						if groupPath == "" {
							groupPath = pkg.Name
						}

						// Normalize: if group contains colons (old format), convert to slashes
						if strings.Contains(groupPath, ":") {
							groupPath = strings.ReplaceAll(groupPath, ":", "/")
						}

						// Split by / to get path parts
						parts := strings.Split(groupPath, "/")
						if len(parts) < 2 {
							continue
						}

						// Navigate to the parent level (all parts except last)
						current := hierarchy
						for i := 0; i < len(parts)-1; i++ {
							part := parts[i]
							if current[part] == nil {
								current[part] = map[string]interface{}{
									"groups":   make(map[string]interface{}),
									"packages": make(map[string]interface{}),
									"count":    0,
								}
							}
							node, ok := current[part].(map[string]interface{})
							if !ok {
								break
							}
							if node["groups"] == nil {
								node["groups"] = make(map[string]interface{})
							}
							groups, ok := node["groups"].(map[string]interface{})
							if !ok {
								break
							}
							current = groups
						}

						// Add package to the parent node (the one we just navigated to)
						// Navigate back to find the parent
						parent := hierarchy
						for i := 0; i < len(parts)-1; i++ {
							part := parts[i]
							node, ok := parent[part].(map[string]interface{})
							if !ok {
								break
							}
							if i == len(parts)-2 {
								// This is the parent - add package here
								if node["packages"] == nil {
									node["packages"] = make(map[string]interface{})
								}
								packages, ok := node["packages"].(map[string]interface{})
								if !ok {
									break
								}
								// Use full package name as key (e.g., "dario.cat/mergo")
								// Check if already exists - if so, just increment count, don't recreate
								if packages[pkg.Name] == nil {
									packages[pkg.Name] = map[string]interface{}{
										"versions": []interface{}{},
										"count":    0,
									}
								}
								pkgNode, ok := packages[pkg.Name].(map[string]interface{})
								if !ok {
									break
								}
								// Increment count (this represents number of versions)
								count, _ := pkgNode["count"].(int)
								pkgNode["count"] = count + 1
								break
							}
							if node["groups"] == nil {
								break
							}
							groups, ok := node["groups"].(map[string]interface{})
							if !ok {
								break
							}
							parent = groups
						}
					}

					if len(hierarchy) > 0 {
						// Aggregate counts and convert to frontend format
						// Track all package names to ensure no duplicates
						allPackageNames := make(map[string]bool)

						var aggregateAndConvert func(level map[string]interface{}) map[string]interface{}
						aggregateAndConvert = func(level map[string]interface{}) map[string]interface{} {
							result := make(map[string]interface{})

							for key, value := range level {
								if node, ok := value.(map[string]interface{}); ok {
									converted := make(map[string]interface{})
									nodeCount := 0

									// Process groups first
									if groups, ok := node["groups"].(map[string]interface{}); ok {
										convertedGroups := aggregateAndConvert(groups)
										for k, v := range convertedGroups {
											// Only add group if not a package with same name
											if packages, hasPackages := node["packages"].(map[string]interface{}); !hasPackages || packages[k] == nil {
												converted[k] = v
											}
											// Sum counts from groups
											if groupMap, ok := v.(map[string]interface{}); ok {
												if count, ok := groupMap["count"].(int); ok {
													nodeCount += count
												}
											}
										}
									}

									// Process packages (take precedence over groups with same name)
									if packages, ok := node["packages"].(map[string]interface{}); ok {
										for pkgKey, pkgValue := range packages {
											// Skip if we've already seen this package name
											if allPackageNames[pkgKey] {
												log.Printf("[getPackageGroups] SKIPPING duplicate package: %s", pkgKey)
												continue
											}
											allPackageNames[pkgKey] = true

											if pkgMap, ok := pkgValue.(map[string]interface{}); ok {
												// Ensure versions array exists
												if pkgMap["versions"] == nil {
													pkgMap["versions"] = []interface{}{}
												}
												// Count this package
												if count, ok := pkgMap["count"].(int); ok {
													nodeCount += count
												}
											}
											// Packages overwrite groups with same name
											converted[pkgKey] = pkgValue
										}
									}

									// Add count
									converted["count"] = nodeCount
									result[key] = converted
								}
							}

							return result
						}

						// Convert and aggregate
						convertedHierarchy := aggregateAndConvert(hierarchy)

						// Sort recursively
						var sortHierarchy func(level map[string]interface{}) map[string]interface{}
						sortHierarchy = func(level map[string]interface{}) map[string]interface{} {
							if level == nil {
								return nil
							}
							sorted := make(map[string]interface{})
							keys := make([]string, 0, len(level))
							for k := range level {
								if k != "count" {
									keys = append(keys, k)
								}
							}
							sort.Strings(keys)

							if count, ok := level["count"]; ok {
								sorted["count"] = count
							}

							for _, k := range keys {
								if val, ok := level[k].(map[string]interface{}); ok {
									sorted[k] = sortHierarchy(val)
								} else {
									sorted[k] = level[k]
								}
							}
							return sorted
						}

						convertedHierarchy = sortHierarchy(convertedHierarchy)
						typeGroups["hierarchy"] = convertedHierarchy
					}
				} else {
					// Flat group structure for other types
					for _, pkg := range indexPkgs {
						// Skip if not cached and showUncached is false
						if !showUncached {
							key := fmt.Sprintf("%s:%s@%s", pkg.Type, pkg.Name, pkg.Version)
							if pkg.Architecture != "" {
								key = fmt.Sprintf("%s:%s@%s:%s", pkg.Type, pkg.Name, pkg.Version, pkg.Architecture)
							}
							if !cachedPackageKeys[key] {
								continue
							}
						}
						group := pkg.Group
						if group == "" {
							group = "default"
						}
						if typeGroups[group] == nil {
							typeGroups[group] = map[string]interface{}{
								"count": 0,
							}
						}
						groupMap := typeGroups[group].(map[string]interface{})
						currentCount := groupMap["count"].(int)
						groupMap["count"] = currentCount + 1
					}
				}
			}
		}

		// Also check cached packages for groups not in index
		// Note: We need to simulate the merge with index to get the correct group assignment
		// This ensures the count matches what listPackages would return
		typeDir := filepath.Join(cacheRoot, string(pmType))
		if _, err := os.Stat(typeDir); err == nil {
			cachedEntries := listPackagesByType(typeDir, pmType, "")

			// Build a map of index packages by name+arch for group lookup
			indexByKeyForGroup := make(map[string][]index.IndexPackage)
			if a.indexDB != nil {
				indexPkgs, err := a.indexDB.GetPackages(pmType)
				if err == nil {
					for _, indexPkg := range indexPkgs {
						key := fmt.Sprintf("%s:%s", indexPkg.Type, indexPkg.Name)
						if indexPkg.Architecture != "" {
							key = fmt.Sprintf("%s:%s:%s", indexPkg.Type, indexPkg.Name, indexPkg.Architecture)
						}
						indexByKeyForGroup[key] = append(indexByKeyForGroup[key], indexPkg)
					}
				}
			}

			// Build a set of index packages to check for duplicates
			indexPackageKeys := make(map[string]bool)
			if a.indexDB != nil {
				indexPkgs, err := a.indexDB.GetPackages(pmType)
				if err == nil {
					for _, indexPkg := range indexPkgs {
						key := fmt.Sprintf("%s@%s", indexPkg.Name, indexPkg.Version)
						if indexPkg.Architecture != "" {
							key = fmt.Sprintf("%s@%s:%s", indexPkg.Name, indexPkg.Version, indexPkg.Architecture)
						}
						indexPackageKeys[key] = true
					}
				}
			}

			for _, entry := range cachedEntries {
				// Skip if this package is already in the index (to avoid duplicates)
				entryKey := fmt.Sprintf("%s@%s", entry.Name, entry.Version)
				if entry.Architecture != "" {
					entryKey = fmt.Sprintf("%s@%s:%s", entry.Name, entry.Version, entry.Architecture)
				}
				if indexPackageKeys[entryKey] {
					// Already in index, skip to avoid duplicate
					continue
				}

				// Simulate the merge: if cached entry has no group, try to get it from index
				group := entry.Group
				if group == "" && a.indexDB != nil {
					// Try to find matching index package to get group
					lookupKey := fmt.Sprintf("%s:%s", entry.Type, entry.Name)
					if entry.Architecture != "" {
						lookupKey = fmt.Sprintf("%s:%s:%s", entry.Type, entry.Name, entry.Architecture)
					}
					if indexPkgsList, exists := indexByKeyForGroup[lookupKey]; exists {
						// Find matching version
						for _, indexPkg := range indexPkgsList {
							cachedVersion := strings.TrimSpace(entry.Version)
							indexVersion := strings.TrimSpace(indexPkg.Version)
							var cachedNormalized, indexNormalized string
							if entry.Type == "apt" {
								cachedNormalized = normalizeDebianVersion(cachedVersion)
								indexNormalized = normalizeDebianVersion(indexVersion)
							} else {
								cachedNormalized = cachedVersion
								indexNormalized = indexVersion
							}
							if indexNormalized == cachedNormalized {
								group = indexPkg.Group
								break
							}
						}
					}
				}
				if group == "" {
					group = "default"
				}
				if pmType == packages.TypeAPT && strings.Contains(group, ":") {
					// Add to hierarchy if not already counted from index
					parts := strings.Split(group, ":")
					if len(parts) >= 3 {
						basePath := parts[0]
						distribution := parts[1]
						component := parts[2]

						// Get or create hierarchy
						var hierarchy map[string]interface{}
						if typeGroups["hierarchy"] == nil {
							hierarchy = make(map[string]interface{})
							typeGroups["hierarchy"] = hierarchy
						} else {
							hierarchy = typeGroups["hierarchy"].(map[string]interface{})
						}

						// Initialize base path if needed
						if hierarchy[basePath] == nil {
							hierarchy[basePath] = make(map[string]interface{})
						}
						baseMap := hierarchy[basePath].(map[string]interface{})

						// Initialize distribution if needed
						if baseMap[distribution] == nil {
							baseMap[distribution] = make(map[string]interface{})
						}
						distMap := baseMap[distribution].(map[string]interface{})

						// Initialize component if needed
						if distMap[component] == nil {
							distMap[component] = map[string]interface{}{
								"count": 0,
							}
						}
						compMap := distMap[component].(map[string]interface{})
						currentCount := compMap["count"].(int)
						compMap["count"] = currentCount + 1

						// Update distribution count
						if distCount, ok := distMap["count"].(int); ok {
							distMap["count"] = distCount + 1
						} else {
							distMap["count"] = 1
						}

						// Update base path count
						if baseCount, ok := baseMap["count"].(int); ok {
							baseMap["count"] = baseCount + 1
						} else {
							baseMap["count"] = 1
						}
					}
					continue
				}
				if pmType == packages.TypeGo && strings.Contains(group, "/") {
					// Skip adding cached packages to hierarchy for Go
					// All cached packages should already be in the index and added to hierarchy
					// This prevents duplicates
					continue
				}
				if typeGroups[group] == nil {
					typeGroups[group] = map[string]interface{}{
						"count": 0,
					}
				}
				groupMap := typeGroups[group].(map[string]interface{})
				currentCount := groupMap["count"].(int)
				groupMap["count"] = currentCount + 1
			}

			// Re-aggregate hierarchy counts after adding cached packages
			if pmType == packages.TypeAPT && typeGroups["hierarchy"] != nil {
				hierarchy := typeGroups["hierarchy"].(map[string]interface{})
				for _, baseData := range hierarchy {
					baseMap := baseData.(map[string]interface{})
					baseCount := 0
					for _, distData := range baseMap {
						if distMap, ok := distData.(map[string]interface{}); ok {
							distCount := 0
							for _, compData := range distMap {
								if compMap, ok := compData.(map[string]interface{}); ok {
									if count, ok := compMap["count"].(int); ok {
										distCount += count
									}
								} else {
									continue
								}
							}
							distMap["count"] = distCount
							baseCount += distCount
						}
					}
					baseMap["count"] = baseCount
				}
			}
			// Re-aggregate hierarchy counts after adding cached packages for Go
			// Use the same recursive aggregation function to handle any depth
			if pmType == packages.TypeGo && typeGroups["hierarchy"] != nil {
				hierarchy := typeGroups["hierarchy"].(map[string]interface{})
				var aggregateCounts func(level map[string]interface{}) int
				aggregateCounts = func(level map[string]interface{}) int {
					totalCount := 0
					for _, value := range level {
						if valueMap, ok := value.(map[string]interface{}); ok {
							// Check if this is a leaf node (package with count)
							if count, hasCount := valueMap["count"].(int); hasCount {
								// Check if this is a leaf (only count) or intermediate (has other keys)
								if len(valueMap) == 1 {
									// Leaf node: package with count only
									totalCount += count
								} else {
									// Intermediate node: recursively aggregate and set count
									subCount := aggregateCounts(valueMap)
									// Preserve existing count if it's higher (shouldn't happen, but be safe)
									if count > subCount {
										subCount = count
									}
									valueMap["count"] = subCount
									totalCount += subCount
								}
							} else {
								// No count yet, recursively aggregate
								subCount := aggregateCounts(valueMap)
								if subCount > 0 {
									valueMap["count"] = subCount
								}
								totalCount += subCount
							}
						}
					}
					return totalCount
				}

				for _, hostnameData := range hierarchy {
					if hostnameMap, ok := hostnameData.(map[string]interface{}); ok {
						hostnameCount := aggregateCounts(hostnameMap)
						hostnameMap["count"] = hostnameCount
					}
				}
			}
		}

		if len(typeGroups) > 0 {
			// Sort flat groups alphabetically
			if len(typeGroups) > 0 {
				// Check if it's a flat structure (has keys that aren't "hierarchy")
				hasHierarchy := false
				for k := range typeGroups {
					if k == "hierarchy" {
						hasHierarchy = true
						break
					}
				}

				if !hasHierarchy {
					// Flat structure - sort the group keys
					sortedTypeGroups := make(map[string]interface{})
					keys := make([]string, 0, len(typeGroups))
					for k := range typeGroups {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					for _, k := range keys {
						sortedTypeGroups[k] = typeGroups[k]
					}
					typeGroups = sortedTypeGroups
				}
			}

			result[string(pmType)] = typeGroups
		}
	}

	return result, nil
}

func (a *API) handlePackageDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		a.handleDeletePackage(w, r)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract package ID from path
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/packages/")
	if path == "" {
		http.Error(w, "Package ID required", http.StatusBadRequest)
		return
	}

	// For now, package ID is the cache path
	// In the future, we could use a hash or UUID
	entries, err := a.listPackages("", "", true) // Include uncached for detail view
	if err != nil {
		http.Error(w, fmt.Sprintf("Error listing packages: %v", err), http.StatusInternalServerError)
		return
	}

	for _, entry := range entries {
		if entry.ID == path {
			writeJSON(w, entry)
			return
		}
	}

	http.Error(w, "Package not found", http.StatusNotFound)
}

// handleNPMTarballMetadata extracts package.json from a cached NPM tarball
// Path format: /api/v1/packages/npm/{name}/{version}/metadata
// Example: /api/v1/packages/npm/@adobe/css-tools/4.4.4/metadata
func (a *API) handleNPMTarballMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract package name and version from path
	// Path: /api/v1/packages/npm/{name}/{version}/metadata
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/packages/npm/")
	path = strings.TrimSuffix(path, "/metadata")

	// URL decode the path to handle encoded package names
	decodedPath, err := url.PathUnescape(path)
	if err != nil {
		// If decoding fails, try using the original path
		decodedPath = path
	}

	parts := strings.Split(decodedPath, "/")
	if len(parts) < 2 {
		http.Error(w, "Invalid path format. Expected: /api/v1/packages/npm/{name}/{version}/metadata", http.StatusBadRequest)
		return
	}

	// Handle scoped packages (e.g., @adobe/css-tools)
	var packageName, version string
	if strings.HasPrefix(parts[0], "@") {
		// Scoped package: @scope/package
		if len(parts) < 3 {
			http.Error(w, "Invalid path format for scoped package", http.StatusBadRequest)
			return
		}
		packageName = parts[0] + "/" + parts[1]
		version = parts[2]
	} else {
		// Regular package
		packageName = parts[0]
		version = parts[1]
	}

	// Find the cached tarball
	log.Printf("[npm-metadata] Looking for package: %s@%s", packageName, version)
	tarballPath, err := a.findNPMTarball(packageName, version)
	if err != nil {
		log.Printf("[npm-metadata] Failed to find package %s@%s: %v", packageName, version, err)
		http.Error(w, fmt.Sprintf("Package not found in cache: %v", err), http.StatusNotFound)
		return
	}
	log.Printf("[npm-metadata] Found tarball for %s@%s: %s", packageName, version, tarballPath)

	// Extract package.json from the tarball
	packageJSON, err := extractPackageJSONFromTarball(tarballPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to extract package.json: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, packageJSON)
}

// handlePyPIMetadata fetches package metadata from PyPI's JSON API
// Path format: /api/v1/packages/pypi/{name}/metadata or /api/v1/packages/pypi/{name}/{version}/metadata
func (a *API) handlePyPIMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract package name and optional version from path
	// Path: /api/v1/packages/pypi/{name}/metadata or /api/v1/packages/pypi/{name}/{version}/metadata
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/packages/pypi/")
	path = strings.TrimSuffix(path, "/metadata")

	// URL decode the path
	decodedPath, err := url.PathUnescape(path)
	if err != nil {
		decodedPath = path
	}

	parts := strings.Split(decodedPath, "/")
	if len(parts) < 1 {
		http.Error(w, "Invalid path format. Expected: /api/v1/packages/pypi/{name}/metadata", http.StatusBadRequest)
		return
	}

	packageName := parts[0]
	var version string
	if len(parts) >= 2 {
		version = parts[1]
	}

	// Build PyPI JSON API URL
	var pypiURL string
	if version != "" {
		pypiURL = fmt.Sprintf("https://pypi.org/pypi/%s/%s/json", packageName, version)
	} else {
		pypiURL = fmt.Sprintf("https://pypi.org/pypi/%s/json", packageName)
	}

	log.Printf("[pypi-metadata] Fetching metadata for %s@%s from %s", packageName, version, pypiURL)

	// Fetch from PyPI JSON API
	resp, err := http.Get(pypiURL)
	if err != nil {
		log.Printf("[pypi-metadata] Failed to fetch from PyPI: %v", err)
		http.Error(w, fmt.Sprintf("Failed to fetch from PyPI: %v", err), http.StatusBadGateway)
		return
	}
	defer util.DeferClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		log.Printf("[pypi-metadata] PyPI returned %d for %s", resp.StatusCode, pypiURL)
		http.Error(w, fmt.Sprintf("PyPI returned %d", resp.StatusCode), resp.StatusCode)
		return
	}

	// Parse JSON response
	var pypiData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&pypiData); err != nil {
		log.Printf("[pypi-metadata] Failed to parse PyPI JSON: %v", err)
		http.Error(w, fmt.Sprintf("Failed to parse PyPI response: %v", err), http.StatusInternalServerError)
		return
	}

	// Extract and format metadata similar to NPM structure
	metadata := make(map[string]interface{})

	// Extract info object (contains most metadata)
	if info, ok := pypiData["info"].(map[string]interface{}); ok {
		// Basic info
		if name, ok := info["name"].(string); ok {
			metadata["name"] = name
		}
		if version, ok := info["version"].(string); ok {
			metadata["version"] = version
		}
		if summary, ok := info["summary"].(string); ok {
			metadata["description"] = summary
		}
		if author, ok := info["author"].(string); ok {
			metadata["author"] = author
		}
		if authorEmail, ok := info["author_email"].(string); ok {
			if author, ok := metadata["author"].(string); ok && author != "" {
				metadata["author"] = fmt.Sprintf("%s <%s>", author, authorEmail)
			} else if authorEmail != "" {
				metadata["author"] = authorEmail
			}
		}
		if license, ok := info["license"].(string); ok {
			metadata["license"] = license
		}
		if homePage, ok := info["home_page"].(string); ok {
			metadata["homepage"] = homePage
		}
		if projectURL, ok := info["project_url"].(string); ok {
			metadata["homepage"] = projectURL
		}
		if projectURLs, ok := info["project_urls"].(map[string]interface{}); ok {
			if homepage, ok := projectURLs["Homepage"].(string); ok {
				metadata["homepage"] = homepage
			}
			if repo, ok := projectURLs["Repository"].(string); ok {
				metadata["repository"] = repo
			}
		}
		if keywords, ok := info["keywords"].(string); ok && keywords != "" {
			// Keywords are comma-separated
			keywordList := strings.Split(keywords, ",")
			for i, k := range keywordList {
				keywordList[i] = strings.TrimSpace(k)
			}
			metadata["keywords"] = keywordList
		}
		if requiresDist, ok := info["requires_dist"].([]interface{}); ok {
			// Extract dependency names from requirement strings
			// Format: "package>=1.0.0" or "package[extra]>=1.0.0"
			var deps []string
			for _, req := range requiresDist {
				if reqStr, ok := req.(string); ok {
					// Extract package name (everything before >=, ==, !=, <, >, or [
					parts := strings.FieldsFunc(reqStr, func(r rune) bool {
						return r == '>' || r == '<' || r == '=' || r == '!' || r == '[' || r == ';'
					})
					if len(parts) > 0 {
						deps = append(deps, strings.TrimSpace(parts[0]))
					}
				}
			}
			metadata["dependencies"] = deps
		}
	}

	log.Printf("[pypi-metadata] Successfully fetched metadata for %s@%s", packageName, version)
	writeJSON(w, metadata)
}

// handleGoMetadata reads metadata from a cached Go module .info file
// Path format: /api/v1/packages/go/{module}/{version}/metadata
// Example: /api/v1/packages/go/github.com/gin-gonic/gin/v1.9.1/metadata
func (a *API) handleGoMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract module name and version from path
	// Path: /api/v1/packages/go/{module}/{version}/metadata
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/packages/go/")
	path = strings.TrimSuffix(path, "/metadata")

	// URL decode the path
	decodedPath, err := url.PathUnescape(path)
	if err != nil {
		decodedPath = path
	}

	// Split module path and version
	// Module path can have multiple segments: github.com/user/repo
	// Version is the last segment
	parts := strings.Split(decodedPath, "/")
	if len(parts) < 2 {
		http.Error(w, "Invalid path format. Expected: /api/v1/packages/go/{module}/{version}/metadata", http.StatusBadRequest)
		return
	}

	version := parts[len(parts)-1]
	modulePath := strings.Join(parts[:len(parts)-1], "/")

	// Build the .info file path: /{module}/@v/{version}.info
	infoPath := fmt.Sprintf("/%s/@v/%s.info", modulePath, version)

	log.Printf("[go-metadata] Looking for .info file: %s", infoPath)

	// Find the cached .info file
	typeDir := filepath.Join(a.cacheDir, "packages", "go")
	if _, err := os.Stat(typeDir); os.IsNotExist(err) {
		http.Error(w, "Go cache directory not found", http.StatusNotFound)
		return
	}

	// The .info file is cached by its URL path hash
	hash := sha256.Sum256([]byte(infoPath))
	infoPathHash := hex.EncodeToString(hash[:])
	cachePath := filepath.Join(typeDir, infoPathHash[0:2], infoPathHash[2:4], infoPathHash)

	// Check if file exists
	if _, err := os.Stat(cachePath); os.IsNotExist(err) {
		// Try to find the .info file by searching metadata files
		log.Printf("[go-metadata] .info file not found at expected path: %s, searching metadata files...", cachePath)

		// Search for .info file by checking metadata
		found := false
		if err := filepath.Walk(typeDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || found {
				return nil
			}

			// Check if this is a .info file by looking at metadata
			if strings.HasSuffix(path, ".meta") {
				metaData, err := os.ReadFile(path)
				if err != nil {
					return nil
				}

				var metadata struct {
					URLPath string `json:"url_path"`
					Name    string `json:"name"`
					Version string `json:"version"`
				}
				if err := json.Unmarshal(metaData, &metadata); err != nil {
					return nil
				}

				// Check if this metadata matches our module and version
				if metadata.URLPath == infoPath ||
					(metadata.Name == modulePath && metadata.Version == version && strings.HasSuffix(metadata.URLPath, ".info")) {
					// Found it! The actual file is the path without .meta
					cachePath = strings.TrimSuffix(path, ".meta")
					found = true
					return filepath.SkipAll // Stop walking
				}
			}
			return nil
		}); err != nil {
			log.Printf("Error walking directory: %v", err)
		}

		// Prefer .zip files over .info files for metadata extraction
		// .zip files contain go.mod with better metadata (GoVersion, etc.)
		// Build the .zip file path: /{module}/@v/{version}.zip
		zipPath := fmt.Sprintf("/%s/@v/%s.zip", modulePath, version)
		zipHash := sha256.Sum256([]byte(zipPath))
		zipPathHash := hex.EncodeToString(zipHash[:])
		zipCachePath := filepath.Join(typeDir, zipPathHash[0:2], zipPathHash[2:4], zipPathHash)

		if _, err := os.Stat(zipCachePath); err == nil {
			// Found .zip file, extract metadata from it (preferred source)
			log.Printf("[go-metadata] Found .zip file, extracting metadata from: %s", zipCachePath)
			metadata, err := a.extractGoMetadataFromZip(zipCachePath, modulePath, version)
			if err != nil {
				log.Printf("[go-metadata] Failed to extract metadata from zip: %v", err)
				// Fall through to .info file if zip extraction fails
			} else {
				writeJSON(w, metadata)
				return
			}
		}

		if !found {
			// Neither .zip nor .info file found
			log.Printf("[go-metadata] Neither .zip nor .info file found in cache for %s@%s", modulePath, version)
			http.Error(w, fmt.Sprintf("Go module files not found in cache. The module %s@%s may not have been cached yet. Try fetching the module first.", modulePath, version), http.StatusNotFound)
			return
		}
	}

	// Fallback: Read and parse the .info JSON file (if .zip file wasn't available)
	data, err := os.ReadFile(cachePath)
	if err != nil {
		log.Printf("[go-metadata] Failed to read .info file: %v", err)
		http.Error(w, fmt.Sprintf("Failed to read .info file: %v", err), http.StatusInternalServerError)
		return
	}

	var infoData map[string]interface{}
	if err := json.Unmarshal(data, &infoData); err != nil {
		log.Printf("[go-metadata] Failed to parse .info JSON: %v", err)
		http.Error(w, fmt.Sprintf("Failed to parse .info file: %v", err), http.StatusInternalServerError)
		return
	}

	// Format metadata similar to other package types
	// Note: .info files have limited metadata - prefer .zip files for better info
	metadata := make(map[string]interface{})
	if version, ok := infoData["Version"].(string); ok {
		metadata["version"] = version
	}

	// Validate Time field - .info files often have zero/missing Time values
	// Only use Time if it's a valid date (not zero, and after 2000-01-01)
	if timeVal, ok := infoData["Time"]; ok {
		var timeStr string
		switch v := timeVal.(type) {
		case string:
			timeStr = v
		case time.Time:
			// If it's already a time.Time, check if it's valid
			if !v.IsZero() && v.After(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
				metadata["published"] = v.Format(time.RFC3339)
			}
		}

		// If it's a string, try to parse and validate
		if timeStr != "" {
			if t, err := time.Parse(time.RFC3339, timeStr); err == nil {
				if !t.IsZero() && t.After(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
					metadata["published"] = timeStr
				}
			}
		}
	}

	// GoVersion doesn't exist in .info files - it's in go.mod inside .zip files
	// Don't try to extract it from .info files

	// Try to fetch description from pkg.go.dev
	if description := a.fetchGoPackageDescription(modulePath); description != "" {
		metadata["description"] = description
	}

	// Find sub-packages (directories) for this module
	// For example, for github.com/bytedance/sonic, find packages whose name starts with github.com/bytedance/sonic/
	if a.indexDB != nil {
		indexPkgs, err := a.indexDB.GetPackages(packages.TypeGo)
		if err == nil {
			// Find all packages whose name is a sub-path of this module
			// e.g., "github.com/bytedance/sonic/loader" is a sub-package of "github.com/bytedance/sonic"
			subPackages := make(map[string]bool)
			modulePrefix := modulePath + "/"
			for _, pkg := range indexPkgs {
				if strings.HasPrefix(pkg.Name, modulePrefix) {
					// Extract the sub-package name (directory) from the package name
					// e.g., "github.com/bytedance/sonic/loader" -> "loader"
					subPath := strings.TrimPrefix(pkg.Name, modulePrefix)
					// Get the first directory name (e.g., "loader" from "loader/subdir")
					subPathParts := strings.Split(subPath, "/")
					if len(subPathParts) > 0 && subPathParts[0] != "" {
						subPackages[subPathParts[0]] = true
					}
				}
			}

			// Convert to sorted slice
			if len(subPackages) > 0 {
				subPkgList := make([]string, 0, len(subPackages))
				for subPkg := range subPackages {
					subPkgList = append(subPkgList, subPkg)
				}
				sort.Strings(subPkgList)
				metadata["directories"] = subPkgList
			}
		}
	}

	log.Printf("[go-metadata] Successfully loaded metadata for %s@%s", modulePath, version)
	writeJSON(w, metadata)
}

// extractGoMetadataFromZip extracts metadata from a Go module zip file
// Go module zip files contain go.mod which has module info and Go version requirements
func (a *API) extractGoMetadataFromZip(zipPath string, modulePath string, version string) (map[string]interface{}, error) {
	// Open the zip file
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open zip file: %w", err)
	}
	defer util.DeferClose(r)

	// Look for go.mod file in the zip
	// Go module zip files have paths like: {module}/go.mod
	// e.g., github.com/user/repo/go.mod
	var goModPath string
	for _, f := range r.File {
		// Check if this is a go.mod file
		if strings.HasSuffix(f.Name, "/go.mod") || f.Name == "go.mod" {
			goModPath = f.Name
			break
		}
	}

	if goModPath == "" {
		return nil, fmt.Errorf("go.mod not found in zip file")
	}

	// Read go.mod file
	goModFile, err := r.Open(goModPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open go.mod in zip: %w", err)
	}
	defer util.DeferClose(goModFile)

	goModContent, err := io.ReadAll(goModFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read go.mod: %w", err)
	}

	// Parse go.mod to extract metadata
	metadata := make(map[string]interface{})
	metadata["version"] = version

	// Parse go.mod content
	// go.mod format:
	// module {modulePath}
	// go {goVersion}
	// require (...)
	goModText := string(goModContent)
	lines := strings.Split(goModText, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Extract Go version requirement
		if strings.HasPrefix(line, "go ") {
			goVersion := strings.TrimSpace(strings.TrimPrefix(line, "go "))
			metadata["goVersion"] = goVersion
		}

		// Extract module path (should match what we already have, but verify)
		if strings.HasPrefix(line, "module ") {
			modPath := strings.TrimSpace(strings.TrimPrefix(line, "module "))
			// Remove comments if any
			if idx := strings.Index(modPath, "//"); idx != -1 {
				modPath = strings.TrimSpace(modPath[:idx])
			}
			metadata["module"] = modPath
		}
	}

	// Try to get file modification time from zip (as published date)
	if len(r.File) > 0 {
		// Use the first file's modification time as an approximation
		firstFile := r.File[0]
		if !firstFile.ModTime().IsZero() {
			metadata["published"] = firstFile.ModTime().Format(time.RFC3339)
		}
	}

	// Try to fetch description from pkg.go.dev
	if description := a.fetchGoPackageDescription(modulePath); description != "" {
		metadata["description"] = description
	}

	log.Printf("[go-metadata] Extracted metadata from zip for %s@%s: goVersion=%v", modulePath, version, metadata["goVersion"])
	return metadata, nil
}

// fetchGoPackageDescription fetches the package description from pkg.go.dev
// Returns the first paragraph from the README section, or empty string if not found
func (a *API) fetchGoPackageDescription(modulePath string) string {
	// Build pkg.go.dev URL: https://pkg.go.dev/{modulePath}
	pkgURL := fmt.Sprintf("https://pkg.go.dev/%s", modulePath)

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	req, err := http.NewRequest("GET", pkgURL, nil)
	if err != nil {
		log.Printf("[go-metadata] Failed to create request for %s: %v", pkgURL, err)
		return ""
	}

	// Set User-Agent to avoid being blocked
	req.Header.Set("User-Agent", "registry-proxy/1.0")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[go-metadata] Failed to fetch %s: %v", pkgURL, err)
		return ""
	}
	defer util.DeferClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		log.Printf("[go-metadata] pkg.go.dev returned %d for %s", resp.StatusCode, pkgURL)
		return ""
	}

	// Read HTML content
	html, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[go-metadata] Failed to read response from pkg.go.dev: %v", err)
		return ""
	}

	// Parse HTML to extract first paragraph from README section
	// The README section typically has a structure like:
	// <div class="Documentation-readmeSection"> or similar
	// We'll look for the first <p> tag after "README" heading
	htmlStr := string(html)

	// Try to find README section - look for common patterns
	// Pattern 1: Look for <h2>README</h2> or similar, then get first <p>
	readmePattern := regexp.MustCompile(`(?i)<h[1-6][^>]*>.*?README.*?</h[1-6]>.*?<p[^>]*>([^<]+)</p>`)
	matches := readmePattern.FindStringSubmatch(htmlStr)
	if len(matches) > 1 {
		description := strings.TrimSpace(matches[1])
		// Clean up HTML entities and extra whitespace
		description = strings.ReplaceAll(description, "&nbsp;", " ")
		description = strings.ReplaceAll(description, "&amp;", "&")
		description = strings.ReplaceAll(description, "&lt;", "<")
		description = strings.ReplaceAll(description, "&gt;", ">")
		description = strings.ReplaceAll(description, "&quot;", "\"")
		// Collapse multiple spaces
		description = regexp.MustCompile(`\s+`).ReplaceAllString(description, " ")
		if len(description) > 0 {
			log.Printf("[go-metadata] Extracted description from pkg.go.dev for %s: %s", modulePath, description[:min(100, len(description))])
			return description
		}
	}

	// Pattern 2: Look for first paragraph in main content area
	// Try to find first meaningful paragraph (not navigation, not empty)
	paragraphPattern := regexp.MustCompile(`<p[^>]*>([^<]{20,500})</p>`)
	paragraphMatches := paragraphPattern.FindAllStringSubmatch(htmlStr, 5)
	for _, match := range paragraphMatches {
		if len(match) > 1 {
			text := strings.TrimSpace(match[1])
			// Skip if it looks like navigation or metadata
			if strings.Contains(strings.ToLower(text), "import") && strings.Contains(strings.ToLower(text), "package") {
				continue
			}
			// Skip if it's too short or looks like a link
			if len(text) < 30 || strings.HasPrefix(text, "http") {
				continue
			}
			// Clean up HTML entities
			text = strings.ReplaceAll(text, "&nbsp;", " ")
			text = strings.ReplaceAll(text, "&amp;", "&")
			text = strings.ReplaceAll(text, "&lt;", "<")
			text = strings.ReplaceAll(text, "&gt;", ">")
			text = strings.ReplaceAll(text, "&quot;", "\"")
			text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")
			if len(text) > 0 {
				log.Printf("[go-metadata] Extracted description from pkg.go.dev for %s: %s", modulePath, text[:min(100, len(text))])
				return text
			}
		}
	}

	log.Printf("[go-metadata] Could not extract description from pkg.go.dev for %s", modulePath)
	return ""
}

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// handleAPKMetadata extracts metadata from a cached APK file using apk command
// Path format: /api/v1/packages/apk/{name}/{version}/metadata
// Example: /api/v1/packages/apk/alpine-baselayout/3.6.0-r0/metadata
func (a *API) handleAPKMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract package name and version from path
	// Path: /api/v1/packages/apk/{name}/{version}/metadata
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/packages/apk/")
	path = strings.TrimSuffix(path, "/metadata")

	// URL decode the path
	decodedPath, err := url.PathUnescape(path)
	if err != nil {
		decodedPath = path
	}

	parts := strings.Split(decodedPath, "/")
	if len(parts) < 2 {
		http.Error(w, "Invalid path format. Expected: /api/v1/packages/apk/{name}/{version}/metadata", http.StatusBadRequest)
		return
	}

	packageName := parts[0]
	version := parts[1]

	log.Printf("[apk-metadata] Looking for package: %s@%s", packageName, version)

	// Find the cached .apk file
	typeDir := filepath.Join(a.cacheDir, "packages", "apk")
	if _, err := os.Stat(typeDir); os.IsNotExist(err) {
		http.Error(w, "APK cache directory not found", http.StatusNotFound)
		return
	}

	var foundAPKPath string
	err = filepath.Walk(typeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		// Look for .meta files
		if !strings.HasSuffix(path, ".meta") {
			return nil
		}

		// Load metadata to check package name and version
		metaData, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var metadata struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			IsIndex bool   `json:"is_index"`
		}
		if err := json.Unmarshal(metaData, &metadata); err != nil {
			return nil
		}

		// Check if this matches the requested package
		if metadata.Name == packageName && metadata.Version == version && !metadata.IsIndex {
			// The actual .apk file is the path without .meta
			apkPath := strings.TrimSuffix(path, ".meta")
			if _, err := os.Stat(apkPath); err == nil {
				foundAPKPath = apkPath
				return filepath.SkipAll
			}
		}

		return nil
	})

	if err != nil && err != filepath.SkipAll {
		http.Error(w, fmt.Sprintf("Error searching for APK file: %v", err), http.StatusInternalServerError)
		return
	}

	if foundAPKPath == "" {
		log.Printf("[apk-metadata] APK file not found in cache: %s@%s", packageName, version)
		http.Error(w, fmt.Sprintf("APK file not found in cache: %s@%s", packageName, version), http.StatusNotFound)
		return
	}

	log.Printf("[apk-metadata] Found APK file: %s", foundAPKPath)

	// Try to use apk command first, but fall back to direct file parsing if it fails
	var output []byte

	// Try apk info command
	cmd := exec.Command("apk", "info", "-a", foundAPKPath)
	output, cmdErr := cmd.Output()
	if cmdErr != nil {
		// apk command failed, try to extract metadata directly from the APK file
		// APK files are gzipped tar archives with a control.tar.gz inside
		log.Printf("[apk-metadata] apk command failed (%v), trying direct file extraction", cmdErr)
		var extractErr error
		output, extractErr = a.extractAPKMetadataDirectly(foundAPKPath)
		if extractErr != nil {
			log.Printf("[apk-metadata] Failed to extract APK metadata: %v", extractErr)
			http.Error(w, fmt.Sprintf("Failed to extract APK metadata: %v", extractErr), http.StatusInternalServerError)
			return
		}
	}

	// Parse apk info output
	// Format is key-value pairs, e.g.:
	// package: alpine-baselayout
	// version: 3.6.0-r0
	// description: Alpine base layout
	// depends: musl
	metadata := make(map[string]interface{})
	lines := strings.Split(string(output), "\n")
	var currentKey string
	var currentValue strings.Builder

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Check if this is a key line (ends with :)
		if strings.HasSuffix(line, ":") {
			// Save previous key-value pair
			if currentKey != "" {
				value := strings.TrimSpace(currentValue.String())
				if value != "" {
					metadata[currentKey] = value
				}
			}
			// Start new key
			currentKey = strings.TrimSuffix(line, ":")
			currentValue.Reset()
		} else if currentKey != "" {
			// This is a continuation of the value
			if currentValue.Len() > 0 {
				currentValue.WriteString(" ")
			}
			currentValue.WriteString(line)
		}
	}

	// Save last key-value pair
	if currentKey != "" {
		value := strings.TrimSpace(currentValue.String())
		if value != "" {
			metadata[currentKey] = value
		}
	}

	// Format metadata similar to other package types
	formatted := make(map[string]interface{})
	if name, ok := metadata["package"].(string); ok {
		formatted["name"] = name
	}
	if version, ok := metadata["version"].(string); ok {
		formatted["version"] = version
	}
	if desc, ok := metadata["description"].(string); ok {
		formatted["description"] = desc
	}
	if deps, ok := metadata["depends"].(string); ok {
		// Dependencies are space-separated
		depsList := strings.Fields(deps)
		formatted["dependencies"] = depsList
	}
	if arch, ok := metadata["arch"].(string); ok {
		formatted["architecture"] = arch
	}
	if size, ok := metadata["size"].(string); ok {
		formatted["size"] = size
	}
	if license, ok := metadata["license"].(string); ok {
		// License might be space-separated if multiple licenses
		// Clean it up: split by space and join with ", " for readability
		licenseParts := strings.Fields(license)
		if len(licenseParts) > 1 {
			formatted["license"] = strings.Join(licenseParts, ", ")
		} else {
			formatted["license"] = license
		}
	}
	if url, ok := metadata["url"].(string); ok {
		formatted["homepage"] = url
	}

	// Log extracted metadata for debugging
	log.Printf("[apk-metadata] Extracted metadata for %s@%s: name=%v, version=%v, description=%v, license=%v, arch=%v",
		packageName, version, formatted["name"], formatted["version"],
		formatted["description"], formatted["license"], formatted["architecture"])

	log.Printf("[apk-metadata] Successfully extracted metadata for %s@%s", packageName, version)
	writeJSON(w, formatted)
}

// extractAPKMetadataDirectly extracts metadata from an APK file without using the apk command
// APK files are gzipped tar archives containing:
// - .PKGINFO (package metadata)
// - control.tar.gz (additional control files)
func (a *API) extractAPKMetadataDirectly(apkPath string) ([]byte, error) {
	file, err := os.Open(apkPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open APK file: %v", err)
	}
	defer util.DeferClose(file)

	// APK files are gzipped tar archives
	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %v", err)
	}
	defer util.DeferClose(gzReader)

	tarReader := tar.NewReader(gzReader)
	metadata := make(map[string]string)

	// Look for .PKGINFO file which contains package metadata
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar: %v", err)
		}

		if header.Name == ".PKGINFO" {
			// Read the .PKGINFO file
			data, err := io.ReadAll(tarReader)
			if err != nil {
				return nil, fmt.Errorf("failed to read .PKGINFO: %v", err)
			}

			// Parse .PKGINFO format (key = value lines)
			// Note: Some fields can appear multiple times (like license, depend)
			// We need to collect all values for these fields
			lines := strings.Split(string(data), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				parts := strings.SplitN(line, " = ", 2)
				if len(parts) == 2 {
					key := strings.TrimSpace(parts[0])
					value := strings.TrimSpace(parts[1])

					// Handle multi-value fields (license, depend can appear multiple times)
					if key == "license" || key == "depend" {
						if existing, ok := metadata[key]; ok {
							// Append to existing value (space-separated)
							metadata[key] = existing + " " + value
						} else {
							metadata[key] = value
						}
					} else {
						// Single-value fields: use last value if multiple (shouldn't happen, but be safe)
						metadata[key] = value
					}
				}
			}
			break
		}
	}

	// Format output similar to apk info -a
	var output strings.Builder
	if pkg, ok := metadata["pkgname"]; ok {
		fmt.Fprintf(&output, "package:\n%s\n", pkg)
	}
	if ver, ok := metadata["pkgver"]; ok {
		fmt.Fprintf(&output, "version:\n%s\n", ver)
	}
	if desc, ok := metadata["pkgdesc"]; ok {
		fmt.Fprintf(&output, "description:\n%s\n", desc)
	}
	if arch, ok := metadata["arch"]; ok {
		fmt.Fprintf(&output, "arch:\n%s\n", arch)
	}
	if size, ok := metadata["size"]; ok {
		fmt.Fprintf(&output, "size:\n%s\n", size)
	}
	if license, ok := metadata["license"]; ok {
		fmt.Fprintf(&output, "license:\n%s\n", license)
	}
	if url, ok := metadata["url"]; ok {
		fmt.Fprintf(&output, "url:\n%s\n", url)
	}
	if deps, ok := metadata["depend"]; ok {
		fmt.Fprintf(&output, "depends:\n%s\n", deps)
	}

	return []byte(output.String()), nil
}

// findNPMTarball finds a cached NPM tarball by package name and version
func (a *API) findNPMTarball(packageName, version string) (string, error) {
	typeDir := filepath.Join(a.cacheDir, "packages", "npm")
	if _, err := os.Stat(typeDir); os.IsNotExist(err) {
		return "", fmt.Errorf("NPM cache directory not found")
	}

	var foundPath string
	var checkedCount int
	err := filepath.Walk(typeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		// Look for .meta files (cached files are stored by hash, not by .tgz extension)
		if !strings.HasSuffix(path, ".meta") {
			return nil
		}

		// The actual cached file is the path without .meta
		cachedFilePath := strings.TrimSuffix(path, ".meta")

		// Check if the cached file exists and is not a directory
		if cachedFileInfo, err := os.Stat(cachedFilePath); err != nil || cachedFileInfo.IsDir() {
			return nil
		}

		// Load metadata to check package name and version
		metaData, err := os.ReadFile(path)
		if err != nil {
			return nil // No metadata, skip
		}

		var metadata struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(metaData, &metadata); err != nil {
			return nil // Invalid metadata, skip
		}

		checkedCount++

		// Check if this matches the requested package (exact match - NPM package names are case-sensitive)
		// Also check that this is not an index file
		if metadata.Name == packageName && metadata.Version == version {
			// Verify this is a tarball (not an index file) by checking URL path
			var fullMetadata struct {
				URLPath string `json:"url_path"`
				IsIndex bool   `json:"is_index"`
			}
			if err := json.Unmarshal(metaData, &fullMetadata); err == nil {
				// Skip if it's an index file or doesn't look like a tarball
				if fullMetadata.IsIndex || !strings.HasSuffix(fullMetadata.URLPath, ".tgz") {
					return nil
				}
			}

			foundPath = cachedFilePath
			log.Printf("[npm-metadata] Found match: %s@%s at %s", packageName, version, cachedFilePath)
			return filepath.SkipAll // Stop walking
		}

		// Debug: log first few mismatches to help troubleshoot
		if checkedCount <= 5 {
			log.Printf("[npm-metadata] Checked: %s@%s (looking for %s@%s)", metadata.Name, metadata.Version, packageName, version)
		}

		return nil
	})

	if err != nil && err != filepath.SkipAll {
		return "", err
	}

	if foundPath == "" {
		log.Printf("[npm-metadata] Package not found: looking for %s@%s (checked %d tarballs)", packageName, version, checkedCount)
		return "", fmt.Errorf("package %s@%s not found in cache", packageName, version)
	}

	return foundPath, nil
}

// extractPackageJSONFromTarball extracts package.json from an NPM tarball
func extractPackageJSONFromTarball(tarballPath string) (map[string]interface{}, error) {
	// Open the tarball file
	file, err := os.Open(tarballPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open tarball: %w", err)
	}
	defer util.DeferClose(file)

	// Create a gzip reader
	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer util.DeferClose(gzReader)

	// Create a tar reader
	tarReader := tar.NewReader(gzReader)

	// Look for package/package.json in the tarball
	// NPM tarballs have the structure: package/package.json
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Check if this is package.json
		// NPM tarballs typically have: package/package.json (root)
		// Dependencies are in: package/node_modules/.../package.json
		// We ONLY want the root package.json, not dependency package.json files
		if header.Typeflag == tar.TypeReg {
			name := header.Name
			// Only match the exact root package.json path
			if name == "package/package.json" {
				// Read the package.json content
				data, err := io.ReadAll(tarReader)
				if err != nil {
					return nil, fmt.Errorf("failed to read package.json: %w", err)
				}

				// Parse JSON
				var packageJSON map[string]interface{}
				if err := json.Unmarshal(data, &packageJSON); err != nil {
					return nil, fmt.Errorf("failed to parse package.json: %w", err)
				}

				return packageJSON, nil
			}
		}
	}

	return nil, fmt.Errorf("package.json not found in tarball")
}

func (a *API) handleDeletePackage(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/packages/")
	if path == "" {
		http.Error(w, "Package ID required", http.StatusBadRequest)
		return
	}

	// Delete the package file and metadata
	// For now, path is the cache path
	if err := deletePackage(path); err != nil {
		http.Error(w, fmt.Sprintf("Error deleting package: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

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

func (a *API) handleDockerRepositoryDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract repository name from path
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/docker/repositories/")
	if path == "" {
		http.Error(w, "Repository name required", http.StatusBadRequest)
		return
	}

	// Get tags for this repository
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

func (a *API) handleURLCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hostname := r.URL.Query().Get("hostname")
	search := r.URL.Query().Get("search")

	cfg := cli.Config{
		CacheDir:     a.cacheDir,
		StatsTracker: a.statsTracker,
	}

	entries, err := listCacheEntries(cfg, hostname)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error listing URL cache: %v", err), http.StatusInternalServerError)
		return
	}

	// Filter by search term if provided
	if search != "" {
		filtered := []cli.CacheEntry{}
		for _, entry := range entries {
			if strings.Contains(strings.ToLower(entry.Hostname), strings.ToLower(search)) ||
				strings.Contains(strings.ToLower(entry.Path), strings.ToLower(search)) {
				filtered = append(filtered, entry)
			}
		}
		entries = filtered
	}

	writeJSON(w, map[string]interface{}{
		"entries": entries,
		"total":   len(entries),
	})
}

func (a *API) handleFlushCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Type     string `json:"type"`
		Hostname string `json:"hostname"`
	}

	// Decode request body if present (allow empty body)
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
			return
		}
	}

	// Determine which package types to flush
	pmTypes := []packages.Type{packages.TypeAPK, packages.TypeAPT, packages.TypeNPM, packages.TypePyPI, packages.TypeGo}
	if req.Type != "" {
		pmTypes = []packages.Type{packages.Type(req.Type)}
	}

	// Step 1: Delete cached files for the specified type(s)
	packagesDir := filepath.Join(a.cacheDir, "packages")
	totalDeletedFiles := 0
	for _, pmType := range pmTypes {
		typeDir := filepath.Join(packagesDir, string(pmType))
		if _, err := os.Stat(typeDir); os.IsNotExist(err) {
			continue // No cache directory for this type
		}

		// Walk the directory and delete all files (but not .meta files separately, as they'll be deleted with their files)
		typeDeletedFiles := 0
		err := filepath.Walk(typeDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}

			// Skip .meta files - they'll be handled when we delete the main file
			if strings.HasSuffix(path, ".meta") {
				return nil
			}

			// Delete the file and its metadata
			if err := deletePackage(path); err != nil {
				log.Printf("Warning: Failed to delete cached file %s: %v", path, err)
			} else {
				typeDeletedFiles++
			}
			return nil
		})

		if err != nil {
			log.Printf("Warning: Error walking cache directory for %s: %v", pmType, err)
		} else {
			log.Printf("Deleted %d cached files for %s", typeDeletedFiles, pmType)
			totalDeletedFiles += typeDeletedFiles
		}
	}

	// Step 2: Clear the index database for the specified type(s)
	// This ensures the index doesn't have stale entries after cache flush
	if a.indexDB != nil {
		indexDir := filepath.Join(a.cacheDir, "index")
		for _, pmType := range pmTypes {
			typeDir := filepath.Join(indexDir, string(pmType))
			packagesPath := filepath.Join(typeDir, "packages.json")
			if err := os.Remove(packagesPath); err != nil && !os.IsNotExist(err) {
				log.Printf("Warning: Failed to clear index database for %s: %v", pmType, err)
			} else {
				log.Printf("Cleared index database for %s", pmType)
			}
		}
	}

	writeJSON(w, map[string]interface{}{
		"message":       "Cache flush completed",
		"type":          req.Type,
		"hostname":      req.Hostname,
		"deleted_files": totalDeletedFiles,
	})
}

func (a *API) handleAccessStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Return access statistics
	// This would aggregate stats from the statsTracker
	writeJSON(w, map[string]interface{}{
		"message": "Access stats endpoint - to be implemented",
	})
}

func (a *API) handleParseIndexes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if a.indexDB == nil {
		http.Error(w, "Index database not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Type string `json:"type"` // Optional: parse specific type, or all if empty
	}

	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
			return
		}
	}

	pmTypes := []packages.Type{packages.TypeAPK, packages.TypeAPT, packages.TypeNPM, packages.TypePyPI, packages.TypeGo}
	if req.Type != "" {
		// Parse only the requested type
		pmTypes = []packages.Type{packages.Type(req.Type)}
	}

	results := make(map[string]interface{})
	totalParsed := 0

	for _, pmType := range pmTypes {
		count, err := a.indexDB.ParseCachedIndexFiles(a.cacheDir, pmType)
		if err != nil {
			results[string(pmType)] = map[string]interface{}{
				"error": err.Error(),
				"count": count,
			}
		} else {
			results[string(pmType)] = map[string]interface{}{
				"count": count,
			}
			totalParsed += count
		}
	}

	writeJSON(w, map[string]interface{}{
		"message":      "Index parsing completed",
		"total_parsed": totalParsed,
		"results":      results,
	})
}

// Helper functions

func (a *API) listPackages(typeFilter, search string, showUncached bool) ([]PackageEntry, error) {
	cacheRoot := filepath.Join(a.cacheDir, "packages")
	pmTypes := []packages.Type{packages.TypeAPK, packages.TypeAPT, packages.TypeNPM, packages.TypePyPI, packages.TypeGo}

	var allEntries []PackageEntry

	// Build a map of cached packages by type/name/version for quick lookup
	cachedPackages := make(map[string]PackageEntry) // Key: "type:name@version:arch" or "type:name@version"

	for _, pmType := range pmTypes {
		if typeFilter != "" && string(pmType) != typeFilter {
			continue
		}

		// Get cached packages from filesystem
		typeDir := filepath.Join(cacheRoot, string(pmType))
		cachedEntries := listPackagesByType(typeDir, pmType, search)
		for _, entry := range cachedEntries {
			// Key is package name + architecture (not version, since there can be multiple versions)
			// Use full key with version for storage, but we'll match by name+arch when merging
			key := fmt.Sprintf("%s:%s@%s", entry.Type, entry.Name, entry.Version)
			if entry.Architecture != "" {
				key = fmt.Sprintf("%s:%s@%s:%s", entry.Type, entry.Name, entry.Version, entry.Architecture)
			}
			entry.Cached = true // Mark as cached
			cachedPackages[key] = entry
		}

		// Get available packages from index database (if available)
		if a.indexDB != nil {
			indexPkgs, err := a.indexDB.GetPackages(pmType)
			if err == nil {
				log.Printf("[api] Loaded %d packages from index database for type %s", len(indexPkgs), pmType)
				// Log group distribution for debugging
				if pmType == packages.TypeAPT && len(indexPkgs) > 0 {
					groupCounts := make(map[string]int)
					for _, pkg := range indexPkgs {
						group := pkg.Group
						if group == "" {
							group = "default"
						}
						groupCounts[group]++
					}
					log.Printf("[api] Index package groups for APT:")
					for group, count := range groupCounts {
						log.Printf("[api]   - %s: %d packages", group, count)
					}
				}
				// Build a map of index packages by name+architecture for quick lookup
				// Key format: type:name or type:name:arch
				indexByKey := make(map[string][]index.IndexPackage)
				for _, indexPkg := range indexPkgs {
					// Filter by search term if provided
					if search != "" {
						searchLower := strings.ToLower(search)
						if !strings.Contains(strings.ToLower(indexPkg.Name), searchLower) &&
							!strings.Contains(strings.ToLower(indexPkg.Version), searchLower) {
							continue
						}
					}

					// Key is package name + architecture (not version)
					key := fmt.Sprintf("%s:%s", indexPkg.Type, indexPkg.Name)
					if indexPkg.Architecture != "" {
						key = fmt.Sprintf("%s:%s:%s", indexPkg.Type, indexPkg.Name, indexPkg.Architecture)
					}
					indexByKey[key] = append(indexByKey[key], indexPkg)
				}

				// Now merge index metadata into cached packages by matching name+arch+version
				matchedCount := 0
				unmatchedCount := 0
				for cachedKey, cachedEntry := range cachedPackages {
					// Build lookup key (name+arch, no version)
					lookupKey := fmt.Sprintf("%s:%s", cachedEntry.Type, cachedEntry.Name)
					if cachedEntry.Architecture != "" {
						lookupKey = fmt.Sprintf("%s:%s:%s", cachedEntry.Type, cachedEntry.Name, cachedEntry.Architecture)
					}

					// Find matching index packages
					if indexPkgsList, exists := indexByKey[lookupKey]; exists {
						// Find the index package that matches the cached version
						found := false
						for _, indexPkg := range indexPkgsList {
							// Trim whitespace from both versions for comparison
							cachedVersion := strings.TrimSpace(cachedEntry.Version)
							indexVersion := strings.TrimSpace(indexPkg.Version)

							// For APT packages, normalize versions by stripping epoch prefix
							// The .deb filename may omit the epoch, but Packages file includes it
							var cachedNormalized, indexNormalized string
							if cachedEntry.Type == "apt" {
								cachedNormalized = normalizeDebianVersion(cachedVersion)
								indexNormalized = normalizeDebianVersion(indexVersion)
							} else {
								cachedNormalized = cachedVersion
								indexNormalized = indexVersion
							}

							if indexNormalized == cachedNormalized {
								// Exact match! Merge metadata
								cachedEntry.Description = indexPkg.Description
								cachedEntry.Dependencies = indexPkg.Dependencies
								cachedEntry.URL = indexPkg.URL
								// Only assign group if cached entry doesn't already have one
								// This preserves groups from cached metadata
								if cachedEntry.Group == "" {
									cachedEntry.Group = indexPkg.Group
								}
								if cachedEntry.Architecture == "" {
									cachedEntry.Architecture = indexPkg.Architecture
								}
								cachedEntry.Cached = true
								cachedPackages[cachedKey] = cachedEntry
								matchedCount++
								if matchedCount <= 5 || unmatchedCount <= 5 {
									log.Printf("[api] Merged index metadata for %s:%s@%s:%s -> group: %s", cachedEntry.Type, cachedEntry.Name, cachedEntry.Version, cachedEntry.Architecture, cachedEntry.Group)
								}
								found = true
								break
							}
						}
						if !found && unmatchedCount < 10 {
							// Log detailed mismatch info - this should NOT happen if package was downloaded
							// If a package was downloaded, its exact version MUST exist in the index
							cachedVersion := strings.TrimSpace(cachedEntry.Version)
							var indexVersions []string
							var indexNormalized []string
							for _, pkg := range indexPkgsList {
								pkgVersion := strings.TrimSpace(pkg.Version)
								indexVersions = append(indexVersions, fmt.Sprintf("'%s'", pkgVersion))
								if cachedEntry.Type == "apt" {
									indexNormalized = append(indexNormalized, fmt.Sprintf("'%s'", normalizeDebianVersion(pkgVersion)))
								}
							}
							if cachedEntry.Type == "apt" {
								cachedNormalized := normalizeDebianVersion(cachedVersion)
								log.Printf("[api] ERROR: Version mismatch for %s (lookup: %s): cached='%s' (normalized='%s'), index has %d entries with versions: %v (normalized: %v)",
									cachedKey, lookupKey, cachedVersion, cachedNormalized, len(indexPkgsList), indexVersions, indexNormalized)
							} else {
								log.Printf("[api] ERROR: Version mismatch for %s (lookup: %s): cached='%s' (len=%d, bytes=%v), index has %d entries with versions: %v",
									cachedKey, lookupKey, cachedVersion, len(cachedVersion), []byte(cachedVersion), len(indexPkgsList), indexVersions)
							}
							unmatchedCount++
						}
					} else {
						// Log when we can't find a match (for debugging)
						if unmatchedCount < 10 {
							log.Printf("[api] No index match for %s:%s@%s:%s (lookup key: %s)", cachedEntry.Type, cachedEntry.Name, cachedEntry.Version, cachedEntry.Architecture, lookupKey)
							unmatchedCount++
						}
					}
				}
				log.Printf("[api] Merged %d cached packages with index metadata, %d unmatched", matchedCount, unmatchedCount)

				// Add uncached packages from index if requested
				// Limit to 1000 uncached packages to avoid performance issues
				if showUncached {
					// Build a set of cached package lookup keys for O(1) lookup
					cachedLookupKeys := make(map[string]bool)
					for _, cachedEntry := range cachedPackages {
						checkKey := fmt.Sprintf("%s:%s", cachedEntry.Type, cachedEntry.Name)
						if cachedEntry.Architecture != "" {
							checkKey = fmt.Sprintf("%s:%s:%s", cachedEntry.Type, cachedEntry.Name, cachedEntry.Architecture)
						}
						cachedLookupKeys[checkKey] = true
					}

					uncachedCount := 0
					const maxUncached = 1000
					for lookupKey, indexPkgsList := range indexByKey {
						if uncachedCount >= maxUncached {
							break
						}

						// Check if we already have any version of this package cached (O(1) lookup)
						if cachedLookupKeys[lookupKey] {
							continue
						}

						// Add all versions of this uncached package (up to limit)
						for _, indexPkg := range indexPkgsList {
							if uncachedCount >= maxUncached {
								break
							}
							entry := PackageEntry{
								ID:           fmt.Sprintf("index:%s:%s@%s", indexPkg.Type, indexPkg.Name, indexPkg.Version),
								Type:         indexPkg.Type,
								Name:         indexPkg.Name,
								Version:      indexPkg.Version,
								Architecture: indexPkg.Architecture,
								Group:        indexPkg.Group,
								Size:         indexPkg.Size,
								Cached:       false, // Not cached yet
								Description:  indexPkg.Description,
								Dependencies: indexPkg.Dependencies,
								URL:          indexPkg.URL,
								CachedAt:     indexPkg.IndexedAt,
							}
							// Use a unique key for each version
							versionKey := fmt.Sprintf("%s:%s@%s", indexPkg.Type, indexPkg.Name, indexPkg.Version)
							if indexPkg.Architecture != "" {
								versionKey = fmt.Sprintf("%s:%s@%s:%s", indexPkg.Type, indexPkg.Name, indexPkg.Version, indexPkg.Architecture)
							}
							cachedPackages[versionKey] = entry
							uncachedCount++
						}
					}
					if uncachedCount >= maxUncached {
						log.Printf("[api] Limited uncached packages to %d (more available in index)", maxUncached)
					} else {
						log.Printf("[api] Added %d uncached packages from index", uncachedCount)
					}
				}
			}
		}
	}

	// Convert map to slice
	for _, entry := range cachedPackages {
		allEntries = append(allEntries, entry)
	}

	return allEntries, nil
}

func listPackagesByType(typeDir string, pmType packages.Type, search string) []PackageEntry {
	var entries []PackageEntry

	// Check if directory exists
	if _, err := os.Stat(typeDir); os.IsNotExist(err) {
		log.Printf("[listPackagesByType] Directory does not exist: %s", typeDir)
		return entries
	}

	// Create a handler to check if a path is an index file
	var handler packages.Handler
	switch pmType {
	case packages.TypeAPK:
		handler = &packages.APKHandler{}
	case packages.TypeAPT:
		handler = &packages.APTHandler{}
	case packages.TypeNPM:
		handler = &packages.NPMHandler{}
	case packages.TypePyPI:
		handler = &packages.PyPIHandler{}
	case packages.TypeGo:
		handler = &packages.GoHandler{}
	default:
		// No handler, skip index filtering
	}

	var filesChecked, indexFilesSkipped, metadataErrors int

	// Walk the cache directory
	if err := filepath.Walk(typeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		if strings.HasSuffix(path, ".meta") {
			return nil
		}

		filesChecked++

		metadata, err := loadPackageMetadata(path)
		if err != nil {
			metadataErrors++
			return nil
		}

		// Skip index files - check metadata first (most reliable)
		if metadata.IsIndex {
			indexFilesSkipped++
			return nil
		}

		// Fallback: check using handler's IsIndex method (for older cached files without IsIndex field)
		if handler != nil && handler.IsIndex(metadata.URLPath) {
			indexFilesSkipped++
			return nil
		}

		// Filter by search term
		if search != "" {
			searchLower := strings.ToLower(search)
			if !strings.Contains(strings.ToLower(metadata.Name), searchLower) &&
				!strings.Contains(strings.ToLower(metadata.Version), searchLower) {
				return nil
			}
		}

		// For Go modules, only show .zip files
		if pmType == packages.TypeGo && !strings.HasSuffix(metadata.URLPath, ".zip") {
			return nil
		}

		// For PyPI, skip .metadata files (they're metadata, not actual packages)
		if pmType == packages.TypePyPI && strings.HasSuffix(metadata.URLPath, ".metadata") {
			return nil
		}

		// Derive group from module path for Go packages if not set or if it contains colons (old format)
		group := metadata.Group
		if pmType == packages.TypeGo && metadata.Name != "" {
			// For Go packages, if group is empty or contains colons (old format), derive it from the module path
			if group == "" || strings.Contains(group, ":") {
				// For Go packages, the group should be the parent path (without the package name)
				// e.g., "dario.cat/mergo" -> group: "dario.cat"
				// e.g., "github.com/user/repo" -> group: "github.com/user"
				moduleParts := strings.Split(metadata.Name, "/")
				if len(moduleParts) >= 2 {
					// Use all parts except the last as the group (parent path)
					// Use / as separator for Go packages
					group = strings.Join(moduleParts[:len(moduleParts)-1], "/")
				} else {
					// Fallback: use the full name if we can't split it
					group = metadata.Name
				}
			}
		}

		entry := PackageEntry{
			ID:            path, // Use path as ID for now
			Type:          string(pmType),
			Name:          metadata.Name,
			Version:       metadata.Version,
			Architecture:  metadata.Architecture,
			Group:         group, // Set group from metadata or derive for Go
			Size:          info.Size(),
			CachedAt:      metadata.CachedAt,
			FetchDuration: metadata.FetchDuration.Milliseconds(),
			AccessCount:   metadata.AccessCount,
		}

		entries = append(entries, entry)
		return nil
	}); err != nil {
		log.Printf("Error walking directory: %v", err)
	}

	if filesChecked > 0 {
		log.Printf("[listPackagesByType] %s: checked %d files, skipped %d index files, %d metadata errors, found %d packages", pmType, filesChecked, indexFilesSkipped, metadataErrors, len(entries))
	}

	return entries
}

func loadPackageMetadata(cachePath string) (packages.Metadata, error) {
	metaPath := cachePath + ".meta"
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return packages.Metadata{}, err
	}

	var metadata packages.Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return packages.Metadata{}, err
	}

	return metadata, nil
}

func sortPackages(entries []PackageEntry, sortBy string) []PackageEntry {
	// Create a copy to avoid modifying original
	sorted := make([]PackageEntry, len(entries))
	copy(sorted, entries)

	switch sortBy {
	case "name":
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Name < sorted[j].Name
		})
	case "size":
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Size > sorted[j].Size
		})
	case "cached_at":
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].CachedAt.After(sorted[j].CachedAt)
		})
	case "access_count":
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].AccessCount > sorted[j].AccessCount
		})
	default:
		// Default: sort by name (then version if names are equal)
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].Name != sorted[j].Name {
				return sorted[i].Name < sorted[j].Name
			}
			// If names are equal, sort by version
			return sorted[i].Version < sorted[j].Version
		})
	}

	return sorted
}

func (a *API) getDockerImageCount() int {
	// This is called from stats summary, which doesn't have a request context
	// Use filesystem-based approach as fallback
	repos, err := a.listDockerRepositories(nil)
	if err != nil {
		return 0
	}
	return len(repos)
}

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
			"name":      repo,
			"tag_count": len(tags),
			"size":      int64(0),
			"date":      "",
			"slices":    0,
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

		// Try all tags to find one with valid size information
		// Also check digest manifests in the repo directory
		repoDir := filepath.Join(manifestDir, registryID, safeRepo)
		allManifests := []string{}

		// Collect all tag manifests
		for tag := range tags {
			allManifests = append(allManifests, filepath.Join(repoDir, tag))
		}

		// Also check for digest manifests in the repo directory
		if entries, err := os.ReadDir(repoDir); err == nil {
			for _, entry := range entries {
				if !entry.IsDir() && strings.HasPrefix(entry.Name(), "sha256:") {
					allManifests = append(allManifests, filepath.Join(repoDir, entry.Name()))
				}
			}
		}

		for _, manifestPath := range allManifests {
			if info, err := os.Stat(manifestPath); err == nil {
				// Track latest modification time
				if info.ModTime().After(latestTime) {
					latestTime = info.ModTime()
				}

				// Read manifest to get layer count and sizes
				manifestData, err := os.ReadFile(manifestPath)
				if err != nil {
					continue
				}

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

				// If it's a manifest list, try to resolve to platform manifests
				if isManifestList && len(manifestList.Manifests) > 0 {
					// Try each platform manifest in the list
					for _, m := range manifestList.Manifests {
						digestManifestPath := filepath.Join(repoDir, m.Digest)
						if digestData, err := os.ReadFile(digestManifestPath); err == nil {
							manifestData = digestData
							break // Use the first one we find
						}
					}
					// If we couldn't find any platform manifests, skip this manifest list
					if isManifestList {
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

		// Set the calculated values
		repoData["size"] = totalSize
		repoData["slices"] = maxLayers
		if !latestTime.IsZero() {
			repoData["date"] = latestTime.Format("2006-01-02 15:04:05")
		}

		result = append(result, repoData)
	}

	return result, nil
}

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
				// Fall back to filesystem on error
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

func deletePackage(path string) error {
	// Delete the package file
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}

	// Delete metadata file
	metaPath := path + ".meta"
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

// listCacheEntries is a wrapper around cli.ListCacheEntries
func listCacheEntries(cfg cli.Config, hostname string) ([]cli.CacheEntry, error) {
	return cli.ListCacheEntries(cfg, hostname)
}

// countPackagesByType quickly counts packages and calculates total size without loading full metadata
func (a *API) countPackagesByType(typeDir string, pmType packages.Type) (count int, totalSize int64, err error) {
	// Create a minimal config for handler creation (only needed for IsIndex check)
	config := packages.Config{
		Type:         pmType,
		UpstreamURL:  "", // Not needed for counting
		MinCacheSize: 0,
	}

	// Create a handler to check if a path is an index file
	var handler packages.Handler
	switch pmType {
	case packages.TypeAPK:
		handler = packages.NewAPKHandler(config, nil, nil, nil, nil)
	case packages.TypeAPT:
		handler = packages.NewAPTHandler(config, nil, nil, nil, nil)
	case packages.TypeNPM:
		handler = packages.NewNPMHandler(config, nil, nil, nil, nil)
	case packages.TypePyPI:
		handler = packages.NewPyPIHandler(config, nil, nil, nil, nil)
	case packages.TypeGo:
		handler = packages.NewGoHandler(config, nil, nil, nil, nil)
	default:
		return 0, 0, fmt.Errorf("unknown package type: %s", pmType)
	}

	// Walk the directory tree
	err = filepath.Walk(typeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		if info.IsDir() {
			return nil
		}

		// Skip metadata files
		if strings.HasSuffix(path, ".json") || strings.HasSuffix(path, ".meta") {
			return nil
		}

		// Check if this is an index file by reading metadata (which has the original URL path)
		metaPath := path + ".meta"
		isIndex := false
		if metaData, err := os.ReadFile(metaPath); err == nil {
			var metadata struct {
				URLPath string `json:"url_path"`
				IsIndex bool   `json:"is_index"`
			}
			if json.Unmarshal(metaData, &metadata) == nil {
				// Use metadata.IsIndex if available (most reliable)
				isIndex = metadata.IsIndex
				// Fallback: check using handler's IsIndex with the original URL path
				if !isIndex && handler != nil && metadata.URLPath != "" {
					isIndex = handler.IsIndex(metadata.URLPath)
				}
			}
		} else {
			// No metadata - use file extension and handler as fallback
			// For NPM: .tgz files are packages, everything else might be an index
			if pmType == packages.TypeNPM {
				isIndex = !strings.HasSuffix(path, ".tgz")
			} else if handler != nil {
				// For other types, try to guess from filename (not ideal, but better than skipping)
				// We can't use handler.IsIndex without the URL path, so we'll be conservative
				// and only skip obvious index files based on extension
				filename := filepath.Base(path)
				if pmType == packages.TypeAPT && !strings.HasSuffix(filename, ".deb") {
					isIndex = true
				} else if pmType == packages.TypeAPK && !strings.HasSuffix(filename, ".apk") {
					isIndex = true
				} else if pmType == packages.TypePyPI && !strings.HasSuffix(filename, ".whl") && !strings.HasSuffix(filename, ".tar.gz") {
					isIndex = true
				} else if pmType == packages.TypeGo && !strings.HasSuffix(filename, ".zip") {
					isIndex = true
				}
			}
		}

		// Skip index files
		if isIndex {
			return nil
		}

		// Count this as a package
		count++
		totalSize += info.Size()
		return nil
	})

	return count, totalSize, err
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	} else if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	} else if d < time.Hour {
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
	return fmt.Sprintf("%.1fh", d.Hours())
}
func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ") // Pretty print for debugging
	if err := encoder.Encode(data); err != nil {
		log.Printf("Error encoding JSON: %v", err)
	}
	// Flush the response if possible
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Export handlers - stream gzipped tarballs of cache contents

// handleExportFull exports the entire cache
func (a *API) handleExportFull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.handleExport(w, r, "full")
}

// handleExportDocker exports only Docker-related cache
func (a *API) handleExportDocker(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.handleExport(w, r, "docker")
}

// handleExportPackages exports only package manager artifacts
func (a *API) handleExportPackages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.handleExport(w, r, "packages")
}

// handleExportRaw exports only raw file cache
func (a *API) handleExportRaw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.handleExport(w, r, "raw")
}

// handleExport handles the actual export logic
func (a *API) handleExport(w http.ResponseWriter, r *http.Request, exportType string) {
	// Check if compression is requested (default: no compression for Docker blobs)
	// Docker blobs are already compressed, so gzip adds overhead without benefit
	compress := r.URL.Query().Get("compress") == "true"

	// Check if OCI format is requested (for Docker exports)
	useOCILayout := r.URL.Query().Get("format") == "oci" && exportType == "docker"

	// Set headers
	var filename string
	if compress {
		filename = fmt.Sprintf("roxxy-cache-%s.tar.gz", exportType)
		w.Header().Set("Content-Type", "application/gzip")
	} else {
		filename = fmt.Sprintf("roxxy-cache-%s.tar", exportType)
		w.Header().Set("Content-Type", "application/x-tar")
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))

	var tarWriter *tar.Writer
	var gzipWriter *gzip.Writer

	// Counter sits immediately below the tar.Writer so its byte count
	// is the canonical "current tar position". The previous manual
	// `currentOffset += 512`-after-WriteHeader scheme silently drifted
	// for files large enough to require PAX extended headers
	// (Format=PAX + Size > ~8GB), where WriteHeader actually emits
	// 1024 bytes (extended header + real header), not 512. The index
	// then pointed 512 bytes behind every entry written after such a
	// file, and OpenBlob's tar.Reader.Next() landed mid-padding and
	// returned "invalid tar header". Trust the counter; the manual
	// arithmetic remains below for log continuity but is overridden
	// at every offset-capture site via `snapOffset()`.
	counter := &countingWriter{}
	if compress {
		// Create gzip writer (streams to response)
		gzipWriter = gzip.NewWriter(w)
		defer util.DeferClose(gzipWriter)
		counter.w = gzipWriter
		tarWriter = tar.NewWriter(counter)
	} else {
		// Write tar directly (no compression)
		counter.w = w
		tarWriter = tar.NewWriter(counter)
	}
	defer util.DeferClose(tarWriter)
	// For Docker exports with OCI format, use OCI layout transformation
	if useOCILayout {
		if err := a.walkAndAddToTarOCI(tarWriter, counter, a.cacheDir, compress); err != nil {
			log.Printf("Export failed: %v", err)
			// Note: Can't change status code after streaming started
			return
		}
	} else {
		// Walk cache directory and stream files (original format)
		if err := a.walkAndAddToTar(tarWriter, counter, a.cacheDir, exportType, compress); err != nil {
			log.Printf("Export failed: %v", err)
			// Note: Can't change status code after streaming started
			return
		}
	}

	// Flush tar writer
	if err := tarWriter.Close(); err != nil {
		log.Printf("Error closing tar writer: %v", err)
	}

	// Flush gzip writer if compression was used
	if gzipWriter != nil {
		if err := gzipWriter.Close(); err != nil {
			log.Printf("Error closing gzip writer: %v", err)
		}
	}
}

// shouldIncludePath determines if a path should be included in the export based on export type
func shouldIncludePath(relPath string, exportType string) bool {
	switch exportType {
	case "full":
		return true // Include everything

	case "docker":
		// Check if path is the directory itself or starts with the directory path
		// Exclude index/ and access-stats.json as they can be regenerated
		return relPath == "docker-blobs" ||
			strings.HasPrefix(relPath, "docker-blobs/") ||
			relPath == "docker-manifests" ||
			strings.HasPrefix(relPath, "docker-manifests/") ||
			relPath == "refcount" ||
			strings.HasPrefix(relPath, "refcount/")

	case "packages":
		// Exclude index/ and access-stats.json as they can be regenerated
		return relPath == "packages" ||
			strings.HasPrefix(relPath, "packages/") ||
			relPath == "refcount" ||
			strings.HasPrefix(relPath, "refcount/")

	case "raw":
		// Exclude index/ and access-stats.json as they can be regenerated
		return relPath == "raw" ||
			strings.HasPrefix(relPath, "raw/") ||
			relPath == "refcount" ||
			strings.HasPrefix(relPath, "refcount/")

	default:
		return false
	}
}

// walkAndAddToTar walks the cache directory and adds files to the tar archive
// Files are written in a specific order to optimize tarball reading:
// 1. Directories first
// 2. Manifests (files that reference other files) before blobs (leaf files)
// 3. Within each category, alphabetical order
func (a *API) walkAndAddToTar(tarWriter *tar.Writer, counter *countingWriter, cacheDir string, exportType string, compress bool) error {
	// Collect all entries first, then sort and write in order
	type tarEntry struct {
		path          string
		relPath       string
		info          os.FileInfo
		isDir         bool
		isManifest    bool
		isIndex       bool // OCI Image Index (multi-platform)
		isManifestDir bool // Directory that contains manifests
		isBlobDir     bool // Directory that contains blobs
	}

	var entries []tarEntry

	// First pass: collect all entries
	err := filepath.Walk(cacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Log but continue - some files may not be accessible
			log.Printf("Error accessing %s: %v", path, err)
			return nil
		}

		// Calculate relative path from cache directory
		relPath, err := filepath.Rel(cacheDir, path)
		if err != nil {
			return err
		}

		// Skip the root directory itself
		if relPath == "." {
			return nil
		}

		// Check if this path should be included
		shouldInclude := shouldIncludePath(relPath, exportType)
		if !shouldInclude {
			// If it's a directory, skip it entirely
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Debug: log what we're including (first few entries)
		if len(entries) < 10 {
			log.Printf("Export: Including %s (dir=%v)", relPath, info.IsDir())
		}

		// Determine if this is a manifest (references other files)
		isManifest := strings.HasPrefix(relPath, "docker-manifests/") &&
			!strings.HasSuffix(relPath, ".meta") &&
			!info.IsDir()

		// Check if this is an OCI Image Index (multi-platform manifest)
		// We need to peek at the file to determine if it's an index
		isIndex := false
		if isManifest {
			// Quick check: read first few bytes to see if it contains "manifests" array
			// This is a heuristic - indexes have "manifests" field, single-platform have "layers"
			if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
				// Check if it's JSON and contains "manifests" (index) vs "layers" (single-platform)
				dataStr := string(data)
				if strings.Contains(dataStr, `"manifests"`) && !strings.Contains(dataStr, `"layers"`) {
					isIndex = true
				}
			}
		}

		// Determine if this is a manifest directory or blob directory
		isManifestDir := info.IsDir() && strings.HasPrefix(relPath, "docker-manifests/")
		isBlobDir := info.IsDir() && strings.HasPrefix(relPath, "docker-blobs/")

		entries = append(entries, tarEntry{
			path:          path,
			relPath:       relPath,
			info:          info,
			isDir:         info.IsDir(),
			isManifest:    isManifest,
			isIndex:       isIndex,
			isManifestDir: isManifestDir,
			isBlobDir:     isBlobDir,
		})

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to walk cache directory: %w", err)
	}

	log.Printf("Export: Collected %d entries for export type %s (cache dir: %s)", len(entries), exportType, cacheDir)
	if len(entries) == 0 {
		log.Printf("Export: No entries found! Cache dir: %s, export type: %s", cacheDir, exportType)
		// List what's actually in the cache directory for debugging
		if entries, err := os.ReadDir(cacheDir); err == nil {
			log.Printf("Export: Cache directory contains: %d entries", len(entries))
			for i, entry := range entries {
				if i < 10 {
					log.Printf("Export:   - %s (dir=%v)", entry.Name(), entry.IsDir())
				}
			}
		}
		return fmt.Errorf("no files found to export for type %s", exportType)
	}

	// Sort entries with proper ordering using priority:
	// Priority 1: Manifest directories (docker-manifests/)
	// Priority 2: Other directories
	// Priority 3: Blob directories (docker-blobs/) - after other dirs but before files
	// Priority 4: Multi-platform manifests (OCI Image Index) - come first
	// Priority 5: Single-platform manifests - come after indexes
	// Priority 6: Blob files
	// Priority 7: Everything else
	// Within same priority, sort alphabetically
	getPriority := func(e tarEntry) int {
		if e.isManifestDir {
			return 1
		}
		if e.isDir && !e.isBlobDir {
			return 2
		}
		if e.isBlobDir {
			return 3
		}
		if e.isIndex {
			return 4 // Multi-platform indexes come first
		}
		if e.isManifest {
			return 5 // Single-platform manifests come after indexes
		}
		if strings.HasPrefix(e.relPath, "docker-blobs/") {
			return 6
		}
		return 7
	}

	sort.Slice(entries, func(i, j int) bool {
		ei, ej := entries[i], entries[j]
		pi, pj := getPriority(ei), getPriority(ej)

		if pi != pj {
			return pi < pj
		}

		// Same priority - sort alphabetically
		return ei.relPath < ej.relPath
	})

	// Track current position in the tarball (for offset calculation)
	// Start at 0, all tar entries are 512-byte aligned
	currentOffset := int64(0)
	log.Printf("Export: Starting offset tracking at position 0")

	// Track offsets for index file (only for non-compressed exports)
	// For HTTP streaming, we can't write the index at the end, but we can still track
	fileOffsets := make(map[string]int64)

	// Second pass: write entries in sorted order
	var filesWritten, dirsWritten int
	for _, entry := range entries {
		// Create tar header
		header, err := tar.FileInfoHeader(entry.info, "")
		if err != nil {
			return fmt.Errorf("failed to create tar header for %s: %w", entry.path, err)
		}

		// Set the name in the archive (use relPath directly, no cache/ prefix)
		header.Name = entry.relPath

		// Record offset BEFORE writing header - this is where the tar entry starts.
		// Flush pending padding from the previous entry then read the byte
		// counter — the exact next-entry offset. The old `currentOffset += 512`
		// scheme silently miscounted under PAX extended headers (Format=PAX
		// + Size > ~8GB), where WriteHeader emits 1024 bytes not 512.
		_ = tarWriter.Flush()
		entryOffset := counter.n
		currentOffset = entryOffset
		log.Printf("Export: [%s] Recording offset BEFORE WriteHeader: %d", entry.relPath, entryOffset)

		// Write header - tar headers are always 512 bytes
		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("failed to write tar header for %s: %w", entry.path, err)
		}
		// Update tracked offset - WriteHeader writes exactly 512 bytes
		currentOffset += 512
		log.Printf("Export: [%s] After WriteHeader: offset updated to %d (wrote 512 bytes)", entry.relPath, currentOffset)

		// Record offset for this entry (the position where the header starts)
		fileOffsets[entry.relPath] = entryOffset
		log.Printf("Export: [%s] Stored offset %d in fileOffsets map", entry.relPath, entryOffset)

		// If it's a directory, we're done
		if entry.isDir {
			dirsWritten++
			log.Printf("Export: [%s] Directory entry complete, final offset: %d", entry.relPath, currentOffset)
			continue
		}

		// Open and copy file contents
		file, err := os.Open(entry.path)
		if err != nil {
			log.Printf("Warning: Failed to open %s: %v", entry.path, err)
			continue // Continue with other files
		}

		// Get file size for logging
		fileInfo, _ := file.Stat()
		fileSize := fileInfo.Size()
		log.Printf("Export: [%s] Starting file copy, file size: %d bytes, offset before copy: %d", entry.relPath, fileSize, currentOffset)

		// Copy file contents to tar
		// tar.Writer automatically handles padding to 512-byte boundaries
		written, err := io.Copy(tarWriter, file)
		if err != nil {
			util.Logerr(file.Close())
			return fmt.Errorf("failed to write file %s to tar: %w", entry.path, err)
		}
		util.Logerr(file.Close())

		// Update tracked offset: add data written, then padding to next 512-byte boundary
		// Tar format pads file data to 512-byte boundaries
		currentOffset += written
		padding := (512 - (written % 512)) % 512
		if padding > 0 {
			currentOffset += padding
		}
		log.Printf("Export: [%s] After file copy: wrote %d bytes, padding: %d bytes, offset updated to %d (512-byte aligned)",
			entry.relPath, written, padding, currentOffset)

		filesWritten++
		log.Printf("Export: [%s] File entry complete, final tracked offset: %d", entry.relPath, currentOffset)
	}

	// Write index file at the end (for all exports, including HTTP streaming)
	// The index allows fast seeking when reading the tarball later
	if len(fileOffsets) > 0 {
		// Ensure current offset is 512-byte aligned before writing index header
		if currentOffset%512 != 0 {
			currentOffset = ((currentOffset + 511) / 512) * 512
		}

		log.Printf("Export: Writing index file, current tracked offset: %d, %d entries to index", currentOffset, len(fileOffsets))
		// Convert byte offsets to block numbers (divide by 512) to save space
		// Block numbers are stored as 32-bit integers, supporting tarballs up to 2TB
		blockOffsets := make(map[string]int32)
		for path, offset := range fileOffsets {
			// Convert byte offset to block number (offset / 512)
			// Offsets are already 512-byte aligned, so this is exact
			blockNum := int32(offset / 512)
			blockOffsets[path] = blockNum
		}
		index := map[string]interface{}{
			"version":      2, // Version 2: block numbers instead of byte offsets
			"file_offsets": blockOffsets,
		}
		indexData, err := json.Marshal(index)
		if err == nil {
			log.Printf("Export: Index JSON size: %d bytes (uncompressed)", len(indexData))

			// Compress the index with gzip
			var compressedData bytes.Buffer
			gzWriter := gzip.NewWriter(&compressedData)
			if _, err := gzWriter.Write(indexData); err == nil {
				if err := gzWriter.Close(); err == nil {
					compressedBytes := compressedData.Bytes()
					log.Printf("Export: Index compressed size: %d bytes (%.1f%% reduction)",
						len(compressedBytes), 100.0*(1.0-float64(len(compressedBytes))/float64(len(indexData))))
					indexData = compressedBytes
				}
			}

			// Write index file entry
			indexHeader := &tar.Header{
				Name:     ".roxxy-index.json.gz",
				Size:     int64(len(indexData)),
				Mode:     0644,
				ModTime:  time.Now(),
				Typeflag: tar.TypeReg,
			}
			indexOffsetBeforeHeader := currentOffset
			log.Printf("Export: Writing index header at offset %d (512-byte aligned)", indexOffsetBeforeHeader)
			if err := tarWriter.WriteHeader(indexHeader); err == nil {
				currentOffset += 512 // Header size
				log.Printf("Export: After index WriteHeader: offset updated to %d", currentOffset)
				if _, err := tarWriter.Write(indexData); err == nil {
					currentOffset += int64(len(indexData))
					// Add padding to next 512-byte boundary
					padding := (512 - (int64(len(indexData)) % 512)) % 512
					if padding > 0 {
						currentOffset += padding
					}
					log.Printf("Export: After index data write: wrote %d bytes, padding: %d bytes, offset updated to %d (512-byte aligned)",
						len(indexData), padding, currentOffset)
					log.Printf("Export: Wrote index file with %d entries, final tracked offset: %d", len(fileOffsets), currentOffset)

					// Log a sample of the offsets stored in the index for debugging
					sampleCount := 0
					blobCount := 0
					for path, offset := range fileOffsets {
						if sampleCount < 5 {
							log.Printf("Export: Index sample - %s -> offset %d", path, offset)
							sampleCount++
						}
						if strings.HasPrefix(path, "docker-blobs/") {
							blobCount++
							if blobCount <= 10 {
								log.Printf("Export: Blob in index - %s -> offset %d", path, offset)
							}
						}
					}
					log.Printf("Export: Total blob entries in index: %d", blobCount)
				} else {
					log.Printf("Export: Error writing index data: %v", err)
				}
			} else {
				log.Printf("Export: Error writing index header: %v", err)
			}
		} else {
			log.Printf("Export: Error marshaling index: %v", err)
		}
	} else {
		log.Printf("Export: No file offsets to write to index")
	}

	log.Printf("Export: Wrote %d directories and %d files", dirsWritten, filesWritten)
	return nil
}

// walkAndAddToTarOCI creates an OCI-compliant image layout tarball from Docker cache
// This transforms the cache structure into OCI format while keeping the offset index
func (a *API) walkAndAddToTarOCI(tarWriter *tar.Writer, counter *countingWriter, cacheDir string, compress bool) error {
	manifestDir := filepath.Join(cacheDir, "docker-manifests")
	blobDir := filepath.Join(cacheDir, "docker-blobs")

	currentOffset := int64(0)
	fileOffsets := make(map[string]int64)

	// Write oci-layout file (required for OCI image layout)
	ociLayout := `{"imageLayoutVersion":"1.0.0"}`
	hdr := &tar.Header{
		Name:    "oci-layout",
		Size:    int64(len(ociLayout)),
		Mode:    0644,
		ModTime: time.Now(),
		Format:  tar.FormatPAX,
	}
	_ = tarWriter.Flush()
	currentOffset = counter.n
	fileOffsets["oci-layout"] = currentOffset
	if err := tarWriter.WriteHeader(hdr); err != nil {
		return fmt.Errorf("failed to write oci-layout: %w", err)
	}
	if _, err := tarWriter.Write([]byte(ociLayout)); err != nil {
		return fmt.Errorf("failed to write oci-layout: %w", err)
	}

	// Collect all manifests and build OCI index
	type manifestEntry struct {
		Digest      string
		Size        int64
		MediaType   string
		Data        []byte
		Annotations map[string]string
	}

	var manifests []manifestEntry
	allBlobDigests := make(map[string]bool)

	// Scan manifest directory to find all manifests
	err := filepath.Walk(manifestDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		// Skip .meta files
		if strings.HasSuffix(path, ".meta") {
			return nil
		}

		// Read manifest
		data, err := os.ReadFile(path)
		if err != nil {
			return nil // Skip files we can't read
		}

		// Calculate digest
		hash := sha256.Sum256(data)
		digest := "sha256:" + hex.EncodeToString(hash[:])

		// Determine media type
		var manifestJSON map[string]interface{}
		mediaType := "application/vnd.oci.image.manifest.v1+json"
		if err := json.Unmarshal(data, &manifestJSON); err == nil {
			if mt, ok := manifestJSON["mediaType"].(string); ok && mt != "" {
				mediaType = mt
			}
		}

		// Extract blob digests from manifest
		// Check if this is a multi-platform index (has "manifests" array)
		if manifests, ok := manifestJSON["manifests"].([]interface{}); ok {
			// This is an OCI image index - extract digests from referenced manifests
			for _, m := range manifests {
				if mEntry, ok := m.(map[string]interface{}); ok {
					if d, ok := mEntry["digest"].(string); ok {
						allBlobDigests[d] = true // The manifest itself is a blob
					}
				}
			}
		} else {
			// Single-platform manifest - extract config and layer digests
			if config, ok := manifestJSON["config"].(map[string]interface{}); ok {
				if d, ok := config["digest"].(string); ok {
					allBlobDigests[d] = true
				}
			}
			if layers, ok := manifestJSON["layers"].([]interface{}); ok {
				for _, layer := range layers {
					if l, ok := layer.(map[string]interface{}); ok {
						if d, ok := l["digest"].(string); ok {
							allBlobDigests[d] = true
						}
					}
				}
			}
		}

		// Extract image reference from path for annotation
		relPath, _ := filepath.Rel(manifestDir, path)
		parts := strings.Split(relPath, string(filepath.Separator))
		var imageRef string
		if len(parts) >= 3 {
			registryID := parts[0]
			safeRepo := parts[1]
			reference := parts[2]
			repoName := util.UnsafeRepoName(safeRepo)
			imageRef = fmt.Sprintf("%s/%s:%s", registryID, repoName, reference)
		}

		annotations := make(map[string]string)
		if imageRef != "" {
			annotations["org.opencontainers.image.ref.name"] = imageRef
		}

		manifests = append(manifests, manifestEntry{
			Digest:      digest,
			Size:        int64(len(data)),
			MediaType:   mediaType,
			Data:        data,
			Annotations: annotations,
		})

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to scan manifests: %w", err)
	}

	// Write all manifests as blobs
	for _, m := range manifests {
		digestPart := strings.TrimPrefix(m.Digest, "sha256:")
		blobPath := fmt.Sprintf("blobs/sha256/%s", digestPart)

		_ = tarWriter.Flush()
		currentOffset = counter.n
		fileOffsets[blobPath] = currentOffset

		hdr := &tar.Header{
			Name:    blobPath,
			Size:    m.Size,
			Mode:    0644,
			ModTime: time.Now(),
			Format:  tar.FormatPAX,
		}
		if err := tarWriter.WriteHeader(hdr); err != nil {
			return fmt.Errorf("failed to write manifest blob header: %w", err)
		}
		if _, err := tarWriter.Write(m.Data); err != nil {
			return fmt.Errorf("failed to write manifest blob: %w", err)
		}
	}

	// Write index.json (OCI image index)
	indexManifests := make([]map[string]interface{}, 0, len(manifests))
	for _, m := range manifests {
		entry := map[string]interface{}{
			"mediaType": m.MediaType,
			"digest":    m.Digest,
			"size":      m.Size,
		}
		if len(m.Annotations) > 0 {
			entry["annotations"] = m.Annotations
		}
		indexManifests = append(indexManifests, entry)
	}

	index := map[string]interface{}{
		"schemaVersion": 2,
		"manifests":     indexManifests,
	}
	indexData, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to create index.json: %w", err)
	}

	_ = tarWriter.Flush()
	currentOffset = counter.n
	fileOffsets["index.json"] = currentOffset

	hdr = &tar.Header{
		Name:    "index.json",
		Size:    int64(len(indexData)),
		Mode:    0644,
		ModTime: time.Now(),
		Format:  tar.FormatPAX,
	}
	if err := tarWriter.WriteHeader(hdr); err != nil {
		return fmt.Errorf("failed to write index.json header: %w", err)
	}
	if _, err := tarWriter.Write(indexData); err != nil {
		return fmt.Errorf("failed to write index.json: %w", err)
	}

	// Write all blobs (transform from cache structure to OCI format)
	blobIndex := 0
	for digest := range allBlobDigests {
		blobIndex++

		// Read blob from cache
		blobPath := filepath.Join(blobDir, "sha256", digest[7:9], digest[9:11], digest)
		data, err := os.ReadFile(blobPath)
		if err != nil {
			// Try alternative path structure
			blobPath = filepath.Join(blobDir, digest)
			data, err = os.ReadFile(blobPath)
			if err != nil {
				log.Printf("Warning: Failed to read blob %s: %v", digest[:19], err)
				continue
			}
		}

		// Write blob in OCI format: blobs/sha256/<digest>
		digestPart := strings.TrimPrefix(digest, "sha256:")
		ociBlobPath := fmt.Sprintf("blobs/sha256/%s", digestPart)

		_ = tarWriter.Flush()
		currentOffset = counter.n
		fileOffsets[ociBlobPath] = currentOffset

		hdr := &tar.Header{
			Name:    ociBlobPath,
			Size:    int64(len(data)),
			Mode:    0644,
			ModTime: time.Now(),
			Format:  tar.FormatPAX,
		}
		if err := tarWriter.WriteHeader(hdr); err != nil {
			return fmt.Errorf("failed to write blob header: %w", err)
		}
		if _, err := tarWriter.Write(data); err != nil {
			return fmt.Errorf("failed to write blob: %w", err)
		}
	}

	// Write offset index at the end (same as original export)
	if len(fileOffsets) > 0 {

		blockOffsets := make(map[string]int32)
		for path, offset := range fileOffsets {
			blockNum := int32(offset / 512)
			blockOffsets[path] = blockNum
		}
		index := map[string]interface{}{
			"version":      2,
			"file_offsets": blockOffsets,
		}
		indexData, err := json.Marshal(index)
		if err == nil {
			// Compress the index
			var compressedData bytes.Buffer
			gzWriter := gzip.NewWriter(&compressedData)
			if _, err := gzWriter.Write(indexData); err == nil {
				if err := gzWriter.Close(); err == nil {
					indexData = compressedData.Bytes()
				}
			}

			// Write index file
			indexHeader := &tar.Header{
				Name:     ".roxxy-index.json.gz",
				Size:     int64(len(indexData)),
				Mode:     0644,
				ModTime:  time.Now(),
				Typeflag: tar.TypeReg,
			}
			if err := tarWriter.WriteHeader(indexHeader); err == nil {
				if _, err := tarWriter.Write(indexData); err == nil {
					log.Printf("Export: Wrote OCI tarball with %d manifest(s), %d blob(s), and offset index", len(manifests), len(allBlobDigests))
				}
			}
		}
	}

	return nil
}

// handleCACertificate serves the CA certificate for download
func (a *API) handleCACertificate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if a.certsDir == "" {
		http.Error(w, "Certificates directory not configured", http.StatusInternalServerError)
		return
	}

	certPath := filepath.Join(a.certsDir, "ca-cert.pem")

	// Check if certificate exists
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		http.Error(w, "CA certificate not found", http.StatusNotFound)
		return
	}

	// Read certificate file
	certData, err := os.ReadFile(certPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read certificate: %v", err), http.StatusInternalServerError)
		return
	}

	// Set appropriate headers for download
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="roxxy-ca.crt"`)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(certData)))

	// Write certificate data
	if _, err := w.Write(certData); err != nil {
		log.Printf("Error writing certificate: %v", err)
	}
}
