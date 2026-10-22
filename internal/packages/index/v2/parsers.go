package v2

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// APTParserV2 parses APT Packages files in v2 format
type APTParserV2 struct{}

func (p *APTParserV2) CanParse(indexPath string) bool {
	if strings.HasSuffix(indexPath, "Packages.gz") ||
		strings.HasSuffix(indexPath, "Packages.xz") ||
		strings.HasSuffix(indexPath, "Packages") {
		return true
	}
	if strings.Contains(indexPath, "/binary-") && strings.Contains(indexPath, "/by-hash/") {
		return true
	}
	if strings.Contains(indexPath, "/binary-") && (strings.Contains(indexPath, "Packages") || strings.Contains(indexPath, "by-hash")) {
		return true
	}
	return false
}

func (p *APTParserV2) Parse(data []byte, indexURL string, contentType string, cachePath string) ([]IndexPackageV2, error) {
	log.Printf("[apt-parser-v2] Parsing index file: %s", indexURL)

	// Decompress if needed - try multiple methods based on extension, content type, and magic bytes
	var err error
	decompressed := false

	// Check by extension first
	if strings.HasSuffix(indexURL, ".xz") {
		data, err = readXZ(data)
		if err == nil {
			decompressed = true
		}
	} else if strings.HasSuffix(indexURL, ".bz2") {
		data, err = readBzip2(data)
		if err == nil {
			decompressed = true
		}
	} else if strings.HasSuffix(indexURL, ".gz") {
		data, err = readGzip(data)
		if err == nil {
			decompressed = true
		}
	}

	// If not decompressed yet, try content type
	if !decompressed {
		if strings.Contains(contentType, "application/x-xz") || strings.Contains(contentType, "application/xz") {
			data, err = readXZ(data)
			if err == nil {
				decompressed = true
			}
		} else if strings.Contains(contentType, "application/x-bzip2") || strings.Contains(contentType, "application/bzip2") {
			data, err = readBzip2(data)
			if err == nil {
				decompressed = true
			}
		} else if strings.Contains(contentType, "application/gzip") || strings.Contains(contentType, "application/x-gzip") {
			data, err = readGzip(data)
			if err == nil {
				decompressed = true
			}
		}
	}

	// If still not decompressed, try magic bytes (detect format)
	if !decompressed && len(data) >= 3 {
		// Check magic bytes
		if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
			// Gzip magic: 1f 8b
			data, err = readGzip(data)
			if err == nil {
				decompressed = true
				log.Printf("[apt-parser-v2] Detected gzip format from magic bytes")
			}
		} else if len(data) >= 3 && data[0] == 'B' && data[1] == 'Z' && data[2] == 'h' {
			// Bzip2 magic: BZh
			data, err = readBzip2(data)
			if err == nil {
				decompressed = true
				log.Printf("[apt-parser-v2] Detected bzip2 format from magic bytes")
			}
		} else if len(data) >= 6 && data[0] == 0xfd && data[1] == '7' && data[2] == 'z' && data[3] == 'X' && data[4] == 'Z' && data[5] == 0x00 {
			// XZ magic: fd 37 7a 58 5a 00
			data, err = readXZ(data)
			if err == nil {
				decompressed = true
				log.Printf("[apt-parser-v2] Detected xz format from magic bytes")
			}
		}
	}

	// Log if we couldn't decompress (might be uncompressed)
	if !decompressed && len(data) > 0 {
		log.Printf("[apt-parser-v2] No decompression applied (file may be uncompressed or unknown format)")
	}

	// Debug: log data size and first few bytes
	if len(data) > 0 {
		previewLen := 200
		if len(data) < previewLen {
			previewLen = len(data)
		}
		preview := string(data[:previewLen])
		log.Printf("[apt-parser-v2] Data size: %d bytes, preview: %q", len(data), preview)
	} else {
		log.Printf("[apt-parser-v2] WARNING: Data is empty after decompression!")
		return []IndexPackageV2{}, nil
	}

	// Group packages by name
	packageMap := make(map[string]*IndexPackageV2)
	entries := strings.Split(string(data), "\n\n")
	log.Printf("[apt-parser-v2] Split into %d entries (by \\n\\n)", len(entries))

	for _, entry := range entries {
		if strings.TrimSpace(entry) == "" {
			continue
		}

		var pkgName, version, arch, filename, url string
		var size int64
		var deps []string
		var license string

		lines := strings.Split(entry, "\n")
		var descriptionLines []string
		inDescription := false

		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				inDescription = false
				continue
			}

			if inDescription && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
				descriptionLines = append(descriptionLines, strings.TrimSpace(line))
				continue
			}

			inDescription = false
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				continue
			}

			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])

			switch key {
			case "Package":
				pkgName = value
			case "Version":
				version = value
			case "Architecture":
				arch = value
			case "Filename":
				filename = value
				baseURL := strings.TrimSuffix(indexURL, "/Packages.gz")
				baseURL = strings.TrimSuffix(baseURL, "/Packages.xz")
				baseURL = strings.TrimSuffix(baseURL, "/Packages")
				url = baseURL + "/" + filename
			case "Size":
				if sizeVal, err := strconv.ParseInt(value, 10, 64); err == nil {
					size = sizeVal
				}
			case "Description":
				_ = value // Description value stored in descriptionLines
				inDescription = true
			case "Depends":
				deps = strings.Split(value, ",")
				for i := range deps {
					deps[i] = strings.TrimSpace(strings.Split(deps[i], " ")[0])
				}
			case "License":
				license = value
			}
		}

		if pkgName == "" || version == "" {
			continue
		}

		// Extract group from indexURL (basePath/distribution/component)
		// Example: http://security.debian.org/debian-security/dists/bookworm-security/main/binary-amd64/Packages.gz
		// Group: debian-security/bookworm-security/main
		group := extractAPTGroupFromURL(indexURL)

		// Create or update package entry
		if packageMap[pkgName] == nil {
			packageMap[pkgName] = &IndexPackageV2{
				Type:         "apt",
				Name:         pkgName,
				Versions:     []PackageVersion{},
				Description:  strings.Join(descriptionLines, " "),
				Dependencies: deps,
				Group:        group,
				IndexedAt:    time.Now(),
			}
		} else if packageMap[pkgName].Group == "" && group != "" {
			// Update group if not set
			packageMap[pkgName].Group = group
		}

		// Add version with license at version level
		versionEntry := PackageVersion{
			Version:      version,
			Architecture: arch,
			CachePath:    url,
			URL:          url,
			Size:         size,
			License:      license, // License is version-specific
			Cached:       false,
			IndexedAt:    time.Now(),
		}
		packageMap[pkgName].Versions = append(packageMap[pkgName].Versions, versionEntry)
	}

	// Convert to slice and sort versions
	var packages []IndexPackageV2
	for _, pkg := range packageMap {
		// Sort versions by semantic version (newest first)
		sort.Slice(pkg.Versions, func(i, j int) bool {
			return compareSemanticVersions(pkg.Versions[i].Version, pkg.Versions[j].Version)
		})
		packages = append(packages, *pkg)
	}

	log.Printf("[apt-parser-v2] Extracted %d packages from %s", len(packages), indexURL)
	return packages, nil
}

