package commands

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/packages"
	v2index "github.com/Delphos-Labs/roxxy/internal/packages/index/v2"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// HandlePackageParseV2 parses index files using v2 format
func HandlePackageParseV2(cfg Config, args []string) {
	var pmType string
	var force bool
	fs := flag.NewFlagSet("package parse", flag.ExitOnError)
	fs.StringVar(&pmType, "type", "", "Package type to parse: apk, apt, npm, pypi, go (default: all types)")
	fs.BoolVar(&force, "force", false, "Force re-parsing and re-enrichment of all packages, even if already enriched")
	util.Logerr(fs.Parse(args))

	// Get index directory from config
	// Create v2 index database using helper function
	indexDir := IndexDBDir(cfg.CacheDir)
	indexDB := v2index.NewIndexDBV2(indexDir)

	// Ensure v2 index exists (migrate from v1 if needed)
	v1IndexDir := IndexDBDir(cfg.CacheDir)
	if err := v2index.EnsureV2Index(v1IndexDir, indexDir); err != nil {
		fmt.Printf("Warning: Migration from v1 failed: %v\n", err)
	}

	typesToParse, err := GetPackageTypesToParseV2(pmType)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		fmt.Println("Valid types: apk, apt, npm, pypi, go")
		os.Exit(1)
	}

	fmt.Println("Parsing index files (v2 format)...")
	fmt.Println()

	fmt.Printf("Cache directory: %s\n", cfg.CacheDir)
	fmt.Printf("Index directory: %s\n", IndexDBDir(cfg.CacheDir))

	for _, pt := range typesToParse {
		fmt.Printf("Parsing %s index files...\n", pt)
		typeDir := PackageTypeDir(cfg.CacheDir, pt)
		fmt.Printf("  Type directory: %s\n", typeDir)

		// Check if directory exists
		if _, err := os.Stat(typeDir); os.IsNotExist(err) {
			fmt.Printf("  ⚠ Directory does not exist: %s\n", typeDir)
			continue
		}

		// Parse cached index files
		// For APT, we need index files to get group information (distribution/component)
		// For APK/NPM/PyPI, index files help discover available packages
		// For Go, we don't parse index files (we use .zip files directly)
		skipIndexFiles := pt == "go"
		if !skipIndexFiles {
			count, err := ParseCachedIndexFilesV2(indexDB, cfg.CacheDir, pt)
			if err != nil {
				fmt.Printf("  Error: %v\n", err)
				continue
			}
			if count > 0 {
				fmt.Printf("  ✓ Parsed %d index file(s)\n", count)
			} else {
				fmt.Printf("  ℹ No index files found for %s\n", pt)
			}
		} else {
			fmt.Printf("  ℹ Skipping index file parsing for Go (using .zip files directly)\n")
		}

		// Scan cached package files and add them to index
		// This is the primary way packages are discovered for APT and Go
		// For other types, this adds packages that might not have index files
		fmt.Printf("  Scanning cached package files...\n")
		addedCount, err := AddCachedPackagesToIndexV2(indexDB, cfg.CacheDir, pt)
		if err != nil {
			fmt.Printf("  ⚠ Warning: Failed to add cached packages to index: %v\n", err)
		} else {
			// Count total unique packages the same way the UI does
			allPkgs, err := indexDB.GetPackages(pt)
			if err == nil {
				uniqueCount := countUniquePackages(pt, allPkgs)
				if addedCount > 0 {
					fmt.Printf("  ✓ Added %d new package(s), total: %d unique packages\n", addedCount, uniqueCount)
				} else {
					fmt.Printf("  ℹ No new cached packages to add (total: %d unique packages)\n", uniqueCount)
				}
			} else {
				if addedCount > 0 {
					fmt.Printf("  ✓ Added %d cached package(s) to index\n", addedCount)
				} else {
					fmt.Printf("  ℹ No new cached packages to add\n")
				}
			}
		}

		// Scan cache and update cached status
		fmt.Printf("  Updating cached status...\n")
		cachedPackages, err := scanCachedPackages(cfg.CacheDir, pt)
		if err != nil {
			fmt.Printf("  ⚠ Warning: Failed to scan cached packages: %v\n", err)
		} else {
			if err := indexDB.UpdateCachedStatus(pt, cachedPackages); err != nil {
				fmt.Printf("  ⚠ Warning: Failed to update cached status: %v\n", err)
			} else {
				fmt.Printf("  ✓ Updated cached status for %d package file(s)\n", len(cachedPackages))

				// Count unique packages with cached versions (what UI shows when showUncached=false)
				allPkgs, err := indexDB.GetPackages(pt)
				if err == nil {
					packagesWithCached := 0
					var packagesWithInvalidGroup []string
					var packagesWithValidGroup []string

					for _, pkg := range allPkgs {
						hasCachedVersion := false
						for _, version := range pkg.Versions {
							if version.Cached {
								hasCachedVersion = true
								break
							}
						}
						if hasCachedVersion {
							packagesWithCached++
							// Check if group is valid for APT (must have 3 parts: basePath/distribution/component)
							if pt == "apt" {
								if pkg.Group == "" || !strings.Contains(pkg.Group, "/") {
									packagesWithInvalidGroup = append(packagesWithInvalidGroup, pkg.Name)
								} else {
									parts := strings.Split(pkg.Group, "/")
									if len(parts) < 3 {
										packagesWithInvalidGroup = append(packagesWithInvalidGroup, pkg.Name)
									} else {
										// Check if group starts with "pub" or contains "pub/repos/apt" - these are filtered out
										if strings.HasPrefix(pkg.Group, "pub/") || strings.Contains(pkg.Group, "pub/repos/apt") {
											packagesWithInvalidGroup = append(packagesWithInvalidGroup, fmt.Sprintf("%s (group: %s)", pkg.Name, pkg.Group))
										} else {
											packagesWithValidGroup = append(packagesWithValidGroup, pkg.Name)
										}
									}
								}
							}
						}
					}

					fmt.Printf("  ℹ %d unique packages have cached versions\n", packagesWithCached)
					if pt == "apt" {
						fmt.Printf("  ℹ %d packages with valid groups (will show in UI)\n", len(packagesWithValidGroup))
						if len(packagesWithInvalidGroup) > 0 {
							fmt.Printf("  ⚠ %d packages with cached versions have invalid groups (won't show in UI):\n", len(packagesWithInvalidGroup))
							for _, pkgName := range packagesWithInvalidGroup {
								fmt.Printf("    - %s\n", pkgName)
							}
						} else {
							fmt.Printf("  ℹ All %d packages have valid groups\n", packagesWithCached)
						}
					}
				}

				// Find orphan packages (in cache but not in index)
				orphans, err := findOrphanPackages(indexDB, cfg.CacheDir, pt)
				if err != nil {
					fmt.Printf("  ⚠ Warning: Failed to find orphan packages: %v\n", err)
				} else if len(orphans) > 0 {
					fmt.Printf("  ⚠ Found %d orphan package(s) in cache (not in index):\n", len(orphans))
					for _, orphan := range orphans {
						fmt.Printf("    - %s@%s", orphan.Name, orphan.Version)
						if orphan.Architecture != "" {
							fmt.Printf(" (%s)", orphan.Architecture)
						}
						fmt.Printf(" - %s\n", orphan.URLPath)
					}
				}
			}
		}

		// Enrich packages (extract license/description)
		fmt.Printf("  Enriching packages...\n")
		enrichedCount, err := enrichPackagesV2(indexDB, cfg.CacheDir, pt, force)
		if err != nil {
			fmt.Printf("  ⚠ Warning: Failed to enrich packages: %v\n", err)
		} else {
			fmt.Printf("  ✓ Enriched %d package(s)\n", enrichedCount)
		}
	}

	fmt.Println()
	fmt.Println("Done!")
	os.Exit(0)
}

