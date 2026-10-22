package api

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/packages"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// handleNPMTarballMetadata extracts package.json from a cached NPM tarball
// Path format: /api/v1/packages/npm/{name}/{version}/metadata
// Example: /api/v1/packages/npm/@adobe/css-tools/4.4.4/metadata
func (a *API) handleNPMTarballMetadata(w http.ResponseWriter, r *http.Request) {
	if !validateMethodGET(w, r) {
		return
	}

	// Extract package name and version from path
	packageName, version, err := parseNPMScopedPackage(r.URL.Path, "/api/v1/packages/npm/")
	if err != nil {
		http.Error(w, fmt.Sprintf("Invalid path format: %v", err), http.StatusBadRequest)
		return
	}

	// Find the cached tarball
	log.Printf("[npm-metadata] Looking for package: %s@%s", packageName, version)
	tarballPath, err := a.findNPMTarball(packageName, version)
	if err != nil {
		log.Printf("[npm-metadata] Failed to find package %s@%s: %v", packageName, version, err)
		http.Error(w, fmt.Sprintf("Package not found in cache: %v", err), http.StatusNotFound)
		return
	}
	log.Printf("[npm-metadata] Found tarball for %s@%s: %s", packageName, version, tarballPath)

	// Extract package.json from the tarball
	packageJSON, err := extractPackageJSONFromTarball(tarballPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to extract package.json: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, packageJSON)
}

// handlePyPIMetadata fetches package metadata from PyPI's JSON API
// Path format: /api/v1/packages/pypi/{name}/metadata or /api/v1/packages/pypi/{name}/{version}/metadata
func (a *API) handlePyPIMetadata(w http.ResponseWriter, r *http.Request) {
	if !validateMethodGET(w, r) {
		return
	}

	// Extract package name and optional version from path
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/packages/pypi/")
	path = strings.TrimSuffix(path, "/metadata")

	// URL decode the path
	decodedPath, err := url.PathUnescape(path)
	if err != nil {
		decodedPath = path
	}

	parts := strings.Split(decodedPath, "/")
	if len(parts) < 1 {
		http.Error(w, "Invalid path format. Expected: /api/v1/packages/pypi/{name}/metadata", http.StatusBadRequest)
		return
	}

	packageName := parts[0]
	var version string
	if len(parts) >= 2 {
		version = parts[1]
	}

	// Build PyPI JSON API URL
	var pypiURL string
	if version != "" {
		pypiURL = fmt.Sprintf("https://pypi.org/pypi/%s/%s/json", packageName, version)
	} else {
		pypiURL = fmt.Sprintf("https://pypi.org/pypi/%s/json", packageName)
	}

	log.Printf("[pypi-metadata] Fetching metadata for %s@%s from %s", packageName, version, pypiURL)

	// Fetch from PyPI JSON API
	resp, err := http.Get(pypiURL)
	if err != nil {
		log.Printf("[pypi-metadata] Failed to fetch from PyPI: %v", err)
		http.Error(w, fmt.Sprintf("Failed to fetch from PyPI: %v", err), http.StatusBadGateway)
		return
	}
	defer util.DeferClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		log.Printf("[pypi-metadata] PyPI returned %d for %s", resp.StatusCode, pypiURL)
		http.Error(w, fmt.Sprintf("PyPI returned %d", resp.StatusCode), resp.StatusCode)
		return
	}

	// Parse JSON response
	var pypiData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&pypiData); err != nil {
		log.Printf("[pypi-metadata] Failed to parse PyPI JSON: %v", err)
		http.Error(w, fmt.Sprintf("Failed to parse PyPI response: %v", err), http.StatusInternalServerError)
		return
	}

	// Extract and format metadata similar to NPM structure
	metadata := make(map[string]interface{})

	// Extract info object (contains most metadata)
	if info, ok := pypiData["info"].(map[string]interface{}); ok {
		// Basic info
		if name, ok := info["name"].(string); ok {
			metadata["name"] = name
		}
		if version, ok := info["version"].(string); ok {
			metadata["version"] = version
		}
		if summary, ok := info["summary"].(string); ok {
			metadata["description"] = summary
		}
		if author, ok := info["author"].(string); ok {
			metadata["author"] = author
		}
		if authorEmail, ok := info["author_email"].(string); ok {
			if author, ok := metadata["author"].(string); ok && author != "" {
				metadata["author"] = fmt.Sprintf("%s <%s>", author, authorEmail)
			} else if authorEmail != "" {
				metadata["author"] = authorEmail
			}
		}
		if license, ok := info["license"].(string); ok {
			metadata["license"] = license
		}
		if homePage, ok := info["home_page"].(string); ok {
			metadata["homepage"] = homePage
		}
		if projectURL, ok := info["project_url"].(string); ok {
			metadata["homepage"] = projectURL
		}
		if projectURLs, ok := info["project_urls"].(map[string]interface{}); ok {
			if homepage, ok := projectURLs["Homepage"].(string); ok {
				metadata["homepage"] = homepage
			}
			if repo, ok := projectURLs["Repository"].(string); ok {
				metadata["repository"] = repo
			}
		}
		if keywords, ok := info["keywords"].(string); ok && keywords != "" {
			// Keywords are comma-separated
			keywordList := strings.Split(keywords, ",")
			for i, k := range keywordList {
				keywordList[i] = strings.TrimSpace(k)
			}
			metadata["keywords"] = keywordList
		}
		if requiresDist, ok := info["requires_dist"].([]interface{}); ok {
			// Extract dependency names from requirement strings
			// Format: "package>=1.0.0" or "package[extra]>=1.0.0"
			var deps []string
			for _, req := range requiresDist {
				if reqStr, ok := req.(string); ok {
					// Extract package name (everything before >=, ==, !=, <, >, or [
					parts := strings.FieldsFunc(reqStr, func(r rune) bool {
						return r == '>' || r == '<' || r == '=' || r == '!' || r == '[' || r == ';'
					})
					if len(parts) > 0 {
						deps = append(deps, strings.TrimSpace(parts[0]))
					}
				}
			}
			metadata["dependencies"] = deps
		}
	}

	log.Printf("[pypi-metadata] Successfully fetched metadata for %s@%s", packageName, version)
	writeJSON(w, metadata)
}

