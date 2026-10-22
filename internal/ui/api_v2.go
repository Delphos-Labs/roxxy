package ui

import (
	"fmt"
	"log"
	"net/http"
	urlpkg "net/url"
	"sort"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	v2index "github.com/Delphos-Labs/roxxy/internal/packages/index/v2"
)

// IndexDBV2Interface is an interface for the v2 index database
type IndexDBV2Interface interface {
	GetPackages(pmType string) ([]v2index.IndexPackageV2, error)
	MergeGoPackageVersions(pkgs []v2index.IndexPackageV2) []v2index.IndexPackageV2
}

// APIV2 provides REST API endpoints using v2 index format
type APIV2 struct {
	cacheDir     string
	statsTracker *cache.StatsTracker
	indexDB      IndexDBV2Interface
}

// NewAPIV2 creates a new v2 API handler
func NewAPIV2(cacheDir string, statsTracker *cache.StatsTracker, indexDB IndexDBV2Interface) *APIV2 {
	return &APIV2{
		cacheDir:     cacheDir,
		statsTracker: statsTracker,
		indexDB:      indexDB,
	}
}

// RegisterRoutes registers v2 API routes (replaces existing endpoints)
func (a *APIV2) RegisterRoutes(mux *http.ServeMux) {
	// Replace existing package endpoints with v2 versions
	mux.HandleFunc("/api/v1/packages/groups", a.corsMiddleware(a.handlePackageTreeV2))
	mux.HandleFunc("/api/v1/packages/for-group", a.corsMiddleware(a.handlePackagesForPathV2))
	mux.HandleFunc("/api/v1/packages/groups/list", a.corsMiddleware(a.handleListGroupsV2))
}

// corsMiddleware adds CORS headers
// CORS is set to "*" to allow same-origin requests from the embedded UI
// The UI is served from the same server, so this is safe
func (a *APIV2) corsMiddleware(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Allow all origins since UI is served from same server
		// In production, you may want to restrict this to specific origins
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "3600")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		handler(w, r)
	}
}

