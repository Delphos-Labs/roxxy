package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/packages"
	"github.com/Delphos-Labs/roxxy/internal/packages/index"
)

// getOrCreateHierarchy gets or creates the hierarchy map from typeGroups
func getOrCreateHierarchy(typeGroups map[string]interface{}) map[string]interface{} {
	var hierarchy map[string]interface{}
	if typeGroups["hierarchy"] == nil {
		hierarchy = make(map[string]interface{})
		typeGroups["hierarchy"] = hierarchy
	} else {
		hierarchy = typeGroups["hierarchy"].(map[string]interface{})
	}
	return hierarchy
}

// incrementCount increments a count in a map, initializing to 1 if it doesn't exist
func incrementCount(node map[string]interface{}) {
	if count, ok := node["count"].(int); ok {
		node["count"] = count + 1
	} else {
		node["count"] = 1
	}
}

// addAPTToHierarchy adds an APT package to the 3-level hierarchy (basePath:distribution:component)
func addAPTToHierarchy(hierarchy map[string]interface{}, basePath, distribution, component string) {
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

// buildPackageKey builds a package key for lookup: "type:name@version" or "type:name@version:arch"
func buildPackageKey(pkgType, name, version, architecture string) string {
	key := fmt.Sprintf("%s:%s@%s", pkgType, name, version)
	if architecture != "" {
		key = fmt.Sprintf("%s:%s@%s:%s", pkgType, name, version, architecture)
	}
	return key
}

// buildPackageLookupKey builds a package lookup key without version: "type:name" or "type:name:arch"
// Used for matching packages across versions (e.g., in index lookups)
func buildPackageLookupKey(pkgType, name, architecture string) string {
	key := fmt.Sprintf("%s:%s", pkgType, name)
	if architecture != "" {
		key = fmt.Sprintf("%s:%s:%s", pkgType, name, architecture)
	}
	return key
}

// getGroupOrDefault returns the group or "default" if empty
func getGroupOrDefault(group string) string {
	if group == "" {
		return "default"
	}
	return group
}

// shouldProcessPackageType checks if a package type should be processed based on typeFilter
func shouldProcessPackageType(typeFilter string, pmType packages.Type) bool {
	if typeFilter == "" {
		return true
	}
	return string(pmType) == typeFilter
}

// shouldIncludePackage checks if a package should be included based on cached status
func shouldIncludePackage(showUncached bool, cachedPackageKeys map[string]bool, pkgType, name, version, architecture string) bool {
	if showUncached {
		return true
	}
	key := buildPackageKey(pkgType, name, version, architecture)
	return cachedPackageKeys[key]
}

// addToFlatGroup adds a package to a flat group structure
func addToFlatGroup(typeGroups map[string]interface{}, group string) {
	if typeGroups[group] == nil {
		typeGroups[group] = map[string]interface{}{
			"count": 0,
		}
	}
	groupMap := typeGroups[group].(map[string]interface{})
	incrementCount(groupMap)
}

// buildCachedPackageKeys builds a map of cached package keys for filtering
func buildCachedPackageKeys(cacheRoot string, pmType packages.Type, showUncached bool) map[string]bool {
	cachedPackageKeys := make(map[string]bool)
	if showUncached {
		return cachedPackageKeys
	}

	typeDir := filepath.Join(cacheRoot, string(pmType))
	if _, err := os.Stat(typeDir); err != nil {
		return cachedPackageKeys
	}

	cachedEntries := listPackagesByType(typeDir, pmType, "")
	for _, entry := range cachedEntries {
		key := buildPackageKey(entry.Type, entry.Name, entry.Version, entry.Architecture)
		cachedPackageKeys[key] = true
	}

	return cachedPackageKeys
}

// aggregateAPTHierarchy aggregates counts up the APT hierarchy (component -> distribution -> basePath)
func aggregateAPTHierarchy(hierarchy map[string]interface{}) {
	if len(hierarchy) == 0 {
		return
	}

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
}

// processAPTIndexPackages processes APT packages from the index database
func processAPTIndexPackages(indexPkgs []index.IndexPackage, typeGroups map[string]interface{}, cachedPackageKeys map[string]bool, showUncached bool) {
	hierarchy := make(map[string]interface{})

	// Limit processing when showUncached is true to avoid self-DDoS
	const maxUncachedPackages = 5000
	processedCount := 0

	for _, pkg := range indexPkgs {
		if !shouldIncludePackage(showUncached, cachedPackageKeys, pkg.Type, pkg.Name, pkg.Version, pkg.Architecture) {
			continue
		}

		// Limit uncached packages to prevent self-DDoS
		if showUncached && processedCount >= maxUncachedPackages {
			break
		}
		processedCount++

		// APT v2 parser uses / separator: basePath/distribution/component
		if pkg.Group != "" && strings.Contains(pkg.Group, "/") {
			parts := strings.Split(pkg.Group, "/")
			if len(parts) >= 3 {
				addAPTToHierarchy(hierarchy, parts[0], parts[1], parts[2])
			}
			continue
		}

		// Flat group or default
		group := pkg.Group
		if group == "" {
			group = "default"
		}
		addToFlatGroup(typeGroups, group)
	}

	if len(hierarchy) > 0 {
		aggregateAPTHierarchy(hierarchy)
		typeGroups["hierarchy"] = hierarchy
	}

	// Add warning if limit was reached
	if showUncached && processedCount >= maxUncachedPackages {
		typeGroups["_limit_reached"] = true
		typeGroups["_limit"] = maxUncachedPackages
		typeGroups["_total_available"] = len(indexPkgs)
	}
}