// handleGoMetadata reads metadata from a cached Go module .info file
// Path format: /api/v1/packages/go/{module}/{version}/metadata
// Example: /api/v1/packages/go/github.com/gin-gonic/gin/v1.9.1/metadata
func (a *API) handleGoMetadata(w http.ResponseWriter, r *http.Request) {
	if !validateMethodGET(w, r) {
		return
	}

	// Extract module name and version from path
	modulePath, version, err := parseGoModulePath(r.URL.Path, "/api/v1/packages/go/")
	if err != nil {
		http.Error(w, fmt.Sprintf("Invalid path format: %v", err), http.StatusBadRequest)
		return
	}

	// Build the .info file path: /{module}/@v/{version}.info
	infoPath := fmt.Sprintf("/%s/@v/%s.info", modulePath, version)

	log.Printf("[go-metadata] Looking for .info file: %s", infoPath)

	// Find the cached .info file
	typeDir := filepath.Join(a.cacheDir, "packages", "go")
	if _, err := os.Stat(typeDir); os.IsNotExist(err) {
		http.Error(w, "Go cache directory not found", http.StatusNotFound)
		return
	}

	// The .info file is cached by its URL path hash
	hash := sha256.Sum256([]byte(infoPath))
	infoPathHash := hex.EncodeToString(hash[:])
	cachePath := filepath.Join(typeDir, infoPathHash[0:2], infoPathHash[2:4], infoPathHash)

	// Prefer .zip files over .info files for metadata extraction
	// .zip files contain go.mod with better metadata (GoVersion, etc.)
	// Build the .zip file path: /{module}/@v/{version}.zip
	zipPath := fmt.Sprintf("/%s/@v/%s.zip", modulePath, version)
	zipHash := sha256.Sum256([]byte(zipPath))
	zipPathHash := hex.EncodeToString(zipHash[:])
	zipCachePath := filepath.Join(typeDir, zipPathHash[0:2], zipPathHash[2:4], zipPathHash)

	if _, err := os.Stat(zipCachePath); err == nil {
		// Found .zip file, extract metadata from it (preferred source)
		log.Printf("[go-metadata] Found .zip file, extracting metadata from: %s", zipCachePath)
		metadata, err := a.extractGoMetadataFromZip(zipCachePath, modulePath, version)
		if err != nil {
			log.Printf("[go-metadata] Failed to extract metadata from zip: %v", err)
			// Fall through to .info file if zip extraction fails
		} else {
			writeJSON(w, metadata)
			return
		}
	}

	// Fallback: try .info file if .zip file wasn't available or extraction failed
	if _, err := os.Stat(cachePath); os.IsNotExist(err) {
		// Try to find the .info file by searching metadata files
		log.Printf("[go-metadata] .info file not found at expected path: %s, searching metadata files...", cachePath)

		// First try to find by URLPath (most direct match)
		foundPath, err := findPackageByURLPath(typeDir, infoPath)
		if err == nil {
			cachePath = foundPath
		} else {
			// Fallback: try to find by Name+Version with .info extension filter
			foundPath, err := findPackageByMetadataWithFilter(typeDir, modulePath, version, func(metadata map[string]interface{}) bool {
				urlPath, _ := metadata["url_path"].(string)
				return strings.HasSuffix(urlPath, ".info")
			})
			if err == nil {
				cachePath = foundPath
			} else {
				// Neither .zip nor .info file found
				log.Printf("[go-metadata] Neither .zip nor .info file found in cache for %s@%s", modulePath, version)
				http.Error(w, fmt.Sprintf("Go module files not found in cache. The module %s@%s may not have been cached yet. Try fetching the module first.", modulePath, version), http.StatusNotFound)
				return
			}
		}
	}

	// Read and parse the .info JSON file
	data, err := os.ReadFile(cachePath)
	if err != nil {
		log.Printf("[go-metadata] Failed to read .info file: %v", err)
		http.Error(w, fmt.Sprintf("Failed to read .info file: %v", err), http.StatusInternalServerError)
		return
	}

	var infoData map[string]interface{}
	if err := json.Unmarshal(data, &infoData); err != nil {
		log.Printf("[go-metadata] Failed to parse .info JSON: %v", err)
		http.Error(w, fmt.Sprintf("Failed to parse .info file: %v", err), http.StatusInternalServerError)
		return
	}

	// Format metadata similar to other package types
	// Note: .info files have limited metadata - prefer .zip files for better info
	metadata := make(map[string]interface{})
	if version, ok := infoData["Version"].(string); ok {
		metadata["version"] = version
	}

	// Validate Time field - .info files often have zero/missing Time values
	// Only use Time if it's a valid date (not zero, and after 2000-01-01)
	if timeVal, ok := infoData["Time"]; ok {
		var timeStr string
		switch v := timeVal.(type) {
		case string:
			timeStr = v
		case time.Time:
			// If it's already a time.Time, check if it's valid
			if !v.IsZero() && v.After(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
				metadata["published"] = v.Format(time.RFC3339)
			}
		}

		// If it's a string, try to parse and validate
		if timeStr != "" {
			if t, err := time.Parse(time.RFC3339, timeStr); err == nil {
				if !t.IsZero() && t.After(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
					metadata["published"] = timeStr
				}
			}
		}
	}

	// GoVersion doesn't exist in .info files - it's in go.mod inside .zip files
	// Don't try to extract it from .info files

	// Try to fetch description from pkg.go.dev
	if description := a.fetchGoPackageDescription(modulePath); description != "" {
		metadata["description"] = description
	}

	// Find sub-packages (directories) for this module
	// For example, for github.com/bytedance/sonic, find packages whose name starts with github.com/bytedance/sonic/
	if a.indexDB != nil {
		indexPkgs, err := a.indexDB.GetPackages(packages.TypeGo)
		if err == nil {
			// Find all packages whose name is a sub-path of this module
			// e.g., "github.com/bytedance/sonic/loader" is a sub-package of "github.com/bytedance/sonic"
			subPackages := make(map[string]bool)
			modulePrefix := modulePath + "/"
			for _, pkg := range indexPkgs {
				if strings.HasPrefix(pkg.Name, modulePrefix) {
					// Extract the sub-package name (directory) from the package name
					// e.g., "github.com/bytedance/sonic/loader" -> "loader"
					subPath := strings.TrimPrefix(pkg.Name, modulePrefix)
					// Get the first directory name (e.g., "loader" from "loader/subdir")
					subPathParts := strings.Split(subPath, "/")
					if len(subPathParts) > 0 && subPathParts[0] != "" {
						subPackages[subPathParts[0]] = true
					}
				}
			}

			// Convert to sorted slice
			if len(subPackages) > 0 {
				subPkgList := make([]string, 0, len(subPackages))
				for subPkg := range subPackages {
					subPkgList = append(subPkgList, subPkg)
				}
				sort.Strings(subPkgList)
				metadata["directories"] = subPkgList
			}
		}
	}

	log.Printf("[go-metadata] Successfully loaded metadata for %s@%s", modulePath, version)
	writeJSON(w, metadata)
}

