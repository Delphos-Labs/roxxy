package packages

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/blakesmith/ar"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// PackageEnricher enriches package metadata by extracting license and package info from cached packages
type PackageEnricher struct {
	cacheDir     string
	indexDB      IndexDBInterface
	queue        chan PackageToEnrich
	processing   map[string]bool // Track packages currently being processed
	procMutex    sync.RWMutex
	stopChan     chan struct{}
	wg           sync.WaitGroup
	cpuThreshold float64 // CPU usage threshold (0.0-1.0) - only process when below this
}

// PackageToEnrich represents a package that needs enrichment
type PackageToEnrich struct {
	Type         Type
	CachePath    string
	Name         string
	Version      string
	Architecture string
}

// Pre-compiled regular expressions for markdown parsing (performance optimization)
var (
	referenceLinkRegex = regexp.MustCompile(`^\[\d+\]:\s*https?://`)
	urlOnlyLineRegex   = regexp.MustCompile(`^https?://`)
)

// NewPackageEnricher creates a new package enricher
func NewPackageEnricher(cacheDir string, indexDB IndexDBInterface, cpuThreshold float64) *PackageEnricher {
	if cpuThreshold <= 0 || cpuThreshold > 1.0 {
		cpuThreshold = 0.5 // Default: only process when CPU < 50%
	}
	return &PackageEnricher{
		cacheDir:     cacheDir,
		indexDB:      indexDB,
		queue:        make(chan PackageToEnrich, 1000), // Buffer up to 1000 packages
		processing:   make(map[string]bool),
		stopChan:     make(chan struct{}),
		cpuThreshold: cpuThreshold,
	}
}

// Start starts the background enrichment worker
func (e *PackageEnricher) Start() {
	e.wg.Add(1)
	go e.worker()
	log.Printf("[enricher] Started background package enrichment worker (CPU threshold: %.0f%%)", e.cpuThreshold*100)
}

// Stop stops the enrichment worker
func (e *PackageEnricher) Stop() {
	close(e.stopChan)
	close(e.queue)
	e.wg.Wait()
	log.Printf("[enricher] Stopped background package enrichment worker")
}

// Enqueue adds a package to the enrichment queue
func (e *PackageEnricher) Enqueue(pkg PackageToEnrich) {
	// Check if already processing or in queue
	key := fmt.Sprintf("%s:%s@%s", pkg.Type, pkg.Name, pkg.Version)
	e.procMutex.RLock()
	if e.processing[key] {
		e.procMutex.RUnlock()
		return // Already processing
	}
	e.procMutex.RUnlock()

	// Try to add to queue (non-blocking)
	select {
	case e.queue <- pkg:
		// Queued successfully
	default:
		// Queue full, skip (don't block)
		log.Printf("[enricher] Queue full, skipping %s@%s", pkg.Name, pkg.Version)
	}
}

// worker is the main background worker that processes packages
func (e *PackageEnricher) worker() {
	defer e.wg.Done()

	ticker := time.NewTicker(5 * time.Second) // Check CPU every 5 seconds
	defer ticker.Stop()

	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C:
			// Check CPU usage
			if !e.isCPULow() {
				continue // CPU too high, skip this cycle
			}

			// Process one package from queue
			select {
			case pkg, ok := <-e.queue:
				if !ok {
					return // Channel closed
				}
				e.processPackage(pkg)
			default:
				// No packages in queue, continue
			}
		}
	}
}

// isCPULow checks if CPU usage is below the threshold
func (e *PackageEnricher) isCPULow() bool {
	// Get CPU usage using runtime stats
	var m1, m2 runtime.MemStats
	var cpuUsage float64

	runtime.ReadMemStats(&m1)
	time.Sleep(100 * time.Millisecond) // Sample over 100ms
	runtime.ReadMemStats(&m2)

	// Calculate CPU usage based on GC and goroutine activity
	// This is a simple heuristic - actual CPU usage would require cgo or external tools
	// We use GC pause time as a proxy for CPU activity
	gcPauseDelta := m2.PauseTotalNs - m1.PauseTotalNs
	if gcPauseDelta > 0 {
		// If GC is active, assume higher CPU usage
		cpuUsage = float64(gcPauseDelta) / 100000000.0 // Normalize
		if cpuUsage > 1.0 {
			cpuUsage = 1.0
		}
	} else {
		// No GC activity, assume low CPU
		cpuUsage = 0.1
	}

	// Also check number of goroutines as a proxy
	goroutineCount := runtime.NumGoroutine()
	if goroutineCount > 100 {
		cpuUsage += 0.2 // High goroutine count suggests activity
	}

	return cpuUsage < e.cpuThreshold
}

// processPackage extracts metadata from a cached package and updates the index
func (e *PackageEnricher) processPackage(pkg PackageToEnrich) {
	key := fmt.Sprintf("%s:%s@%s", pkg.Type, pkg.Name, pkg.Version)

	// Mark as processing
	e.procMutex.Lock()
	if e.processing[key] {
		e.procMutex.Unlock()
		return // Already processing
	}
	e.procMutex.Unlock()

	// Check if already enriched in index
	if e.isAlreadyEnriched(pkg) {
		return // Already has license info
	}

	e.procMutex.Lock()
	e.processing[key] = true
	e.procMutex.Unlock()

	defer func() {
		e.procMutex.Lock()
		delete(e.processing, key)
		e.procMutex.Unlock()
	}()

	// Processing package (only log if verbose/debug mode needed)

	// Extract metadata based on package type
	metadata := e.ExtractMetadata(pkg)
	if metadata == nil {
		return // Failed to extract
	}

	// Update index with enriched metadata
	e.updateIndex(pkg, metadata)
}

// isAlreadyEnriched checks if the package already has license info in the index
func (e *PackageEnricher) isAlreadyEnriched(pkg PackageToEnrich) bool {
	if e.indexDB == nil {
		return false
	}

	// Try to get packages from index
	indexPkgs, err := e.indexDB.GetPackages(pkg.Type)
	if err != nil {
		return false
	}

	// Check if this package exists and has license
	// Use reflection to access fields without importing the concrete type
	for _, pkgInterface := range indexPkgs {
		v := reflect.ValueOf(pkgInterface)
		if v.Kind() == reflect.Pointer {
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			continue
		}

		// Get Name, Version, and License fields
		nameField := v.FieldByName("Name")
		versionField := v.FieldByName("Version")
		licenseField := v.FieldByName("License")

		if !nameField.IsValid() || !versionField.IsValid() || !licenseField.IsValid() {
			continue
		}

		if nameField.Kind() == reflect.String && versionField.Kind() == reflect.String && licenseField.Kind() == reflect.String {
			if nameField.String() == pkg.Name && versionField.String() == pkg.Version {
				if licenseField.String() != "" {
					return true // Already has license
				}
			}
		}
	}

	return false
}

// ExtractMetadata extracts metadata from a cached package file
// Exported for use by v2 enricher
func (e *PackageEnricher) ExtractMetadata(pkg PackageToEnrich) map[string]interface{} {
	switch pkg.Type {
	case TypeAPK:
		return e.extractAPKMetadata(pkg)
	case TypeNPM:
		return e.extractNPMMetadata(pkg)
	case TypePyPI:
		return e.extractPyPIMetadata(pkg)
	case TypeGo:
		return e.extractGoMetadata(pkg)
	case TypeAPT:
		return e.extractAPTMetadata(pkg)
	default:
		return nil
	}
}

// extractAPKMetadata extracts metadata from an APK file
func (e *PackageEnricher) extractAPKMetadata(pkg PackageToEnrich) map[string]interface{} {
	// Use direct file extraction - it's much faster than running `apk info` command
	// The `apk info` command has significant overhead (fork/exec, validation, etc.)
	// Direct extraction just reads the .PKGINFO file from the tarball
	return e.extractAPKMetadataDirectly(pkg.CachePath)
}

// extractAPKMetadataDirectly extracts metadata directly from APK file
func (e *PackageEnricher) extractAPKMetadataDirectly(apkPath string) map[string]interface{} {
	file, err := os.Open(apkPath)
	if err != nil {
		return nil
	}
	defer util.DeferClose(file)

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return nil
	}
	defer util.DeferClose(gzReader)

	tarReader := tar.NewReader(gzReader)
	metadata := make(map[string]string)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil
		}

		if header.Name == ".PKGINFO" {
			data, err := io.ReadAll(tarReader)
			if err != nil {
				return nil
			}

			lines := strings.Split(string(data), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				parts := strings.SplitN(line, " = ", 2)
				if len(parts) == 2 {
					key := strings.TrimSpace(parts[0])
					value := strings.TrimSpace(parts[1])

					if key == "license" || key == "depend" {
						if existing, ok := metadata[key]; ok {
							metadata[key] = existing + " " + value
						} else {
							metadata[key] = value
						}
					} else {
						metadata[key] = value
					}
				}
			}
			break
		}
	}

	result := make(map[string]interface{})
	if name, ok := metadata["pkgname"]; ok {
		result["name"] = name
	}
	if version, ok := metadata["pkgver"]; ok {
		result["version"] = version
	}
	if license, ok := metadata["license"]; ok {
		result["license"] = license
	}
	if desc, ok := metadata["pkgdesc"]; ok {
		result["description"] = desc
	}

	return result
}