// APKParserV2 parses APK index files in v2 format
type APKParserV2 struct{}

func (p *APKParserV2) CanParse(indexPath string) bool {
	return strings.HasSuffix(indexPath, "APKINDEX.tar.gz") || strings.HasSuffix(indexPath, "APKINDEX")
}

func (p *APKParserV2) Parse(data []byte, indexURL string, contentType string, cachePath string) ([]IndexPackageV2, error) {
	log.Printf("[apk-parser-v2] Parsing index file: %s", indexURL)

	// Decompress gzip
	gzReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer util.DeferClose(gzReader)

	// Read tar archive
	tarReader := tar.NewReader(gzReader)
	var apkindexData []byte

	// Find APKINDEX file in the tar archive
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar: %w", err)
		}

		if header.Name == "APKINDEX" {
			apkindexData, err = io.ReadAll(tarReader)
			if err != nil {
				return nil, fmt.Errorf("failed to read APKINDEX: %w", err)
			}
			break
		}
	}

	if apkindexData == nil {
		return nil, fmt.Errorf("APKINDEX file not found in tar archive")
	}

	// Group packages by name
	packageMap := make(map[string]*IndexPackageV2)
	content := string(apkindexData)

	// Split entries by C: line (each entry starts with C:)
	entries := strings.Split(content, "\nC:")
	if len(entries) == 0 {
		return []IndexPackageV2{}, nil
	}

	// First entry might not have C: prefix, handle it separately
	firstEntry := entries[0]
	if !strings.HasPrefix(firstEntry, "C:") {
		firstEntry = "C:" + firstEntry
	}
	entries[0] = firstEntry

	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		var pkgName, version, arch, url string
		var size int64
		var deps []string
		var license, description string

		lines := strings.Split(entry, "\n")
		for _, line := range lines {
			if len(line) < 2 || line[1] != ':' {
				continue
			}

			key := line[0:1]
			value := strings.TrimSpace(line[2:])

			switch key {
			case "P": // Package name
				pkgName = value
			case "V": // Version
				version = value
			case "A": // Architecture
				arch = value
			case "S": // Size
				if sizeVal, err := strconv.ParseInt(value, 10, 64); err == nil {
					size = sizeVal
				}
			case "T": // Description
				description = value
			case "L": // License
				license = value
			case "U": // URL
				url = value
			case "D": // Dependencies
				// Dependencies are space-separated, format: "so:libc.musl-x86_64.so.1 so:libxcb.so.1"
				// or "cmd:2bwm=0.3-r3" for command dependencies
				depFields := strings.Fields(value)
				for _, dep := range depFields {
					// Clean up dependency format (remove version constraints for display)
					// Format: "so:libname" or "cmd:command=version" or "package=version"
					if idx := strings.Index(dep, "="); idx > 0 {
						dep = dep[:idx]
					}
					// Remove prefix like "so:" or "cmd:" for cleaner display
					if idx := strings.Index(dep, ":"); idx > 0 {
						dep = dep[idx+1:]
					}
					if dep != "" {
						deps = append(deps, dep)
					}
				}
			}
		}

		if pkgName == "" || version == "" {
			continue
		}

		// Construct the actual package download URL from the index file path
		// Index file: /alpine/v3.23/main/x86_64/APKINDEX.tar.gz
		// Package: /alpine/v3.23/main/x86_64/package-version-release.apk
		basePath := strings.TrimSuffix(indexURL, "/APKINDEX.tar.gz")
		if basePath != indexURL {
			packageFilename := fmt.Sprintf("%s-%s.apk", pkgName, version)
			url = basePath + "/" + packageFilename
		}

		// Extract group from indexURL ({version}/branch)
		// Example: http://dl-cdn.alpinelinux.org/alpine/v3.23/main/x86_64/APKINDEX.tar.gz
		// Group: 3.23/main
		group := extractAPKGroupFromURL(indexURL)

		// Create or update package entry
		if packageMap[pkgName] == nil {
			packageMap[pkgName] = &IndexPackageV2{
				Type:         "apk",
				Name:         pkgName,
				Versions:     []PackageVersion{},
				Description:  description,
				Dependencies: deps,
				Group:        group,
				IndexedAt:    time.Now(),
			}
		} else {
			// Update description if not set and we have one
			if packageMap[pkgName].Description == "" && description != "" {
				packageMap[pkgName].Description = description
			}
			// Update group if not set
			if packageMap[pkgName].Group == "" && group != "" {
				packageMap[pkgName].Group = group
			}
		}

		// Add version with license at version level
		versionEntry := PackageVersion{
			Version:      version,
			Architecture: arch,
			CachePath:    url,
			URL:          url,
			Size:         size,
			License:      license, // License is version-specific
			Cached:       false,
			IndexedAt:    time.Now(),
		}
		packageMap[pkgName].Versions = append(packageMap[pkgName].Versions, versionEntry)
	}

	// Convert to slice and sort versions
	var packages []IndexPackageV2
	for _, pkg := range packageMap {
		// Sort versions by semantic version (newest first)
		sort.Slice(pkg.Versions, func(i, j int) bool {
			return compareSemanticVersions(pkg.Versions[i].Version, pkg.Versions[j].Version)
		})
		packages = append(packages, *pkg)
	}

	log.Printf("[apk-parser-v2] Extracted %d packages from %s", len(packages), indexURL)
	return packages, nil
}