// handlePackageTreeV2 returns flat, sorted list of packages
// Frontend builds tree by splitting package name paths
// If group parameter is provided, filters packages by group path
func (a *APIV2) handlePackageTreeV2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	typeFilter := r.URL.Query().Get("type")
	groupFilter := r.URL.Query().Get("group") // Group path to filter by (e.g., "3.23/main" for APK, "debian-security/bookworm-security/main" for APT)
	showUncached := r.URL.Query().Get("show_uncached") == "true"

	pmTypes := []string{"apk", "apt", "npm", "pypi", "go"}
	result := make(map[string][]PackageResponseV2)

	// Limit total packages to prevent self-DDoS when showUncached is true
	const maxTotalPackages = 10000
	totalPackagesProcessed := 0
	packageLimitReached := false

	for _, pmType := range pmTypes {
		if typeFilter != "" && typeFilter != pmType {
			continue
		}

		if a.indexDB == nil {
			continue
		}

		pkgs, err := a.indexDB.GetPackages(pmType)
		if err != nil {
			log.Printf("[api-v2] ERROR: Error getting packages for %s: %v", pmType, err)
			continue
		}

		// Limit packages when showUncached is true to prevent self-DDoS
		if showUncached && len(pkgs) > 0 {
			remaining := maxTotalPackages - totalPackagesProcessed
			if remaining <= 0 {
				log.Printf("[api-v2] Reached limit of %d packages, skipping remaining types", maxTotalPackages)
				break
			}
			if len(pkgs) > remaining {
				log.Printf("[api-v2] Limiting %s packages from %d to %d to prevent self-DDoS", pmType, len(pkgs), remaining)
				pkgs = pkgs[:remaining]
			}
			totalPackagesProcessed += len(pkgs)
		}

		// For Go packages, merge versions from /vN variants
		if pmType == "go" {
			pkgs = a.indexDB.MergeGoPackageVersions(pkgs)
		}

		// Filter by group if provided
		if groupFilter != "" {
			var filteredByGroup []v2index.IndexPackageV2
			for _, pkg := range pkgs {
				// For APT/APK, filter by Group field
				// For Go/NPM/PyPI, filter by package name prefix
				if pmType == "apt" || pmType == "apk" {
					// Use group from package if available, otherwise extract from URL
					group := pkg.Group
					if group == "" && len(pkg.Versions) > 0 && pkg.Versions[0].URL != "" {
						switch pmType {
						case "apt":
							group = extractAPTGroup(pkg.Versions[0].URL)
						case "apk":
							group = extractAPKGroup(pkg.Versions[0].URL)
						}
					}
					// Normalize APT groups: strip "pub/repos/" prefix if present
					if pmType == "apt" && group != "" {
						if strings.HasPrefix(group, "pub/repos/") {
							group = strings.TrimPrefix(group, "pub/repos/")
						} else if strings.HasPrefix(group, "pub/") {
							group = strings.TrimPrefix(group, "pub/")
						}
					}
					// For APT, if groupFilter is 2 parts (distribution/component), match against last 2 parts of group (basePath/distribution/component)
					if pmType == "apt" && group != "" && groupFilter != "" {
						groupParts := strings.Split(group, "/")
						filterParts := strings.Split(groupFilter, "/")

						if len(filterParts) == 2 && len(groupParts) >= 3 {
							// Filter is "distribution/component", match against last 2 parts of group
							distribution := groupParts[1]
							component := groupParts[2]
							if distribution == filterParts[0] && component == filterParts[1] {
								filteredByGroup = append(filteredByGroup, pkg)
							}
						} else if len(filterParts) == 1 && len(groupParts) >= 2 {
							// Filter is just "distribution", match against distribution part
							if groupParts[1] == filterParts[0] {
								filteredByGroup = append(filteredByGroup, pkg)
							}
						} else {
							// Fallback: exact match or prefix match
							if group == groupFilter || strings.HasPrefix(group, groupFilter+"/") {
								filteredByGroup = append(filteredByGroup, pkg)
							}
						}
					} else {
						// For APK or other cases, use exact match or prefix match
						if group == groupFilter || strings.HasPrefix(group, groupFilter+"/") {
							filteredByGroup = append(filteredByGroup, pkg)
						}
					}
				} else {
					// For Go/NPM/PyPI, filter by package name prefix
					if strings.HasPrefix(pkg.Name, groupFilter+"/") || pkg.Name == groupFilter {
						filteredByGroup = append(filteredByGroup, pkg)
					}
				}
			}
			pkgs = filteredByGroup
		}

		// Filter by cached status if needed
		// Only filter if we're confident cached status has been updated
		// Check if MOST packages have cached flags set (not just any)
		hasAnyCachedFlags := false
		cachedCount := 0
		totalVersions := 0
		cachedRatio := 0.0
		if !showUncached {
			// Count how many versions have cached flags set
			for _, pkg := range pkgs {
				for _, version := range pkg.Versions {
					totalVersions++
					if version.Cached {
						hasAnyCachedFlags = true
						cachedCount++
					}
				}
			}

			// Only filter if at least 10% of versions have cached flags set
			// This indicates cached status has been updated (not just a few stray flags)
			if totalVersions > 0 {
				cachedRatio = float64(cachedCount) / float64(totalVersions)
			}

			if hasAnyCachedFlags && cachedRatio >= 0.1 {
				// Cached status has been updated - only show packages with at least one cached version
				var filtered []v2index.IndexPackageV2
				for _, pkg := range pkgs {
					hasCachedVersion := false
					for _, version := range pkg.Versions {
						if version.Cached {
							hasCachedVersion = true
							break
						}
					}
					if hasCachedVersion {
						filtered = append(filtered, pkg)
					}
				}
				pkgs = filtered
			}
			// If cached status hasn't been updated (or only a few flags set), show all packages
		}

		// Convert to response format
		var response []PackageResponseV2
		for _, pkg := range pkgs {
			// Filter versions by cached status if needed
			versions := pkg.Versions
			if !showUncached {
				var cachedVersions []v2index.PackageVersion
				for _, version := range pkg.Versions {
					if version.Cached {
						cachedVersions = append(cachedVersions, version)
					}
				}
				// Only include package if it has at least one cached version
				if len(cachedVersions) == 0 {
					continue
				}
				versions = cachedVersions
			}

			// Get short name for Go packages
			shortName := pkg.Name
			if pmType == "go" {
				shortName = getGoShortName(pkg.Name)
			}

			// Use group from package if available, otherwise extract from URL
			group := pkg.Group
			if group == "" && len(versions) > 0 && versions[0].URL != "" {
				switch pmType {
				case "apt":
					group = extractAPTGroup(versions[0].URL)
				case "apk":
					group = extractAPKGroup(versions[0].URL)
				}
			}

			// Normalize APT groups: strip "pub/repos/" prefix if present
			// This fixes packages that were indexed before we added the filtering
			if pmType == "apt" && group != "" {
				if strings.HasPrefix(group, "pub/repos/") {
					group = strings.TrimPrefix(group, "pub/repos/")
				} else if strings.HasPrefix(group, "pub/") {
					group = strings.TrimPrefix(group, "pub/")
				}
			}

			response = append(response, PackageResponseV2{
				Name:         pkg.Name,
				ShortName:    shortName,
				Type:         pkg.Type,
				Versions:     convertVersions(versions),
				Description:  pkg.Description,
				License:      pkg.License,
				Dependencies: pkg.Dependencies,
				Group:        group,
			})
		}

		// Sort by full name
		sort.Slice(response, func(i, j int) bool {
			return response[i].Name < response[j].Name
		})

		// Always set result[pmType] to an array (even if empty) to avoid null values
		result[pmType] = response
		if len(response) > 0 {
			log.Printf("[api-v2] Returning %d packages for type %s", len(response), pmType)
		}
		if pmType == "apt" && !showUncached {
			// Count packages with groups vs without for debugging
			withGroup := 0
			withoutGroup := 0
			invalidGroup := 0
			for _, pkg := range response {
				if pkg.Group == "" {
					withoutGroup++
				} else {
					parts := strings.Split(pkg.Group, "/")
					if len(parts) >= 3 {
						withGroup++
					} else {
						invalidGroup++
					}
				}
			}
			log.Printf("[api-v2] APT packages: %d with valid groups, %d without groups, %d with invalid groups", withGroup, withoutGroup, invalidGroup)
		}

		// Check if we hit the limit
		if showUncached && totalPackagesProcessed >= maxTotalPackages {
			packageLimitReached = true
			break
		}
	}

	// If group filter is provided, return packages as a flat array (for loadPackagesForGroup)
	// Otherwise build groups structure (for initial load)
	if groupFilter != "" && typeFilter != "" {
		// Return packages for the specific type as a flat array
		packagesArray := result[typeFilter]
		if packagesArray == nil {
			packagesArray = []PackageResponseV2{}
		}
		writeJSON(w, map[string]interface{}{
			"packages": packagesArray,
			"total":    len(packagesArray),
		})
	} else {
		// Build groups structure from packages
		groups := buildGroupsFromPackages(result, showUncached)

		response := map[string]interface{}{
			"groups":   groups,
			"packages": result, // Also include packages for fallback
			"total":    countTotalPackages(result),
		}

		// Add warnings if limit was reached
		if packageLimitReached {
			response["_warnings"] = map[string]interface{}{
				"package_limit_reached": true,
				"package_limit":         maxTotalPackages,
			}
		}

		writeJSON(w, response)
	}
}

