package v2

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// indexPackageV1 represents the v1 index package format (for migration only)
type indexPackageV1 struct {
	Type         string    `json:"type"`
	Name         string    `json:"name"`
	Version      string    `json:"version"`
	Architecture string    `json:"architecture,omitempty"`
	Group        string    `json:"group,omitempty"`
	Filename     string    `json:"filename"`
	Size         int64     `json:"size"`
	Description  string    `json:"description,omitempty"`
	License      string    `json:"license,omitempty"`
	Dependencies []string  `json:"dependencies,omitempty"`
	URL          string    `json:"url"`
	IndexedAt    time.Time `json:"indexed_at"`
	Cached       bool      `json:"cached"`
}

// MigrateFromV1 migrates packages from v1 index format to v2 format
// Reads old IndexPackage format, converts to IndexPackageV2
// Merges versions from same package name
// For Go packages: handles /vN variants correctly
func MigrateFromV1(v1IndexDir string, v2IndexDir string) error {
	log.Printf("[migrate] Starting migration from v1 to v2 index format")

	pmTypes := []string{"apk", "apt", "npm", "pypi", "go"}

	for _, pmType := range pmTypes {
		v1TypeDir := filepath.Join(v1IndexDir, pmType)
		v1PackagesPath := filepath.Join(v1TypeDir, "packages.json")

		// Check if v1 index exists
		if _, err := os.Stat(v1PackagesPath); os.IsNotExist(err) {
			log.Printf("[migrate] No v1 index found for %s, skipping", pmType)
			continue
		}

		// Read v1 packages
		data, err := os.ReadFile(v1PackagesPath)
		if err != nil {
			log.Printf("[migrate] Failed to read v1 index for %s: %v", pmType, err)
			continue
		}

		var v1Packages []indexPackageV1
		if err := json.Unmarshal(data, &v1Packages); err != nil {
			log.Printf("[migrate] Failed to parse v1 index for %s: %v", pmType, err)
			continue
		}

		if len(v1Packages) == 0 {
			log.Printf("[migrate] No packages to migrate for %s", pmType)
			continue
		}

		log.Printf("[migrate] Migrating %d packages from v1 to v2 for %s", len(v1Packages), pmType)

		// Convert v1 packages to v2 format
		packageMap := make(map[string]*IndexPackageV2)

		for i := range v1Packages {
			v1Pkg := &v1Packages[i]
			// For Go packages, decode the name if it contains !
			pkgName := v1Pkg.Name
			if pmType == "go" && v1Pkg.Name != "" {
				// Check if name needs decoding
				decoded := decodeGoModulePath(v1Pkg.Name)
				if decoded != v1Pkg.Name {
					pkgName = decoded
				}
			}

			// Create or get package entry
			if packageMap[pkgName] == nil {
				packageMap[pkgName] = &IndexPackageV2{
					Type:         v1Pkg.Type,
					Name:         pkgName,
					Versions:     []PackageVersion{},
					Description:  v1Pkg.Description,
					License:      v1Pkg.License,
					Dependencies: v1Pkg.Dependencies,
					IndexedAt:    v1Pkg.IndexedAt,
				}
			}

			// Add version
			version := PackageVersion{
				Version:      v1Pkg.Version,
				Architecture: v1Pkg.Architecture,
				CachePath:    v1Pkg.URL, // Use URL as cache path
				URL:          v1Pkg.URL,
				Size:         v1Pkg.Size,
				Cached:       v1Pkg.Cached,
				IndexedAt:    v1Pkg.IndexedAt,
			}

			// Check if version already exists
			exists := false
			for _, existingVersion := range packageMap[pkgName].Versions {
				if existingVersion.Version == version.Version && existingVersion.Architecture == version.Architecture {
					exists = true
					break
				}
			}

			if !exists {
				packageMap[pkgName].Versions = append(packageMap[pkgName].Versions, version)
			}
		}

		// Convert to slice
		var v2Packages []IndexPackageV2
		for _, pkg := range packageMap {
			v2Packages = append(v2Packages, *pkg)
		}

		// Create v2 index database and store
		v2DB := NewIndexDBV2(v2IndexDir)
		if err := v2DB.StorePackages(pmType, v2Packages); err != nil {
			log.Printf("[migrate] Failed to store v2 packages for %s: %v", pmType, err)
			continue
		}

		log.Printf("[migrate] Successfully migrated %d packages to v2 format for %s", len(v2Packages), pmType)
	}

	log.Printf("[migrate] Migration complete")
	return nil
}

// ShouldMigrate checks if v2 index exists, returns true if migration is needed
func ShouldMigrate(v2IndexDir string) bool {
	// Check if any v2 index files exist
	pmTypes := []string{"apk", "apt", "npm", "pypi", "go"}

	for _, pmType := range pmTypes {
		v2TypeDir := filepath.Join(v2IndexDir, pmType)
		v2PackagesPath := filepath.Join(v2TypeDir, "packages_v2.json")

		if _, err := os.Stat(v2PackagesPath); err == nil {
			// At least one v2 index exists, assume migration not needed
			return false
		}
	}

	// No v2 index files found, migration needed
	return true
}

// EnsureV2Index ensures v2 index exists, migrating from v1 if needed
func EnsureV2Index(v1IndexDir string, v2IndexDir string) error {
	if !ShouldMigrate(v2IndexDir) {
		log.Printf("[migrate] V2 index already exists, skipping migration")
		return nil
	}

	// Check if v1 index exists
	v1TypeDir := filepath.Join(v1IndexDir, "go") // Check at least one type
	v1PackagesPath := filepath.Join(v1TypeDir, "packages.json")

	if _, err := os.Stat(v1PackagesPath); os.IsNotExist(err) {
		log.Printf("[migrate] No v1 index found, creating empty v2 index")
		// Create empty v2 index structure
		if err := os.MkdirAll(v2IndexDir, 0755); err != nil {
			return err
		}
		return nil
	}

	// Migrate from v1
	return MigrateFromV1(v1IndexDir, v2IndexDir)
}