// NPMParserV2 parses NPM registry JSON in v2 format
type NPMParserV2 struct{}

func (p *NPMParserV2) CanParse(indexPath string) bool {
	return !strings.Contains(indexPath, "/-/") && !strings.HasSuffix(indexPath, ".tgz")
}

func (p *NPMParserV2) Parse(data []byte, indexURL string, contentType string, cachePath string) ([]IndexPackageV2, error) {
	var npmPkg struct {
		Name        string                            `json:"name"`
		Description string                            `json:"description"`
		Versions    map[string]map[string]interface{} `json:"versions"`
		Time        map[string]string                 `json:"time"`
	}

	if err := json.Unmarshal(data, &npmPkg); err != nil {
		return nil, fmt.Errorf("failed to parse NPM JSON: %w", err)
	}

	if npmPkg.Name == "" {
		return []IndexPackageV2{}, nil
	}

	// Collect all valid versions first
	versionMap := make(map[string]map[string]interface{})
	var versionStrings []string

	for version, versionData := range npmPkg.Versions {
		dist, ok := versionData["dist"].(map[string]interface{})
		if !ok {
			continue
		}

		tarball, _ := dist["tarball"].(string)
		if tarball == "" {
			continue
		}

		versionMap[version] = versionData
		versionStrings = append(versionStrings, version)
	}

	// Sort versions by time (newest first), falling back to semantic version comparison
	sortedVersions := sortVersionsByTimeAndVersion(versionStrings, npmPkg.Time)

	// Build version entries and extract dependencies from sorted versions (newest first)
	var versions []PackageVersion
	var deps []string
	hasDeps := false

	for _, version := range sortedVersions {
		versionData := versionMap[version]
		dist, ok := versionData["dist"].(map[string]interface{})
		if !ok {
			continue
		}

		tarball, _ := dist["tarball"].(string)
		if tarball == "" {
			continue
		}

		var size int64
		if s, ok := dist["size"].(float64); ok {
			size = int64(s)
		} else if s, ok := dist["unpackedSize"].(float64); ok {
			size = int64(s)
		}

		// Extract license for this version (license is version-specific)
		var versionLicense string
		if lic, ok := versionData["license"].(string); ok && lic != "" {
			versionLicense = lic
		} else if licObj, ok := versionData["license"].(map[string]interface{}); ok {
			if licType, ok := licObj["type"].(string); ok {
				versionLicense = licType
			}
		}

		versionEntry := PackageVersion{
			Version:   version,
			CachePath: tarball,
			URL:       tarball,
			Size:      size,
			License:   versionLicense, // License is version-specific
			Cached:    false,
			IndexedAt: time.Now(),
		}
		versions = append(versions, versionEntry)

		// Extract package-level dependencies from versions (newest first)
		// Only update if we don't have it yet (prioritize newer versions)
		if !hasDeps {
			if depMap, ok := versionData["dependencies"].(map[string]interface{}); ok && len(depMap) > 0 {
				deps = make([]string, 0, len(depMap))
				for depName := range depMap {
					deps = append(deps, depName)
				}
				hasDeps = true
			}
		}

		// Once we have dependencies, continue building version entries but stop updating metadata
		// (we've already checked the newest versions first, hasDeps tracks this)
	}

	pkg := IndexPackageV2{
		Type:        "npm",
		Name:        npmPkg.Name,
		Versions:    versions, // Already sorted by sortVersionsByTimeAndVersion
		Description: npmPkg.Description,
		// License is version-specific, not package-level
		Dependencies: deps,
		IndexedAt:    time.Now(),
	}

	return []IndexPackageV2{pkg}, nil
}