// handlePackagesForPathV2 filters packages by name prefix (for tree navigation)
func (a *APIV2) handlePackagesForPathV2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	typeFilter := r.URL.Query().Get("type")
	groupFilter := r.URL.Query().Get("group") // Package name prefix to filter by
	showUncached := r.URL.Query().Get("show_uncached") == "true"

	if typeFilter == "" || groupFilter == "" {
		http.Error(w, "type and group parameters required", http.StatusBadRequest)
		return
	}

	if a.indexDB == nil {
		writeJSON(w, map[string]interface{}{
			"packages": []PackageResponseV2{},
			"total":    0,
		})
		return
	}

	pkgs, err := a.indexDB.GetPackages(typeFilter)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error getting packages: %v", err), http.StatusInternalServerError)
		return
	}

	// For Go packages, merge versions from /vN variants
	if typeFilter == "go" {
		pkgs = a.indexDB.MergeGoPackageVersions(pkgs)
	}

	// Filter by group prefix and cached status
	var filtered []v2index.IndexPackageV2
	for _, pkg := range pkgs {
		// Check if package name starts with group prefix
		if strings.HasPrefix(pkg.Name, groupFilter+"/") || pkg.Name == groupFilter {
			// Also check cached status if needed
			if !showUncached {
				hasCachedVersion := false
				for _, version := range pkg.Versions {
					if version.Cached {
						hasCachedVersion = true
						break
					}
				}
				if !hasCachedVersion {
					continue
				}
			}
			filtered = append(filtered, pkg)
		}
	}

	// Convert to response format
	var response []PackageResponseV2
	for _, pkg := range filtered {
		// Filter versions by cached status if needed
		versions := pkg.Versions
		if !showUncached {
			var cachedVersions []v2index.PackageVersion
			for _, version := range pkg.Versions {
				if version.Cached {
					cachedVersions = append(cachedVersions, version)
				}
			}
			// Only include package if it has at least one cached version
			if len(cachedVersions) == 0 {
				continue
			}
			versions = cachedVersions
		}

		shortName := pkg.Name
		if typeFilter == "go" {
			shortName = getGoShortName(pkg.Name)
		}

		// Use group from package if available, otherwise extract from URL
		group := pkg.Group
		if group == "" && len(versions) > 0 && versions[0].URL != "" {
			switch typeFilter {
			case "apt":
				group = extractAPTGroup(versions[0].URL)
			case "apk":
				group = extractAPKGroup(versions[0].URL)
			}
		}

		response = append(response, PackageResponseV2{
			Name:        pkg.Name,
			ShortName:   shortName,
			Type:        pkg.Type,
			Versions:    convertVersions(versions),
			Description: pkg.Description,
			License:     pkg.License,
			Group:       group,
		})
	}

	// Sort by full name
	sort.Slice(response, func(i, j int) bool {
		return response[i].Name < response[j].Name
	})

	writeJSON(w, map[string]interface{}{
		"packages": response,
		"total":    len(response),
		"group":    groupFilter,
	})
}

