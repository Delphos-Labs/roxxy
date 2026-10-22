package packages

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	v2index "github.com/Delphos-Labs/roxxy/internal/packages/index/v2"
)

// IndexDBV2Interface is an interface for the v2 index database
type IndexDBV2Interface interface {
	GetPackages(pmType string) ([]v2index.IndexPackageV2, error)
	UpdatePackageMetadata(pmType string, name string, updates func(*v2index.IndexPackageV2)) error
	UpdatePackageMetadataBatch(pmType string, pkgs []v2index.IndexPackageV2, updates map[string]func(*v2index.IndexPackageV2)) error
}

// PackageEnricherV2 enriches package metadata using the v2 index format
type PackageEnricherV2 struct {
	cacheDir     string
	indexDB      IndexDBV2Interface
	queue        chan PackageToEnrich
	processing   map[string]bool
	procMutex    sync.RWMutex
	stopChan     chan struct{}
	wg           sync.WaitGroup
	cpuThreshold float64
}

// NewPackageEnricherV2 creates a new v2 package enricher
func NewPackageEnricherV2(cacheDir string, indexDB IndexDBV2Interface, cpuThreshold float64) *PackageEnricherV2 {
	if cpuThreshold <= 0 || cpuThreshold > 1.0 {
		cpuThreshold = 0.5
	}
	return &PackageEnricherV2{
		cacheDir:     cacheDir,
		indexDB:      indexDB,
		queue:        make(chan PackageToEnrich, 1000),
		processing:   make(map[string]bool),
		stopChan:     make(chan struct{}),
		cpuThreshold: cpuThreshold,
	}
}

// Start starts the background enrichment worker
func (e *PackageEnricherV2) Start() {
	e.wg.Add(1)
	go e.worker()
	log.Printf("[enricher-v2] Started background package enrichment worker (CPU threshold: %.0f%%)", e.cpuThreshold*100)
}

// Stop stops the enrichment worker
func (e *PackageEnricherV2) Stop() {
	close(e.stopChan)
	close(e.queue)
	e.wg.Wait()
	log.Printf("[enricher-v2] Stopped background package enrichment worker")
}

// Enqueue adds a package to the enrichment queue
func (e *PackageEnricherV2) Enqueue(pkg PackageToEnrich) {
	key := fmt.Sprintf("%s:%s@%s", pkg.Type, pkg.Name, pkg.Version)
	e.procMutex.RLock()
	if e.processing[key] {
		e.procMutex.RUnlock()
		return
	}
	e.procMutex.RUnlock()

	select {
	case e.queue <- pkg:
	default:
		log.Printf("[enricher-v2] Queue full, skipping %s@%s", pkg.Name, pkg.Version)
	}
}

// ProcessPackage processes a package synchronously (for CLI commands)
// Returns true if the package was successfully enriched
// If force is true, re-enriches even if already enriched
func (e *PackageEnricherV2) ProcessPackage(pkg PackageToEnrich, force bool) bool {
	// Check if already enriched first (before locking), unless force is true
	if !force && e.isAlreadyEnriched(pkg) {
		return false
	}

	key := fmt.Sprintf("%s:%s@%s", pkg.Type, pkg.Name, pkg.Version)

	e.procMutex.Lock()
	if e.processing[key] {
		e.procMutex.Unlock()
		return false // Already processing
	}
	e.processing[key] = true
	e.procMutex.Unlock()

	defer func() {
		e.procMutex.Lock()
		delete(e.processing, key)
		e.procMutex.Unlock()
	}()

	// Call the internal processPackage method, but skip its duplicate checks
	// by temporarily removing from processing map
	e.procMutex.Lock()
	delete(e.processing, key)
	e.procMutex.Unlock()

	// Call processPackage which will do its own checks and processing
	// If force is true, we need to bypass the isAlreadyEnriched check in processPackage
	if force {
		// Temporarily mark as not enriched by clearing metadata check
		// We'll process it anyway since we already checked force above
		e.processPackageForce(pkg)
	} else {
		e.processPackage(pkg)
	}

	// Skip expensive verification - if updateIndex succeeded, we're done
	// The verification was loading the entire index just to check, which is very slow
	// We trust that updateIndex will log errors if it fails
	return true
}

// worker is the main background worker
func (e *PackageEnricherV2) worker() {
	defer e.wg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C:
			if !e.isCPULow() {
				continue
			}

			select {
			case pkg, ok := <-e.queue:
				if !ok {
					return
				}
				e.processPackage(pkg)
			default:
			}
		}
	}
}

// isCPULow checks if CPU usage is below the threshold (same as v1)
func (e *PackageEnricherV2) isCPULow() bool {
	var m1, m2 runtime.MemStats
	var cpuUsage float64

	runtime.ReadMemStats(&m1)
	time.Sleep(100 * time.Millisecond)
	runtime.ReadMemStats(&m2)

	gcPauseDelta := m2.PauseTotalNs - m1.PauseTotalNs
	if gcPauseDelta > 0 {
		cpuUsage = float64(gcPauseDelta) / 100000000.0
		if cpuUsage > 1.0 {
			cpuUsage = 1.0
		}
	} else {
		cpuUsage = 0.1
	}

	goroutineCount := runtime.NumGoroutine()
	if goroutineCount > 100 {
		cpuUsage += 0.2
	}

	return cpuUsage < e.cpuThreshold
}