// extractNPMMetadata extracts metadata from an NPM tarball
func (e *PackageEnricher) extractNPMMetadata(pkg PackageToEnrich) map[string]interface{} {
	file, err := os.Open(pkg.CachePath)
	if err != nil {
		log.Printf("[enricher] Failed to open NPM tarball %s: %v", pkg.CachePath, err)
		return nil
	}
	defer util.DeferClose(file)

	// Check file size to ensure it's not empty
	fileInfo, err := file.Stat()
	if err != nil {
		log.Printf("[enricher] Failed to stat NPM tarball %s: %v", pkg.CachePath, err)
		return nil
	}
	if fileInfo.Size() == 0 {
		log.Printf("[enricher] NPM tarball %s is empty", pkg.CachePath)
		return nil
	}

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		log.Printf("[enricher] Failed to create gzip reader for %s: %v (file size: %d bytes)", pkg.CachePath, err, fileInfo.Size())
		return nil
	}
	defer util.DeferClose(gzReader)

	tarReader := tar.NewReader(gzReader)
	metadata := make(map[string]interface{})
	foundPackageJSON := false
	entriesSeen := 0
	var entryNames []string

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("[enricher] Failed to read tar entry from %s (after %d entries): %v", pkg.CachePath, entriesSeen, err)
			return nil
		}

		entriesSeen++
		if header.Typeflag == tar.TypeReg {
			name := header.Name
			entryNames = append(entryNames, name)

			// NPM tarballs have structure: {dirname}/package.json where {dirname} is usually "package"
			// but can also be the package name (e.g., "swagger-jsdoc/package.json")
			// Dependencies are in: {dirname}/node_modules/.../package.json
			// We want the root package.json (first level directory), not dependency package.json files
			// Pattern: exactly one "/" before "package.json" and not in node_modules
			if strings.HasSuffix(name, "/package.json") && !strings.Contains(name, "node_modules") {
				// Check that it's exactly one level deep (e.g., "package/package.json" or "swagger-jsdoc/package.json")
				parts := strings.Split(name, "/")
				if len(parts) == 2 && parts[1] == "package.json" {
					// Found package.json at expected path
					data, err := io.ReadAll(tarReader)
					if err != nil {
						log.Printf("[enricher] Failed to read package.json from %s: %v", pkg.CachePath, err)
						return nil
					}

					var packageJSON map[string]interface{}
					if err := json.Unmarshal(data, &packageJSON); err != nil {
						log.Printf("[enricher] Failed to parse package.json from %s: %v", pkg.CachePath, err)
						return nil
					}

					// Extract relevant fields
					if name, ok := packageJSON["name"].(string); ok {
						metadata["name"] = name
					}
					if version, ok := packageJSON["version"].(string); ok {
						metadata["version"] = version
					}
					if license, ok := packageJSON["license"].(string); ok {
						metadata["license"] = license
					} else if licenses, ok := packageJSON["licenses"].([]interface{}); ok && len(licenses) > 0 {
						// Handle old "licenses" array format
						if licenseObj, ok := licenses[0].(map[string]interface{}); ok {
							if licenseType, ok := licenseObj["type"].(string); ok {
								metadata["license"] = licenseType
							}
						}
					}
					if description, ok := packageJSON["description"].(string); ok {
						metadata["description"] = description
					}
					if deps, ok := packageJSON["dependencies"].(map[string]interface{}); ok {
						depsList := make([]string, 0, len(deps))
						for dep := range deps {
							depsList = append(depsList, dep)
						}
						metadata["dependencies"] = depsList
					}

					return metadata
				}
			}
		}
	}

	if !foundPackageJSON {
		log.Printf("[enricher] package/package.json not found in %s (scanned %d entries). Entries: %v", pkg.CachePath, entriesSeen, entryNames)
		// Try alternative: maybe package.json is at root without "package/" prefix
		// Re-read the tarball to check
		if _, err := file.Seek(0, 0); err == nil {
			gzReader2, err := gzip.NewReader(file)
			if err == nil {
				tarReader2 := tar.NewReader(gzReader2)
				for {
					header, err := tarReader2.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						break
					}
					if header.Typeflag == tar.TypeReg && strings.HasSuffix(header.Name, "package.json") && !strings.Contains(header.Name, "node_modules") {
						// Found a package.json that's not in node_modules - try it
						log.Printf("[enricher] Found alternative package.json path: %s in %s", header.Name, pkg.CachePath)
						data, err := io.ReadAll(tarReader2)
						if err == nil {
							var packageJSON map[string]interface{}
							if err := json.Unmarshal(data, &packageJSON); err == nil {
								// Extract relevant fields
								if name, ok := packageJSON["name"].(string); ok {
									metadata["name"] = name
								}
								if version, ok := packageJSON["version"].(string); ok {
									metadata["version"] = version
								}
								if license, ok := packageJSON["license"].(string); ok {
									metadata["license"] = license
								} else if licenses, ok := packageJSON["licenses"].([]interface{}); ok && len(licenses) > 0 {
									if licenseObj, ok := licenses[0].(map[string]interface{}); ok {
										if licenseType, ok := licenseObj["type"].(string); ok {
											metadata["license"] = licenseType
										}
									}
								}
								if description, ok := packageJSON["description"].(string); ok {
									metadata["description"] = description
								}
								if deps, ok := packageJSON["dependencies"].(map[string]interface{}); ok {
									depsList := make([]string, 0, len(deps))
									for dep := range deps {
										depsList = append(depsList, dep)
									}
									metadata["dependencies"] = depsList
								}
								util.Logerr(gzReader2.Close())
								return metadata
							}
						}
						break
					}
				}
				util.Logerr(gzReader2.Close())
			}
		}
	}
	return nil
}

// extractPyPIMetadata extracts metadata from a PyPI package (wheel or sdist)
func (e *PackageEnricher) extractPyPIMetadata(pkg PackageToEnrich) map[string]interface{} {
	var metadata map[string]interface{}

	// Cache files are stored with hash-based names, so we need to check the URL path
	// Load metadata to get the URL path
	metaPath := pkg.CachePath + ".meta"
	metaData, err := os.ReadFile(metaPath)
	var urlPath string
	if err == nil {
		var meta struct {
			URLPath string `json:"url_path"`
		}
		if json.Unmarshal(metaData, &meta) == nil {
			urlPath = meta.URLPath
		}
	}

	// Determine file type from URL path or cache path
	isWheel := strings.HasSuffix(urlPath, ".whl") || strings.HasSuffix(pkg.CachePath, ".whl")
	isSDist := strings.HasSuffix(urlPath, ".tar.gz") || strings.HasSuffix(pkg.CachePath, ".tar.gz")

	// For wheels, try to extract METADATA
	if isWheel {
		metadata = e.extractWheelMetadata(pkg.CachePath)
		// Also try to extract from LICENSE/README files in the wheel
		if metadata != nil {
			e.enrichPyPIMetadataFromFiles(pkg.CachePath, metadata, true)
		}
		return metadata
	}

	// For source distributions, try to extract PKG-INFO
	if isSDist {
		metadata = e.extractSDistMetadata(pkg.CachePath)
		// Also try to extract from LICENSE/README files in the sdist
		if metadata != nil {
			e.enrichPyPIMetadataFromFiles(pkg.CachePath, metadata, false)
		}
		return metadata
	}

	// If we couldn't determine from URL path, try to detect by file content
	// Check if it's a zip file (wheel) or gzip (sdist)
	file, err := os.Open(pkg.CachePath)
	if err != nil {
		return nil
	}
	defer util.DeferClose(file)

	// Read first few bytes to detect file type
	magic := make([]byte, 4)
	if n, _ := file.Read(magic); n >= 2 {
		// ZIP files start with PK (0x50 0x4B)
		if magic[0] == 0x50 && magic[1] == 0x4B {
			// Likely a wheel
			metadata = e.extractWheelMetadata(pkg.CachePath)
			if metadata != nil {
				e.enrichPyPIMetadataFromFiles(pkg.CachePath, metadata, true)
			}
			return metadata
		}
		// GZIP files start with 0x1F 0x8B
		if magic[0] == 0x1F && magic[1] == 0x8B {
			// Likely a source distribution
			metadata = e.extractSDistMetadata(pkg.CachePath)
			if metadata != nil {
				e.enrichPyPIMetadataFromFiles(pkg.CachePath, metadata, false)
			}
			return metadata
		}
	}

	return metadata
}

// enrichPyPIMetadataFromFiles enriches PyPI metadata by extracting from LICENSE and README files
func (e *PackageEnricher) enrichPyPIMetadataFromFiles(pkgPath string, metadata map[string]interface{}, isWheel bool) {
	if isWheel {
		// For wheels, extract from zip
		r, err := zip.OpenReader(pkgPath)
		if err != nil {
			return
		}
		defer util.DeferClose(r)

		// Extract license if not already present or unclear
		if license, ok := metadata["license"].(string); !ok || license == "" || license == "UNKNOWN" {
			if extractedLicense := e.extractLicenseFromPyPIZip(r); extractedLicense != "" {
				metadata["license"] = extractedLicense
			}
		}

		// Extract description from README if not present or too short
		if desc, ok := metadata["description"].(string); !ok || desc == "" || len(desc) < 50 {
			if extractedDesc := e.extractDescriptionFromPyPIZip(r); extractedDesc != "" {
				metadata["description"] = extractedDesc
			}
		}
	} else {
		// For source distributions, extract from tar.gz
		file, err := os.Open(pkgPath)
		if err != nil {
			return
		}
		defer util.DeferClose(file)

		gzReader, err := gzip.NewReader(file)
		if err != nil {
			return
		}
		defer util.DeferClose(gzReader)

		tarReader := tar.NewReader(gzReader)

		// Extract both license and description in one pass
		var extractedLicense, extractedDesc string
		needLicense := false
		needDesc := false

		if license, ok := metadata["license"].(string); !ok || license == "" || license == "UNKNOWN" {
			needLicense = true
		}
		if desc, ok := metadata["description"].(string); !ok || desc == "" || len(desc) < 50 {
			needDesc = true
		}

		if needLicense || needDesc {
			// Read tar once and extract both
			for {
				header, err := tarReader.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					break
				}

				if header.Typeflag == tar.TypeReg {
					baseName := strings.ToUpper(filepath.Base(header.Name))

					// Check for LICENSE files
					if needLicense && extractedLicense == "" {
						licenseFiles := []string{"LICENSE", "LICENSE.txt", "LICENSE.md", "LICENCE", "LICENCE.txt", "LICENCE.md"}
						for _, licenseFile := range licenseFiles {
							if baseName == strings.ToUpper(licenseFile) || strings.HasSuffix(baseName, "/"+strings.ToUpper(licenseFile)) {
								content, err := io.ReadAll(tarReader)
								if err == nil {
									extractedLicense = e.detectLicenseFromContent(string(content))
								}
								break
							}
						}
					}

					// Check for README files
					if needDesc && extractedDesc == "" {
						readmeFiles := []string{"README", "README.txt", "README.md", "README.rst", "README.org"}
						for _, readmeFile := range readmeFiles {
							if baseName == strings.ToUpper(readmeFile) || strings.HasSuffix(baseName, "/"+strings.ToUpper(readmeFile)) {
								content, err := io.ReadAll(tarReader)
								if err == nil {
									extractedDesc = e.extractFirstParagraph(string(content))
								}
								break
							}
						}
					}

					// Stop if we found both
					if (!needLicense || extractedLicense != "") && (!needDesc || extractedDesc != "") {
						break
					}
				}
			}

			// Update metadata
			if extractedLicense != "" {
				metadata["license"] = extractedLicense
			}
			if extractedDesc != "" {
				metadata["description"] = extractedDesc
			}
		}
	}
}