// handleAPKMetadata extracts metadata from a cached APK file using apk command
// Path format: /api/v1/packages/apk/{name}/{version}/metadata
// Example: /api/v1/packages/apk/alpine-baselayout/3.6.0-r0/metadata
func (a *API) handleAPKMetadata(w http.ResponseWriter, r *http.Request) {
	if !validateMethodGET(w, r) {
		return
	}

	// Extract package name and version from path
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/packages/apk/")
	path = strings.TrimSuffix(path, "/metadata")

	// URL decode the path
	decodedPath, err := url.PathUnescape(path)
	if err != nil {
		decodedPath = path
	}

	parts := strings.Split(decodedPath, "/")
	if len(parts) < 2 {
		http.Error(w, "Invalid path format. Expected: /api/v1/packages/apk/{name}/{version}/metadata", http.StatusBadRequest)
		return
	}

	packageName := parts[0]
	version := parts[1]

	log.Printf("[apk-metadata] Looking for package: %s@%s", packageName, version)

	// Find the cached .apk file
	typeDir := filepath.Join(a.cacheDir, "packages", "apk")
	if _, err := os.Stat(typeDir); os.IsNotExist(err) {
		http.Error(w, "APK cache directory not found", http.StatusNotFound)
		return
	}

	// Use findPackageByMetadata helper
	foundAPKPath, err := findPackageByMetadata(typeDir, packageName, version)
	if err != nil {
		log.Printf("[apk-metadata] APK file not found in cache: %s@%s: %v", packageName, version, err)
		http.Error(w, fmt.Sprintf("APK file not found in cache: %s@%s", packageName, version), http.StatusNotFound)
		return
	}

	log.Printf("[apk-metadata] Found APK file: %s", foundAPKPath)

	// Try to use apk command first, but fall back to direct file parsing if it fails
	var output []byte

	// Try apk info command
	cmd := exec.Command("apk", "info", "-a", foundAPKPath)
	output, cmdErr := cmd.Output()
	if cmdErr != nil {
		// apk command failed, try to extract metadata directly from the APK file
		// APK files are gzipped tar archives with a control.tar.gz inside
		log.Printf("[apk-metadata] apk command failed (%v), trying direct file extraction", cmdErr)
		var extractErr error
		output, extractErr = a.extractAPKMetadataDirectly(foundAPKPath)
		if extractErr != nil {
			log.Printf("[apk-metadata] Failed to extract APK metadata: %v", extractErr)
			http.Error(w, fmt.Sprintf("Failed to extract APK metadata: %v", extractErr), http.StatusInternalServerError)
			return
		}
	}

	// Parse apk info output
	// Format is key-value pairs, e.g.:
	// package: alpine-baselayout
	// version: 3.6.0-r0
	// description: Alpine base layout
	// depends: musl
	metadata := make(map[string]interface{})
	lines := strings.Split(string(output), "\n")
	var currentKey string
	var currentValue strings.Builder

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Check if this is a key line (ends with :)
		if strings.HasSuffix(line, ":") {
			// Save previous key-value pair
			if currentKey != "" {
				value := strings.TrimSpace(currentValue.String())
				if value != "" {
					metadata[currentKey] = value
				}
			}
			// Start new key
			currentKey = strings.TrimSuffix(line, ":")
			currentValue.Reset()
		} else if currentKey != "" {
			// This is a continuation of the value
			if currentValue.Len() > 0 {
				currentValue.WriteString(" ")
			}
			currentValue.WriteString(line)
		}
	}

	// Save last key-value pair
	if currentKey != "" {
		value := strings.TrimSpace(currentValue.String())
		if value != "" {
			metadata[currentKey] = value
		}
	}

	// Format metadata similar to other package types
	formatted := make(map[string]interface{})
	if name, ok := metadata["package"].(string); ok {
		formatted["name"] = name
	}
	if version, ok := metadata["version"].(string); ok {
		formatted["version"] = version
	}
	if desc, ok := metadata["description"].(string); ok {
		formatted["description"] = desc
	}
	if deps, ok := metadata["depends"].(string); ok {
		// Dependencies are space-separated
		depsList := strings.Fields(deps)
		formatted["dependencies"] = depsList
	}
	if arch, ok := metadata["arch"].(string); ok {
		formatted["architecture"] = arch
	}
	if size, ok := metadata["size"].(string); ok {
		formatted["size"] = size
	}
	if license, ok := metadata["license"].(string); ok {
		// License might be space-separated if multiple licenses
		// Clean it up: split by space and join with ", " for readability
		licenseParts := strings.Fields(license)
		if len(licenseParts) > 1 {
			formatted["license"] = strings.Join(licenseParts, ", ")
		} else {
			formatted["license"] = license
		}
	}
	if url, ok := metadata["url"].(string); ok {
		formatted["homepage"] = url
	}

	// Log extracted metadata for debugging
	log.Printf("[apk-metadata] Extracted metadata for %s@%s: name=%v, version=%v, description=%v, license=%v, arch=%v",
		packageName, version, formatted["name"], formatted["version"],
		formatted["description"], formatted["license"], formatted["architecture"])

	log.Printf("[apk-metadata] Successfully extracted metadata for %s@%s", packageName, version)
	writeJSON(w, formatted)
}