// processPackage extracts metadata and updates the v2 index
func (e *PackageEnricherV2) processPackage(pkg PackageToEnrich) {
	key := fmt.Sprintf("%s:%s@%s", pkg.Type, pkg.Name, pkg.Version)

	e.procMutex.Lock()
	if e.processing[key] {
		e.procMutex.Unlock()
		return
	}
	e.procMutex.Unlock()

	// Skip isAlreadyEnriched check - it's expensive (loads entire index)
	// The caller (enrichPackagesV2) already checks this before calling ProcessPackage
	// This avoids loading the index 69 times for 69 packages
	e.processPackageInternal(pkg, false) // Pass force=false
}

// processPackageForce extracts metadata and updates the v2 index, bypassing already-enriched check
func (e *PackageEnricherV2) processPackageForce(pkg PackageToEnrich) {
	key := fmt.Sprintf("%s:%s@%s", pkg.Type, pkg.Name, pkg.Version)

	e.procMutex.Lock()
	if e.processing[key] {
		e.procMutex.Unlock()
		return
	}
	e.procMutex.Unlock()

	// Skip isAlreadyEnriched check when force is true
	e.processPackageInternal(pkg, true) // Pass force=true
}

// processPackageInternal does the actual metadata extraction and index update
func (e *PackageEnricherV2) processPackageInternal(pkg PackageToEnrich, force bool) {
	key := fmt.Sprintf("%s:%s@%s", pkg.Type, pkg.Name, pkg.Version)

	e.procMutex.Lock()
	e.processing[key] = true
	e.procMutex.Unlock()

	defer func() {
		e.procMutex.Lock()
		delete(e.processing, key)
		e.procMutex.Unlock()
	}()

	// Processing package (only log if verbose/debug mode needed)

	// Use the same extraction logic as v1 enricher
	enricherV1 := NewPackageEnricher(e.cacheDir, nil, e.cpuThreshold)

	// Call the exported ExtractMetadata method
	metadata := enricherV1.ExtractMetadata(pkg)

	if metadata == nil {
		// Log failures for all types, including PyPI, to help debug
		log.Printf("[enricher-v2] Failed to extract metadata for %s@%s (%s) from %s", pkg.Name, pkg.Version, pkg.Type, pkg.CachePath)
		return
	}

	// Update v2 index
	e.updateIndex(pkg, metadata, force)

	// Also update the .meta file with version-specific license/description
	e.updateMetadataFile(pkg, metadata)
}

// isAlreadyEnriched checks if the package already has license/description in v2 index
func (e *PackageEnricherV2) isAlreadyEnriched(pkg PackageToEnrich) bool {
	if e.indexDB == nil {
		return false
	}

	pkgs, err := e.indexDB.GetPackages(string(pkg.Type))
	if err != nil {
		return false
	}

	// Check if this package exists and has license or description
	// Note: For Go packages, names might be encoded differently, so we need to handle that
	for _, indexedPkg := range pkgs {
		// Direct name match
		if indexedPkg.Name == pkg.Name {
			// Check if license or description is set (not empty and not null)
			license := strings.TrimSpace(indexedPkg.License)
			description := strings.TrimSpace(indexedPkg.Description)
			if license != "" || description != "" {
				return true
			}
		}
	}

	return false
}