// GetPackageTypesToParseV2 returns list of package types to parse
func GetPackageTypesToParseV2(pmType string) ([]string, error) {
	validTypes := map[string]bool{
		"apk":  true,
		"apt":  true,
		"npm":  true,
		"pypi": true,
		"go":   true,
	}

	if pmType == "" {
		// Parse all types
		return []string{"apk", "apt", "npm", "pypi", "go"}, nil
	}

	if !validTypes[pmType] {
		return nil, fmt.Errorf("invalid package type: %s", pmType)
	}

	return []string{pmType}, nil
}

// ParseCachedIndexFilesV2 scans cache directory and parses index files using v2 format
func ParseCachedIndexFilesV2(indexDB *v2index.IndexDBV2, cacheDir string, pmType string) (int, error) {
	typeDir := filepath.Join(cacheDir, "packages", pmType)
	if _, err := os.Stat(typeDir); os.IsNotExist(err) {
		return 0, nil
	}

	// Note: We don't clear the existing index file here
	// StorePackages will merge new packages with existing ones correctly

	// Get parser for this type
	parser := getParserV2(indexDB, pmType)
	if parser == nil {
		return 0, nil
	}

	// Collect all packages from all index files
	var allPackages []v2index.IndexPackageV2
	var parsedCount int
	parsedFiles := make(map[string]bool)

	// Walk the cache directory
	log.Printf("[index-v2] Scanning %s for index files...", typeDir)
	err := filepath.WalkDir(typeDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		if strings.HasSuffix(path, ".meta") {
			return nil
		}

		if parsedFiles[path] {
			return nil
		}

		// Load metadata to check if this is an index file
		metaPath := path + ".meta"
		metaData, err := os.ReadFile(metaPath)
		if err != nil {
			return nil
		}

		var metadata struct {
			URLPath     string `json:"url_path"`
			IsIndex     bool   `json:"is_index"`
			ContentType string `json:"content_type,omitempty"`
		}
		if err := json.Unmarshal(metaData, &metadata); err != nil {
			return nil
		}

		isIndex := metadata.IsIndex
		if !isIndex && metadata.URLPath != "" {
			isIndex = parser.CanParse(metadata.URLPath)
		}

		if !isIndex || !parser.CanParse(metadata.URLPath) {
			return nil
		}

		log.Printf("[index-v2] Found index file: %s (%s)", filepath.Base(path), metadata.URLPath)

		// Read and parse
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("Warning: Failed to read index file %s: %v", path, err)
			return nil
		}

		packages, err := parser.Parse(data, metadata.URLPath, metadata.ContentType, path)
		if err != nil {
			log.Printf("Warning: Failed to parse index file %s: %v", path, err)
			return nil
		}

		allPackages = append(allPackages, packages...)
		parsedFiles[path] = true
		parsedCount++
		log.Printf("[index-v2] Parsed index file: %s (%s) - %d packages", filepath.Base(path), metadata.URLPath, len(packages))
		return nil
	})

	if err != nil {
		return parsedCount, fmt.Errorf("error walking cache directory: %w", err)
	}

	// Only store packages if we found any (don't overwrite existing index with empty list)
	if len(allPackages) > 0 {
		if err := indexDB.StorePackages(pmType, allPackages); err != nil {
			return parsedCount, fmt.Errorf("failed to store packages: %w", err)
		}

		// Count unique packages the same way the UI does
		uniqueCount := countUniquePackages(pmType, allPackages)
		log.Printf("[index-v2] Rebuilt v2 index for %s: %d unique packages from %d index files", pmType, uniqueCount, parsedCount)
	} else {
		log.Printf("[index-v2] No packages found from index files for %s (skipping StorePackages to preserve existing index)", pmType)
	}

	return parsedCount, nil
}

