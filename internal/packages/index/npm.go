package index

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"
)

// NPMParser parses NPM registry JSON responses
type NPMParser struct{}

// CanParse returns true if this is an NPM registry index file (JSON metadata, not .tgz packages)
func (p *NPMParser) CanParse(indexPath string) bool {
	// NPM index files are JSON metadata responses (e.g., /express, /express/4.18.2)
	// NPM packages are tarballs (e.g., /express/-/express-4.18.2.tgz)
	// Only parse JSON responses, not .tgz files
	return !strings.Contains(indexPath, "/-/") && !strings.HasSuffix(indexPath, ".tgz")
}

// npmPackageJSON represents the structure of an NPM registry response
type npmPackageJSON struct {
	Name        string                    `json:"name"`
	Description string                    `json:"description"`
	Versions    map[string]npmVersionJSON `json:"versions"`
	Time        map[string]string         `json:"time"`
}

// npmVersionJSON represents a single version in an NPM package
type npmVersionJSON struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Description  string            `json:"description"`
	License      interface{}       `json:"license"` // Can be string or object like {"type": "MIT", "url": "..."}
	Dependencies map[string]string `json:"dependencies"`
	Dist         npmDistJSON       `json:"dist"`
}

// npmDistJSON represents the distribution information for an NPM package
type npmDistJSON struct {
	Tarball      string `json:"tarball"`
	Shasum       string `json:"shasum"`
	Size         int64  `json:"size"`
	UnpackedSize int64  `json:"unpackedSize"`
}

// Parse extracts package metadata from NPM registry JSON
func (p *NPMParser) Parse(data []byte, indexURL string, contentType string, cachePath string) ([]IndexPackage, error) {
	var npmPkg npmPackageJSON
	if err := json.Unmarshal(data, &npmPkg); err != nil {
		return nil, fmt.Errorf("failed to parse NPM JSON: %w", err)
	}

	if npmPkg.Name == "" {
		// This might be a single version response (e.g., /express/4.18.2)
		// Try parsing as a single version
		var singleVersion npmVersionJSON
		if err := json.Unmarshal(data, &singleVersion); err != nil {
			return nil, fmt.Errorf("failed to parse NPM JSON (neither package nor version): %w", err)
		}
		if singleVersion.Name == "" || singleVersion.Version == "" {
			return nil, fmt.Errorf("invalid NPM JSON: missing name or version")
		}
		return p.parseSingleVersion(singleVersion, indexURL), nil
	}

	// Parse all versions from the package JSON
	var packages []IndexPackage
	for version, versionData := range npmPkg.Versions {
		if versionData.Dist.Tarball == "" {
			continue // Skip versions without tarball
		}

		// Extract dependencies
		var deps []string
		for depName := range versionData.Dependencies {
			deps = append(deps, depName)
		}

		// Determine size (prefer unpackedSize, fallback to size)
		size := versionData.Dist.UnpackedSize
		if size == 0 {
			size = versionData.Dist.Size
		}

		// Use version-specific description if available, otherwise package description
		description := versionData.Description
		if description == "" {
			description = npmPkg.Description
		}

		// Extract license (can be string or object)
		license := ""
		if versionData.License != nil {
			switch v := versionData.License.(type) {
			case string:
				license = v
			case map[string]interface{}:
				if licenseType, ok := v["type"].(string); ok {
					license = licenseType
				}
			}
		}

		// Extract package name from URL path if scoped (e.g., /@types/node -> @types/node)
		packageName := npmPkg.Name
		if packageName == "" {
			// Fallback: extract from URL
			parts := strings.Split(strings.Trim(indexURL, "/"), "/")
			if len(parts) > 0 {
				if strings.HasPrefix(parts[0], "@") && len(parts) > 1 {
					packageName = parts[0] + "/" + parts[1]
				} else {
					packageName = parts[0]
				}
			}
		}

		pkg := IndexPackage{
			Type:         "npm",
			Name:         packageName,
			Version:      version,
			Filename:     filepath.Base(versionData.Dist.Tarball),
			Size:         size,
			Description:  description,
			License:      license,
			Dependencies: deps,
			URL:          versionData.Dist.Tarball,
			IndexedAt:    time.Now(),
			Cached:       false, // Will be set during merge
		}

		packages = append(packages, pkg)
	}

	log.Printf("[npm-parser] Parsed %d versions from %s", len(packages), npmPkg.Name)
	return packages, nil
}

// parseSingleVersion parses a single version JSON response
func (p *NPMParser) parseSingleVersion(versionData npmVersionJSON, indexURL string) []IndexPackage {
	var deps []string
	for depName := range versionData.Dependencies {
		deps = append(deps, depName)
	}

	size := versionData.Dist.UnpackedSize
	if size == 0 {
		size = versionData.Dist.Size
	}

	// Extract package name from URL if not in JSON
	packageName := versionData.Name
	if packageName == "" {
		parts := strings.Split(strings.Trim(indexURL, "/"), "/")
		if len(parts) > 0 {
			if strings.HasPrefix(parts[0], "@") && len(parts) > 1 {
				packageName = parts[0] + "/" + parts[1]
			} else {
				packageName = parts[0]
			}
		}
	}

	// Extract license (can be string or object)
	license := ""
	if versionData.License != nil {
		switch v := versionData.License.(type) {
		case string:
			license = v
		case map[string]interface{}:
			if licenseType, ok := v["type"].(string); ok {
				license = licenseType
			}
		}
	}

	return []IndexPackage{
		{
			Type:         "npm",
			Name:         packageName,
			Version:      versionData.Version,
			Filename:     filepath.Base(versionData.Dist.Tarball),
			Size:         size,
			Description:  versionData.Description,
			License:      license,
			Dependencies: deps,
			URL:          versionData.Dist.Tarball,
			IndexedAt:    time.Now(),
			Cached:       false,
		},
	}
}
