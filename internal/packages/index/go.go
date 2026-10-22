package index

import (
	"encoding/json"
	"strings"
	"time"
)

// GoParser parses Go module index files
// Note: .info files have minimal data, but we still parse them to discover packages
// The actual metadata (license, description) comes from .zip files via the enricher
type GoParser struct{}

// CanParse returns true if this is a Go module index file
// We only parse .info files, not /list files, to avoid duplicates
// /list files just list available versions, while .info files have actual metadata
func (p *GoParser) CanParse(indexPath string) bool {
	return strings.HasSuffix(indexPath, ".info")
}

// goInfo represents the structure of a Go module .info file
type goInfo struct {
	Version string    `json:"Version"`
	Time    time.Time `json:"Time"`
	GoMod   string    `json:"GoMod,omitempty"`
}

// Parse extracts package metadata from Go module index files
func (p *GoParser) Parse(data []byte, indexURL string, contentType string, cachePath string) ([]IndexPackage, error) {
	var packages []IndexPackage

	// Handle .info files (single module version)
	if strings.HasSuffix(indexURL, ".info") {
		var info goInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return nil, err
		}

		// Extract module path from URL: /golang.org/x/net/@v/v0.42.0.info
		// Module path is everything before /@v/
		modulePath := ""
		parts := strings.Split(indexURL, "/")
		for i, part := range parts {
			if part == "@v" && i > 0 {
				modulePath = strings.Join(parts[1:i], "/")
				break
			}
		}

		if modulePath == "" {
			return packages, nil // Can't determine module path
		}

		// Build hierarchical group: use module path directly with / separators
		// e.g., golang.org/x/net -> golang.org/x/net
		// Strip /vN suffix to group v0/v1 with v2+ versions
		group := p.buildGroup(modulePath)

		// Construct the .zip URL from the .info URL for matching with cached packages
		zipURL := strings.TrimSuffix(indexURL, ".info") + ".zip"

		pkg := IndexPackage{
			Type:      "go",
			Name:      modulePath,
			Version:   info.Version,
			Group:     group,
			Filename:  strings.TrimPrefix(indexURL, "/"),
			Size:      int64(len(data)),
			URL:       zipURL, // Store .zip URL for matching with cached packages
			IndexedAt: time.Now(),
			Cached:    false, // Will be updated later
		}

		packages = append(packages, pkg)
		return packages, nil
	}

	// We no longer parse /list files to avoid duplicates
	// /list files just list available versions, while .info files have actual metadata
	// Parsing both creates duplicate entries for the same package versions

	return packages, nil
}

// buildGroup creates a hierarchical group string from a module path
// Returns the module path with /vN suffix stripped, using / separators
// e.g., "golang.org/x/net" -> "golang.org/x/net"
// e.g., "github.com/user/repo" -> "github.com/user/repo"
// e.g., "github.com/go-git/go-billy/v5" -> "github.com/go-git/go-billy" (strips /vN suffix)
func (p *GoParser) buildGroup(modulePath string) string {
	parts := strings.Split(modulePath, "/")
	if len(parts) < 2 {
		return "default"
	}

	// Strip /vN suffix from the last part if present
	// e.g., "github.com/go-git/go-billy/v5" -> "github.com/go-git/go-billy"
	// This groups v0/v1 (no suffix) with v2+ (with /vN suffix) as the same package
	if len(parts) > 0 {
		lastPart := parts[len(parts)-1]
		// Check if last part matches /vN pattern (where N is a digit)
		if strings.HasPrefix(lastPart, "v") && len(lastPart) > 1 {
			// Check if it's all digits after "v"
			isVersionSuffix := true
			for _, r := range lastPart[1:] {
				if r < '0' || r > '9' {
					isVersionSuffix = false
					break
				}
			}
			if isVersionSuffix {
				// Remove the /vN part
				parts = parts[:len(parts)-1]
			}
		}
	}

	// Return the module path with / separators (not :)
	// This makes it much simpler and leverages existing hierarchical path handling
	return strings.Join(parts, "/")
}