// getParserV2 gets the parser for a package type
func getParserV2(indexDB *v2index.IndexDBV2, pmType string) v2index.ParserV2 {
	return indexDB.GetParser(pmType)
}

// countUniquePackages counts unique packages the same way the UI does
// For APT: counts unique packages per component (group) - same package in different components counts separately
// For other types: counts unique package names
func countUniquePackages(pmType string, packages []v2index.IndexPackageV2) int {
	if pmType == "apt" {
		// For APT, the UI counts unique packages at the component level
		// Same package name in different components counts as separate packages
		// This matches the API's packageCounts logic which counts per component
		packageSet := make(map[string]bool)
		for _, pkg := range packages {
			// Use group + name to count unique packages per component
			// This matches how the UI counts: unique packages at component level
			key := pkg.Name
			if pkg.Group != "" {
				// Include group to count per component (basePath/distribution/component)
				key = fmt.Sprintf("%s:%s", pkg.Group, pkg.Name)
			}
			packageSet[key] = true
		}
		return len(packageSet)
	}

	// For other types, count unique package names (same as UI)
	// The UI counts packages (not versions) recursively through groups
	packageSet := make(map[string]bool)
	for _, pkg := range packages {
		packageSet[pkg.Name] = true
	}
	return len(packageSet)
}

