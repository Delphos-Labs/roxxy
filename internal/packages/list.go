package packages

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// IndexPackageData represents index metadata that can enrich cache entries
type IndexPackageData struct {
	Description  string
	License      string
	Name         string
	Version      string
	Architecture string
}

// CacheEntry represents a cached package
type CacheEntry struct {
	Type          Type
	Name          string
	Version       string
	Architecture  string
	Description   string
	License       string
	Path          string
	Size          int64
	CachedAt      time.Time
	FetchDuration time.Duration
	AccessCount   int
}

// ListCache lists all cached packages organized by type
// sortBy can be: "name" (name then version), "time" (cached time), "size" (file size), "accessed" (access count)
// indexDataByURL is an optional map of URL -> IndexPackageData to enrich cache entries with Description/License
func ListCache(cacheRoot string, filterType string, sortBy string, indexDataByURL map[string]IndexPackageData) {
	// Check if cache directory exists
	if _, err := os.Stat(cacheRoot); os.IsNotExist(err) {
		fmt.Printf("No package cache found at: %s\n", cacheRoot)
		fmt.Printf("Expected cache directory structure: %s/{apk,apt,npm,pypi,go}/\n", cacheRoot)
		fmt.Printf("\nNote: Packages are cached when fetched through the proxy.\n")
		fmt.Printf("Make sure the roxxy server is running and packages are being fetched.\n")
		return
	}

	// Map to store packages organized by type
	packagesByType := make(map[Type][]CacheEntry)

	// Walk the cache directory
	pmTypes := []Type{TypeAPK, TypeAPT, TypeNPM, TypePyPI, TypeGo}
	for _, pmType := range pmTypes {
		// Skip if filtering by type
		if filterType != "" && string(pmType) != filterType {
			continue
		}

		typeDir := filepath.Join(cacheRoot, string(pmType))
		if _, err := os.Stat(typeDir); os.IsNotExist(err) {
			if filterType != "" && string(pmType) == filterType {
				fmt.Printf("No cache directory found for type '%s' at: %s\n", pmType, typeDir)
			}
			continue
		}

		// Walk the content-addressable storage
		if err := filepath.Walk(typeDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}

			// Skip .meta files
			if strings.HasSuffix(path, ".meta") {
				return nil
			}

			// Try to load metadata
			metadata, err := loadMetadata(path)

			// For Go modules, skip index files - only show .zip packages
			// Check metadata URL path to determine if it's a .zip file
			if pmType == TypeGo {
				if err != nil {
					// No metadata, can't determine type - skip it
					return nil
				}
				// Check if URL path ends with .zip (actual package) vs .info/.mod/.list (index files)
				if !strings.HasSuffix(metadata.URLPath, ".zip") {
					return nil
				}
			}

			// For npm, skip index files (JSON metadata) - only show .tgz packages
			// NPM index files are JSON responses (e.g., /express, /express/4.18.2)
			// NPM packages are tarballs (e.g., /express/-/express-4.18.2.tgz)
			if pmType == TypeNPM {
				if err != nil {
					// No metadata, can't determine type - skip it
					return nil
				}
				// Check if it's an index file: doesn't contain "/-/" and doesn't end with .tgz
				// Or check is_index flag if available
				if metadata.IsIndex || (!strings.Contains(metadata.URLPath, "/-/") && !strings.HasSuffix(metadata.URLPath, ".tgz")) {
					return nil
				}
			}

			// For APK, skip index files (APKINDEX.tar.gz) - only show .apk packages
			// APK index files: APKINDEX.tar.gz (repository metadata)
			// APK packages: .apk files
			if pmType == TypeAPK {
				if err != nil {
					// No metadata, can't determine type - skip it
					return nil
				}
				// Check if it's an index file using is_index flag or URL path pattern
				if metadata.IsIndex || !strings.HasSuffix(metadata.URLPath, ".apk") {
					return nil
				}
			}

			// For APT, skip index files (Release, Packages, Sources, etc.) - only show .deb packages
			// APT index files: Release, InRelease, Release.gpg, Packages.gz, Packages, Sources.gz, Sources, etc.
			// APT packages: .deb files
			if pmType == TypeAPT {
				if err != nil {
					// No metadata, can't determine type - skip it
					return nil
				}
				// Check if it's an index file using is_index flag or URL path pattern
				if metadata.IsIndex || !strings.HasSuffix(metadata.URLPath, ".deb") {
					return nil
				}
			}

			entry := CacheEntry{
				Type:     pmType,
				Path:     path,
				Size:     info.Size(),
				CachedAt: info.ModTime(),
			}

			if err == nil {
				entry.Name = metadata.Name
				entry.Version = metadata.Version
				entry.Architecture = metadata.Architecture
				entry.Description = metadata.Description
				entry.License = metadata.License
				entry.FetchDuration = metadata.FetchDuration
				entry.AccessCount = metadata.AccessCount

				// Enrich with index data by matching URL
				if indexPkg, found := indexDataByURL[metadata.URLPath]; found {
					// Use index data for Description and License if available
					if entry.Description == "" && indexPkg.Description != "" {
						entry.Description = indexPkg.Description
					}
					if entry.License == "" && indexPkg.License != "" {
						entry.License = indexPkg.License
					}
					// Also use index data for Name/Version if cache metadata is missing
					if entry.Name == "" && indexPkg.Name != "" {
						entry.Name = indexPkg.Name
					}
					if entry.Version == "" && indexPkg.Version != "" {
						entry.Version = indexPkg.Version
					}
					if entry.Architecture == "" && indexPkg.Architecture != "" {
						entry.Architecture = indexPkg.Architecture
					}
				}
			}

			packagesByType[pmType] = append(packagesByType[pmType], entry)
			return nil
		}); err != nil {
			log.Printf("Error walking directory: %v", err)
		}
	}

	// Display results
	if len(packagesByType) == 0 {
		if filterType != "" {
			fmt.Printf("No cached packages found for type: %s\n", filterType)
		} else {
			fmt.Println("No cached packages found")
		}
		return
	}

	// Print summary
	totalPackages := 0
	totalSize := int64(0)
	for _, entries := range packagesByType {
		totalPackages += len(entries)
		for _, entry := range entries {
			totalSize += entry.Size
		}
	}

	fmt.Printf("Package Manager Cache Summary\n")
	fmt.Printf("═════════════════════════════\n")
	fmt.Printf("Total packages: %d\n", totalPackages)
	fmt.Printf("Total size: %s\n\n", FormatBytes(totalSize))

	// Print by type
	for _, pmType := range pmTypes {
		entries, exists := packagesByType[pmType]
		if !exists || len(entries) == 0 {
			continue
		}

		typeSize := int64(0)
		for _, entry := range entries {
			typeSize += entry.Size
		}

		fmt.Printf("📦 %s (%d packages, %s)\n", string(pmType), len(entries), FormatBytes(typeSize))
		fmt.Printf("   └─ Cache: %s\n", filepath.Join(cacheRoot, string(pmType)))

		// If filtering by type, show all packages. Otherwise, show only recent 5
		showAll := filterType != ""
		maxToShow := 5
		if showAll {
			maxToShow = len(entries)
		}

		if len(entries) > maxToShow && !showAll {
			fmt.Printf("   └─ Recent packages:\n")
			// Sort entries
			sortedEntries := make([]CacheEntry, len(entries))
			copy(sortedEntries, entries)
			sortPackages(sortedEntries, sortBy)
			for i := 0; i < maxToShow && i < len(sortedEntries); i++ {
				entry := sortedEntries[i]
				displayName := formatPackageEntry(entry)
				extraInfo := formatPackageExtraInfo(entry)
				if entry.FetchDuration > 0 {
					fmt.Printf("      • %s (%s, fetched in %dms, %d accesses)%s\n", displayName, FormatBytes(entry.Size), entry.FetchDuration.Milliseconds(), entry.AccessCount, extraInfo)
				} else {
					fmt.Printf("      • %s (%s, %d accesses)%s\n", displayName, FormatBytes(entry.Size), entry.AccessCount, extraInfo)
				}
			}
			if len(entries) > maxToShow {
				fmt.Printf("      ... and %d more\n", len(entries)-maxToShow)
			}
		} else {
			// Show all entries (either <= 5 total, or --type was specified)
			if showAll && len(entries) > 0 {
				fmt.Printf("   └─ Packages:\n")
			}
			// Sort entries
			sortedEntries := make([]CacheEntry, len(entries))
			copy(sortedEntries, entries)
			sortPackages(sortedEntries, sortBy)
			for _, entry := range sortedEntries {
				displayName := formatPackageEntry(entry)
				extraInfo := formatPackageExtraInfo(entry)
				if showAll {
					// When showing all, use bullet format
					if entry.FetchDuration > 0 {
						fmt.Printf("      • %s (%s, fetched in %dms, %d accesses)%s\n", displayName, FormatBytes(entry.Size), entry.FetchDuration.Milliseconds(), entry.AccessCount, extraInfo)
					} else {
						fmt.Printf("      • %s (%s, %d accesses)%s\n", displayName, FormatBytes(entry.Size), entry.AccessCount, extraInfo)
					}
				} else {
					// When showing <= 5, use tree format
					if entry.FetchDuration > 0 {
						fmt.Printf("   └─ %s (%s, fetched in %dms, %d accesses)%s\n", displayName, FormatBytes(entry.Size), entry.FetchDuration.Milliseconds(), entry.AccessCount, extraInfo)
					} else {
						fmt.Printf("   └─ %s (%s, %d accesses)%s\n", displayName, FormatBytes(entry.Size), entry.AccessCount, extraInfo)
					}
				}
			}
		}
		fmt.Println()
	}

	// Calculate and display time saved
	var totalTimeSaved time.Duration
	var packagesWithHits int
	for _, entries := range packagesByType {
		for _, entry := range entries {
			if entry.FetchDuration > 0 && entry.AccessCount > 1 {
				timeSaved := entry.FetchDuration * time.Duration(entry.AccessCount-1)
				totalTimeSaved += timeSaved
				packagesWithHits++
			}
		}
	}

	if totalTimeSaved > 0 {
		fmt.Println()
		fmt.Printf("⚡ Performance Summary\n")
		fmt.Printf("═════════════════════\n")
		fmt.Printf("Packages with cache hits: %d\n", packagesWithHits)
		fmt.Printf("Total time saved: %s\n", formatDuration(totalTimeSaved))
	}
}

