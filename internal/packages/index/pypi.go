package index

import (
	"log"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// PyPIParser parses PyPI simple index HTML pages
type PyPIParser struct{}

// CanParse returns true if this is a PyPI simple index page
func (p *PyPIParser) CanParse(indexPath string) bool {
	return strings.HasPrefix(indexPath, "/simple/")
}

// Parse extracts package metadata from PyPI simple index HTML
// PyPI simple index pages contain HTML with links to package files
// Example: <a href="../../packages/.../requests-2.31.0-py3-none-any.whl">requests-2.31.0-py3-none-any.whl</a>
func (p *PyPIParser) Parse(data []byte, indexURL string, contentType string, cachePath string) ([]IndexPackage, error) {
	log.Printf("[pypi-parser] Parsing index file: %s", indexURL)

	html := string(data)
	var packages []IndexPackage

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

	seenVersions := make(map[string]bool) // Track unique versions

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

		// Create unique key for version (package:version)
		versionKey := pkgName + ":" + version
		if seenVersions[versionKey] {
			continue // Skip duplicate versions
		}
		seenVersions[versionKey] = true

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

		pkg := IndexPackage{
			Type:      "pypi",
			Name:      pkgName,
			Version:   version,
			Filename:  filename,
			URL:       url,
			IndexedAt: time.Now(),
			Cached:    false, // Will be set during merge
		}

		packages = append(packages, pkg)
	}

	log.Printf("[pypi-parser] Extracted %d packages from %s", len(packages), indexURL)
	return packages, nil
}