// scanCachedPackages scans the cache directory and builds a map of cached packages
// Returns map with keys like "apk:package@version" or "apk:package@version:arch"
func scanCachedPackages(cacheDir string, pmType string) (map[string]bool, error) {
	typeDir := filepath.Join(cacheDir, "packages", pmType)
	if _, err := os.Stat(typeDir); os.IsNotExist(err) {
		return make(map[string]bool), nil
	}

	cachedPackages := make(map[string]bool)

	err := packages.WalkPackageFiles(typeDir, func(info *packages.CachedFileInfo) error {
		// Build key: "type:name@version" or "type:name@version:arch"
		key := fmt.Sprintf("%s:%s@%s", pmType, info.Name, info.Version)
		if info.Architecture != "" {
			key = fmt.Sprintf("%s:%s@%s:%s", pmType, info.Name, info.Version, info.Architecture)
		}

		cachedPackages[key] = true
		return nil
	})

	return cachedPackages, err
}

// OrphanPackage represents a package file in cache that's not in the index
type OrphanPackage struct {
	Name         string
	Version      string
	Architecture string
	URLPath      string
}

// findOrphanPackages finds packages that are in cache but not in index
func findOrphanPackages(indexDB *v2index.IndexDBV2, cacheDir string, pmType string) ([]OrphanPackage, error) {
	typeDir := filepath.Join(cacheDir, "packages", pmType)
	if _, err := os.Stat(typeDir); os.IsNotExist(err) {
		return []OrphanPackage{}, nil
	}

	// Get all packages from index
	indexPkgs, err := indexDB.GetPackages(pmType)
	if err != nil {
		return nil, err
	}

	// Build a map of packages in index: "name@version" or "name@version:arch"
	indexMap := make(map[string]bool)
	for _, pkg := range indexPkgs {
		for _, version := range pkg.Versions {
			key := fmt.Sprintf("%s@%s", pkg.Name, version.Version)
			if version.Architecture != "" {
				key = fmt.Sprintf("%s@%s:%s", pkg.Name, version.Version, version.Architecture)
			}
			indexMap[key] = true
		}
	}

	// Scan cache and find orphans
	var orphans []OrphanPackage
	err = packages.WalkPackageFiles(typeDir, func(info *packages.CachedFileInfo) error {
		// Skip if we don't have name and version
		if info.Name == "" || info.Version == "" {
			return nil
		}

		// Normalize package name (same as AddCachedPackagesToIndexV2)
		packageName := info.Name
		switch pmType {
		case "go":
			packageName = v2index.DecodeGoModulePath(info.Name)
		case "pypi":
			if info.URLPath != "" {
				parts := strings.Split(strings.Trim(info.URLPath, "/"), "/")
				if len(parts) >= 2 && parts[0] == "simple" {
					packageName = parts[1]
				} else {
					packageName = strings.ReplaceAll(info.Name, "_", "-")
				}
			} else {
				packageName = strings.ReplaceAll(info.Name, "_", "-")
			}
		}

		// Check if this package/version is in index
		key := fmt.Sprintf("%s@%s", packageName, info.Version)
		if info.Architecture != "" {
			key = fmt.Sprintf("%s@%s:%s", packageName, info.Version, info.Architecture)
		}

		if !indexMap[key] {
			// Also check with original name for PyPI
			if pmType == "pypi" && packageName != info.Name {
				key2 := fmt.Sprintf("%s@%s", info.Name, info.Version)
				if !indexMap[key2] {
					orphans = append(orphans, OrphanPackage{
						Name:         packageName,
						Version:      info.Version,
						Architecture: info.Architecture,
						URLPath:      info.URLPath,
					})
				}
			} else {
				orphans = append(orphans, OrphanPackage{
					Name:         packageName,
					Version:      info.Version,
					Architecture: info.Architecture,
					URLPath:      info.URLPath,
				})
			}
		}

		return nil
	})

	return orphans, err
}