// PyPIParserV2 parses PyPI index files in v2 format
type PyPIParserV2 struct{}

func (p *PyPIParserV2) CanParse(indexPath string) bool {
	return strings.HasSuffix(indexPath, "/simple/") || strings.Contains(indexPath, "/simple/")
}

func (p *PyPIParserV2) Parse(data []byte, indexURL string, contentType string, cachePath string) ([]IndexPackageV2, error) {
	log.Printf("[pypi-parser-v2] Parsing PyPI index file: %s", indexURL)

	html := string(data)
	packageMap := make(map[string]*IndexPackageV2)

	// Extract package name from URL path
	// Format: /simple/{package}/ or /simple/{package}
	packageName := ""
	parts := strings.Split(strings.Trim(indexURL, "/"), "/")
	if len(parts) >= 2 && parts[0] == "simple" {
		packageName = parts[1]
	}

	// Regex to match <a> tags with package file links
	// Matches: <a href="...">package-version-*.whl</a> or <a href="...">package-version.tar.gz</a>
	linkPattern := regexp.MustCompile(`<a[^>]+href=["']([^"']+)["'][^>]*>([^<]+)</a>`)
	matches := linkPattern.FindAllStringSubmatch(html, -1)

	for _, match := range matches {
		if len(match) < 3 {
			continue
		}

		href := match[1]
		filename := match[2]

		// Skip non-package files (like ../, parent directory links)
		if !strings.Contains(filename, ".") || strings.HasPrefix(filename, "../") {
			continue
		}

		// Extract version from filename
		// Formats: package-version-py3-none-any.whl, package-version.tar.gz, etc.
		var version string
		var pkgName string

		// Try wheel format: package-version-py3-none-any.whl
		if strings.HasSuffix(filename, ".whl") {
			baseName := strings.TrimSuffix(filename, ".whl")
			parts := strings.Split(baseName, "-")
			if len(parts) >= 2 {
				pkgName = parts[0]
				version = parts[1]
			}
		} else if strings.HasSuffix(filename, ".tar.gz") {
			// Source distribution: package-version.tar.gz
			baseName := strings.TrimSuffix(filename, ".tar.gz")
			idx := strings.LastIndex(baseName, "-")
			if idx > 0 {
				pkgName = baseName[:idx]
				version = baseName[idx+1:]
			}
		} else if strings.HasSuffix(filename, ".zip") {
			// Zip format: package-version.zip
			baseName := strings.TrimSuffix(filename, ".zip")
			idx := strings.LastIndex(baseName, "-")
			if idx > 0 {
				pkgName = baseName[:idx]
				version = baseName[idx+1:]
			}
		} else {
			// Unknown format, skip
			continue
		}

		// Use package name from URL if we couldn't extract it
		if pkgName == "" && packageName != "" {
			pkgName = packageName
		}

		if pkgName == "" || version == "" {
			continue
		}

		// Build full URL (resolve relative hrefs)
		url := href
		if strings.HasPrefix(href, "../") {
			// Relative URL, resolve against indexURL
			basePath := strings.TrimSuffix(indexURL, "/")
			basePath = strings.TrimSuffix(basePath, filepath.Base(basePath))
			url = basePath + strings.TrimPrefix(href, "../")
		} else if !strings.HasPrefix(href, "http://") && !strings.HasPrefix(href, "https://") {
			// Relative URL without ../
			basePath := strings.TrimSuffix(indexURL, "/")
			basePath = strings.TrimSuffix(basePath, filepath.Base(basePath))
			url = basePath + "/" + strings.TrimPrefix(href, "/")
		}

		// Get or create package entry
		if packageMap[pkgName] == nil {
			packageMap[pkgName] = &IndexPackageV2{
				Type:      "pypi",
				Name:      pkgName,
				Versions:  []PackageVersion{},
				IndexedAt: time.Now(),
			}
		}

		// Check if version already exists
		versionExists := false
		for _, v := range packageMap[pkgName].Versions {
			if v.Version == version {
				versionExists = true
				break
			}
		}

		if !versionExists {
			// Add new version
			packageMap[pkgName].Versions = append(packageMap[pkgName].Versions, PackageVersion{
				Version:   version,
				CachePath: url, // Use URL as cache path for lookup
				URL:       url,
				Cached:    false, // Will be set during merge
				IndexedAt: time.Now(),
			})
		}
	}

	// Convert map to slice
	result := make([]IndexPackageV2, 0, len(packageMap))
	for _, pkg := range packageMap {
		// Sort versions by semantic version (newest first)
		versions := pkg.Versions
		sort.Slice(versions, func(i, j int) bool {
			return compareSemanticVersions(versions[i].Version, versions[j].Version)
		})
		pkg.Versions = versions
		result = append(result, *pkg)
	}

	log.Printf("[pypi-parser-v2] Extracted %d packages from %s", len(result), indexURL)
	return result, nil
}

