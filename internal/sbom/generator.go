package sbom

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CycloneDXBOM represents a CycloneDX SBOM document
type CycloneDXBOM struct {
	BOMFormat    string                `json:"bomFormat"`
	SpecVersion  string                `json:"specVersion"`
	Version      int                   `json:"version"`
	SerialNumber string                `json:"serialNumber"`
	Metadata     CycloneDXMetadata     `json:"metadata"`
	Components   []CycloneDXComponent  `json:"components"`
	Dependencies []CycloneDXDependency `json:"dependencies,omitempty"`
}

// CycloneDXMetadata contains metadata about the SBOM
type CycloneDXMetadata struct {
	Timestamp string              `json:"timestamp"`
	Tools     []CycloneDXTool     `json:"tools"`
	Component *CycloneDXComponent `json:"component,omitempty"`
}

// CycloneDXTool represents the tool that generated the SBOM
type CycloneDXTool struct {
	Vendor  string `json:"vendor"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// CycloneDXComponent represents a component in the SBOM
type CycloneDXComponent struct {
	Type        string                   `json:"type"` // "application", "library", "container", etc.
	BOMRef      string                   `json:"bom-ref"`
	Name        string                   `json:"name"`
	Version     string                   `json:"version,omitempty"`
	Purl        string                   `json:"purl,omitempty"` // Package URL
	Description string                   `json:"description,omitempty"`
	Licenses    []CycloneDXLicenseChoice `json:"licenses,omitempty"`
	Hashes      []CycloneDXHash          `json:"hashes,omitempty"`
	Properties  []CycloneDXProperty      `json:"properties,omitempty"`
}

// CycloneDXLicenseChoice represents a license
type CycloneDXLicenseChoice struct {
	License    *CycloneDXLicense `json:"license,omitempty"`
	Expression string            `json:"expression,omitempty"`
}

// CycloneDXLicense represents a single license
type CycloneDXLicense struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// CycloneDXHash represents a hash/digest
type CycloneDXHash struct {
	Alg     string `json:"alg"`     // "SHA-256", "MD5", etc.
	Content string `json:"content"` // The hash value
}

// CycloneDXProperty represents a custom property
type CycloneDXProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// CycloneDXDependency represents a dependency relationship
type CycloneDXDependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

// GenerateCycloneDX generates a CycloneDX SBOM from a session
func GenerateCycloneDX(session *Session) (*CycloneDXBOM, error) {
	bom := &CycloneDXBOM{
		BOMFormat:    "CycloneDX",
		SpecVersion:  "1.5",
		Version:      1,
		SerialNumber: fmt.Sprintf("urn:uuid:%s", session.ID),
		Metadata: CycloneDXMetadata{
			Timestamp: session.StartedAt.Format(time.RFC3339),
			Tools: []CycloneDXTool{
				{
					Vendor:  "Roxxy",
					Name:    "roxxy",
					Version: "1.0",
				},
			},
		},
		Components: make([]CycloneDXComponent, 0),
	}

	// Convert artifacts to components
	for i, artifact := range session.Artifacts {
		component := artifactToComponent(artifact, i)
		bom.Components = append(bom.Components, component)
	}

	return bom, nil
}

// artifactToComponent converts an Artifact to a CycloneDXComponent
func artifactToComponent(artifact Artifact, index int) CycloneDXComponent {
	component := CycloneDXComponent{
		BOMRef: fmt.Sprintf("pkg-%d", index),
		Name:   artifact.Name,
	}

	// Set component type based on artifact type
	if artifact.Type == "docker" {
		component.Type = "container"
		component.BOMRef = fmt.Sprintf("container-%d", index)
		// For Docker images, use the digest as the primary identifier
		if artifact.Digest != "" {
			component.BOMRef = fmt.Sprintf("container-%s", artifact.Digest[:16])
		}
	} else {
		component.Type = "library"
	}

	// Set version
	if artifact.Version != "" {
		component.Version = artifact.Version
	}

	// Set description
	if artifact.Description != "" {
		component.Description = artifact.Description
	}

	// Set license
	if artifact.License != "" {
		license := parseLicense(artifact.License)
		component.Licenses = []CycloneDXLicenseChoice{
			{
				License: license,
			},
		}
	}

	// Generate PURL (Package URL) if possible
	switch artifact.Type {
	case "package":
		component.Purl = generatePURL(artifact)
	case "docker":
		// Generate Docker PURL: pkg:docker/<repo>@<tag>?digest=<digest>
		component.Purl = generateDockerPURL(artifact)
	}

	// Add digest/hash if available
	if artifact.Digest != "" {
		// Extract hash content (remove "sha256:" prefix if present)
		hashContent := strings.TrimPrefix(artifact.Digest, "sha256:")
		component.Hashes = []CycloneDXHash{
			{
				Alg:     "SHA-256",
				Content: hashContent,
			},
		}
	}

	// Add properties for additional metadata
	properties := []CycloneDXProperty{
		{
			Name:  "artifactType",
			Value: artifact.Type,
		},
	}
	if artifact.PackageType != "" {
		properties = append(properties, CycloneDXProperty{
			Name:  "packageType",
			Value: artifact.PackageType,
		})
	}
	if artifact.Architecture != "" {
		properties = append(properties, CycloneDXProperty{
			Name:  "architecture",
			Value: artifact.Architecture,
		})
	}
	if artifact.URL != "" {
		properties = append(properties, CycloneDXProperty{
			Name:  "sourceUrl",
			Value: artifact.URL,
		})
	}
	component.Properties = properties

	return component
}

// parseLicense parses a license string and returns a CycloneDXLicense
// Attempts to identify common license IDs (SPDX format)
func parseLicense(licenseStr string) *CycloneDXLicense {
	// Common license mappings
	licenseMap := map[string]string{
		"MIT":          "MIT",
		"Apache-2.0":   "Apache-2.0",
		"Apache 2.0":   "Apache-2.0",
		"GPL-2.0":      "GPL-2.0-only",
		"GPL-3.0":      "GPL-3.0-only",
		"LGPL-2.1":     "LGPL-2.1-only",
		"LGPL-3.0":     "LGPL-3.0-only",
		"BSD-2-Clause": "BSD-2-Clause",
		"BSD-3-Clause": "BSD-3-Clause",
		"ISC":          "ISC",
		"MPL-2.0":      "MPL-2.0",
		"EPL-1.0":      "EPL-1.0",
		"EPL-2.0":      "EPL-2.0",
		"AGPL-3.0":     "AGPL-3.0-only",
	}

	// Try exact match first
	if id, ok := licenseMap[licenseStr]; ok {
		return &CycloneDXLicense{
			ID: id,
		}
	}

	// Try case-insensitive match
	for key, id := range licenseMap {
		if equalsIgnoreCase(licenseStr, key) {
			return &CycloneDXLicense{
				ID: id,
			}
		}
	}

	// If no match, use the original string as the name
	return &CycloneDXLicense{
		Name: licenseStr,
	}
}

// generateDockerPURL generates a Package URL for Docker images
func generateDockerPURL(artifact Artifact) string {
	if artifact.Name == "" {
		return ""
	}

	// Docker PURL format: pkg:docker/<repo>@<tag>?digest=<digest>
	purl := "pkg:docker/" + artifact.Name
	if artifact.Version != "" {
		purl += "@" + artifact.Version
	}

	// Add digest as qualifier
	if artifact.Digest != "" {
		purl += "?digest=" + artifact.Digest
	}

	return purl
}

// generatePURL generates a Package URL (purl) for the artifact
func generatePURL(artifact Artifact) string {
	if artifact.PackageType == "" || artifact.Name == "" {
		return ""
	}

	// Map package types to PURL types
	purlType := ""
	switch artifact.PackageType {
	case "apk":
		purlType = "apk"
	case "apt":
		purlType = "deb"
	case "npm":
		purlType = "npm"
	case "pypi":
		purlType = "pypi"
	case "go":
		purlType = "golang"
	default:
		return ""
	}

	// Build PURL: pkg:{type}/{namespace}/{name}@{version}?{qualifiers}
	purl := fmt.Sprintf("pkg:%s/%s", purlType, artifact.Name)
	if artifact.Version != "" {
		purl += "@" + artifact.Version
	}

	// Add qualifiers
	qualifiers := []string{}
	if artifact.Architecture != "" {
		qualifiers = append(qualifiers, fmt.Sprintf("arch=%s", artifact.Architecture))
	}
	if len(qualifiers) > 0 {
		purl += "?" + strings.Join(qualifiers, "&")
	}

	return purl
}

// equalsIgnoreCase performs case-insensitive string comparison
func equalsIgnoreCase(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if toLower(a[i]) != toLower(b[i]) {
			return false
		}
	}
	return true
}

func toLower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// ToJSON converts the SBOM to JSON
func (bom *CycloneDXBOM) ToJSON() ([]byte, error) {
	return json.MarshalIndent(bom, "", "  ")
}