// enrichPackagesV2 enriches packages by extracting license and description from cached files
// Scans all cached package files (not just ones in index) to enrich them
// If force is true, re-enriches packages even if they already have metadata
func enrichPackagesV2(indexDB *v2index.IndexDBV2, cacheDir string, pmType string, force bool) (int, error) {
	typeDir := filepath.Join(cacheDir, "packages", pmType)
	if _, err := os.Stat(typeDir); os.IsNotExist(err) {
		return 0, nil
	}

	// Determine package type
	var pkgType packages.Type
	switch pmType {
	case "apk":
		pkgType = packages.TypeAPK
	case "apt":
		pkgType = packages.TypeAPT
	case "npm":
		pkgType = packages.TypeNPM
	case "pypi":
		pkgType = packages.TypePyPI
	case "go":
		pkgType = packages.TypeGo
	default:
		return 0, nil
	}

	// Load index once at the start for batch updates (much faster than loading 69 times)
	indexPkgs, err := indexDB.GetPackages(pmType)
	if err != nil {
		log.Printf("[enrich] Warning: Failed to load index for %s: %v", pmType, err)
		indexPkgs = []v2index.IndexPackageV2{}
	}

	// Build enriched map and package map for fast lookups
	enrichedMap := make(map[string]bool)               // name -> true if enriched
	pkgMap := make(map[string]*v2index.IndexPackageV2) // name -> package pointer
	for i := range indexPkgs {
		pkg := &indexPkgs[i]
		pkgMap[pkg.Name] = pkg
		if !force && (pkg.License != "" || pkg.Description != "") {
			enrichedMap[pkg.Name] = true
		}
	}

	// Collect all updates in memory, then write once at the end (much faster)
	// This avoids loading and writing the index 69 times (once per package)
	batchUpdates := make(map[string]func(*v2index.IndexPackageV2))

	enrichedCount := 0
	processedCount := 0
	// Walk cache directory and process packages synchronously
	// Use common helper to walk package files (not index files)
	err = packages.WalkPackageFiles(typeDir, func(info *packages.CachedFileInfo) error {
		// For Go, only enrich .zip files (not .info files)
		// Check URL path since cache files are stored by hash, not extension
		if pmType == "go" && !strings.HasSuffix(info.URLPath, ".zip") {
			return nil
		}

		// Skip if already enriched (unless force is true)
		if !force && enrichedMap[info.Name] {
			return nil
		}

		// Process synchronously
		pkgToEnrich := packages.PackageToEnrich{
			Type:         pkgType,
			CachePath:    info.Path,
			Name:         info.Name,
			Version:      info.Version,
			Architecture: info.Architecture,
		}

		// Extract metadata (but don't update index yet - batch it)
		// Use v1 enricher to extract metadata (same logic as v2 enricher uses internally)
		enricherV1 := packages.NewPackageEnricher(cacheDir, nil, 1.0)
		metadata := enricherV1.ExtractMetadata(pkgToEnrich)
		if metadata != nil {
			// Update .meta file immediately (small file, fast operation)
			updateMetaFile(pkgToEnrich.CachePath, metadata)

			// Build update function for this package (batch index update)
			updateFunc := buildUpdateFunc(pkgToEnrich, metadata, force, pkgMap)
			if updateFunc != nil {
				// Determine package name (same logic as updateIndex)
				packageName := determinePackageName(pkgToEnrich, metadata, pkgMap)
				batchUpdates[packageName] = updateFunc
				enrichedCount++
			}
		}
		processedCount++

		// Log progress every 50 packages
		if processedCount%50 == 0 {
			log.Printf("[enrich] Processed %d packages so far, enriched %d", processedCount, enrichedCount)
		}

		// Only limit when not using --force (to avoid taking too long during normal operation)
		if !force && processedCount >= 100 {
			log.Printf("[enrich] Reached limit of 100 packages (use --force to process all), stopping. Processed %d, enriched %d", processedCount, enrichedCount)
			return filepath.SkipAll
		}

		return nil
	})

	if err != nil {
		return enrichedCount, err
	}

	// Apply all updates in a single batch write (much faster than 69 individual writes)
	if len(batchUpdates) > 0 {
		if err := indexDB.UpdatePackageMetadataBatch(pmType, indexPkgs, batchUpdates); err != nil {
			log.Printf("[enrich] Warning: Failed to batch update index: %v", err)
		}
	}

	return enrichedCount, nil
}