// handleListGroupsV2 returns a list of available group prefixes for a given package type
func (a *APIV2) handleListGroupsV2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	typeFilter := r.URL.Query().Get("type")
	if typeFilter == "" {
		http.Error(w, "type parameter required", http.StatusBadRequest)
		return
	}

	if a.indexDB == nil {
		writeJSON(w, map[string]interface{}{
			"groups": []string{},
		})
		return
	}

	pkgs, err := a.indexDB.GetPackages(typeFilter)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error getting packages: %v", err), http.StatusInternalServerError)
		return
	}

	// Extract unique group prefixes from package names
	groupSet := make(map[string]bool)
	for _, pkg := range pkgs {
		// Split package name by / to get path segments
		parts := strings.Split(pkg.Name, "/")
		if len(parts) > 1 {
			// Add first segment as a group (e.g., "github.com" from "github.com/go-git/go-git")
			groupSet[parts[0]] = true
			// Also add two-segment groups for deeper hierarchies (e.g., "github.com/go-git")
			if len(parts) > 2 {
				groupSet[strings.Join(parts[:2], "/")] = true
			}
		} else {
			// Single-segment package name (no group)
			groupSet[""] = true
		}
	}

	// Convert to sorted slice
	groups := make([]string, 0, len(groupSet))
	for group := range groupSet {
		if group != "" {
			groups = append(groups, group)
		}
	}
	sort.Strings(groups)

	writeJSON(w, map[string]interface{}{
		"type":   typeFilter,
		"groups": groups,
		"total":  len(groups),
	})
}

// PackageResponseV2 is the response format for v2 API
type PackageResponseV2 struct {
	Name         string              `json:"name"`       // Full decoded name
	ShortName    string              `json:"short_name"` // Short name for display (Go packages)
	Type         string              `json:"type"`
	Versions     []VersionResponseV2 `json:"versions"`
	Description  string              `json:"description,omitempty"`
	License      string              `json:"license,omitempty"`
	Dependencies []string            `json:"dependencies,omitempty"`
	Group        string              `json:"group,omitempty"` // Group/repository path (e.g., "debian-security:bookworm-security:main" for APT)
}

// VersionResponseV2 is the version format in v2 API response
type VersionResponseV2 struct {
	ID           string `json:"id"` // Unique identifier for this version (version+architecture or URL)
	Version      string `json:"version"`
	Architecture string `json:"architecture,omitempty"`
	Cached       bool   `json:"cached"`
	Size         int64  `json:"size"`
	URL          string `json:"url,omitempty"`
	License      string `json:"license,omitempty"`     // Version-specific license
	Description  string `json:"description,omitempty"` // Version-specific description
}

