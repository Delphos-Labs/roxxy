package v2

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
)

// IndexDBV2 stores parsed package metadata in the new v2 format
type IndexDBV2 struct {
	indexDir string
	parsers  map[string]ParserV2
}

// NewIndexDBV2 creates a new v2 index database
func NewIndexDBV2(indexDir string) *IndexDBV2 {
	db := &IndexDBV2{
		indexDir: indexDir,
		parsers:  make(map[string]ParserV2),
	}

	// Register parsers
	db.parsers["apk"] = &APKParserV2{}
	db.parsers["apt"] = &APTParserV2{}
	db.parsers["pypi"] = &PyPIParserV2{}
	db.parsers["npm"] = &NPMParserV2{}
	db.parsers["go"] = &GoParserV2{}

	// Ensure index directory exists
	// Only try to create if indexDir is not empty
	if indexDir != "" {
		if err := os.MkdirAll(indexDir, 0755); err != nil {
			log.Printf("Warning: Failed to create index dir %s: %v", indexDir, err)
		}
	} else {
		log.Printf("Warning: indexDir is empty - index operations may fail")
	}

	return db
}

// StorePackages stores parsed packages, merging by package name and appending versions
func (db *IndexDBV2) StorePackages(pmType string, pkgs []IndexPackageV2) error {
	// Store as JSON file: index/{type}/packages_v2.json
	typeDir := filepath.Join(db.indexDir, pmType)
	if err := os.MkdirAll(typeDir, 0755); err != nil {
		return fmt.Errorf("failed to create type dir: %w", err)
	}

	// Load existing packages
	existingPath := filepath.Join(typeDir, "packages_v2.json")
	existing := make(map[string]*IndexPackageV2)

	if data, err := os.ReadFile(existingPath); err == nil {
		var existingList []IndexPackageV2
		if err := json.Unmarshal(data, &existingList); err == nil {
			for i := range existingList {
				pkg := &existingList[i]
				existing[pkg.Name] = pkg
			}
		}
	}

	// Merge new packages by name, appending versions
	for _, pkg := range pkgs {
		if existing[pkg.Name] == nil {
			// New package, create entry
			newPkg := pkg
			existing[pkg.Name] = &newPkg
		} else {
			// Existing package, merge versions
			existingPkg := existing[pkg.Name]
			// Add new versions that don't already exist
			versionMap := make(map[string]bool)
			for _, v := range existingPkg.Versions {
				key := v.Version
				if v.Architecture != "" {
					key = fmt.Sprintf("%s:%s", v.Version, v.Architecture)
				}
				versionMap[key] = true
			}
			for _, newVersion := range pkg.Versions {
				key := newVersion.Version
				if newVersion.Architecture != "" {
					key = fmt.Sprintf("%s:%s", newVersion.Version, newVersion.Architecture)
				}
				if !versionMap[key] {
					existingPkg.Versions = append(existingPkg.Versions, newVersion)
				}
			}
			// Update metadata if new package has better info (description is package-level only)
			if pkg.Description != "" && existingPkg.Description == "" {
				existingPkg.Description = pkg.Description
			}
			// License is version-specific, not package-level
			if len(pkg.Dependencies) > 0 && len(existingPkg.Dependencies) == 0 {
				existingPkg.Dependencies = pkg.Dependencies
			}
			// Update Group if not set (preserve group information)
			if existingPkg.Group == "" && pkg.Group != "" {
				existingPkg.Group = pkg.Group
			}
		}
	}

	// Convert back to list and sort versions
	var allPackages []IndexPackageV2
	for _, pkg := range existing {
		// Sort versions by semantic version (newest first)
		sort.Slice(pkg.Versions, func(i, j int) bool {
			return compareSemanticVersions(pkg.Versions[i].Version, pkg.Versions[j].Version)
		})
		allPackages = append(allPackages, *pkg)
	}

	// Save updated packages
	data, err := json.MarshalIndent(allPackages, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal packages: %w", err)
	}

	if err := os.WriteFile(existingPath, data, 0644); err != nil {
		log.Printf("[index-v2] ERROR: StorePackages(%s): failed to write file: %v", pmType, err)
		return fmt.Errorf("failed to write packages: %w", err)
	}

	return nil
}