// buildUpdateFunc creates an update function for a package's metadata
func buildUpdateFunc(pkg packages.PackageToEnrich, metadata map[string]interface{}, force bool, pkgMap map[string]*v2index.IndexPackageV2) func(*v2index.IndexPackageV2) {
	hasLicense := false
	hasDescription := false
	hasDependencies := false

	if license, ok := metadata["license"].(string); ok && license != "" {
		hasLicense = true
	}
	if desc, ok := metadata["description"].(string); ok && desc != "" {
		hasDescription = true
	}
	if deps, ok := metadata["dependencies"].([]string); ok && len(deps) > 0 {
		hasDependencies = true
	}

	if !hasLicense && !hasDescription && !hasDependencies {
		return nil
	}

	return func(p *v2index.IndexPackageV2) {
		// Update version-specific license (license is version-level only)
		for i := range p.Versions {
			if p.Versions[i].Version == pkg.Version {
				// Found the matching version, update its license
				if hasLicense && (force || p.Versions[i].License == "") {
					p.Versions[i].License = metadata["license"].(string)
				}
				break
			}
		}

		// Update package-level description (description is package-level only)
		if hasDescription {
			newDesc := metadata["description"].(string)
			if newDesc != "" && len(newDesc) > len(p.Description) {
				p.Description = newDesc
			}
		}
		if hasDependencies && (force || len(p.Dependencies) == 0) {
			p.Dependencies = metadata["dependencies"].([]string)
		}
	}
}

// determinePackageName determines the correct package name for updating the index
func determinePackageName(pkg packages.PackageToEnrich, metadata map[string]interface{}, pkgMap map[string]*v2index.IndexPackageV2) string {
	packageName := pkg.Name
	if pkg.Type != packages.TypeNPM {
		// For non-NPM packages, use name from metadata if available
		if nameFromMetadata, ok := metadata["name"].(string); ok && nameFromMetadata != "" {
			packageName = nameFromMetadata
		}
	} else {
		// For NPM, always use cached name (from URL path)
		packageName = pkg.Name
	}

	// For PyPI, check if canonical name exists, otherwise use original
	if pkg.Type == packages.TypePyPI && packageName != pkg.Name {
		if _, exists := pkgMap[pkg.Name]; exists {
			return pkg.Name
		}
		if _, exists := pkgMap[packageName]; exists {
			return packageName
		}
		// Neither exists, use canonical name
		return packageName
	}

	return packageName
}

// updateMetaFile updates the .meta file with version-specific license/description
func updateMetaFile(cachePath string, metadata map[string]interface{}) {
	metaPath := cachePath + ".meta"

	// Read existing metadata
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		// No .meta file exists, skip (shouldn't happen, but be safe)
		return
	}

	// Unmarshal into a map to preserve all fields
	var metaMap map[string]interface{}
	if err := json.Unmarshal(metaData, &metaMap); err != nil {
		return // Can't parse, skip
	}

	// Update license/description if we have them
	updated := false
	if license, ok := metadata["license"].(string); ok && license != "" {
		metaMap["license"] = license
		updated = true
	}
	if desc, ok := metadata["description"].(string); ok && desc != "" {
		metaMap["description"] = desc
		updated = true
	}

	// Write back if updated
	if updated {
		newData, err := json.Marshal(metaMap)
		if err != nil {
			return // Silently skip on error
		}

		util.Logerr(os.WriteFile(metaPath, newData, 0644)) // Ignore errors - not critical
	}
}