// findNPMTarball finds a cached NPM tarball by package name and version
// This is a wrapper around findPackageByMetadata with NPM-specific filtering (must be .tgz)
func (a *API) findNPMTarball(packageName, version string) (string, error) {
	typeDir := filepath.Join(a.cacheDir, "packages", "npm")
	if _, err := os.Stat(typeDir); os.IsNotExist(err) {
		return "", fmt.Errorf("NPM cache directory not found")
	}

	// Use the generic finder
	foundPath, err := findPackageByMetadata(typeDir, packageName, version)
	if err != nil {
		return "", err
	}

	// NPM-specific: verify this is actually a tarball (not an index file) by checking URL path
	// The generic finder already checks !IsIndex, but we also need to ensure it's a .tgz file
	metaPath := foundPath + ".meta"
	metaData, err := os.ReadFile(metaPath)
	if err == nil {
		var metadata struct {
			URLPath string `json:"url_path"`
			IsIndex bool   `json:"is_index"`
		}
		if err := json.Unmarshal(metaData, &metadata); err == nil {
			// Double-check: skip if it's an index file or doesn't look like a tarball
			if metadata.IsIndex || !strings.HasSuffix(metadata.URLPath, ".tgz") {
				return "", fmt.Errorf("package %s@%s found but is not a tarball (URLPath: %s, IsIndex: %v)", packageName, version, metadata.URLPath, metadata.IsIndex)
			}
		}
	}

	log.Printf("[npm-metadata] Found match: %s@%s at %s", packageName, version, foundPath)
	return foundPath, nil
}