// extractLicenseFromPyPIZip extracts license from LICENSE files in a wheel
func (e *PackageEnricher) extractLicenseFromPyPIZip(r *zip.ReadCloser) string {
	licenseFiles := []string{"LICENSE", "LICENSE.txt", "LICENSE.md", "LICENCE", "LICENCE.txt", "LICENCE.md"}
	for _, f := range r.File {
		baseName := strings.ToUpper(filepath.Base(f.Name))
		for _, licenseFile := range licenseFiles {
			if baseName == strings.ToUpper(licenseFile) || strings.HasSuffix(baseName, "/"+strings.ToUpper(licenseFile)) {
				rc, err := f.Open()
				if err != nil {
					continue
				}
				defer util.DeferClose(rc)

				content, err := io.ReadAll(rc)
				if err != nil {
					continue
				}

				return e.detectLicenseFromContent(string(content))
			}
		}
	}
	return ""
}

// extractDescriptionFromPyPIZip extracts description from README files in a wheel
func (e *PackageEnricher) extractDescriptionFromPyPIZip(r *zip.ReadCloser) string {
	readmeFiles := []string{"README", "README.txt", "README.md", "README.rst", "README.org"}
	for _, f := range r.File {
		baseName := strings.ToUpper(filepath.Base(f.Name))
		for _, readmeFile := range readmeFiles {
			if baseName == strings.ToUpper(readmeFile) || strings.HasSuffix(baseName, "/"+strings.ToUpper(readmeFile)) {
				rc, err := f.Open()
				if err != nil {
					continue
				}
				defer util.DeferClose(rc)

				content, err := io.ReadAll(rc)
				if err != nil {
					continue
				}

				return e.extractFirstParagraph(string(content))
			}
		}
	}
	return ""
}

// detectLicenseFromContent detects license type from LICENSE file content
func (e *PackageEnricher) detectLicenseFromContent(content string) string {
	licenseText := strings.ToUpper(content)

	// Check for common license identifiers
	if strings.Contains(licenseText, "MIT") {
		return "MIT"
	}
	if strings.Contains(licenseText, "APACHE") && strings.Contains(licenseText, "2.0") {
		return "Apache-2.0"
	}
	if strings.Contains(licenseText, "BSD") {
		if strings.Contains(licenseText, "3-CLAUSE") || strings.Contains(licenseText, "3 CLAUSE") {
			return "BSD-3-Clause"
		}
		if strings.Contains(licenseText, "2-CLAUSE") || strings.Contains(licenseText, "2 CLAUSE") {
			return "BSD-2-Clause"
		}
		return "BSD"
	}
	if strings.Contains(licenseText, "GPL") {
		if strings.Contains(licenseText, "3") {
			return "GPL-3.0"
		}
		if strings.Contains(licenseText, "2") {
			return "GPL-2.0"
		}
		return "GPL"
	}
	if strings.Contains(licenseText, "ISC") {
		return "ISC"
	}

	// If no standard license found, return first line (often has license name)
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && len(lines[0]) < 100 {
		return strings.TrimSpace(lines[0])
	}

	return "See LICENSE file"
}

// extractFirstParagraph extracts the first paragraph from text (used for README files)
func (e *PackageEnricher) extractFirstParagraph(text string) string {
	// For markdown, skip frontmatter and headers
	if strings.HasPrefix(text, "---") {
		parts := strings.SplitN(text, "---", 3)
		if len(parts) >= 3 {
			text = parts[2]
		}
	}

	// Split into lines and find first non-empty paragraph
	lines := strings.Split(text, "\n")
	var paragraph strings.Builder
	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Skip headers, empty lines, badges, etc.
		if line == "" {
			if paragraph.Len() > 0 {
				break // End of first paragraph
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue // Skip headers
		}
		if strings.HasPrefix(line, "[!") || strings.HasPrefix(line, "![") {
			continue // Skip badges
		}
		if strings.HasPrefix(line, "<!--") {
			continue // Skip HTML comments
		}

		// Add to paragraph
		if paragraph.Len() > 0 {
			paragraph.WriteString(" ")
		}
		paragraph.WriteString(line)

		// Stop at first sentence (if we have a reasonable length)
		if paragraph.Len() > 100 && (strings.HasSuffix(line, ".") || strings.HasSuffix(line, "!") || strings.HasSuffix(line, "?")) {
			break
		}

		// Limit paragraph length
		if paragraph.Len() > 500 {
			// Truncate at last sentence
			text := paragraph.String()
			lastPeriod := strings.LastIndex(text, ".")
			if lastPeriod > 100 {
				return text[:lastPeriod+1]
			}
			return text[:500] + "..."
		}
	}

	return strings.TrimSpace(paragraph.String())
}

// extractWheelMetadata extracts metadata from a wheel file
func (e *PackageEnricher) extractWheelMetadata(wheelPath string) map[string]interface{} {
	r, err := zip.OpenReader(wheelPath)
	if err != nil {
		return nil
	}
	defer util.DeferClose(r)

	// Look for METADATA file in .dist-info directory
	for _, f := range r.File {
		if strings.Contains(f.Name, ".dist-info/METADATA") {
			rc, err := f.Open()
			if err != nil {
				continue
			}
			defer util.DeferClose(rc)

			data, err := io.ReadAll(rc)
			if err != nil {
				continue
			}

			return e.parsePKGINFO(string(data))
		}
	}

	return nil
}

// extractSDistMetadata extracts metadata from a source distribution
func (e *PackageEnricher) extractSDistMetadata(sdistPath string) map[string]interface{} {
	file, err := os.Open(sdistPath)
	if err != nil {
		return nil
	}
	defer util.DeferClose(file)

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return nil
	}
	defer util.DeferClose(gzReader)

	tarReader := tar.NewReader(gzReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil
		}

		if strings.HasSuffix(header.Name, "/PKG-INFO") {
			data, err := io.ReadAll(tarReader)
			if err != nil {
				return nil
			}

			return e.parsePKGINFO(string(data))
		}
	}

	return nil
}

// parsePKGINFO parses PKG-INFO format (used by PyPI)
// METADATA format can have:
// - Single-line fields: "Key: value"
// - Multi-line fields with continuation (indented lines)
// - Description field that comes after "Description:" or "Description-Content-Type:" followed by blank line
func (e *PackageEnricher) parsePKGINFO(content string) map[string]interface{} {
	metadata := make(map[string]interface{})
	lines := strings.Split(content, "\n")

	var descriptionLines []string
	inDescription := false
	var currentKey string
	seenDescriptionHeader := false
	seenDescriptionContentType := false

	for _, line := range lines {
		trimmedLine := strings.TrimSpace(line)

		// Check if we're in description content (after Description-Content-Type and blank line)
		if inDescription {
			// Stop at next field (line starting with a key:)
			if strings.Contains(trimmedLine, ":") && !strings.HasPrefix(trimmedLine, " ") && !strings.HasPrefix(trimmedLine, "\t") {
				// This is a new field, stop collecting description
				inDescription = false
				currentKey = ""
			} else if trimmedLine != "" {
				// Collect description content
				descriptionLines = append(descriptionLines, trimmedLine)
				continue
			} else if len(descriptionLines) > 0 {
				// Empty line after description content - might be end, but continue collecting
				// (some descriptions have blank lines within them)
				continue
			}
		}

		// Empty line - might signal start of description content
		if trimmedLine == "" {
			if seenDescriptionHeader && !inDescription && len(descriptionLines) == 0 {
				// We saw "Description:" or "Description-Content-Type:" and now we have a blank line
				// The description content starts on the next non-empty line
				inDescription = true
				currentKey = "Description"
			} else if !inDescription {
				currentKey = ""
			}
			continue
		}

		// PKG-INFO format: Key: value
		if idx := strings.Index(trimmedLine, ":"); idx > 0 {
			key := strings.TrimSpace(trimmedLine[:idx])
			value := strings.TrimSpace(trimmedLine[idx+1:])

			// End previous field
			if currentKey == "Description" && inDescription {
				inDescription = false
			}
			currentKey = key

			switch key {
			case "Name":
				metadata["name"] = value
			case "Version":
				metadata["version"] = value
			case "License":
				metadata["license"] = value
			case "Summary":
				// Summary is usually a single line, but use it if we don't have Description
				if _, ok := metadata["description"]; !ok && value != "" {
					metadata["description"] = value
				}
			case "Description":
				// Description field - value might be empty, content comes after blank line
				if value != "" {
					descriptionLines = []string{value}
				} else {
					descriptionLines = []string{}
				}
				seenDescriptionHeader = true
				// Don't set inDescription yet - wait for blank line
			case "Description-Content-Type":
				// This indicates description content follows after a blank line
				seenDescriptionContentType = true
				seenDescriptionHeader = true
				descriptionLines = []string{}
				// Don't set inDescription yet - wait for blank line
			}
		} else if inDescription && currentKey == "Description" {
			// Continuation of description (shouldn't happen with modern format, but handle it)
			if trimmedLine != "" {
				descriptionLines = append(descriptionLines, trimmedLine)
			}
		}
	}

	// Join description lines if we collected any
	if len(descriptionLines) > 0 {
		// Filter out empty lines and join
		var filtered []string
		for _, line := range descriptionLines {
			if strings.TrimSpace(line) != "" {
				filtered = append(filtered, strings.TrimSpace(line))
			}
		}
		if len(filtered) > 0 {
			// Take first paragraph (up to first blank line or first 500 chars)
			desc := strings.Join(filtered, " ")
			// Limit length
			if len(desc) > 500 {
				// Try to truncate at sentence boundary
				if lastPeriod := strings.LastIndex(desc[:500], "."); lastPeriod > 100 {
					desc = desc[:lastPeriod+1]
				} else {
					desc = desc[:500] + "..."
				}
			}
			metadata["description"] = desc
		}
	}

	// Debug logging for empty descriptions
	if _, ok := metadata["description"]; !ok {
		log.Printf("[enricher] No description found in METADATA (saw Description header: %v, Content-Type: %v, lines: %d)",
			seenDescriptionHeader, seenDescriptionContentType, len(lines))
	}

	return metadata
}