// AddCachedPackagesToIndexV2 scans cached package files and adds them to the index
// This ensures packages cached without index files (e.g., Go .zip files without .info files) are included
func AddCachedPackagesToIndexV2(indexDB *v2index.IndexDBV2, cacheDir string, pmType string) (int, error) {
	typeDir := filepath.Join(cacheDir, "packages", pmType)
	if _, err := os.Stat(typeDir); os.IsNotExist(err) {
		return 0, nil
	}

	var addedCount int
	var packagesToAdd []v2index.IndexPackageV2

	// Load existing packages ONCE (not once per file!)
	existingPkgs, err := indexDB.GetPackages(pmType)
	if err != nil {
		log.Printf("[add-cached] Warning: Failed to load existing packages: %v", err)
		existingPkgs = []v2index.IndexPackageV2{}
	}

	// Build a map for faster lookup
	existingPkgMap := make(map[string]*v2index.IndexPackageV2)
	for i := range existingPkgs {
		existingPkgMap[existingPkgs[i].Name] = &existingPkgs[i]
	}

	// Walk cached package files
	var processedCount, skippedCount int
	err = packages.WalkPackageFiles(typeDir, func(info *packages.CachedFileInfo) error {
		processedCount++

		// For Go, only process .zip files
		if pmType == "go" && !strings.HasSuffix(info.URLPath, ".zip") {
			skippedCount++
			return nil
		}

		// Skip if we don't have name and version
		if info.Name == "" || info.Version == "" {
			log.Printf("[add-cached] Skipping %s: name=%q version=%q", info.URLPath, info.Name, info.Version)
			skippedCount++
			return nil
		}

		// Get file size
		fileInfo, err := os.Stat(info.Path)
		if err != nil {
			return nil
		}

		// Normalize package name based on type
		packageName := info.Name
		switch pmType {
		// Decode Go module proxy encoding
		case "go":
			packageName = v2index.DecodeGoModulePath(info.Name)
		case "pypi":
			// PyPI package names are normalized to use hyphens (PEP 503)
			// Filenames can have underscores, but canonical names use hyphens
			// Extract from URL path if available (has canonical name), otherwise normalize
			if info.URLPath != "" {
				// URL path format: /simple/package-name/package-name-version.whl
				parts := strings.Split(strings.Trim(info.URLPath, "/"), "/")
				if len(parts) >= 2 && parts[0] == "simple" {
					packageName = parts[1] // This is the canonical name from the URL
				} else {
					// Fallback: normalize underscores to hyphens
					packageName = strings.ReplaceAll(info.Name, "_", "-")
				}
			} else {
				// No URL path, normalize underscores to hyphens
				packageName = strings.ReplaceAll(info.Name, "_", "-")
			}
		}

		// Create version entry
		version := v2index.PackageVersion{
			Version:      info.Version,
			Architecture: info.Architecture, // Include architecture for proper matching
			CachePath:    info.URLPath,      // Use URL path for cache lookup
			URL:          info.URLPath,
			Size:         fileInfo.Size(),
			Cached:       true, // These are cached files
			IndexedAt:    time.Now(),
		}

		// Check if package already exists in index (using cached map)
		var existingPkg *v2index.IndexPackageV2
		if pkg, found := existingPkgMap[packageName]; found {
			existingPkg = pkg
		} else if pmType == "pypi" && packageName != info.Name {
			// For PyPI, also check if original name (with underscores) exists
			if pkg, found := existingPkgMap[info.Name]; found {
				existingPkg = pkg
				// Use the existing name (with underscores) instead of creating a duplicate
				packageName = info.Name
			}
		}

		if existingPkg != nil {
			// Package exists - check if version already exists before calling AddVersion
			versionExists := false
			for _, v := range existingPkg.Versions {
				if v.Version == version.Version {
					// Match architecture: both must be equal (including both empty)
					if v.Architecture == version.Architecture {
						versionExists = true
						break
					}
				}
			}

			if !versionExists {
				// Version doesn't exist, add it
				if err := indexDB.AddVersion(pmType, packageName, version); err != nil {
					log.Printf("Warning: Failed to add version %s to package %s: %v", info.Version, packageName, err)
				} else {
					addedCount++
				}
			}
			// If version exists, skip (no need to update since we're just adding cached packages)
			return nil
		}

		// Package doesn't exist, create new entry
		pkg := v2index.IndexPackageV2{
			Type:      pmType,
			Name:      packageName,
			Versions:  []v2index.PackageVersion{version},
			IndexedAt: time.Now(),
		}
		packagesToAdd = append(packagesToAdd, pkg)
		return nil
	})

	if err != nil {
		return addedCount, err
	}

	log.Printf("[add-cached] Processed %d files, skipped %d, adding %d new packages, %d new versions",
		processedCount, skippedCount, len(packagesToAdd), addedCount)

	// Store new packages
	if len(packagesToAdd) > 0 {
		log.Printf("[add-cached] Storing %d new packages to index", len(packagesToAdd))
		if err := indexDB.StorePackages(pmType, packagesToAdd); err != nil {
			return addedCount, fmt.Errorf("failed to store packages: %w", err)
		}
		// Verify packages were stored
		verifyPkgs, err := indexDB.GetPackages(pmType)
		if err == nil {
			log.Printf("[add-cached] Index now contains %d packages after storing", len(verifyPkgs))
		}
		addedCount += len(packagesToAdd)
	}

	return addedCount, nil
}
