package api

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/packages"
	"github.com/Delphos-Labs/roxxy/internal/packages/index"
)

// handlePackageGroups handles the package groups endpoint
func (a *API) handlePackageGroups(w http.ResponseWriter, r *http.Request) {
	if !validateMethodGET(w, r) {
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
			// e.g., filter "github.com:bytedance" should match "github.com:bytedance:sonic:loader"
			// e.g., filter "gopkg.in" should match "gopkg.in:yaml.v3"
			groupMatches := false
			if strings.Contains(entryGroup, ":") {
				// Entry has hierarchical group - check if it starts with filter
				// If filter also has ":", check for exact prefix match
				// If filter has no ":", check if entry starts with filter + ":"
				if strings.Contains(groupFilter, ":") {
					// Both hierarchical: check if entry starts with filter (with ":" separator)
					// e.g., "github.com:bytedance:sonic:loader" starts with "github.com:bytedance:"
					if strings.HasPrefix(entryGroup, groupFilter+":") || entryGroup == groupFilter {
						groupMatches = true
					}
				} else {
					// Filter is flat but entry is hierarchical: check if entry starts with filter + ":"
					// e.g., "gopkg.in:yaml.v3" starts with "gopkg.in:"
					if strings.HasPrefix(entryGroup, groupFilter+":") {
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
		if !shouldProcessPackageType(typeFilter, pmType) {
			continue
		}

		typeGroups := make(map[string]interface{})

		// Build a set of cached packages if we need to filter by cached status
		cachedPackageKeys := buildCachedPackageKeys(cacheRoot, pmType, showUncached)

		// Get groups from index database
		if a.indexDB != nil {
			indexPkgs, err := a.indexDB.GetPackages(pmType)
			if err == nil && len(indexPkgs) > 0 {
				switch pmType {
				case packages.TypeAPT:
					processAPTIndexPackages(indexPkgs, typeGroups, cachedPackageKeys, showUncached)
				case packages.TypeGo:
					processGoIndexPackages(indexPkgs, typeGroups, cachedPackageKeys, showUncached)
				default:
					// Flat group structure for other types
					processFlatIndexPackages(indexPkgs, typeGroups, cachedPackageKeys, showUncached)
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
						key := buildPackageLookupKey(indexPkg.Type, indexPkg.Name, indexPkg.Architecture)
						indexByKeyForGroup[key] = append(indexByKeyForGroup[key], indexPkg)
					}
				}
			}

			for _, entry := range cachedEntries {

				// Simulate the merge: if cached entry has no group, try to get it from index
				group := entry.Group
				if group == "" && a.indexDB != nil {
					// Try to find matching index package to get group
					lookupKey := buildPackageLookupKey(entry.Type, entry.Name, entry.Architecture)
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
				group = getGroupOrDefault(group)
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
						incrementCount(compMap)

						// Update distribution count
						incrementCount(distMap)

						// Update base path count
						incrementCount(baseMap)
					}
					continue
				}
				if pmType == packages.TypeGo && strings.Contains(group, ":") {
					// Add to hierarchy if not already counted from index
					// Convert group format (colons) to package name format (slashes) to match index structure
					packageName := strings.ReplaceAll(group, ":", "/")
					parts := strings.Split(packageName, "/")

					if len(parts) == 0 {
						continue
					}

					hierarchy := getOrCreateHierarchy(typeGroups)

					// Check if this package path already exists in hierarchy (from index)
					// Navigate through the tree to see if the leaf node exists
					currentLevel := hierarchy
					exists := true
					for i := 0; i < len(parts); i++ {
						part := parts[i]
						if currentLevel[part] == nil {
							exists = false
							break
						}
						if nodeMap, ok := currentLevel[part].(map[string]interface{}); ok {
							if i < len(parts)-1 {
								currentLevel = nodeMap
							}
						} else {
							exists = false
							break
						}
					}

					// If already exists, skip (already counted from index)
					if exists {
						continue
					}

					// Use the same simple tree structure as processGoIndexPackages
					// Navigate/create the tree structure
					currentLevel = hierarchy
					for i := 0; i < len(parts); i++ {
						part := parts[i]

						if currentLevel[part] == nil {
							// Create new node
							currentLevel[part] = map[string]interface{}{
								"count": 0,
							}
						}

						// Increment count
						if nodeMap, ok := currentLevel[part].(map[string]interface{}); ok {
							if count, ok := nodeMap["count"].(int); ok {
								nodeMap["count"] = count + 1
							} else {
								nodeMap["count"] = 1
							}

							// If not last part, navigate deeper
							if i < len(parts)-1 {
								currentLevel = nodeMap
							}
						} else {
							break
						}
					}
					continue
				}
				if typeGroups[group] == nil {
					typeGroups[group] = map[string]interface{}{
						"count": 0,
					}
				}
				groupMap := typeGroups[group].(map[string]interface{})
				incrementCount(groupMap)
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
