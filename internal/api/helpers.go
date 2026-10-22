package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/cli"
	"github.com/Delphos-Labs/roxxy/internal/packages"
)

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

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// formatDuration formats a duration as a human-readable string
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

// writeJSON writes a JSON response to the HTTP response writer
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

// sortPackages sorts package entries by the specified field and order
func sortPackages(entries []PackageEntry, sortBy string) []PackageEntry {
	return sortPackagesWithOrder(entries, sortBy, "asc")
}

// sortPackagesWithOrder sorts package entries by the specified field and order
func sortPackagesWithOrder(entries []PackageEntry, sortBy, sortOrder string) []PackageEntry {
	// Create a copy to avoid modifying original
	sorted := make([]PackageEntry, len(entries))
	copy(sorted, entries)

	// Default ascending for text fields, descending for numeric/date fields
	asc := sortOrder != "desc"

	switch sortBy {
	case "name":
		sort.Slice(sorted, func(i, j int) bool {
			if asc {
				return sorted[i].Name < sorted[j].Name
			}
			return sorted[i].Name > sorted[j].Name
		})
	case "size":
		sort.Slice(sorted, func(i, j int) bool {
			if asc {
				return sorted[i].Size < sorted[j].Size
			}
			return sorted[i].Size > sorted[j].Size
		})
	case "age", "cached_at":
		sort.Slice(sorted, func(i, j int) bool {
			if asc {
				return sorted[i].CachedAt.Before(sorted[j].CachedAt)
			}
			return sorted[i].CachedAt.After(sorted[j].CachedAt)
		})
	case "access_count":
		sort.Slice(sorted, func(i, j int) bool {
			if asc {
				return sorted[i].AccessCount < sorted[j].AccessCount
			}
			return sorted[i].AccessCount > sorted[j].AccessCount
		})
	case "last_accessed":
		sort.Slice(sorted, func(i, j int) bool {
			if asc {
				return sorted[i].LastAccessed.Before(sorted[j].LastAccessed)
			}
			return sorted[i].LastAccessed.After(sorted[j].LastAccessed)
		})
	default:
		// Default: sort by name (then version if names are equal)
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].Name != sorted[j].Name {
				if asc {
					return sorted[i].Name < sorted[j].Name
				}
				return sorted[i].Name > sorted[j].Name
			}
			// If names are equal, sort by version
			if asc {
				return sorted[i].Version < sorted[j].Version
			}
			return sorted[i].Version > sorted[j].Version
		})
	}

	return sorted
}

// listCacheEntries is a wrapper around cli.ListCacheEntries
func listCacheEntries(cfg cli.Config, hostname string) ([]cli.CacheEntry, error) {
	return cli.ListCacheEntries(cfg, hostname)
}
