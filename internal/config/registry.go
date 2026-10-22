package config

import (
	"fmt"
	"log"
	"strings"
)

// PathRewriteFunc takes pathPrefix, repositoryPath, and incoming path and returns rewritten path
type PathRewriteFunc func(pathPrefix, repositoryPath, path string) string

// pathRewriters maps rewrite function names to their implementations
var pathRewriters = map[string]PathRewriteFunc{
	"pypi": RewritePyPIPath,
}

// RegisterPathRewriter registers a path rewrite function
func RegisterPathRewriter(name string, fn PathRewriteFunc) {
	pathRewriters[name] = fn
}

// GetPathRewriter returns a path rewrite function by name
func GetPathRewriter(name string) (PathRewriteFunc, bool) {
	fn, ok := pathRewriters[name]
	return fn, ok
}

// RewritePyPIPath converts files.pythonhosted.org CDN paths to Nexus PyPI proxy format
// CDN format: /repository/pypi-proxy/packages/XX/YY/hash/package-version-...whl
// Nexus format: /repository/pypi-proxy/packages/package-name/version/package-version-...whl
func RewritePyPIPath(pathPrefix, repositoryPath, path string) string {
	// Match pattern: /repository/pypi-proxy/packages/XX/YY/hash/filename.whl
	if !strings.HasPrefix(path, repositoryPath+"/packages/") {
		return path
	}

	// Extract filename from path
	parts := strings.Split(path, "/")
	if len(parts) < 5 {
		return path
	}

	filename := parts[len(parts)-1]

	// Parse filename to extract package name and version
	// Format: package_name-version-python-abi-platform.whl or package_name-version.tar.gz
	// Package names can contain hyphens, so we need to find where the version starts
	// Versions always start with a digit (PEP 440)

	// Remove file extension to simplify parsing
	nameWithoutExt := filename
	if strings.HasSuffix(filename, ".whl") {
		nameWithoutExt = strings.TrimSuffix(filename, ".whl")
	} else if strings.HasSuffix(filename, ".tar.gz") {
		nameWithoutExt = strings.TrimSuffix(filename, ".tar.gz")
	} else if strings.HasSuffix(filename, ".zip") {
		nameWithoutExt = strings.TrimSuffix(filename, ".zip")
	}

	// Split on hyphens and find the first segment that starts with a digit
	segments := strings.Split(nameWithoutExt, "-")
	if len(segments) < 2 {
		return path
	}

	versionIdx := -1
	for i, seg := range segments {
		if len(seg) > 0 && seg[0] >= '0' && seg[0] <= '9' {
			versionIdx = i
			break
		}
	}

	if versionIdx <= 0 {
		// No version found or it's the first segment
		return path
	}

	// Package name is everything before the version
	packageName := strings.Join(segments[:versionIdx], "-")
	version := segments[versionIdx]

	// Convert underscores in package name to hyphens (PyPI convention)
	packageName = strings.ReplaceAll(packageName, "_", "-")

	// Construct Nexus path format
	newPath := fmt.Sprintf("%s/packages/%s/%s/%s", repositoryPath, packageName, version, filename)

	log.Printf("PyPI path rewrite: %s -> %s", path, newPath)
	return newPath
}

// FindBestMatchingConfig finds the best matching config for a hostname and path
// Returns the config with the longest matching path_prefix
func FindBestMatchingConfig(registries RegistryMap, hostname, path string) (RegistryConfig, bool) {
	configs, exists := registries[hostname]
	if !exists || len(configs) == 0 {
		return RegistryConfig{}, false
	}

	// Find the config with the longest matching path prefix
	var bestMatch RegistryConfig
	bestMatchLen := -1
	foundMatch := false

	for _, config := range configs {
		if config.PathPrefix == "" {
			// No path prefix - matches any path
			if bestMatchLen < 0 {
				bestMatch = config
				bestMatchLen = 0
				foundMatch = true
			}
		} else if strings.HasPrefix(path, config.PathPrefix) {
			// Path matches this prefix
			prefixLen := len(config.PathPrefix)
			if prefixLen > bestMatchLen {
				bestMatch = config
				bestMatchLen = prefixLen
				foundMatch = true
			}
		}
	}

	return bestMatch, foundMatch
}