// extractPackageJSONFromTarball extracts package.json from an NPM tarball
func extractPackageJSONFromTarball(tarballPath string) (map[string]interface{}, error) {
	// Open the tarball file
	file, err := os.Open(tarballPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open tarball: %w", err)
	}
	defer util.DeferClose(file)

	// Create a gzip reader
	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer util.DeferClose(gzReader)

	// Create a tar reader
	tarReader := tar.NewReader(gzReader)

	// Look for package/package.json in the tarball
	// NPM tarballs have the structure: package/package.json
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Check if this is package.json
		// NPM tarballs typically have: package/package.json (root)
		// Dependencies are in: package/node_modules/.../package.json
		// We ONLY want the root package.json, not dependency package.json files
		if header.Typeflag == tar.TypeReg {
			name := header.Name
			// Only match the exact root package.json path
			if name == "package/package.json" {
				// Read the package.json content
				data, err := io.ReadAll(tarReader)
				if err != nil {
					return nil, fmt.Errorf("failed to read package.json: %w", err)
				}

				// Parse JSON
				var packageJSON map[string]interface{}
				if err := json.Unmarshal(data, &packageJSON); err != nil {
					return nil, fmt.Errorf("failed to parse package.json: %w", err)
				}

				return packageJSON, nil
			}
		}
	}

	return nil, fmt.Errorf("package.json not found in tarball")
}