// convertVersions converts v2 index versions to response format
func convertVersions(versions []v2index.PackageVersion) []VersionResponseV2 {
	result := make([]VersionResponseV2, len(versions))
	for i, v := range versions {
		// Generate unique ID: version + architecture (if present), or use URL as fallback
		id := v.Version
		if v.Architecture != "" {
			id = fmt.Sprintf("%s:%s", v.Version, v.Architecture)
		}
		if v.URL != "" && id == "" {
			id = v.URL
		}
		// If still empty, use index as fallback
		if id == "" {
			id = fmt.Sprintf("version-%d", i)
		}

		result[i] = VersionResponseV2{
			ID:           id,
			Version:      v.Version,
			Architecture: v.Architecture,
			Cached:       v.Cached,
			Size:         v.Size,
			URL:          v.URL,
			License:      v.License,     // Version-specific license
			Description:  v.Description, // Version-specific description
		}
	}
	return result
}

// getGoShortName extracts short name from Go module path
func getGoShortName(modulePath string) string {
	parts := strings.Split(modulePath, "/")
	if len(parts) > 0 {
		// Check if last part is /vN and get the one before it
		lastPart := parts[len(parts)-1]
		if strings.HasPrefix(lastPart, "v") && len(lastPart) > 1 {
			// Check if it's all digits after "v"
			isVersionSuffix := true
			for i := 1; i < len(lastPart); i++ {
				if lastPart[i] < '0' || lastPart[i] > '9' {
					isVersionSuffix = false
					break
				}
			}
			if isVersionSuffix && len(parts) > 1 {
				return parts[len(parts)-2]
			}
		}
		return lastPart
	}
	return modulePath
}

// extractAPKGroup extracts Alpine release information from APK package URL
// URL format: http://dl-cdn.alpinelinux.org/alpine/v3.23/main/x86_64/package-1.0-r0.apk
// Returns: "3.23/main" or "3.23" (release/branch or just release) or empty string if can't parse
func extractAPKGroup(url string) string {
	if url == "" {
		return ""
	}

	// Parse URL to get path
	u, err := urlpkg.Parse(url)
	if err != nil {
		return ""
	}

	path := u.Path

	// Look for /alpine/v{version}/ pattern
	// Format: /alpine/v{version}/{branch}/{arch}/...
	if !strings.Contains(path, "/alpine/v") {
		return ""
	}

	// Find /alpine/v in the path
	alpineIdx := strings.Index(path, "/alpine/v")
	if alpineIdx < 0 {
		return ""
	}

	// Extract everything after /alpine/v
	afterAlpine := path[alpineIdx+8:] // Skip "/alpine/v"

	// Split by / to get version, branch, arch
	parts := strings.Split(afterAlpine, "/")
	if len(parts) < 1 {
		return ""
	}

	// First part is the version (e.g., "3.23")
	version := parts[0]
	if version == "" {
		return ""
	}

	// Build group: {version}/{branch} or just {version} (no need to add "v" - it's already in the URL path)
	var groupParts []string
	groupParts = append(groupParts, version)

	// Add branch if present (e.g., "main", "community", "testing")
	if len(parts) >= 2 && parts[1] != "" {
		groupParts = append(groupParts, parts[1])
	}

	return strings.Join(groupParts, "/")
}

// extractAPTGroup extracts repository group information from APT package URL
// URL format: http://security.debian.org/debian-security/dists/bookworm-security/main/binary-amd64/pool/main/l/libssl/libssl3_3.0.11-1~deb12u2_amd64.deb
// Returns: "debian-security:bookworm-security:main" or empty string if can't parse
func extractAPTGroup(url string) string {
	if url == "" {
		return ""
	}

	// Parse URL to get path
	u, err := urlpkg.Parse(url)
	if err != nil {
		return ""
	}

	path := u.Path

	// Look for /dists/ pattern: /{base-path}/dists/{distribution}/{component}/binary-{arch}
	// Example: /debian-security/dists/bookworm-security/main/binary-amd64
	if !strings.Contains(path, "/dists/") {
		return ""
	}

	// Split into base path and dists path
	parts := strings.Split(path, "/dists/")
	if len(parts) < 2 {
		return ""
	}

	// Extract base path (e.g., /debian-security or /debian)
	basePath := strings.Trim(parts[0], "/")
	if basePath == "" {
		return ""
	}

	// Parse dists path: {distribution}/{component}/binary-{arch}
	// Or: {distribution}/updates/{component}/binary-{arch}
	distParts := strings.Split(parts[1], "/")
	if len(distParts) < 2 {
		return ""
	}

	// Build group string with hierarchy: basePath:distribution:component
	var groupParts []string
	groupParts = append(groupParts, basePath)

	// Add distribution (e.g., bookworm-security, bookworm, bookworm-updates)
	distribution := distParts[0]
	if distribution != "" {
		groupParts = append(groupParts, distribution)
	}

	// Extract component (e.g., "main", "contrib", "non-free")
	component := ""
	if len(distParts) >= 2 {
		// Check if this is a security/updates repository
		if distParts[1] == "updates" && len(distParts) >= 3 {
			// Security repository: suite/updates/component
			component = distParts[2]
		} else {
			// Regular repository: suite/component
			component = distParts[1]
		}
	}

	if component != "" {
		groupParts = append(groupParts, component)
	}

	// Join with / to create hierarchical group (matches URL structure)
	return strings.Join(groupParts, "/")
}

