package index

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/Delphos-Labs/roxxy/internal/packages"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// IndexPackage represents a package extracted from an index file
type IndexPackage struct {
	Type         string    `json:"type"`
	Name         string    `json:"name"`
	Version      string    `json:"version"`
	Architecture string    `json:"architecture,omitempty"`
	Group        string    `json:"group,omitempty"` // Component/group (e.g., "main", "security", "contrib")
	Filename     string    `json:"filename"`
	Size         int64     `json:"size"`
	Description  string    `json:"description,omitempty"`
	License      string    `json:"license,omitempty"`
	Dependencies []string  `json:"dependencies,omitempty"`
	URL          string    `json:"url"`
	IndexedAt    time.Time `json:"indexed_at"`
	Cached       bool      `json:"cached"` // True if actual file is cached
}

// Parser is an interface for parsing index files
type Parser interface {
	// Parse extracts package metadata from index file data
	// For most parsers, data contains the file contents.
	// For Go zip files, data may be empty and cachePath should be used instead.
	Parse(data []byte, indexURL string, contentType string, cachePath string) ([]IndexPackage, error)
	// CanParse returns true if this parser can handle the given index file
	CanParse(indexPath string) bool
}

// IndexDB stores parsed package metadata
type IndexDB struct {
	indexDir string
	parsers  map[packages.Type]Parser
}

// NewIndexDB creates a new index database
func NewIndexDB(indexDir string) *IndexDB {
	db := &IndexDB{
		indexDir: indexDir,
		parsers:  make(map[packages.Type]Parser),
	}

	// Register parsers
	db.parsers[packages.TypeAPK] = &APKParser{}
	db.parsers[packages.TypeAPT] = &APTParser{}
	db.parsers[packages.TypePyPI] = &PyPIParser{}
	db.parsers[packages.TypeNPM] = &NPMParser{}
	db.parsers[packages.TypeGo] = &GoParser{}

	// Ensure index directory exists
	if err := os.MkdirAll(indexDir, 0755); err != nil {
		log.Printf("Warning: Failed to create index dir %s: %v", indexDir, err)
	}

	return db
}

// ParseIndexFile parses an index file and stores the extracted packages
func (db *IndexDB) ParseIndexFile(cachePath string, indexURL string, pmType packages.Type, contentType string) error {
	parser, exists := db.parsers[pmType]
	if !exists {
		return nil // No parser for this type, skip silently
	}

	if !parser.CanParse(indexURL) {
		return nil // Parser can't handle this index file
	}

	// Read the cached index file
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return fmt.Errorf("failed to read index file: %w", err)
	}

	// Parse the index file
	packages, err := parser.Parse(data, indexURL, contentType, cachePath)
	if err != nil {
		return fmt.Errorf("failed to parse index file: %w", err)
	}

	log.Printf("[index] Parsed %d packages from %s", len(packages), indexURL)
	// Don't store here - return packages to caller for batch storage
	return nil
}

// storePackages stores parsed packages in the index database
func (db *IndexDB) storePackages(pmType packages.Type, pkgs []IndexPackage) error {
	// Store as JSON file: index/{type}/packages.json
	typeDir := filepath.Join(db.indexDir, string(pmType))
	if err := os.MkdirAll(typeDir, 0755); err != nil {
		return fmt.Errorf("failed to create type dir: %w", err)
	}

	// Load existing packages
	existingPath := filepath.Join(typeDir, "packages.json")
	existing := make(map[string]IndexPackage)

	if data, err := os.ReadFile(existingPath); err == nil {
		var existingList []IndexPackage
		if err := json.Unmarshal(data, &existingList); err == nil {
			// For Go packages, filter existing packages when loading to remove any old sub-packages
			if pmType == packages.TypeGo {
				existingList = filterGoSubPackages(existingList)
			}
			for _, pkg := range existingList {
				key := fmt.Sprintf("%s@%s", pkg.Name, pkg.Version)
				if pkg.Architecture != "" {
					key = fmt.Sprintf("%s@%s:%s", pkg.Name, pkg.Version, pkg.Architecture)
				}
				existing[key] = pkg
			}
		}
	}

	// Merge new packages (newer entries override older ones)
	for _, pkg := range pkgs {
		key := fmt.Sprintf("%s@%s", pkg.Name, pkg.Version)
		if pkg.Architecture != "" {
			key = fmt.Sprintf("%s@%s:%s", pkg.Name, pkg.Version, pkg.Architecture)
		}
		existing[key] = pkg
	}

	// Convert back to list
	var allPackages []IndexPackage
	for _, pkg := range existing {
		allPackages = append(allPackages, pkg)
	}

	// For Go packages, filter out sub-packages again after merging (they should only appear in parent metadata)
	if pmType == packages.TypeGo {
		allPackages = filterGoSubPackages(allPackages)
	}

	// Save updated packages
	data, err := json.MarshalIndent(allPackages, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal packages: %w", err)
	}

	if err := os.WriteFile(existingPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write packages: %w", err)
	}

	return nil
}

