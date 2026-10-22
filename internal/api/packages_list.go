package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/packages"
	"github.com/Delphos-Labs/roxxy/internal/packages/index"
)

// handlePackages handles the packages listing endpoint
func (a *API) handlePackages(w http.ResponseWriter, r *http.Request) {
	if !validateMethodGET(w, r) {
		return
	}

	// Parse query parameters
	typeFilter := r.URL.Query().Get("type")
	search := r.URL.Query().Get("search")
	sortBy := r.URL.Query().Get("sort_by")
	if sortBy == "" {
		sortBy = r.URL.Query().Get("sort") // backwards compatibility
	}
	sortOrder := r.URL.Query().Get("sort_order")
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
	if sortOrder == "" {
		sortOrder = "asc"
	}
	entries = sortPackagesWithOrder(entries, sortBy, sortOrder)

	// Return all packages (no pagination for hierarchical tree view)
	response := map[string]interface{}{
		"packages": entries,
		"total":    len(entries),
	}

	writeJSON(w, response)
}

// listPackages lists all packages, merging cached and index data
func (a *API) listPackages(typeFilter, search string, showUncached bool) ([]PackageEntry, error) {
	cacheRoot := filepath.Join(a.cacheDir, "packages")
	log.Printf("[api] listPackages: cacheDir=%s, cacheRoot=%s", a.cacheDir, cacheRoot)
	pmTypes := []packages.Type{packages.TypeAPK, packages.TypeAPT, packages.TypeNPM, packages.TypePyPI, packages.TypeGo}

	var allEntries []PackageEntry

	// Build a map of cached packages by type/name/version for quick lookup
	cachedPackages := make(map[string]PackageEntry) // Key: "type:name@version:arch" or "type:name@version"

	for _, pmType := range pmTypes {
		if !shouldProcessPackageType(typeFilter, pmType) {
			continue
		}

		// Get cached packages from filesystem
		typeDir := filepath.Join(cacheRoot, string(pmType))
		cachedEntries := listPackagesByType(typeDir, pmType, search)
		for _, entry := range cachedEntries {
			// Key is package name + architecture (not version, since there can be multiple versions)
			// Use full key with version for storage, but we'll match by name+arch when merging
			key := buildPackageKey(entry.Type, entry.Name, entry.Version, entry.Architecture)
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
					key := buildPackageLookupKey(indexPkg.Type, indexPkg.Name, indexPkg.Architecture)
					indexByKey[key] = append(indexByKey[key], indexPkg)
				}

				// Now merge index metadata into cached packages by matching name+arch+version
				matchedCount := 0
				unmatchedCount := 0
				for cachedKey, cachedEntry := range cachedPackages {
					// Build lookup key (name+arch, no version)
					lookupKey := buildPackageLookupKey(cachedEntry.Type, cachedEntry.Name, cachedEntry.Architecture)

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
						checkKey := buildPackageLookupKey(cachedEntry.Type, cachedEntry.Name, cachedEntry.Architecture)
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
							versionKey := buildPackageKey(indexPkg.Type, indexPkg.Name, indexPkg.Version, indexPkg.Architecture)
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

// listPackagesByType lists packages of a specific type from the filesystem
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

		// Derive group from module path for Go packages if not set
		group := metadata.Group
		if group == "" && pmType == packages.TypeGo && metadata.Name != "" {
			// Build hierarchical group from module path: hostname:group:package
			moduleParts := strings.Split(metadata.Name, "/")
			if len(moduleParts) >= 2 {
				group = strings.Join(moduleParts, ":")
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

// loadPackageMetadata loads package metadata from a .meta file
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

// countPackagesByType quickly counts packages and calculates total size without loading full metadata
func (a *API) countPackagesByType(typeDir string, pmType packages.Type) (count int, totalSize int64, err error) {
	// Create a minimal config for handler creation (only needed for IsIndex check)
	// Set CacheDir to avoid warnings in NewBase, even though we won't use it for counting
	config := packages.Config{
		Type:         pmType,
		UpstreamURL:  "",         // Not needed for counting
		CacheDir:     a.cacheDir, // Set to actual cacheDir to avoid empty CacheDir warning
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