// GetPackages returns all packages of the given type, sorted by name
func (db *IndexDBV2) GetPackages(pmType string) ([]IndexPackageV2, error) {
	typeDir := filepath.Join(db.indexDir, pmType)
	packagesPath := filepath.Join(typeDir, "packages_v2.json")

	data, err := os.ReadFile(packagesPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []IndexPackageV2{}, nil
		}
		log.Printf("[index-v2] ERROR: GetPackages(%s): failed to read file: %v", pmType, err)
		return nil, fmt.Errorf("failed to read packages: %w", err)
	}

	var pkgs []IndexPackageV2
	if err := json.Unmarshal(data, &pkgs); err != nil {
		log.Printf("[index-v2] ERROR: GetPackages(%s): failed to unmarshal JSON: %v", pmType, err)
		return nil, fmt.Errorf("failed to parse packages: %w", err)
	}

	// Sort versions for each package and sort packages by name
	for i := range pkgs {
		sort.Slice(pkgs[i].Versions, func(a, b int) bool {
			return compareSemanticVersions(pkgs[i].Versions[a].Version, pkgs[i].Versions[b].Version)
		})
	}
	sort.Slice(pkgs, func(i, j int) bool {
		return pkgs[i].Name < pkgs[j].Name
	})

	return pkgs, nil
}

// GetPackagesForType returns packages for a specific type, sorted by name
func (db *IndexDBV2) GetPackagesForType(pmType string) ([]IndexPackageV2, error) {
	return db.GetPackages(pmType)
}

