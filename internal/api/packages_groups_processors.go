package api

import (
	"sort"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/packages/index"
)

// processFlatIndexPackages processes packages with flat group structure
func processFlatIndexPackages(indexPkgs []index.IndexPackage, typeGroups map[string]interface{}, cachedPackageKeys map[string]bool, showUncached bool) {
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

		group := getGroupOrDefault(pkg.Group)
		addToFlatGroup(typeGroups, group)
	}

	// Add warning if limit was reached
	if showUncached && processedCount >= maxUncachedPackages {
		typeGroups["_limit_reached"] = true
		typeGroups["_limit"] = maxUncachedPackages
		typeGroups["_total_available"] = len(indexPkgs)
	}
}

// sortHierarchy recursively sorts hierarchy keys
func sortHierarchy(level map[string]interface{}) map[string]interface{} {
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

	// Add count first if it exists
	if count, ok := level["count"]; ok {
		sorted["count"] = count
	}

	// Add sorted keys
	for _, k := range keys {
		if val, ok := level[k].(map[string]interface{}); ok {
			sorted[k] = sortHierarchy(val)
		} else {
			sorted[k] = level[k]
		}
	}
	return sorted
}

// processGoIndexPackages processes Go packages from the index database
// Simple approach: sort by name, split by '/', build flat tree - no "groups" or "packages" keys
func processGoIndexPackages(indexPkgs []index.IndexPackage, typeGroups map[string]interface{}, cachedPackageKeys map[string]bool, showUncached bool) {
	// Filter and collect packages
	validPkgs := make([]index.IndexPackage, 0)
	for _, pkg := range indexPkgs {
		if !shouldIncludePackage(showUncached, cachedPackageKeys, pkg.Type, pkg.Name, pkg.Version, pkg.Architecture) {
			continue
		}
		validPkgs = append(validPkgs, pkg)
	}

	if len(validPkgs) == 0 {
		return
	}

	// Sort packages by name
	sort.Slice(validPkgs, func(i, j int) bool {
		return validPkgs[i].Name < validPkgs[j].Name
	})

	// Build simple tree by splitting on '/' - just a flat tree structure
	hierarchy := make(map[string]interface{})

	for _, pkg := range validPkgs {
		// Split package name by '/' to get path segments
		// e.g., "gopkg.in/yaml.v3" -> ["gopkg.in", "yaml.v3"]
		// e.g., "github.com/go-git/go-billy/v5" -> ["github.com", "go-git", "go-billy", "v5"]
		parts := strings.Split(pkg.Name, "/")

		if len(parts) == 0 {
			continue
		}

		// Navigate/create the tree structure - simple flat tree
		currentLevel := hierarchy
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
	}

	// Aggregate counts up the tree (children count up to parent)
	var aggregateCounts func(level map[string]interface{}) int
	aggregateCounts = func(level map[string]interface{}) int {
		totalCount := 0
		for key, value := range level {
			if key == "count" {
				continue
			}
			if valueMap, ok := value.(map[string]interface{}); ok {
				subCount := aggregateCounts(valueMap)
				// Update count to be sum of children (if children exist)
				if subCount > 0 {
					valueMap["count"] = subCount
					totalCount += subCount
				} else if count, ok := valueMap["count"].(int); ok {
					// Leaf node - use its own count
					totalCount += count
				}
			}
		}
		return totalCount
	}

	// Aggregate counts for top level
	for _, value := range hierarchy {
		if valueMap, ok := value.(map[string]interface{}); ok {
			count := aggregateCounts(valueMap)
			if count > 0 {
				valueMap["count"] = count
			}
		}
	}

	// Sort the hierarchy
	sortedHierarchy := sortHierarchy(hierarchy)

	typeGroups["hierarchy"] = sortedHierarchy

	// Add warning if limit was reached
	const maxUncachedPackages = 5000
	if showUncached && len(validPkgs) >= maxUncachedPackages {
		typeGroups["_limit_reached"] = true
		typeGroups["_limit"] = maxUncachedPackages
		typeGroups["_total_available"] = len(indexPkgs)
	}
}