// extractGoMetadataFromZip extracts metadata from a Go module zip file
// Go module zip files contain go.mod which has module info and Go version requirements
func (a *API) extractGoMetadataFromZip(zipPath string, modulePath string, version string) (map[string]interface{}, error) {
	// Open the zip file
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open zip file: %w", err)
	}
	defer util.DeferClose(r)

	// Look for go.mod file in the zip
	// Go module zip files have paths like: {module}/go.mod
	// e.g., github.com/user/repo/go.mod
	var goModPath string
	for _, f := range r.File {
		// Check if this is a go.mod file
		if strings.HasSuffix(f.Name, "/go.mod") || f.Name == "go.mod" {
			goModPath = f.Name
			break
		}
	}

	if goModPath == "" {
		return nil, fmt.Errorf("go.mod not found in zip file")
	}

	// Read go.mod file
	goModFile, err := r.Open(goModPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open go.mod in zip: %w", err)
	}
	defer util.DeferClose(goModFile)

	goModContent, err := io.ReadAll(goModFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read go.mod: %w", err)
	}

	// Parse go.mod to extract metadata
	metadata := make(map[string]interface{})
	metadata["version"] = version

	// Parse go.mod content
	// go.mod format:
	// module {modulePath}
	// go {goVersion}
	// require (...)
	goModText := string(goModContent)
	lines := strings.Split(goModText, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Extract Go version requirement
		if strings.HasPrefix(line, "go ") {
			goVersion := strings.TrimSpace(strings.TrimPrefix(line, "go "))
			metadata["goVersion"] = goVersion
		}

		// Extract module path (should match what we already have, but verify)
		if strings.HasPrefix(line, "module ") {
			modPath := strings.TrimSpace(strings.TrimPrefix(line, "module "))
			// Remove comments if any
			if idx := strings.Index(modPath, "//"); idx != -1 {
				modPath = strings.TrimSpace(modPath[:idx])
			}
			metadata["module"] = modPath
		}
	}

	// Try to get file modification time from zip (as published date)
	if len(r.File) > 0 {
		// Use the first file's modification time as an approximation
		firstFile := r.File[0]
		if !firstFile.ModTime().IsZero() {
			metadata["published"] = firstFile.ModTime().Format(time.RFC3339)
		}
	}

	// Try to fetch description from pkg.go.dev
	if description := a.fetchGoPackageDescription(modulePath); description != "" {
		metadata["description"] = description
	}

	log.Printf("[go-metadata] Extracted metadata from zip for %s@%s: goVersion=%v", modulePath, version, metadata["goVersion"])
	return metadata, nil
}

// fetchGoPackageDescription fetches the package description from pkg.go.dev
// Returns the first paragraph from the README section, or empty string if not found
func (a *API) fetchGoPackageDescription(modulePath string) string {
	// Build pkg.go.dev URL: https://pkg.go.dev/{modulePath}
	pkgURL := fmt.Sprintf("https://pkg.go.dev/%s", modulePath)

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	req, err := http.NewRequest("GET", pkgURL, nil)
	if err != nil {
		log.Printf("[go-metadata] Failed to create request for %s: %v", pkgURL, err)
		return ""
	}

	// Set User-Agent to avoid being blocked
	req.Header.Set("User-Agent", "roxxy/1.0")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[go-metadata] Failed to fetch %s: %v", pkgURL, err)
		return ""
	}
	defer util.DeferClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		log.Printf("[go-metadata] pkg.go.dev returned %d for %s", resp.StatusCode, pkgURL)
		return ""
	}

	// Read HTML content
	html, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[go-metadata] Failed to read response from pkg.go.dev: %v", err)
		return ""
	}

	// Parse HTML to extract first paragraph from README section
	// The README section typically has a structure like:
	// <div class="Documentation-readmeSection"> or similar
	// We'll look for the first <p> tag after "README" heading
	htmlStr := string(html)

	// Try to find README section - look for common patterns
	// Pattern 1: Look for <h2>README</h2> or similar, then get first <p>
	readmePattern := regexp.MustCompile(`(?i)<h[1-6][^>]*>.*?README.*?</h[1-6]>.*?<p[^>]*>([^<]+)</p>`)
	matches := readmePattern.FindStringSubmatch(htmlStr)
	if len(matches) > 1 {
		description := strings.TrimSpace(matches[1])
		// Clean up HTML entities and extra whitespace
		description = strings.ReplaceAll(description, "&nbsp;", " ")
		description = strings.ReplaceAll(description, "&amp;", "&")
		description = strings.ReplaceAll(description, "&lt;", "<")
		description = strings.ReplaceAll(description, "&gt;", ">")
		description = strings.ReplaceAll(description, "&quot;", "\"")
		// Collapse multiple spaces
		description = regexp.MustCompile(`\s+`).ReplaceAllString(description, " ")
		if len(description) > 0 {
			log.Printf("[go-metadata] Extracted description from pkg.go.dev for %s: %s", modulePath, description[:min(100, len(description))])
			return description
		}
	}

	// Pattern 2: Look for first paragraph in main content area
	// Try to find first meaningful paragraph (not navigation, not empty)
	paragraphPattern := regexp.MustCompile(`<p[^>]*>([^<]{20,500})</p>`)
	paragraphMatches := paragraphPattern.FindAllStringSubmatch(htmlStr, 5)
	for _, match := range paragraphMatches {
		if len(match) > 1 {
			text := strings.TrimSpace(match[1])
			// Skip if it looks like navigation or metadata
			if strings.Contains(strings.ToLower(text), "import") && strings.Contains(strings.ToLower(text), "package") {
				continue
			}
			// Skip if it's too short or looks like a link
			if len(text) < 30 || strings.HasPrefix(text, "http") {
				continue
			}
			// Clean up HTML entities
			text = strings.ReplaceAll(text, "&nbsp;", " ")
			text = strings.ReplaceAll(text, "&amp;", "&")
			text = strings.ReplaceAll(text, "&lt;", "<")
			text = strings.ReplaceAll(text, "&gt;", ">")
			text = strings.ReplaceAll(text, "&quot;", "\"")
			text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")
			if len(text) > 0 {
				log.Printf("[go-metadata] Extracted description from pkg.go.dev for %s: %s", modulePath, text[:min(100, len(text))])
				return text
			}
		}
	}

	log.Printf("[go-metadata] Could not extract description from pkg.go.dev for %s", modulePath)
	return ""
}