// Helper functions for decompression
func readGzip(data []byte) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer util.DeferClose(gr)
	return io.ReadAll(gr)
}

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

func readBzip2(data []byte) ([]byte, error) {
	bz2Reader := bzip2.NewReader(bytes.NewReader(data))
	decompressed, err := io.ReadAll(bz2Reader)
	if err != nil {
		return nil, fmt.Errorf("bzip2 decompression failed: %w", err)
	}
	return decompressed, nil
}

// compareSemanticVersions compares two semantic version strings.
// Returns true if v1 is greater than v2 (newer version).
// Handles basic semver format: major.minor.patch[-prerelease][+build]
// Falls back to string comparison if versions don't match semver format.
func compareSemanticVersions(v1, v2 string) bool {
	// Simple semver comparison: split by dots and compare parts
	parts1 := strings.Split(strings.Split(v1, "+")[0], "-")[0] // Remove build and prerelease
	parts2 := strings.Split(strings.Split(v2, "+")[0], "-")[0]

	nums1 := strings.Split(parts1, ".")
	nums2 := strings.Split(parts2, ".")

	// Compare each numeric part
	maxLen := len(nums1)
	if len(nums2) > maxLen {
		maxLen = len(nums2)
	}

	for i := 0; i < maxLen; i++ {
		var n1, n2 int64
		if i < len(nums1) {
			n1, _ = strconv.ParseInt(nums1[i], 10, 64)
		}
		if i < len(nums2) {
			n2, _ = strconv.ParseInt(nums2[i], 10, 64)
		}

		if n1 > n2 {
			return true
		}
		if n1 < n2 {
			return false
		}
	}

	// If numeric parts are equal, compare full strings (handles prerelease/build)
	return v1 > v2
}