// extractGoMetadata extracts metadata from a Go module zip
func (e *PackageEnricher) extractGoMetadata(pkg PackageToEnrich) map[string]interface{} {
	r, err := zip.OpenReader(pkg.CachePath)
	if err != nil {
		return nil
	}
	defer util.DeferClose(r)

	metadata := make(map[string]interface{})
	metadata["version"] = pkg.Version

	// Look for go.mod
	for _, f := range r.File {
		if strings.HasSuffix(f.Name, "/go.mod") || f.Name == "go.mod" {
			rc, err := f.Open()
			if err != nil {
				continue
			}
			defer util.DeferClose(rc)

			data, err := io.ReadAll(rc)
			if err != nil {
				continue
			}

			// Parse go.mod
			lines := strings.Split(string(data), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "go ") {
					goVersion := strings.TrimSpace(strings.TrimPrefix(line, "go "))
					metadata["goVersion"] = goVersion
				}
			}
			break
		}
	}

	// Extract license from LICENSE files
	license := e.extractLicenseFromGoZip(r)
	if license != "" {
		metadata["license"] = license
	}

	// Extract description from README files
	description := e.extractDescriptionFromGoZip(r)
	if description != "" {
		// Check if the description is satisfying (not too short, not just code/links)
		if e.isSatisfyingDescription(description) {
			metadata["description"] = description
		} else {
			// Try to fetch from repo API as fallback
			if repoDesc := e.fetchRepoDescription(pkg.Name); repoDesc != "" {
				metadata["description"] = repoDesc
			} else {
				// Use the README description even if not ideal, better than nothing
				metadata["description"] = description
			}
		}
	} else {
		// No description from README, try repo API
		if repoDesc := e.fetchRepoDescription(pkg.Name); repoDesc != "" {
			metadata["description"] = repoDesc
		}
	}

	return metadata
}

// extractLicenseFromGoZip extracts license information from a Go module zip file
func (e *PackageEnricher) extractLicenseFromGoZip(r *zip.ReadCloser) string {
	// Look for LICENSE files (case-insensitive)
	licenseFiles := []string{"LICENSE", "LICENSE.txt", "LICENSE.md", "LICENCE", "LICENCE.txt", "LICENCE.md"}
	for _, f := range r.File {
		baseName := strings.ToUpper(filepath.Base(f.Name))
		for _, licenseFile := range licenseFiles {
			if baseName == strings.ToUpper(licenseFile) || strings.HasSuffix(baseName, "/"+strings.ToUpper(licenseFile)) {
				// Read LICENSE file
				rc, err := f.Open()
				if err != nil {
					continue
				}
				defer util.DeferClose(rc)

				content, err := io.ReadAll(rc)
				if err != nil {
					continue
				}

				// Try to extract license type from content
				// Common patterns: "MIT License", "Apache License 2.0", etc.
				licenseText := string(content)
				licenseText = strings.ToUpper(licenseText)

				// Check for common license identifiers
				if strings.Contains(licenseText, "MIT") {
					return "MIT"
				}
				if strings.Contains(licenseText, "APACHE") && strings.Contains(licenseText, "2.0") {
					return "Apache-2.0"
				}
				if strings.Contains(licenseText, "BSD") {
					if strings.Contains(licenseText, "3-CLAUSE") || strings.Contains(licenseText, "3 CLAUSE") {
						return "BSD-3-Clause"
					}
					if strings.Contains(licenseText, "2-CLAUSE") || strings.Contains(licenseText, "2 CLAUSE") {
						return "BSD-2-Clause"
					}
					return "BSD"
				}
				if strings.Contains(licenseText, "GPL") {
					if strings.Contains(licenseText, "3") {
						return "GPL-3.0"
					}
					if strings.Contains(licenseText, "2") {
						return "GPL-2.0"
					}
					return "GPL"
				}
				if strings.Contains(licenseText, "ISC") {
					return "ISC"
				}

				// If no standard license found, return first line (often has license name)
				lines := strings.Split(string(content), "\n")
				if len(lines) > 0 && len(lines[0]) < 100 {
					return strings.TrimSpace(lines[0])
				}

				return "See LICENSE file"
			}
		}
	}

	// Fallback: check go.mod for license comment (some modules have it)
	for _, f := range r.File {
		if strings.HasSuffix(f.Name, "/go.mod") || f.Name == "go.mod" {
			rc, err := f.Open()
			if err != nil {
				continue
			}
			defer util.DeferClose(rc)

			content, err := io.ReadAll(rc)
			if err != nil {
				continue
			}

			// Look for license comment in go.mod
			lines := strings.Split(string(content), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "//") {
					comment := strings.TrimSpace(strings.TrimPrefix(line, "//"))
					if strings.Contains(strings.ToUpper(comment), "LICENSE") {
						// Try to extract license name from comment
						upperComment := strings.ToUpper(comment)
						if strings.Contains(upperComment, "MIT") {
							return "MIT"
						}
						if strings.Contains(upperComment, "APACHE") {
							return "Apache-2.0"
						}
						if strings.Contains(upperComment, "BSD") {
							return "BSD"
						}
						if strings.Contains(upperComment, "GPL") {
							return "GPL"
						}
					}
				}
			}
		}
	}

	return ""
}

// extractDescriptionFromGoZip extracts the first paragraph from README files in a Go module zip
func (e *PackageEnricher) extractDescriptionFromGoZip(r *zip.ReadCloser) string {
	// Look for README files (case-insensitive, various extensions)
	readmeFiles := []string{"README", "README.txt", "README.md", "README.rst", "README.org"}
	for _, f := range r.File {
		baseName := strings.ToUpper(filepath.Base(f.Name))
		for _, readmeFile := range readmeFiles {
			if baseName == strings.ToUpper(readmeFile) || strings.HasSuffix(baseName, "/"+strings.ToUpper(readmeFile)) {
				// Read README file
				rc, err := f.Open()
				if err != nil {
					continue
				}
				defer util.DeferClose(rc)

				content, err := io.ReadAll(rc)
				if err != nil {
					continue
				}

				// Extract first paragraph with improved markdown parsing
				text := string(content)
				desc := e.extractGoDescriptionFromMarkdown(text)
				if desc != "" {
					return desc
				}
			}
		}
	}

	return ""
}