// GetPackages returns all packages of the given type from the index
func (db *IndexDB) GetPackages(pmType packages.Type) ([]IndexPackage, error) {
	typeDir := filepath.Join(db.indexDir, string(pmType))
	packagesPath := filepath.Join(typeDir, "packages.json")

	data, err := os.ReadFile(packagesPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []IndexPackage{}, nil
		}
		return nil, fmt.Errorf("failed to read packages: %w", err)
	}

	var pkgs []IndexPackage
	if err := json.Unmarshal(data, &pkgs); err != nil {
		return nil, fmt.Errorf("failed to parse packages: %w", err)
	}

	// For Go packages, filter out sub-packages when reading
	if pmType == packages.TypeGo {
		pkgs = filterGoSubPackages(pkgs)
	}

	return pkgs, nil
}

// GetPackagesAsInterface returns all packages as []interface{} to satisfy the interface
// This is a wrapper to avoid import cycles
func (db *IndexDB) GetPackagesAsInterface(pmType packages.Type) ([]interface{}, error) {
	pkgs, err := db.GetPackages(pmType)
	if err != nil {
		return nil, err
	}
	result := make([]interface{}, len(pkgs))
	for i := range pkgs {
		result[i] = &pkgs[i]
	}
	return result, nil
}

// isGoSubPackage returns true if candidate is a sub-package of mainModule
// A sub-package is any package whose name starts with mainModule + "/"
// e.g., "github.com/bytedance/sonic/loader" is a sub-package of "github.com/bytedance/sonic"
func isGoSubPackage(mainModule, candidate string) bool {
	if mainModule == "" || candidate == "" {
		return false
	}
	prefix := mainModule + "/"
	return strings.HasPrefix(candidate, prefix)
}

// filterGoSubPackages filters out Go sub-packages, keeping only main modules
func filterGoSubPackages(pkgs []IndexPackage) []IndexPackage {
	if len(pkgs) == 0 {
		return pkgs
	}

	// Build a map of all package names for quick lookup
	packageNames := make(map[string]bool)
	for _, pkg := range pkgs {
		if pkg.Name != "" {
			packageNames[pkg.Name] = true
		}
	}

	// Filter out sub-packages
	var filtered []IndexPackage
	for _, pkg := range pkgs {
		if pkg.Name == "" {
			// Keep packages without names (shouldn't happen, but be safe)
			filtered = append(filtered, pkg)
			continue
		}

		// Check if this package is a sub-package of any other package
		isSubPackage := false
		for otherName := range packageNames {
			if otherName != pkg.Name && isGoSubPackage(otherName, pkg.Name) {
				isSubPackage = true
				break
			}
		}

		if !isSubPackage {
			filtered = append(filtered, pkg)
		}
	}

	return filtered
}