// sortVersionsByTimeAndVersion sorts versions by time (newest first), falling back to semantic version comparison.
// versions is a slice of version strings, times is a map of version -> time string (RFC3339 format).
// Returns sorted version strings (newest first).
func sortVersionsByTimeAndVersion(versions []string, times map[string]string) []string {
	type versionWithSortKey struct {
		version string
		time    time.Time
		hasTime bool
	}

	versionKeys := make([]versionWithSortKey, 0, len(versions))
	for _, v := range versions {
		vk := versionWithSortKey{version: v, hasTime: false}

		if timeStr, ok := times[v]; ok {
			if parsedTime, err := time.Parse(time.RFC3339, timeStr); err == nil {
				vk.time = parsedTime
				vk.hasTime = true
			}
		}

		versionKeys = append(versionKeys, vk)
	}

	// Sort: versions with time first (by time, newest first), then versions without time (by semver, newest first)
	sort.Slice(versionKeys, func(i, j int) bool {
		hasTimeI := versionKeys[i].hasTime
		hasTimeJ := versionKeys[j].hasTime

		// If both have time, sort by time (newest first)
		if hasTimeI && hasTimeJ {
			return versionKeys[i].time.After(versionKeys[j].time)
		}

		// If only one has time, prefer the one with time
		if hasTimeI {
			return true
		}
		if hasTimeJ {
			return false
		}

		// Neither has time: fall back to semantic version comparison (newest first)
		return compareSemanticVersions(versionKeys[i].version, versionKeys[j].version)
	})

	// Extract sorted versions
	result := make([]string, len(versionKeys))
	for i, vk := range versionKeys {
		result[i] = vk.version
	}

	return result
}