// extractGoDescriptionFromMarkdown extracts the first paragraph from markdown, parsing links and skipping badges
func (e *PackageEnricher) extractGoDescriptionFromMarkdown(text string) string {
	// Skip YAML frontmatter
	if strings.HasPrefix(text, "---") {
		parts := strings.SplitN(text, "---", 3)
		if len(parts) >= 3 {
			text = parts[2]
		}
	}

	// Split into lines
	lines := strings.Split(text, "\n")

	// Track if we've seen the first header (title)
	seenFirstHeader := false
	// Track if we're inside a code block (between ``` markers)
	inCodeBlock := false
	// Track if the previous line was a header (to skip underline)
	previousWasHeader := false
	var paragraph strings.Builder

	for i, line := range lines {
		originalLine := line
		trimmedLine := strings.TrimSpace(line)

		// Track code block state (fenced code blocks with ```)
		// Check if line starts with ``` (after trimming whitespace)
		if strings.HasPrefix(trimmedLine, "```") {
			inCodeBlock = !inCodeBlock // Toggle state
			previousWasHeader = false
			continue
		}

		// Skip all lines while inside a code block
		if inCodeBlock {
			previousWasHeader = false
			continue
		}

		// Also check if this line is inside a code block by looking for indentation
		// Code blocks in markdown can be indented with 4 spaces
		// But only skip if it looks like code, not if it's a list item
		if strings.HasPrefix(originalLine, "    ") && !strings.HasPrefix(trimmedLine, "- ") &&
			!strings.HasPrefix(trimmedLine, "* ") && !strings.HasPrefix(trimmedLine, "+ ") &&
			!strings.HasPrefix(trimmedLine, "1. ") && !strings.HasPrefix(trimmedLine, "2. ") {
			// Check if it looks like code (contains "go get" or other code patterns)
			if strings.Contains(trimmedLine, "go get") ||
				strings.Contains(trimmedLine, "package ") ||
				strings.Contains(trimmedLine, "import ") ||
				strings.Contains(trimmedLine, "func ") {
				continue
			}
		}

		line = trimmedLine

		// Skip empty lines
		if line == "" {
			previousWasHeader = false
			if paragraph.Len() > 0 {
				break // End of first paragraph
			}
			continue
		}

		// Check if this is a markdown underline (===, ---, ___) for a header
		// These are lines that are all the same character (3+ times)
		isUnderline := false
		if len(line) >= 3 {
			firstChar := line[0]
			if (firstChar == '=' || firstChar == '-' || firstChar == '_') &&
				strings.Count(line, string(firstChar)) == len(line) {
				isUnderline = true
			}
		}

		// Skip underlines (they follow headers)
		if isUnderline {
			previousWasHeader = false // Reset after skipping underline
			if !seenFirstHeader {
				seenFirstHeader = true // Mark that we've seen the first header section
			}
			continue
		}

		// Skip headers - but mark when we've seen the first one
		if strings.HasPrefix(line, "#") {
			previousWasHeader = true
			if !seenFirstHeader {
				seenFirstHeader = true
			}
			continue
		}

		// Skip lines that look like headers with badges (e.g., "go-socks5 [![Build Status]...")
		// These often have underlines on the next line
		if previousWasHeader || (i > 0 && i < len(lines)-1) {
			// Check if next line is an underline
			nextLine := ""
			if i+1 < len(lines) {
				nextLine = strings.TrimSpace(lines[i+1])
			}
			nextIsUnderline := false
			if len(nextLine) >= 3 {
				firstChar := nextLine[0]
				if (firstChar == '=' || firstChar == '-' || firstChar == '_') &&
					strings.Count(nextLine, string(firstChar)) == len(nextLine) {
					nextIsUnderline = true
				}
			}

			// If this line has badges and next line is underline, skip this line
			if nextIsUnderline && e.isBadgeLine(line) {
				previousWasHeader = true
				if !seenFirstHeader {
					seenFirstHeader = true
				}
				continue
			}
		}

		previousWasHeader = false

		// Skip reference-style link definitions: [1]: https://...
		if referenceLinkRegex.MatchString(line) {
			continue
		}

		// Skip lines that are just URLs
		if urlOnlyLineRegex.MatchString(line) {
			continue
		}

		// Skip lines that are entirely within code block markers (``` ... ```)
		// This catches single-line code blocks like "``` go get ... ```"
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") && strings.HasSuffix(trimmed, "```") {
			continue
		}
		// Also skip if line has 6+ backticks (likely a code block)
		if strings.Count(line, "`") >= 6 {
			continue
		}
		// Skip lines that start with ``` and contain code (even if not properly closed)
		if strings.HasPrefix(trimmed, "```") {
			// Check if it looks like a code block (has language identifier or code-like content)
			afterBackticks := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			// Remove closing ``` if present
			afterBackticks = strings.TrimSuffix(afterBackticks, "```")
			afterBackticks = strings.TrimSpace(afterBackticks)
			if afterBackticks != "" {
				// Check if it's a language identifier (short word) or code
				words := strings.Fields(afterBackticks)
				if len(words) == 1 && len(words[0]) < 20 {
					// Likely a language identifier like "go", "bash", etc.
					continue
				}
				// Check if it contains code-like patterns - be very aggressive here
				lowerAfterBackticks := strings.ToLower(afterBackticks)
				if strings.Contains(lowerAfterBackticks, "go get") ||
					strings.Contains(lowerAfterBackticks, "package ") ||
					strings.Contains(lowerAfterBackticks, "import ") ||
					strings.Contains(lowerAfterBackticks, "github.com") ||
					strings.Contains(lowerAfterBackticks, "golang.org") {
					continue
				}
			}
		}
		// Also check if line contains ```go get``` pattern (code block with go get)
		// This catches cases where the line itself contains the code block markers
		if strings.Contains(trimmed, "```") {
			// Extract content between backticks
			parts := strings.Split(trimmed, "```")
			for i := 1; i < len(parts); i += 2 { // Check odd indices (content between ```)
				if i < len(parts) {
					content := strings.TrimSpace(parts[i])
					lowerContent := strings.ToLower(content)
					// Check if it contains "go get" or other code patterns - be very aggressive
					if strings.Contains(lowerContent, "go get") ||
						strings.Contains(lowerContent, "package ") ||
						strings.Contains(lowerContent, "import ") ||
						strings.Contains(lowerContent, "github.com") ||
						strings.Contains(lowerContent, "golang.org") {
						continue
					}
				}
			}
		}

		// Skip lines that are just "go get" commands (even outside code blocks, they're not descriptive)
		// This is a very common pattern in READMEs and should always be skipped
		if strings.HasPrefix(trimmed, "go get ") {
			// Check if it's mostly just the command and package path
			words := strings.Fields(trimmed)
			if len(words) <= 3 {
				continue // "go get github.com/user/package" - skip it
			}
			// Even if longer, if it's clearly a command with a package path, skip it
			if len(words) <= 5 && (strings.Contains(trimmed, "github.com") || strings.Contains(trimmed, "golang.org")) {
				continue
			}
		}
		// Also skip if line contains "go get" and is mostly just that command
		if strings.Contains(trimmed, "go get ") {
			// Remove "go get" and check if what's left is mostly just a package path
			withoutGoGet := strings.ReplaceAll(trimmed, "go get", "")
			withoutGoGet = strings.TrimSpace(withoutGoGet)
			// If what's left is short and looks like a package path (contains / or .)
			if len(withoutGoGet) < 100 && (strings.Contains(withoutGoGet, "/") || strings.Contains(withoutGoGet, ".")) {
				// Check if it's mostly just the package path (not descriptive text)
				words := strings.Fields(withoutGoGet)
				if len(words) <= 3 {
					continue
				}
			}
		}

		// Skip lines that look like code (indented with 4+ spaces or tabs, common in markdown)
		if strings.HasPrefix(originalLine, "    ") || strings.HasPrefix(originalLine, "\t") {
			// But allow if it's part of a list item or has meaningful text
			if !strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "* ") && !strings.HasPrefix(line, "+ ") {
				// Check if it's mostly code-like (contains common code patterns)
				if strings.Contains(line, "func ") || strings.Contains(line, "package ") ||
					strings.Contains(line, "import ") || strings.Contains(line, "var ") ||
					strings.Contains(line, "const ") || strings.Contains(line, "type ") ||
					strings.Contains(line, "if ") || strings.Contains(line, "for ") {
					continue
				}
			}
		}

		// Skip horizontal rules (including when they're part of a line)
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "***") || strings.HasPrefix(line, "___") ||
			strings.HasPrefix(line, "===") || strings.HasPrefix(line, "===") {
			continue
		}

		// Skip lines that have badges followed by underlines on the same line
		// Pattern: "title [![badge](url)](url) ========="
		// Check if line ends with underline characters
		trimmedEnd := strings.TrimRight(line, "=_-")
		if len(trimmedEnd) < len(line) && len(line)-len(trimmedEnd) >= 3 {
			// Line ends with underline characters, check if it has badges
			if e.isBadgeLine(trimmedEnd) || strings.Contains(trimmedEnd, "[![") || strings.Contains(trimmedEnd, "](http") {
				// This is a title with badges followed by underline - skip it
				previousWasHeader = true
				if !seenFirstHeader {
					seenFirstHeader = true
				}
				continue
			}
		}

		// Skip badges and images (various formats)
		// Check if line contains badge patterns
		if e.isBadgeLine(line) {
			continue
		}

		// Skip HTML comments
		if strings.HasPrefix(line, "<!--") {
			continue
		}

		// Only start collecting after we've seen the first header
		if !seenFirstHeader {
			continue
		}

		// Skip short phrases that are not descriptive
		// These are often section headers or instruction starters
		lowerLine := strings.ToLower(line)
		shortPhrases := []string{
			"introduction",
			"install",
			"installation",
			"usage",
			"example",
			"examples",
			"getting started",
			"quick start",
			"overview",
		}
		for _, phrase := range shortPhrases {
			// Check if line is just the phrase or starts with it followed by colon
			if lowerLine == phrase ||
				lowerLine == phrase+":" ||
				strings.HasPrefix(lowerLine, phrase+": ") {
				// Skip if it's too short (likely just a header)
				if len(line) < 50 {
					continue
				}
			}
		}
		// Also skip common instruction starters
		if strings.HasPrefix(lowerLine, "install the package with:") ||
			strings.HasPrefix(lowerLine, "install with:") ||
			strings.HasPrefix(lowerLine, "to install:") {
			if len(line) < 100 {
				continue
			}
		}

		// Skip lines that are mostly URLs or links
		if e.isMostlyLinks(line) {
			continue
		}

		// Skip lines that are just "package X" (not descriptive)
		if strings.HasPrefix(trimmed, "package ") {
			words := strings.Fields(trimmed)
			if len(words) <= 2 {
				continue // "package pretty" or "package main" - skip it
			}
		}

		// Skip lines that are list items with mostly links (like "- name: https://...")
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
			// Check if it's mostly links
			linkCount := 0
			words := strings.Fields(line)
			for _, word := range words {
				if strings.HasPrefix(word, "http://") || strings.HasPrefix(word, "https://") {
					linkCount++
				}
			}
			// If it's mostly links, skip it
			if len(words) > 0 && float64(linkCount)/float64(len(words)) > 0.3 {
				continue
			}
			// If it's a list item with descriptive text, we might want to keep it
			// But only if it scores well (handled by scoreDescriptionLine below)
		}

		// Parse markdown links: [text](url) -> text
		// Also handle reference-style links: [text][ref] -> text
		parsedLine := e.parseMarkdownLinks(line)

		// Skip if after parsing we have very little text (mostly links were removed)
		if len(parsedLine) < 20 && len(line) > 50 {
			continue
		}

		// Skip if the parsed line is too short or just a single word/phrase
		if len(parsedLine) < 30 {
			words := strings.Fields(parsedLine)
			if len(words) <= 3 {
				continue
			}
		}

		// Score this line - prefer natural sentence starts
		score := e.scoreDescriptionLine(parsedLine)
		if score < 0 {
			continue // Skip low-scoring lines
		}

		// Add to paragraph
		if paragraph.Len() > 0 {
			paragraph.WriteString(" ")
		}
		paragraph.WriteString(parsedLine)

		// Stop at first sentence (if we have a reasonable length)
		if paragraph.Len() > 100 && (strings.HasSuffix(parsedLine, ".") || strings.HasSuffix(parsedLine, "!") || strings.HasSuffix(parsedLine, "?")) {
			break
		}

		// Limit paragraph length
		if paragraph.Len() > 500 {
			// Truncate at last sentence
			text := paragraph.String()
			lastPeriod := strings.LastIndex(text, ".")
			if lastPeriod > 100 {
				desc := html.UnescapeString(text[:lastPeriod+1])
				return strings.TrimSpace(desc)
			}
			desc := html.UnescapeString(text[:500] + "...")
			return strings.TrimSpace(desc)
		}
	}

	desc := strings.TrimSpace(paragraph.String())
	// Decode HTML entities in the final description
	desc = html.UnescapeString(desc)
	return desc
}