// MergeGoPackageVersions merges versions from /vN variants for Go packages
// Groups packages by base module path (strips /vN suffix) and merges versions
func (db *IndexDBV2) MergeGoPackageVersions(pkgs []IndexPackageV2) []IndexPackageV2 {
	if len(pkgs) == 0 {
		return pkgs
	}

	// Group by base module path
	grouped := make(map[string]*IndexPackageV2)

	for i := range pkgs {
		pkg := &pkgs[i]
		if pkg.Type != "go" {
			// Not a Go package, keep as-is
			key := pkg.Name
			if grouped[key] == nil {
				grouped[key] = pkg
			}
			continue
		}

		// Get base module path (strip /vN)
		basePath := getGoBaseModulePath(pkg.Name)

		if grouped[basePath] == nil {
			// First package with this base path
			newPkg := *pkg
			newPkg.Name = basePath // Use base path as the merged name
			grouped[basePath] = &newPkg
		} else {
			// Merge versions from this variant into the base package
			basePkg := grouped[basePath]

			// Add versions that don't already exist
			versionMap := make(map[string]bool)
			for _, v := range basePkg.Versions {
				key := v.Version
				if v.Architecture != "" {
					key = fmt.Sprintf("%s:%s", v.Version, v.Architecture)
				}
				versionMap[key] = true
			}

			for _, newVersion := range pkg.Versions {
				key := newVersion.Version
				if newVersion.Architecture != "" {
					key = fmt.Sprintf("%s:%s", newVersion.Version, newVersion.Architecture)
				}
				if !versionMap[key] {
					basePkg.Versions = append(basePkg.Versions, newVersion)
				}
			}

			// Update metadata if this variant has better info (description is package-level only)
			if pkg.Description != "" && basePkg.Description == "" {
				basePkg.Description = pkg.Description
			}
			// License is version-specific, not package-level
			if len(pkg.Dependencies) > 0 && len(basePkg.Dependencies) == 0 {
				basePkg.Dependencies = pkg.Dependencies
			}
		}
	}

	// Convert back to slice, sort versions, and sort packages
	var result []IndexPackageV2
	for _, pkg := range grouped {
		// Sort versions by semantic version (newest first)
		sort.Slice(pkg.Versions, func(i, j int) bool {
			return compareSemanticVersions(pkg.Versions[i].Version, pkg.Versions[j].Version)
		})
		result = append(result, *pkg)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result
}

// UpdatePackageMetadata updates metadata for a specific package
func (db *IndexDBV2) UpdatePackageMetadata(pmType string, name string, updates func(*IndexPackageV2)) error {
	pkgs, err := db.GetPackages(pmType)
	if err != nil {
		return err
	}

	// Find and update the matching package
	updated := false
	for i := range pkgs {
		if pkgs[i].Name == name {
			updates(&pkgs[i])
			updated = true
			break
		}
	}

	if !updated {
		log.Printf("[index-v2] Package not found in index for metadata update: type=%s, name=%s (total packages: %d)", pmType, name, len(pkgs))
		return fmt.Errorf("package not found: type=%s, name=%s", pmType, name)
	}

	// Save updated packages
	typeDir := filepath.Join(db.indexDir, pmType)
	packagesPath := filepath.Join(typeDir, "packages_v2.json")

	data, err := json.MarshalIndent(pkgs, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal packages: %w", err)
	}

	return os.WriteFile(packagesPath, data, 0644)
}

// UpdatePackageMetadataBatch updates metadata for multiple packages in a single operation
// This is much more efficient than calling UpdatePackageMetadata multiple times
// pkgs should be the current state of all packages (can be modified in place)
// Returns the updated packages list
func (db *IndexDBV2) UpdatePackageMetadataBatch(pmType string, pkgs []IndexPackageV2, updates map[string]func(*IndexPackageV2)) error {
	// Apply all updates to the in-memory packages
	for name, updateFunc := range updates {
		found := false
		for i := range pkgs {
			if pkgs[i].Name == name {
				updateFunc(&pkgs[i])
				found = true
				break
			}
		}
		if !found {
			log.Printf("[index-v2] Package not found in batch update: type=%s, name=%s", pmType, name)
		}
	}

	// Save updated packages once
	typeDir := filepath.Join(db.indexDir, pmType)
	packagesPath := filepath.Join(typeDir, "packages_v2.json")

	data, err := json.MarshalIndent(pkgs, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal packages: %w", err)
	}

	return os.WriteFile(packagesPath, data, 0644)
}

// AddVersion adds a version to an existing package, or updates it if it already exists
func (db *IndexDBV2) AddVersion(pmType string, name string, version PackageVersion) error {
	return db.UpdatePackageMetadata(pmType, name, func(pkg *IndexPackageV2) {
		// Check if version already exists
		for i := range pkg.Versions {
			v := &pkg.Versions[i]
			if v.Version == version.Version {
				// Match architecture: both must be equal (including both empty)
				// For APT packages, architecture is important for matching
				archMatch := v.Architecture == version.Architecture
				if archMatch {
					// Version already exists, update it (especially Cached status)
					v.Cached = version.Cached
					v.Size = version.Size
					v.CachePath = version.CachePath
					v.URL = version.URL
					// Update architecture if it was missing
					if v.Architecture == "" && version.Architecture != "" {
						v.Architecture = version.Architecture
					}
					// Don't overwrite IndexedAt if it's already set
					if v.IndexedAt.IsZero() {
						v.IndexedAt = version.IndexedAt
					}
					return
				}
			}
		}
		// Add new version
		pkg.Versions = append(pkg.Versions, version)
		// Sort versions by semantic version (newest first)
		sort.Slice(pkg.Versions, func(i, j int) bool {
			return compareSemanticVersions(pkg.Versions[i].Version, pkg.Versions[j].Version)
		})
	})
}

// UpdateCachedStatus updates the Cached flag for packages based on what's actually cached
func (db *IndexDBV2) UpdateCachedStatus(pmType string, cachedPackages map[string]bool) error {
	pkgs, err := db.GetPackages(pmType)
	if err != nil {
		return err
	}

	// Update cached status for each version
	for i := range pkgs {
		for j := range pkgs[i].Versions {
			key := fmt.Sprintf("%s:%s@%s", pmType, pkgs[i].Name, pkgs[i].Versions[j].Version)
			if pkgs[i].Versions[j].Architecture != "" {
				key = fmt.Sprintf("%s:%s@%s:%s", pmType, pkgs[i].Name, pkgs[i].Versions[j].Version, pkgs[i].Versions[j].Architecture)
			}
			pkgs[i].Versions[j].Cached = cachedPackages[key]
		}
		// Sort versions by semantic version (newest first)
		sort.Slice(pkgs[i].Versions, func(a, b int) bool {
			return compareSemanticVersions(pkgs[i].Versions[a].Version, pkgs[i].Versions[b].Version)
		})
	}

	// Save updated packages
	typeDir := filepath.Join(db.indexDir, pmType)
	packagesPath := filepath.Join(typeDir, "packages_v2.json")

	data, err := json.MarshalIndent(pkgs, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal packages: %w", err)
	}

	return os.WriteFile(packagesPath, data, 0644)
}

// GetParser returns the parser for a package type
func (db *IndexDBV2) GetParser(pmType string) ParserV2 {
	return db.parsers[pmType]
}

// ParserV2 is an interface for parsing index files in v2 format
type ParserV2 interface {
	// Parse extracts package metadata from index file data
	Parse(data []byte, indexURL string, contentType string, cachePath string) ([]IndexPackageV2, error)
	// CanParse returns true if this parser can handle the given index file
	CanParse(indexPath string) bool
}