// extractAPTGroupFromURL extracts repository group information from APT index URL
// URL format: http://security.debian.org/debian-security/dists/bookworm-security/main/binary-amd64/Packages.gz
// Returns: "debian-security/bookworm-security/main" or empty string if can't parse
func extractAPTGroupFromURL(indexURL string) string {
	if indexURL == "" {
		return ""
	}

	// Look for /dists/ pattern: /{base-path}/dists/{distribution}/{component}/binary-{arch}
	// Example: /debian-security/dists/bookworm-security/main/binary-amd64
	if !strings.Contains(indexURL, "/dists/") {
		return ""
	}

	// Split into base path and dists path
	parts := strings.Split(indexURL, "/dists/")
	if len(parts) < 2 {
		return ""
	}

	// Extract base path (e.g., /debian-security or /debian)
	// Remove protocol and host if present
	basePath := parts[0]
	if strings.Contains(basePath, "://") {
		// Extract path after host
		urlParts := strings.SplitN(basePath, "://", 2)
		if len(urlParts) == 2 {
			pathParts := strings.SplitN(urlParts[1], "/", 2)
			if len(pathParts) > 1 {
				basePath = "/" + pathParts[1]
			} else {
				basePath = ""
			}
		}
	}
	basePath = strings.Trim(basePath, "/")
	if basePath == "" {
		return ""
	}

	// Strip common path prefixes that aren't part of the repository base path
	// Examples: "pub/repos/apt" -> "apt", "pub/repos/debian" -> "debian"
	if strings.HasPrefix(basePath, "pub/repos/") {
		basePath = strings.TrimPrefix(basePath, "pub/repos/")
	} else if strings.HasPrefix(basePath, "pub/") {
		// Handle cases like "pub/apt" -> "apt"
		basePath = strings.TrimPrefix(basePath, "pub/")
	}

	// Filter out invalid base paths (empty or just "pub")
	if basePath == "" || basePath == "pub" {
		return ""
	}

	// Parse dists path: {distribution}/{component}/binary-{arch}
	distParts := strings.Split(parts[1], "/")
	if len(distParts) < 2 {
		return ""
	}

	// Build group string with hierarchy: basePath/distribution/component
	var groupParts []string
	groupParts = append(groupParts, basePath)
	groupParts = append(groupParts, distParts[0]) // distribution
	groupParts = append(groupParts, distParts[1]) // component

	return strings.Join(groupParts, "/")
}

// extractAPKGroupFromURL extracts Alpine release information from APK index URL
// URL format: http://dl-cdn.alpinelinux.org/alpine/v3.23/main/x86_64/APKINDEX.tar.gz
// Returns: "3.23/main" or "3.23" (release/branch or just release) or empty string if can't parse
func extractAPKGroupFromURL(indexURL string) string {
	if indexURL == "" {
		return ""
	}

	// Look for /alpine/v{version}/ pattern
	// Format: /alpine/v{version}/{branch}/{arch}/...
	if !strings.Contains(indexURL, "/alpine/v") {
		return ""
	}

	// Find /alpine/v in the URL
	alpineIdx := strings.Index(indexURL, "/alpine/v")
	if alpineIdx < 0 {
		return ""
	}

	// Extract everything after /alpine/v
	afterAlpine := indexURL[alpineIdx+8:] // Skip "/alpine/v"

	// Split by / to get version, branch, arch
	parts := strings.Split(afterAlpine, "/")
	if len(parts) < 1 {
		return ""
	}

	// First part is the version (e.g., "3.23")
	version := parts[0]
	if version == "" {
		return ""
	}

	// Build group: {version}/{branch} or just {version} (no need to add "v" - it's already in the URL path)
	var groupParts []string
	groupParts = append(groupParts, version)

	// Add branch if present (e.g., "main", "community", "testing")
	if len(parts) >= 2 && parts[1] != "" {
		groupParts = append(groupParts, parts[1])
	}

	return strings.Join(groupParts, "/")
}