// loadMetadata loads package metadata from a .meta file
func loadMetadata(cachePath string) (Metadata, error) {
	metaPath := cachePath + ".meta"
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return Metadata{}, err
	}
	var metadata Metadata
	err = json.Unmarshal(data, &metadata)
	return metadata, err
}

// formatPackageEntry formats a package entry for display
func formatPackageEntry(entry CacheEntry) string {
	if entry.Name != "" {
		// We have metadata
		if entry.Type == TypeGo {
			// For Go modules, show: module@version
			if entry.Version != "" {
				return fmt.Sprintf("%s@%s", entry.Name, entry.Version)
			}
			return entry.Name
		}
		if entry.Architecture != "" {
			return fmt.Sprintf("%s %s (%s)", entry.Name, entry.Version, entry.Architecture)
		} else if entry.Version != "" {
			return fmt.Sprintf("%s %s", entry.Name, entry.Version)
		}
		return entry.Name
	}
	// No metadata, try to extract from path for Go modules
	if entry.Type == TypeGo {
		// Try to parse Go module path from cache path
		// Cache paths are hashed, but we can try to load metadata from the .meta file
		// This is a fallback if metadata wasn't loaded
		return filepath.Base(entry.Path)
	}
	// No metadata, show hash
	return filepath.Base(entry.Path)
}