// scoreDescriptionLine scores a line for how good it is as a description
// Returns positive score for good lines, negative for lines to skip
// Prefers lines that start with natural sentence patterns
func (e *PackageEnricher) scoreDescriptionLine(line string) int {
	line = strings.TrimSpace(line)
	if line == "" {
		return -1
	}

	lowerLine := strings.ToLower(line)

	// High score for lines starting with natural sentence patterns
	naturalStarts := []string{
		"this ",
		"the ",
		"a ",
		"an ",
		"provides ",
		"package ",
		"is ",
		"implements ",
		"contains ",
		"offers ",
		"supports ",
		"allows ",
		"enables ",
		"includes ",
	}

	for _, start := range naturalStarts {
		if strings.HasPrefix(lowerLine, start) {
			// Good start, but check it's not too short
			if len(line) >= 30 {
				return 10
			}
		}
	}

	// Medium score for lines that are descriptive but don't start with natural patterns
	// Check if it's a complete sentence (ends with punctuation)
	if strings.HasSuffix(line, ".") || strings.HasSuffix(line, "!") || strings.HasSuffix(line, "?") {
		if len(line) >= 40 {
			return 5
		}
	}

	// Low score (but not negative) for other lines that are reasonably long
	if len(line) >= 50 {
		return 1
	}

	// Negative score for lines that are likely not good descriptions
	// Lines that are mostly links, code, or very short
	if len(line) < 30 {
		return -1
	}

	// Check if it's mostly links or code
	words := strings.Fields(line)
	if len(words) == 0 {
		return -1
	}

	linkCount := 0
	for _, word := range words {
		if strings.HasPrefix(word, "http://") || strings.HasPrefix(word, "https://") {
			linkCount++
		}
	}
	if float64(linkCount)/float64(len(words)) > 0.4 {
		return -1 // Mostly links
	}

	return 0 // Neutral
}

// isBadgeLine checks if a line is a badge/image line
func (e *PackageEnricher) isBadgeLine(line string) bool {
	lowerLine := strings.ToLower(line)

	// Check for markdown image syntax
	if strings.HasPrefix(line, "![") || strings.HasPrefix(line, "[![") {
		return true
	}

	// Check for HTML image tags
	if strings.Contains(line, "<img") || strings.Contains(line, "<a href") {
		return true
	}

	// Check for badge-related URLs and patterns
	badgePatterns := []string{
		"shields.io",
		"badge",
		"travis-ci",
		"travis-ci.org",
		"circleci",
		"github.com/actions",
		"goreportcard.com",
		"coveralls.io",
		"godoc.org",
		".svg",
		".png",
		"status.svg",
		"badge.svg",
	}

	// If line contains badge patterns and is mostly links/URLs, it's a badge line
	hasBadgePattern := false
	for _, pattern := range badgePatterns {
		if strings.Contains(lowerLine, pattern) {
			hasBadgePattern = true
			break
		}
	}

	if hasBadgePattern {
		// Check if line is mostly URLs/links (more than 50% of line is URL-like)
		urlCount := 0
		words := strings.Fields(line)
		for _, word := range words {
			if strings.HasPrefix(word, "http://") || strings.HasPrefix(word, "https://") {
				urlCount++
			}
		}
		if len(words) > 0 && float64(urlCount)/float64(len(words)) > 0.5 {
			return true
		}
	}

	return false
}

// isMostlyLinks checks if a line is mostly composed of links/URLs
func (e *PackageEnricher) isMostlyLinks(line string) bool {
	words := strings.Fields(line)
	if len(words) == 0 {
		return false
	}

	linkCount := 0
	for _, word := range words {
		// Check for URLs
		if strings.HasPrefix(word, "http://") || strings.HasPrefix(word, "https://") {
			linkCount++
			continue
		}
		// Check for markdown links [text](url) or [text][ref]
		if strings.Contains(word, "](") || strings.Contains(word, "][") {
			linkCount++
			continue
		}
	}

	// If more than 60% of words are links, consider it mostly links
	return float64(linkCount)/float64(len(words)) > 0.6
}

// parseMarkdownLinks converts markdown links to their text content
// [text](url) -> text
// [text][ref] -> text
// ![alt](url) -> alt (but should be filtered out before this)
func (e *PackageEnricher) parseMarkdownLinks(text string) string {
	// Handle inline links: [text](url)
	for {
		start := strings.Index(text, "[")
		if start == -1 {
			break
		}
		end := strings.Index(text[start:], "]")
		if end == -1 {
			break
		}
		end += start

		// Check if this is a link (has () or [] after the ])
		linkText := text[start+1 : end]
		afterBracket := text[end+1:]

		// Check for inline link: [text](url)
		if len(afterBracket) > 0 && afterBracket[0] == '(' {
			// Find closing paren - need to handle nested parens in URLs
			// URLs can contain parentheses, so we need to find the matching closing paren
			parenDepth := 1
			parenEnd := -1
			for i := 1; i < len(afterBracket); i++ {
				if afterBracket[i] == '(' {
					parenDepth++
				} else if afterBracket[i] == ')' {
					parenDepth--
					if parenDepth == 0 {
						parenEnd = i
						break
					}
				}
			}
			if parenEnd != -1 {
				// Replace [text](url) with text
				text = text[:start] + linkText + text[end+parenEnd+2:]
				continue
			}
		}

		// Check for reference link: [text][ref]
		if len(afterBracket) > 0 && afterBracket[0] == '[' {
			refEnd := strings.Index(afterBracket[1:], "]")
			if refEnd != -1 {
				// Replace [text][ref] with text
				text = text[:start] + linkText + text[end+refEnd+2:]
				continue
			}
		}

		// Not a link, skip this bracket
		text = text[:start] + "[" + text[start+1:]
		break
	}

	// Remove any remaining markdown formatting
	// Bold: **text** -> text
	text = strings.ReplaceAll(text, "**", "")
	text = strings.ReplaceAll(text, "__", "")
	// Italic: *text* -> text (but be careful not to remove list markers)
	// Code: `text` -> text
	for {
		start := strings.Index(text, "`")
		if start == -1 {
			break
		}
		end := strings.Index(text[start+1:], "`")
		if end == -1 {
			break
		}
		end += start + 1
		codeText := text[start+1 : end]
		text = text[:start] + codeText + text[end+1:]
	}

	return text
}

// isSatisfyingDescription checks if a description is good enough (not too short, not just code/links)
func (e *PackageEnricher) isSatisfyingDescription(desc string) bool {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return false
	}

	// Too short
	if len(desc) < 30 {
		return false
	}

	// Check if it's just code blocks or commands
	lowerDesc := strings.ToLower(desc)
	if strings.HasPrefix(lowerDesc, "```") ||
		strings.HasPrefix(lowerDesc, "go get") ||
		strings.HasPrefix(lowerDesc, "package ") {
		return false
	}

	// Check if it's mostly links
	words := strings.Fields(desc)
	if len(words) == 0 {
		return false
	}
	linkCount := 0
	for _, word := range words {
		if strings.HasPrefix(word, "http://") || strings.HasPrefix(word, "https://") {
			linkCount++
		}
	}
	if float64(linkCount)/float64(len(words)) > 0.4 {
		return false
	}

	// Check if it starts with natural sentence patterns (good sign)
	naturalStarts := []string{"this ", "the ", "a ", "an ", "provides ", "package ", "is ", "implements "}
	for _, start := range naturalStarts {
		if strings.HasPrefix(lowerDesc, start) {
			return true
		}
	}

	// If it's reasonably long and not mostly links, it's probably okay
	return len(desc) >= 50
}

// fetchRepoDescription fetches the repository description from the package URL (e.g., GitHub API)
func (e *PackageEnricher) fetchRepoDescription(packageName string) string {
	// Extract repo URL from Go package name
	// Format: github.com/user/repo or golang.org/x/...
	parts := strings.Split(packageName, "/")
	if len(parts) < 3 {
		return "" // Not a valid repo URL format
	}

	host := parts[0]
	user := parts[1]
	repo := parts[2]

	// Remove /vN suffix from repo name if present
	if strings.HasPrefix(repo, "v") && len(repo) > 1 {
		// Check if it's a version suffix
		isVersion := true
		for i := 1; i < len(repo); i++ {
			if repo[i] < '0' || repo[i] > '9' {
				isVersion = false
				break
			}
		}
		if isVersion && len(parts) > 3 {
			repo = parts[3] // Use the part after /vN
		}
	}

	// Only support GitHub for now (most common)
	if host != "github.com" {
		return ""
	}

	// Construct GitHub API URL
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s", user, repo)

	// Use upstream client to fetch (bypasses proxy)
	// getUpstreamClient is in base.go, same package
	client := getUpstreamClient()
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "roxxy/1.0")

	// Set a short timeout for this fallback
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := client.Do(req)
	if err != nil {
		// Silently fail - we're offline or API is unavailable
		return ""
	}
	defer util.DeferClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var repoData struct {
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&repoData); err != nil {
		return ""
	}

	if repoData.Description != "" {
		descLen := len(repoData.Description)
		if descLen > 50 {
			descLen = 50
		}
		log.Printf("[enricher] Fetched repo description for %s: %s", packageName, repoData.Description[:descLen])
		return repoData.Description
	}

	return ""
}