// extractAPKMetadataDirectly extracts metadata from an APK file without using the apk command
// APK files are gzipped tar archives containing:
// - .PKGINFO (package metadata)
// - control.tar.gz (additional control files)
func (a *API) extractAPKMetadataDirectly(apkPath string) ([]byte, error) {
	file, err := os.Open(apkPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open APK file: %v", err)
	}
	defer util.DeferClose(file)

	// APK files are gzipped tar archives
	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %v", err)
	}
	defer util.DeferClose(gzReader)

	tarReader := tar.NewReader(gzReader)
	metadata := make(map[string]string)

	// Look for .PKGINFO file which contains package metadata
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar: %v", err)
		}

		if header.Name == ".PKGINFO" {
			// Read the .PKGINFO file
			data, err := io.ReadAll(tarReader)
			if err != nil {
				return nil, fmt.Errorf("failed to read .PKGINFO: %v", err)
			}

			// Parse .PKGINFO format (key = value lines)
			// Note: Some fields can appear multiple times (like license, depend)
			// We need to collect all values for these fields
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

					// Handle multi-value fields (license, depend can appear multiple times)
					if key == "license" || key == "depend" {
						if existing, ok := metadata[key]; ok {
							// Append to existing value (space-separated)
							metadata[key] = existing + " " + value
						} else {
							metadata[key] = value
						}
					} else {
						// Single-value fields: use last value if multiple (shouldn't happen, but be safe)
						metadata[key] = value
					}
				}
			}
			break
		}
	}

	// Format output similar to apk info -a
	var output strings.Builder
	if pkg, ok := metadata["pkgname"]; ok {
		fmt.Fprintf(&output, "package:\n%s\n", pkg)
	}
	if ver, ok := metadata["pkgver"]; ok {
		fmt.Fprintf(&output, "version:\n%s\n", ver)
	}
	if desc, ok := metadata["pkgdesc"]; ok {
		fmt.Fprintf(&output, "description:\n%s\n", desc)
	}
	if arch, ok := metadata["arch"]; ok {
		fmt.Fprintf(&output, "arch:\n%s\n", arch)
	}
	if size, ok := metadata["size"]; ok {
		fmt.Fprintf(&output, "size:\n%s\n", size)
	}
	if license, ok := metadata["license"]; ok {
		fmt.Fprintf(&output, "license:\n%s\n", license)
	}
	if url, ok := metadata["url"]; ok {
		fmt.Fprintf(&output, "url:\n%s\n", url)
	}
	if deps, ok := metadata["depend"]; ok {
		fmt.Fprintf(&output, "depends:\n%s\n", deps)
	}

	return []byte(output.String()), nil
}