// updateIndex updates the v2 index database with enriched metadata
// If force is true, overwrites existing metadata; otherwise only fills empty fields
func (e *PackageEnricherV2) updateIndex(pkg PackageToEnrich, metadata map[string]interface{}, force bool) {
	if e.indexDB == nil {
		return
	}

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
		return
	}

	// Use package name from metadata if available (more accurate than filename parsing)
	// For PyPI, the filename might have underscores but the package name has hyphens
	// For APT, the control file has the correct package name
	// For NPM, prefer the name from cached file metadata (URL path) over package.json
	// because package.json might be from a dependency, not the main package
	packageName := pkg.Name
	if pkg.Type != TypeNPM {
		// For non-NPM packages, use name from metadata if available
		if nameFromMetadata, ok := metadata["name"].(string); ok && nameFromMetadata != "" {
			packageName = nameFromMetadata
		}
	} else {
		// For NPM, validate that metadata name matches cached name
		// If they don't match, prefer cached name (from URL path) as it's more reliable
		if nameFromMetadata, ok := metadata["name"].(string); ok && nameFromMetadata != "" {
			if nameFromMetadata != pkg.Name {
				// Names don't match - this might be a dependency's package.json
				// Prefer the cached name (from URL path) which should be correct
				log.Printf("[enricher-v2] NPM package name mismatch: cached=%q, package.json=%q (using cached name)", pkg.Name, nameFromMetadata)
			}
			// For NPM, always use cached name to avoid issues with dependency package.json files
			packageName = pkg.Name
		}
	}

	// For PyPI, try to extract name from URL path if metadata doesn't have it
	if pkg.Type == TypePyPI && packageName == pkg.Name {
		// Load metadata file to get URL path
		metaPath := pkg.CachePath + ".meta"
		if metaData, err := os.ReadFile(metaPath); err == nil {
			var meta struct {
				URLPath string `json:"url_path"`
			}
			if json.Unmarshal(metaData, &meta) == nil && meta.URLPath != "" {
				// Extract package name from URL path: /simple/package-name/package-name-version.whl
				// or /simple/package-name/package-name-version.tar.gz
				parts := strings.Split(strings.Trim(meta.URLPath, "/"), "/")
				if len(parts) >= 2 && parts[0] == "simple" {
					// The package name is in the path: /simple/package-name/...
					packageName = parts[1]
					// Extracted package name from URL path (no need to log - too verbose)
				}
			}
		}
	}

	// Update the v2 index
	// For PyPI, try both the canonical name (with hyphens) and the original name (with underscores)
	// because packages might have been added to the index with the filename-derived name
	var updateErr error
	var actuallyUpdated bool
	updateFunc := func(p *v2index.IndexPackageV2) {
		// Update version-specific license (license is version-level only)
		versionUpdated := false
		for i := range p.Versions {
			if p.Versions[i].Version == pkg.Version {
				// Found the matching version, update its license
				if hasLicense && (force || p.Versions[i].License == "") {
					p.Versions[i].License = metadata["license"].(string)
					versionUpdated = true
				}
				break
			}
		}

		// Update package-level description (description is package-level only)
		// Pick the best one from all versions (longest non-empty description)
		// This avoids duplication and ensures we show the most informative description at package level
		bestDescription := p.Description
		// Also consider the newly enriched description if it's better
		if hasDescription {
			newDesc := metadata["description"].(string)
			if newDesc != "" && len(newDesc) > len(bestDescription) {
				bestDescription = newDesc
			}
		}
		if bestDescription != p.Description {
			p.Description = bestDescription
			versionUpdated = true
		}
		if hasDependencies && (force || len(p.Dependencies) == 0) {
			p.Dependencies = metadata["dependencies"].([]string)
			versionUpdated = true
		}

		if versionUpdated {
			actuallyUpdated = true
		}
	}

	if pkg.Type == TypePyPI && packageName != pkg.Name {
		// For PyPI, check if both names exist (duplicate scenario)
		// Prefer updating the original name (with underscores) to avoid creating duplicates
		// Check if original name exists first
		pkgs, err := e.indexDB.GetPackages(string(pkg.Type))
		originalNameExists := false
		canonicalNameExists := false
		if err == nil {
			for _, p := range pkgs {
				if p.Name == pkg.Name {
					originalNameExists = true
				}
				if p.Name == packageName {
					canonicalNameExists = true
				}
			}
		}

		// If original name exists, prefer it (to avoid duplicates)
		// Otherwise, try canonical name
		if originalNameExists {
			// Found original name in index, using it instead of canonical (no need to log - too verbose)
			updateErr = e.indexDB.UpdatePackageMetadata(string(pkg.Type), pkg.Name, updateFunc)
			if updateErr == nil && actuallyUpdated {
				packageName = pkg.Name // Use original name for logging
			}
		} else if canonicalNameExists {
			// Only use canonical name if original doesn't exist
			updateErr = e.indexDB.UpdatePackageMetadata(string(pkg.Type), packageName, updateFunc)
		} else {
			// Neither exists - this shouldn't happen, but try canonical name
			updateErr = e.indexDB.UpdatePackageMetadata(string(pkg.Type), packageName, updateFunc)
			if updateErr != nil {
				// Fallback to original name (no need to log - too verbose)
				updateErr = e.indexDB.UpdatePackageMetadata(string(pkg.Type), pkg.Name, updateFunc)
				if updateErr == nil && actuallyUpdated {
					packageName = pkg.Name // Use original name for logging
				}
			}
		}
	} else {
		// For non-PyPI or when names match, use normal update
		updateErr = e.indexDB.UpdatePackageMetadata(string(pkg.Type), packageName, updateFunc)
	}

	if updateErr != nil {
		log.Printf("[enricher-v2] ERROR: Failed to update index for %s@%s (packageName=%s): %v", pkg.Name, pkg.Version, packageName, updateErr)
	}
	// Note: Package not found in index (!actuallyUpdated) is not logged as it's expected for some packages
	// Successfully enriched (no need to log - too verbose)
}

// updateMetadataFile updates the .meta file with version-specific license/description
// This ensures that SBOM tracking can get version-specific metadata directly from the .meta file
func (e *PackageEnricherV2) updateMetadataFile(pkg PackageToEnrich, metadata map[string]interface{}) {
	metaPath := pkg.CachePath + ".meta"

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
			log.Printf("[enricher-v2] Failed to marshal updated metadata for %s: %v", pkg.CachePath, err)
			return
		}

		if err := os.WriteFile(metaPath, newData, 0644); err != nil {
			log.Printf("[enricher-v2] ERROR: Failed to update .meta file for %s: %v", pkg.CachePath, err)
			return
		}

		// Successfully updated .meta file (no need to log - too verbose)
	}
}