// extractAPTMetadata extracts metadata from a .deb file
// .deb files are ar archives containing:
//   - debian-binary (version string)
//   - control.tar.gz, control.tar.xz, or control.tar.zst (contains control file with metadata)
//   - data.tar.* (contains actual files, not needed for metadata)
func (e *PackageEnricher) extractAPTMetadata(pkg PackageToEnrich) map[string]interface{} {
	file, err := os.Open(pkg.CachePath)
	if err != nil {
		log.Printf("[enricher] Failed to open .deb file %s: %v", pkg.CachePath, err)
		return nil
	}
	defer util.DeferClose(file)

	// Parse ar archive to find control.tar.gz/xz/zst and data.tar.*
	// Use blakesmith/ar library for reliable ar parsing
	var controlData []byte
	var dataData []byte
	var controlIsXZ, controlIsZST bool
	var dataIsXZ, dataIsZST bool

	arReader := ar.NewReader(file)
	for {
		header, err := arReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("[enricher] Failed to read ar archive: %v", err)
			return nil
		}

		filename := strings.TrimRight(header.Name, "\x00 ")

		// Skip debian-binary entry (just contains version string like "2.0\n")
		if filename == "debian-binary" {
			// Read and discard the data
			_, err := io.Copy(io.Discard, arReader)
			if err != nil {
				log.Printf("[enricher] Failed to skip debian-binary: %v", err)
			}
			continue
		}

		if filename == "control.tar.gz" || filename == "control.tar.xz" || filename == "control.tar.zst" {
			controlIsXZ = strings.HasSuffix(filename, ".xz")
			controlIsZST = strings.HasSuffix(filename, ".zst")
			controlData, err = io.ReadAll(arReader)
			if err != nil {
				log.Printf("[enricher] Failed to read control archive: %v", err)
				return nil
			}
			// Continue to look for data.tar.* in case we need it for license extraction
			continue
		} else if strings.HasPrefix(filename, "data.tar.") {
			dataIsXZ = strings.HasSuffix(filename, ".xz")
			dataIsZST = strings.HasSuffix(filename, ".zst")
			dataData, err = io.ReadAll(arReader)
			if err != nil {
				log.Printf("[enricher] Failed to read data archive: %v", err)
				// Don't fail completely if data archive read fails
				dataData = nil
			}
			// If we have both control and data, we can stop
			if controlData != nil {
				break
			}
			continue
		} else {
			// Unknown entry - skip it by reading and discarding
			_, err := io.Copy(io.Discard, arReader)
			if err != nil {
				log.Printf("[enricher] Warning: failed to skip ar entry %q: %v", filename, err)
			}
			continue
		}
	}

	if controlData == nil {
		log.Printf("[enricher] control.tar.gz/xz/zst not found in .deb file: %s", pkg.CachePath)
		return nil
	}

	// Decompress control archive
	// Try different compression formats based on filename, with fallback to content detection
	var tarData []byte

	// Helper to try decompression with a specific format
	tryDecompress := func(format string, decompressFunc func() ([]byte, error)) bool {
		data, err := decompressFunc()
		if err == nil && data != nil {
			tarData = data
			return true
		}
		return false
	}

	// Try formats in order based on filename hint, then fallback to all formats
	attemptedFormats := []string{}

	if controlIsXZ {
		attemptedFormats = append(attemptedFormats, "xz")
		if tryDecompress("xz", func() ([]byte, error) {
			xzReader, err := xz.NewReader(bytes.NewReader(controlData))
			if err != nil {
				return nil, err
			}
			return io.ReadAll(xzReader)
		}) {
			// Success with xz
		} else {
			// Try other formats
			attemptedFormats = append(attemptedFormats, "gzip", "zstd")
			if !tryDecompress("gzip", func() ([]byte, error) {
				gzReader, err := gzip.NewReader(bytes.NewReader(controlData))
				if err != nil {
					return nil, err
				}
				defer util.DeferClose(gzReader)
				return io.ReadAll(gzReader)
			}) {
				tryDecompress("zstd", func() ([]byte, error) {
					zstdReader, err := zstd.NewReader(bytes.NewReader(controlData))
					if err != nil {
						return nil, err
					}
					defer zstdReader.Close()
					return io.ReadAll(zstdReader)
				})
			}
		}
	} else if controlIsZST {
		attemptedFormats = append(attemptedFormats, "zstd")
		if tryDecompress("zstd", func() ([]byte, error) {
			zstdReader, err := zstd.NewReader(bytes.NewReader(controlData))
			if err != nil {
				return nil, err
			}
			defer zstdReader.Close()
			return io.ReadAll(zstdReader)
		}) {
			// Success with zstd
		} else {
			// Try other formats
			attemptedFormats = append(attemptedFormats, "gzip", "xz")
			if !tryDecompress("gzip", func() ([]byte, error) {
				gzReader, err := gzip.NewReader(bytes.NewReader(controlData))
				if err != nil {
					return nil, err
				}
				defer util.DeferClose(gzReader)
				return io.ReadAll(gzReader)
			}) {
				tryDecompress("xz", func() ([]byte, error) {
					xzReader, err := xz.NewReader(bytes.NewReader(controlData))
					if err != nil {
						return nil, err
					}
					return io.ReadAll(xzReader)
				})
			}
		}
	} else {
		// Default: try gzip first
		attemptedFormats = append(attemptedFormats, "gzip")
		if tryDecompress("gzip", func() ([]byte, error) {
			gzReader, err := gzip.NewReader(bytes.NewReader(controlData))
			if err != nil {
				return nil, err
			}
			defer util.DeferClose(gzReader)
			return io.ReadAll(gzReader)
		}) {
			// Success with gzip
		} else {
			// Try other formats
			attemptedFormats = append(attemptedFormats, "zstd", "xz")
			if !tryDecompress("zstd", func() ([]byte, error) {
				zstdReader, err := zstd.NewReader(bytes.NewReader(controlData))
				if err != nil {
					return nil, err
				}
				defer zstdReader.Close()
				return io.ReadAll(zstdReader)
			}) {
				tryDecompress("xz", func() ([]byte, error) {
					xzReader, err := xz.NewReader(bytes.NewReader(controlData))
					if err != nil {
						return nil, err
					}
					return io.ReadAll(xzReader)
				})
			}
		}
	}

	// If all decompression attempts failed, return error
	if tarData == nil {
		log.Printf("[enricher] Failed to decompress control archive (tried formats: %v)", attemptedFormats)
		return nil
	}

	// Parse tar archive to find control file
	tarReader := tar.NewReader(bytes.NewReader(tarData))
	metadata := make(map[string]interface{})
	var descriptionLines []string
	inDescription := false

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("[enricher] Failed to read tar: %v", err)
			return nil
		}

		// Look for control file (usually just "control" or "./control")
		if header.Name == "control" || header.Name == "./control" {
			controlContent, err := io.ReadAll(tarReader)
			if err != nil {
				log.Printf("[enricher] Failed to read control file: %v", err)
				return nil
			}

			// Parse control file (Debian control format: Key: value)
			lines := strings.Split(string(controlContent), "\n")
			for _, line := range lines {
				// Check for continuation lines BEFORE trimming (they start with space/tab)
				if inDescription && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
					descriptionLines = append(descriptionLines, strings.TrimSpace(line))
					continue
				}

				line = strings.TrimSpace(line)
				if line == "" {
					inDescription = false
					continue
				}

				inDescription = false
				// Control file format: Key: value
				if idx := strings.Index(line, ":"); idx > 0 {
					key := strings.TrimSpace(line[:idx])
					value := strings.TrimSpace(line[idx+1:])

					switch key {
					case "Package":
						metadata["name"] = value
					case "Version":
						metadata["version"] = value
					case "License", "Licenses":
						// Some packages use "Licenses" (plural)
						if _, ok := metadata["license"]; !ok || metadata["license"] == "" {
							metadata["license"] = value
						}
					case "Description":
						descriptionLines = []string{value}
						inDescription = true
					case "Depends", "Pre-Depends":
						// Parse dependencies (comma-separated, with version constraints)
						deps := strings.Split(value, ",")
						var depList []string
						for _, dep := range deps {
							// Extract just the package name (before space or version constraint)
							depName := strings.TrimSpace(strings.Split(dep, " ")[0])
							if depName != "" {
								depList = append(depList, depName)
							}
						}
						if len(depList) > 0 {
							// Merge with existing dependencies
							if existing, ok := metadata["dependencies"].([]string); ok {
								depList = append(existing, depList...)
							}
							metadata["dependencies"] = depList
						}
					}
				}
			}
			break
		}
	}

	// Join description lines
	if len(descriptionLines) > 0 {
		metadata["description"] = strings.Join(descriptionLines, " ")
	}

	// If we didn't find the control file, return nil (not empty map)
	// This indicates extraction failed, not that we found nothing
	if len(metadata) == 0 {
		log.Printf("[enricher] No metadata extracted from .deb file (control file not found or empty): %s", pkg.CachePath)
		return nil
	}

	// Ensure we have at least a name (required for matching)
	if _, ok := metadata["name"]; !ok {
		log.Printf("[enricher] Warning: extracted metadata from .deb file but no Package name found: %s", pkg.CachePath)
		// Try to use the filename-derived name as fallback
		if pkg.Name != "" {
			metadata["name"] = pkg.Name
		} else {
			log.Printf("[enricher] No package name available, cannot update index: %s", pkg.CachePath)
			return nil
		}
	}

	// If we don't have a license from the control file, try to extract from copyright file in data tarball
	if _, ok := metadata["license"]; !ok || metadata["license"] == "" {
		if dataData != nil {
			license := e.extractLicenseFromDebData(dataData, dataIsXZ, dataIsZST)
			if license != "" {
				metadata["license"] = license
			}
		}
	}

	// Successfully extracted metadata (no need to log - too verbose)

	return metadata
}