// sortPackages sorts a slice of CacheEntry based on the sortBy parameter
// sortBy can be: "name" (name then version, default), "time" (cached time), "size" (file size), "accessed" (access count)
func sortPackages(entries []CacheEntry, sortBy string) {
	if sortBy == "" {
		sortBy = "name" // Default to name
	}

	switch sortBy {
	case "name":
		sort.Slice(entries, func(i, j int) bool {
			// Sort by name first
			if entries[i].Name != entries[j].Name {
				return entries[i].Name < entries[j].Name
			}
			// Then by version
			return entries[i].Version < entries[j].Version
		})
	case "time":
		sort.Slice(entries, func(i, j int) bool {
			// Most recent first
			return entries[i].CachedAt.After(entries[j].CachedAt)
		})
	case "size":
		sort.Slice(entries, func(i, j int) bool {
			// Largest first
			return entries[i].Size > entries[j].Size
		})
	case "accessed":
		sort.Slice(entries, func(i, j int) bool {
			// Most accessed first
			if entries[i].AccessCount != entries[j].AccessCount {
				return entries[i].AccessCount > entries[j].AccessCount
			}
			// Tie-breaker: most recent first
			return entries[i].CachedAt.After(entries[j].CachedAt)
		})
	default:
		// Unknown sort, default to name
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].Name != entries[j].Name {
				return entries[i].Name < entries[j].Name
			}
			return entries[i].Version < entries[j].Version
		})
	}
}

// formatPackageExtraInfo formats additional package information (Description, License)
func formatPackageExtraInfo(entry CacheEntry) string {
	var parts []string
	if entry.Description != "" {
		// Truncate description if too long
		desc := entry.Description
		if len(desc) > 60 {
			desc = desc[:57] + "..."
		}
		parts = append(parts, fmt.Sprintf(" - %s", desc))
	}
	if entry.License != "" {
		parts = append(parts, fmt.Sprintf(" [%s]", entry.License))
	}
	return strings.Join(parts, "")
}

// formatDuration formats a duration in a human-readable way
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