// ParseCachedIndexFiles scans the cache directory for index files and parses them
// This is called on-demand from the UI, not during normal proxy operation
// It rebuilds the index from scratch by clearing existing data first
func (db *IndexDB) ParseCachedIndexFiles(cacheDir string, pmType packages.Type) (int, error) {
	parser, exists := db.parsers[pmType]
	if !exists {
		return 0, nil // No parser for this type
	}

	typeDir := filepath.Join(cacheDir, "packages", string(pmType))
	if _, err := os.Stat(typeDir); os.IsNotExist(err) {
		return 0, nil // No cache directory for this type
	}

	// Clear existing index database for this type to rebuild from scratch
	// This ensures we don't have stale entries from packages that are no longer cached
	indexTypeDir := filepath.Join(db.indexDir, string(pmType))
	existingPath := filepath.Join(indexTypeDir, "packages.json")
	if err := os.Remove(existingPath); err != nil && !os.IsNotExist(err) {
		log.Printf("Warning: Failed to clear existing index for %s: %v", pmType, err)
		// Continue anyway - we'll overwrite it
	}

	// Collect all packages from all index files first, then save once at the end
	var allPackages []IndexPackage
	var parsedCount int
	var lastError error
	parsedFiles := make(map[string]bool) // Track parsed files to avoid duplicates

	// Walk the cache directory looking for index files
	log.Printf("[index] Scanning %s for index files...", typeDir)
	filesChecked := 0
	filesWithMetadata := 0
	filesWithIsIndex := 0
	// Use WalkDir instead of Walk for better performance (no stat calls)
	err := filepath.WalkDir(typeDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		// Skip .meta files
		if strings.HasSuffix(path, ".meta") {
			return nil
		}

		filesChecked++

		// Skip if we've already parsed this file
		if parsedFiles[path] {
			return nil
		}

		// Try to load metadata to check if this is an index file
		metaPath := path + ".meta"
		metaData, err := os.ReadFile(metaPath)
		if err != nil {
			return nil // No metadata, can't determine URL path, skip
		}

		filesWithMetadata++

		var metadata struct {
			URLPath     string `json:"url_path"`
			IsIndex     bool   `json:"is_index"`
			ContentType string `json:"content_type,omitempty"`
		}
		if err := json.Unmarshal(metaData, &metadata); err != nil {
			return nil // Invalid metadata, skip
		}

		// Check if this is an index file:
		// 1. If metadata has IsIndex=true, use that (new cached files)
		// 2. Otherwise, check if parser recognizes it as an index file (old cached files without IsIndex field)
		isIndex := metadata.IsIndex
		if isIndex {
			filesWithIsIndex++
		}
		if !isIndex && metadata.URLPath != "" {
			// Fallback: check if parser thinks this is an index file
			// This handles old cached files that don't have IsIndex in metadata
			isIndex = parser.CanParse(metadata.URLPath)
		}

		// Only parse if it's an index file and the parser can handle it
		if !isIndex {
			return nil
		}

		// Check if parser can handle this specific index file
		// Note: APTHandler.IsIndex() marks many files as index files (Release, Sources, etc.)
		// but APTParser only handles Packages files, so we need this check
		if !parser.CanParse(metadata.URLPath) {
			// Log why we're skipping (for debugging) - log first few for all types
			if parsedCount < 10 {
				log.Printf("[index] Skipping index file %s (parser can't handle: %s)", filepath.Base(path), metadata.URLPath)
			}
			return nil // Parser can't handle this index file
		}

		// Log that we found an index file
		log.Printf("[index] Found index file: %s (%s)", filepath.Base(path), metadata.URLPath)

		// Read and parse the index file directly (don't store yet)
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("Warning: Failed to read index file %s: %v", path, err)
			lastError = err
			return nil // Continue parsing other files
		}

		packages, err := parser.Parse(data, metadata.URLPath, metadata.ContentType, path)
		if err != nil {
			log.Printf("Warning: Failed to parse index file %s: %v", path, err)
			lastError = err
			return nil // Continue parsing other files
		}

		// Collect packages (don't save yet)
		allPackages = append(allPackages, packages...)
		parsedFiles[path] = true
		parsedCount++
		log.Printf("[index] Parsed index file: %s (%s) - %d packages", filepath.Base(path), metadata.URLPath, len(packages))
		return nil
	})

	if err != nil {
		return parsedCount, fmt.Errorf("error walking cache directory: %w", err)
	}

	log.Printf("[index] Walk complete: checked %d files, %d with metadata, %d with is_index=true", filesChecked, filesWithMetadata, filesWithIsIndex)

	// Now save all collected packages at once (filtered for Go packages)
	if err := db.storePackages(pmType, allPackages); err != nil {
		return parsedCount, fmt.Errorf("failed to store packages: %w", err)
	}

	if lastError != nil && parsedCount == 0 {
		return 0, lastError
	}

	log.Printf("[index] Rebuilt index for %s: %d packages from %d index files", pmType, len(allPackages), parsedCount)
	return parsedCount, nil
}

// UpdateCachedStatus updates the Cached flag for packages based on what's actually cached
func (db *IndexDB) UpdateCachedStatus(pmType packages.Type, cachedPackages map[string]bool) error {
	pkgs, err := db.GetPackages(pmType)
	if err != nil {
		return err
	}

	// Update cached status
	for i := range pkgs {
		key := fmt.Sprintf("%s@%s", pkgs[i].Name, pkgs[i].Version)
		if pkgs[i].Architecture != "" {
			key = fmt.Sprintf("%s@%s:%s", pkgs[i].Name, pkgs[i].Version, pkgs[i].Architecture)
		}
		pkgs[i].Cached = cachedPackages[key]
	}

	// Save updated packages
	typeDir := filepath.Join(db.indexDir, string(pmType))
	packagesPath := filepath.Join(typeDir, "packages.json")

	data, err := json.MarshalIndent(pkgs, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal packages: %w", err)
	}

	return os.WriteFile(packagesPath, data, 0644)
}

// UpdatePackageMetadata updates metadata for a specific package in the index
func (db *IndexDB) UpdatePackageMetadata(pmType packages.Type, name, version string, updates func(interface{})) error {
	pkgs, err := db.GetPackages(pmType)
	if err != nil {
		return err
	}

	// Find and update the matching package
	updated := false
	for i := range pkgs {
		if pkgs[i].Name == name && pkgs[i].Version == version {
			updates(&pkgs[i])
			updated = true
			break
		}
	}

	if !updated {
		return nil // Package not found in index, nothing to update
	}

	// Save updated packages
	typeDir := filepath.Join(db.indexDir, string(pmType))
	packagesPath := filepath.Join(typeDir, "packages.json")

	data, err := json.MarshalIndent(pkgs, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal packages: %w", err)
	}

	return os.WriteFile(packagesPath, data, 0644)
}

// Helper function to read gzipped data
func readGzip(data []byte) ([]byte, error) {
	gr, err := gzip.NewReader(strings.NewReader(string(data)))
	if err != nil {
		return nil, err
	}
	defer util.DeferClose(gr)

	return io.ReadAll(gr)
}

// Helper function to read xz compressed data
// Uses Go xz library for decompression (pure Go, no external dependencies)
func readXZ(data []byte) ([]byte, error) {
	xzReader, err := xz.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create xz reader: %w", err)
	}

	decompressed, err := io.ReadAll(xzReader)
	if err != nil {
		return nil, fmt.Errorf("xz decompression failed: %w", err)
	}

	return decompressed, nil
}
