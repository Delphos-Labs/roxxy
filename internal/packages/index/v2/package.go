package v2

import (
	"time"
)

// IndexPackageV2 represents a package with all its versions
// One entry per package name (decoded), with array of versions
type IndexPackageV2 struct {
	Type         string           `json:"type"`
	Name         string           `json:"name"`     // Decoded name (e.g., "github.com/Microsoft/go-winio/v5")
	Versions     []PackageVersion `json:"versions"` // Array of versions
	Description  string           `json:"description,omitempty"`
	License      string           `json:"license,omitempty"`
	Dependencies []string         `json:"dependencies,omitempty"`
	Group        string           `json:"group,omitempty"` // Hierarchical group (e.g., "repo/distro/component" for APT, "3.23/main" for APK)
	IndexedAt    time.Time        `json:"indexed_at"`
}

// PackageVersion represents a single version of a package
type PackageVersion struct {
	Version      string    `json:"version"`
	Architecture string    `json:"architecture,omitempty"`
	CachePath    string    `json:"cache_path"` // Encoded URL path for cache lookup
	Cached       bool      `json:"cached"`
	Size         int64     `json:"size"`
	URL          string    `json:"url"`                   // Original encoded URL
	License      string    `json:"license,omitempty"`     // Version-specific license
	Description  string    `json:"description,omitempty"` // Version-specific description
	IndexedAt    time.Time `json:"indexed_at"`
}