// extractLicenseFromDebData extracts license from copyright files in the data tarball
func (e *PackageEnricher) extractLicenseFromDebData(dataData []byte, isXZ bool, isZST bool) string {
	// Decompress data archive
	// Use same fallback approach as control archive
	var tarData []byte

	// Helper to try decompression with a specific format
	tryDecompress := func(format string, decompressFunc func() ([]byte, error)) bool {
		data, err := decompressFunc()
		if err == nil && data != nil {
			tarData = data
			return true
		}
		return false
	}

	// Determine which formats to try and in what order
	var formatsToTry []struct {
		name string
		fn   func() ([]byte, error)
	}

	if isXZ {
		formatsToTry = []struct {
			name string
			fn   func() ([]byte, error)
		}{
			{"xz", func() ([]byte, error) {
				xzReader, err := xz.NewReader(bytes.NewReader(dataData))
				if err != nil {
					return nil, err
				}
				return io.ReadAll(xzReader)
			}},
			{"gzip", func() ([]byte, error) {
				gzReader, err := gzip.NewReader(bytes.NewReader(dataData))
				if err != nil {
					return nil, err
				}
				defer util.DeferClose(gzReader)
				return io.ReadAll(gzReader)
			}},
			{"zstd", func() ([]byte, error) {
				zstdReader, err := zstd.NewReader(bytes.NewReader(dataData))
				if err != nil {
					return nil, err
				}
				defer zstdReader.Close()
				return io.ReadAll(zstdReader)
			}},
		}
	} else if isZST {
		formatsToTry = []struct {
			name string
			fn   func() ([]byte, error)
		}{
			{"zstd", func() ([]byte, error) {
				zstdReader, err := zstd.NewReader(bytes.NewReader(dataData))
				if err != nil {
					return nil, err
				}
				defer zstdReader.Close()
				return io.ReadAll(zstdReader)
			}},
			{"gzip", func() ([]byte, error) {
				gzReader, err := gzip.NewReader(bytes.NewReader(dataData))
				if err != nil {
					return nil, err
				}
				defer util.DeferClose(gzReader)
				return io.ReadAll(gzReader)
			}},
			{"xz", func() ([]byte, error) {
				xzReader, err := xz.NewReader(bytes.NewReader(dataData))
				if err != nil {
					return nil, err
				}
				return io.ReadAll(xzReader)
			}},
		}
	} else {
		// Default: try gzip first
		formatsToTry = []struct {
			name string
			fn   func() ([]byte, error)
		}{
			{"gzip", func() ([]byte, error) {
				gzReader, err := gzip.NewReader(bytes.NewReader(dataData))
				if err != nil {
					return nil, err
				}
				defer util.DeferClose(gzReader)
				return io.ReadAll(gzReader)
			}},
			{"zstd", func() ([]byte, error) {
				zstdReader, err := zstd.NewReader(bytes.NewReader(dataData))
				if err != nil {
					return nil, err
				}
				defer zstdReader.Close()
				return io.ReadAll(zstdReader)
			}},
			{"xz", func() ([]byte, error) {
				xzReader, err := xz.NewReader(bytes.NewReader(dataData))
				if err != nil {
					return nil, err
				}
				return io.ReadAll(xzReader)
			}},
		}
	}

	// Try each format until one succeeds
	for _, format := range formatsToTry {
		if tryDecompress(format.name, format.fn) {
			// Success!
			break
		}
	}

	// If all decompression attempts failed, return empty (no license found)
	if tarData == nil {
		formatNames := make([]string, len(formatsToTry))
		for i, f := range formatsToTry {
			formatNames[i] = f.name
		}
		log.Printf("[enricher] Failed to decompress data archive (tried formats: %v)", formatNames)
		return ""
	}

	// Parse tar archive to find copyright files
	tarReader := tar.NewReader(bytes.NewReader(tarData))
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("[enricher] Failed to read data tar: %v", err)
			break
		}

		// Look for copyright files in /usr/share/doc/*/copyright
		if strings.Contains(header.Name, "/usr/share/doc/") && strings.HasSuffix(header.Name, "/copyright") {
			content, err := io.ReadAll(tarReader)
			if err != nil {
				log.Printf("[enricher] Failed to read copyright file: %v", err)
				continue
			}

			// Parse copyright file (Debian copyright format)
			license := e.parseDebianCopyrightFile(string(content))
			if license != "" {
				return license
			}
		}
	}

	return ""
}

// parseDebianCopyrightFile parses a Debian copyright file to extract license information
func (e *PackageEnricher) parseDebianCopyrightFile(content string) string {
	lines := strings.Split(content, "\n")
	var license string
	inLicenseSection := false
	var licenseLines []string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			inLicenseSection = false
			continue
		}

		// Look for "License:" field (Debian copyright format)
		if strings.HasPrefix(line, "License:") {
			license = strings.TrimSpace(strings.TrimPrefix(line, "License:"))
			if license != "" {
				return license
			}
			inLicenseSection = true
			continue
		}

		// Some copyright files have a "License" section header
		if strings.HasPrefix(line, "License") && strings.Contains(line, ":") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				license = strings.TrimSpace(parts[1])
				if license != "" {
					return license
				}
			}
			inLicenseSection = true
			continue
		}

		// Collect license lines if we're in a license section
		if inLicenseSection {
			// Skip common headers
			if strings.HasPrefix(line, "Copyright") || strings.HasPrefix(line, "Files:") ||
				strings.HasPrefix(line, "On Debian") || strings.HasPrefix(line, "License:") {
				continue
			}
			licenseLines = append(licenseLines, line)
			// Stop after collecting a reasonable amount
			if len(licenseLines) > 10 {
				break
			}
		}
	}

	// If we collected license lines, try to detect license from content
	if len(licenseLines) > 0 {
		licenseText := strings.Join(licenseLines, " ")
		return e.detectLicenseFromContent(licenseText)
	}

	return ""
}

// updateIndex updates the index database with enriched metadata
func (e *PackageEnricher) updateIndex(pkg PackageToEnrich, metadata map[string]interface{}) {
	if e.indexDB == nil {
		return
	}

	// Check if we have any updates to make
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
		return // Nothing to update
	}

	// We'll need to use a type assertion in the callback
	// For now, let's use a simpler approach: pass a function that works with the concrete type
	// The interface implementation will handle the type assertion
	err := e.indexDB.UpdatePackageMetadata(pkg.Type, pkg.Name, pkg.Version, func(p interface{}) {
		// Use reflection to access and set fields without importing the concrete type
		// This avoids the import cycle with index.IndexPackage
		v := reflect.ValueOf(p)
		if v.Kind() != reflect.Pointer {
			return
		}
		v = v.Elem()
		if v.Kind() != reflect.Struct {
			return
		}

		// Set License field
		if hasLicense {
			if field := v.FieldByName("License"); field.IsValid() && field.CanSet() && field.Kind() == reflect.String {
				field.SetString(metadata["license"].(string))
			}
		}

		// Set Description field if empty
		if hasDescription {
			if field := v.FieldByName("Description"); field.IsValid() && field.CanSet() && field.Kind() == reflect.String {
				if field.String() == "" {
					field.SetString(metadata["description"].(string))
				}
			}
		}

		// Set Dependencies field if empty
		if hasDependencies {
			if field := v.FieldByName("Dependencies"); field.IsValid() && field.CanSet() && field.Kind() == reflect.Slice {
				if field.Len() == 0 {
					deps := metadata["dependencies"].([]string)
					field.Set(reflect.ValueOf(deps))
				}
			}
		}
	})
	if err != nil {
		log.Printf("[enricher] ERROR: Failed to update index for %s@%s: %v", pkg.Name, pkg.Version, err)
	}
	// Successfully enriched (no need to log - too verbose)
}

// ScanAndEnqueue scans the cache directory and enqueues packages that need enrichment
func (e *PackageEnricher) ScanAndEnqueue() {
	log.Printf("[enricher] Scanning cache for packages to enrich...")

	typeDirs := []Type{TypeAPK, TypeAPT, TypeNPM, TypePyPI, TypeGo}
	for _, pmType := range typeDirs {
		typeDir := filepath.Join(e.cacheDir, "packages", string(pmType))
		if _, err := os.Stat(typeDir); os.IsNotExist(err) {
			continue
		}

		// Walk the directory
		if err := filepath.Walk(typeDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}

			// Skip metadata files
			if strings.HasSuffix(path, ".meta") {
				return nil
			}

			// Load metadata to get package info
			metaPath := path + ".meta"
			metaData, err := os.ReadFile(metaPath)
			if err != nil {
				return nil
			}

			var metadata Metadata
			if err := json.Unmarshal(metaData, &metadata); err != nil {
				return nil
			}

			// Skip index files
			if metadata.IsIndex {
				return nil
			}

			// Enqueue if we have name and version
			if metadata.Name != "" && metadata.Version != "" {
				e.Enqueue(PackageToEnrich{
					Type:         pmType,
					CachePath:    path,
					Name:         metadata.Name,
					Version:      metadata.Version,
					Architecture: metadata.Architecture,
				})
			}

			return nil
		}); err != nil {
			log.Printf("Error walking directory: %v", err)
		}
	}

	log.Printf("[enricher] Scan complete, packages queued for enrichment")
}