// countTotalPackages counts total packages across all types
func countTotalPackages(packages map[string][]PackageResponseV2) int {
	total := 0
	for _, pkgs := range packages {
		total += len(pkgs)
	}
	return total
}

// buildGroupsFromPackages builds the groups hierarchy structure from packages
func buildGroupsFromPackages(packagesByType map[string][]PackageResponseV2, showUncached bool) map[string]interface{} {
	groups := make(map[string]interface{})

	for pmType, packages := range packagesByType {
		if len(packages) == 0 {
			groups[pmType] = map[string]interface{}{
				"hierarchy": make(map[string]interface{}),
			}
			continue
		}

		typeGroups := make(map[string]interface{})

		if pmType == "apt" {
			// Build 2-level hierarchy: distribution/component (skip basePath since it's redundant)
			// Each basePath (debian, debian-security, apt) only has one distribution anyway
			// Count unique packages (not versions) at the component level
			hierarchy := make(map[string]interface{})
			packageCounts := make(map[string]int) // Track unique packages per component (key: "distribution/component")

			for _, pkg := range packages {
				group := pkg.Group
				// Normalize APT groups: strip "pub/repos/" prefix if present
				// This fixes packages that were indexed before we added the filtering
				if group != "" {
					if strings.HasPrefix(group, "pub/repos/") {
						group = strings.TrimPrefix(group, "pub/repos/")
					} else if strings.HasPrefix(group, "pub/") {
						group = strings.TrimPrefix(group, "pub/")
					}
				}
				if group != "" && strings.Contains(group, "/") {
					parts := strings.Split(group, "/")
					if len(parts) >= 3 {
						// Skip basePath (parts[0]), use distribution/component directly
						distribution := parts[1]
						component := parts[2]

						// Count unique packages at component level
						compKey := fmt.Sprintf("%s/%s", distribution, component)
						packageCounts[compKey]++

						// Initialize distribution
						if hierarchy[distribution] == nil {
							hierarchy[distribution] = make(map[string]interface{})
						}
						distMap := hierarchy[distribution].(map[string]interface{})

						// Initialize component
						if distMap[component] == nil {
							distMap[component] = map[string]interface{}{
								"count": 0,
							}
						}
					}
				}
			}

			// Now aggregate counts up the hierarchy
			// First, set component counts
			for compKey, count := range packageCounts {
				parts := strings.Split(compKey, "/")
				if len(parts) >= 2 {
					distribution := parts[0]
					component := parts[1]

					if distMap, ok := hierarchy[distribution].(map[string]interface{}); ok {
						if compMap, ok := distMap[component].(map[string]interface{}); ok {
							compMap["count"] = count
						}
					}
				}
			}

			// Aggregate distribution counts (sum of all components)
			for _, distMap := range hierarchy {
				if distMapTyped, ok := distMap.(map[string]interface{}); ok {
					total := 0
					for key, value := range distMapTyped {
						if key == "count" {
							continue
						}
						if compMap, ok := value.(map[string]interface{}); ok {
							if count, ok := compMap["count"].(int); ok {
								total += count
							}
						}
					}
					distMapTyped["count"] = total
				}
			}

			// Log hierarchy for debugging
			log.Printf("[api-v2] Built APT hierarchy with %d base paths", len(hierarchy))

			if len(hierarchy) > 0 {
				typeGroups["hierarchy"] = hierarchy
			}
		} else {
			// For other types, use flat structure (handled by frontend)
			typeGroups["hierarchy"] = make(map[string]interface{})
		}

		groups[pmType] = typeGroups
	}

	log.Printf("[api-v2] buildGroupsFromPackages: returning groups for types: %v", getMapKeys(groups))
	return groups
}

// getMapKeys returns the keys of a map as a slice (helper for logging)
func getMapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
