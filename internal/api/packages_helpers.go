package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// validateMethodGET validates that the request method is GET
func validateMethodGET(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// validateMethodPOST validates that the request method is POST
func validateMethodPOST(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// parseNPMScopedPackage handles NPM scoped packages (e.g., @scope/package)
// Returns: packageName, version, error
func parseNPMScopedPackage(urlPath, prefix string) (string, string, error) {
	path := strings.TrimPrefix(urlPath, prefix)
	path = strings.TrimSuffix(path, "/metadata")

	decodedPath, err := url.PathUnescape(path)
	if err != nil {
		decodedPath = path
	}

	parts := strings.Split(decodedPath, "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid path format: expected {name}/{version}")
	}

	var packageName, version string
	if strings.HasPrefix(parts[0], "@") {
		// Scoped package: @scope/package
		if len(parts) < 3 {
			return "", "", fmt.Errorf("invalid path format for scoped package")
		}
		packageName = parts[0] + "/" + parts[1]
		version = parts[2]
	} else {
		// Regular package
		packageName = parts[0]
		version = parts[1]
	}

	return packageName, version, nil
}

// parseGoModulePath extracts module path and version from a URL path
// Path format: /api/v1/packages/go/{module}/{version}/metadata
// Returns: modulePath, version, error
func parseGoModulePath(urlPath, prefix string) (string, string, error) {
	path := strings.TrimPrefix(urlPath, prefix)
	path = strings.TrimSuffix(path, "/metadata")

	decodedPath, err := url.PathUnescape(path)
	if err != nil {
		decodedPath = path
	}

	parts := strings.Split(decodedPath, "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid path format: expected {module}/{version}")
	}

	version := parts[len(parts)-1]
	modulePath := strings.Join(parts[:len(parts)-1], "/")

	return modulePath, version, nil
}

// findPackageByMetadata walks a directory looking for a package file matching name and version
// Returns the path to the package file (without .meta extension)
func findPackageByMetadata(typeDir, packageName, version string) (string, error) {
	return findPackageByMetadataWithFilter(typeDir, packageName, version, nil)
}

// findPackageByMetadataWithFilter walks a directory looking for a package file matching name and version,
// with an optional custom matcher function. The matcher receives the full metadata and can perform
// additional checks (e.g., URLPath matching, file extension checks).
// Returns the path to the package file (without .meta extension)
func findPackageByMetadataWithFilter(typeDir, packageName, version string, matcher func(map[string]interface{}) bool) (string, error) {
	var foundPath string
	err := filepath.Walk(typeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		// Look for .meta files
		if !strings.HasSuffix(path, ".meta") {
			return nil
		}

		// Load metadata to check package name and version
		metaData, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var metadata map[string]interface{}
		if err := json.Unmarshal(metaData, &metadata); err != nil {
			return nil
		}

		// Extract fields for basic matching
		name, _ := metadata["name"].(string)
		ver, _ := metadata["version"].(string)
		isIndex, _ := metadata["is_index"].(bool)

		// Basic match: name and version must match, and not an index file
		basicMatch := name == packageName && ver == version && !isIndex

		// If basic match passes, check custom matcher if provided
		if basicMatch {
			if matcher != nil && !matcher(metadata) {
				return nil // Custom matcher rejected it
			}

			// The actual package file is the path without .meta
			packagePath := strings.TrimSuffix(path, ".meta")
			if _, err := os.Stat(packagePath); err == nil {
				foundPath = packagePath
				return filepath.SkipAll
			}
		}

		return nil
	})

	if err != nil && err != filepath.SkipAll {
		return "", err
	}

	if foundPath == "" {
		return "", fmt.Errorf("package %s@%s not found in cache", packageName, version)
	}

	return foundPath, nil
}

// findPackageByURLPath walks a directory looking for a package file matching a specific URLPath
// This is useful for Go modules where files are identified by their URL path (e.g., /{module}/@v/{version}.info)
func findPackageByURLPath(typeDir, urlPath string) (string, error) {
	var foundPath string
	err := filepath.Walk(typeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		// Look for .meta files
		if !strings.HasSuffix(path, ".meta") {
			return nil
		}

		// Load metadata to check URL path
		metaData, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var metadata struct {
			URLPath string `json:"url_path"`
			IsIndex bool   `json:"is_index"`
		}
		if err := json.Unmarshal(metaData, &metadata); err != nil {
			return nil
		}

		// Check if URL path matches and not an index file
		if metadata.URLPath == urlPath && !metadata.IsIndex {
			// The actual package file is the path without .meta
			packagePath := strings.TrimSuffix(path, ".meta")
			if _, err := os.Stat(packagePath); err == nil {
				foundPath = packagePath
				return filepath.SkipAll
			}
		}

		return nil
	})

	if err != nil && err != filepath.SkipAll {
		return "", err
	}

	if foundPath == "" {
		return "", fmt.Errorf("package with URLPath %s not found in cache", urlPath)
	}

	return foundPath, nil
}
