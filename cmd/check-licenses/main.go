package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/sbom"
)

func main() {
	var (
		sbomFile       = flag.String("sbom", "", "Path to SBOM JSON file (required)")
		projectLicense = flag.String("license", "", "Project license (SPDX identifier, e.g., MIT, Apache-2.0, GPL-3.0) (required)")
		showAll        = flag.Bool("all", false, "Show all components, not just incompatible ones")
		verbose        = flag.Bool("verbose", false, "Show detailed license information")
	)
	flag.Parse()

	if *sbomFile == "" {
		fmt.Fprintf(os.Stderr, "Error: -sbom is required\n")
		flag.Usage()
		os.Exit(1)
	}

	if *projectLicense == "" {
		fmt.Fprintf(os.Stderr, "Error: -license is required\n")
		flag.Usage()
		os.Exit(1)
	}

	// Normalize project license
	projectLicenseID := normalizeLicenseID(*projectLicense)

	// Read SBOM file
	data, err := os.ReadFile(*sbomFile)
	if err != nil {
		log.Fatalf("Failed to read SBOM file: %v", err)
	}

	// Parse SBOM
	var bom sbom.CycloneDXBOM
	if err := json.Unmarshal(data, &bom); err != nil {
		log.Fatalf("Failed to parse SBOM: %v", err)
	}

	// Check licenses
	checker := NewLicenseChecker(projectLicenseID)
	incompatible := []ComponentLicense{}
	noLicense := []ComponentLicense{}
	allComponents := []ComponentLicense{}

	for _, component := range bom.Components {
		compLicense := ComponentLicense{
			Name:    component.Name,
			Version: component.Version,
			Purl:    component.Purl,
		}

		// Extract license from component
		licenseID := extractLicenseID(component.Licenses)
		compLicense.LicenseID = licenseID

		if licenseID == "" {
			noLicense = append(noLicense, compLicense)
			if *showAll {
				allComponents = append(allComponents, compLicense)
			}
			continue
		}

		// Check compatibility
		isCompatible, reason := checker.IsCompatible(licenseID)
		compLicense.Compatible = isCompatible
		compLicense.Reason = reason

		if !isCompatible {
			incompatible = append(incompatible, compLicense)
		}

		if *showAll {
			allComponents = append(allComponents, compLicense)
		}
	}

	// Print results
	fmt.Printf("License Compatibility Check\n")
	fmt.Printf("===========================\n\n")
	fmt.Printf("Project License: %s\n", projectLicenseID)
	fmt.Printf("SBOM File: %s\n", *sbomFile)
	fmt.Printf("Total Components: %d\n\n", len(bom.Components))

	if len(noLicense) > 0 {
		fmt.Printf("⚠️  Components without license information: %d\n", len(noLicense))
		if *verbose {
			for _, comp := range noLicense {
				fmt.Printf("  - %s@%s", comp.Name, comp.Version)
				if comp.Purl != "" {
					fmt.Printf(" (%s)", comp.Purl)
				}
				fmt.Println()
			}
		}
		fmt.Println()
	}

	if len(incompatible) > 0 {
		fmt.Printf("❌ Incompatible licenses found: %d\n\n", len(incompatible))
		for _, comp := range incompatible {
			fmt.Printf("  Component: %s@%s\n", comp.Name, comp.Version)
			if comp.Purl != "" {
				fmt.Printf("    PURL: %s\n", comp.Purl)
			}
			fmt.Printf("    License: %s\n", comp.LicenseID)
			if comp.Reason != "" {
				fmt.Printf("    Reason: %s\n", comp.Reason)
			}
			fmt.Println()
		}
		os.Exit(1)
	}

	if *showAll {
		fmt.Printf("✅ All components with licenses are compatible:\n\n")
		for _, comp := range allComponents {
			if comp.LicenseID != "" {
				status := "✅"
				if !comp.Compatible {
					status = "❌"
				}
				fmt.Printf("  %s %s@%s - %s", status, comp.Name, comp.Version, comp.LicenseID)
				if comp.Reason != "" {
					fmt.Printf(" (%s)", comp.Reason)
				}
				fmt.Println()
			}
		}
	} else {
		fmt.Printf("✅ All components with licenses are compatible!\n")
	}
}

type ComponentLicense struct {
	Name       string
	Version    string
	Purl       string
	LicenseID  string
	Compatible bool
	Reason     string
}

type LicenseChecker struct {
	projectLicense string
	compatibility  map[string]map[string]bool
}

func NewLicenseChecker(projectLicense string) *LicenseChecker {
	checker := &LicenseChecker{
		projectLicense: projectLicense,
		compatibility:  buildCompatibilityMatrix(),
	}
	return checker
}

func (lc *LicenseChecker) IsCompatible(dependencyLicense string) (bool, string) {
	// Same license is always compatible
	if normalizeLicenseID(dependencyLicense) == lc.projectLicense {
		return true, "Same license"
	}

	// Check compatibility matrix
	if compat, ok := lc.compatibility[lc.projectLicense]; ok {
		if isCompat, found := compat[normalizeLicenseID(dependencyLicense)]; found {
			if isCompat {
				return true, "Compatible"
			}
			return false, "Incompatible license"
		}
	}

	// Unknown license - be conservative
	return false, "Unknown license compatibility"
}

func buildCompatibilityMatrix() map[string]map[string]bool {
	// Build a compatibility matrix
	// true = compatible, false = incompatible
	matrix := make(map[string]map[string]bool)

	// MIT is compatible with almost everything (except proprietary)
	matrix["MIT"] = map[string]bool{
		"MIT":           true,
		"Apache-2.0":    true,
		"BSD-2-Clause":  true,
		"BSD-3-Clause":  true,
		"ISC":           true,
		"MPL-2.0":       true,
		"LGPL-2.1-only": true,
		"LGPL-3.0-only": true,
		"GPL-2.0-only":  true,
		"GPL-3.0-only":  true,
		"AGPL-3.0-only": true,
	}

	// Apache-2.0 is compatible with most (except GPL-2.0-only)
	matrix["Apache-2.0"] = map[string]bool{
		"MIT":           true,
		"Apache-2.0":    true,
		"BSD-2-Clause":  true,
		"BSD-3-Clause":  true,
		"ISC":           true,
		"MPL-2.0":       true,
		"LGPL-2.1-only": true,
		"LGPL-3.0-only": true,
		"GPL-3.0-only":  true, // Apache-2.0 is compatible with GPL-3.0
		"AGPL-3.0-only": true,
		"GPL-2.0-only":  false, // Apache-2.0 is NOT compatible with GPL-2.0-only
	}

	// GPL-3.0 is only compatible with GPL-3.0 and AGPL-3.0
	matrix["GPL-3.0-only"] = map[string]bool{
		"GPL-3.0-only":  true,
		"AGPL-3.0-only": true,
		"LGPL-3.0-only": true, // LGPL-3.0 is compatible with GPL-3.0
		"MIT":           false,
		"Apache-2.0":    false,
		"BSD-2-Clause":  false,
		"BSD-3-Clause":  false,
		"ISC":           false,
		"MPL-2.0":       false,
		"LGPL-2.1-only": false,
		"GPL-2.0-only":  false, // GPL-3.0 and GPL-2.0 are incompatible
	}

	// GPL-2.0-only is only compatible with GPL-2.0-only
	matrix["GPL-2.0-only"] = map[string]bool{
		"GPL-2.0-only":  true,
		"LGPL-2.1-only": true, // LGPL-2.1 is compatible with GPL-2.0
		"MIT":           false,
		"Apache-2.0":    false,
		"BSD-2-Clause":  false,
		"BSD-3-Clause":  false,
		"ISC":           false,
		"MPL-2.0":       false,
		"LGPL-3.0-only": false,
		"GPL-3.0-only":  false,
		"AGPL-3.0-only": false,
	}

	// AGPL-3.0 is only compatible with AGPL-3.0 and GPL-3.0
	matrix["AGPL-3.0-only"] = map[string]bool{
		"AGPL-3.0-only": true,
		"GPL-3.0-only":  true,
		"LGPL-3.0-only": true,
		"MIT":           false,
		"Apache-2.0":    false,
		"BSD-2-Clause":  false,
		"BSD-3-Clause":  false,
		"ISC":           false,
		"MPL-2.0":       false,
		"LGPL-2.1-only": false,
		"GPL-2.0-only":  false,
	}

	// LGPL-2.1-only is compatible with GPL-2.0 and permissive licenses
	matrix["LGPL-2.1-only"] = map[string]bool{
		"LGPL-2.1-only": true,
		"GPL-2.0-only":  true,
		"MIT":           true,
		"Apache-2.0":    true,
		"BSD-2-Clause":  true,
		"BSD-3-Clause":  true,
		"ISC":           true,
		"MPL-2.0":       true,
		"LGPL-3.0-only": false,
		"GPL-3.0-only":  false,
		"AGPL-3.0-only": false,
	}

	// LGPL-3.0-only is compatible with GPL-3.0 and permissive licenses
	matrix["LGPL-3.0-only"] = map[string]bool{
		"LGPL-3.0-only": true,
		"GPL-3.0-only":  true,
		"AGPL-3.0-only": true,
		"MIT":           true,
		"Apache-2.0":    true,
		"BSD-2-Clause":  true,
		"BSD-3-Clause":  true,
		"ISC":           true,
		"MPL-2.0":       true,
		"LGPL-2.1-only": false,
		"GPL-2.0-only":  false,
	}

	// BSD licenses are compatible with most licenses
	matrix["BSD-2-Clause"] = map[string]bool{
		"MIT":           true,
		"Apache-2.0":    true,
		"BSD-2-Clause":  true,
		"BSD-3-Clause":  true,
		"ISC":           true,
		"MPL-2.0":       true,
		"LGPL-2.1-only": true,
		"LGPL-3.0-only": true,
		"GPL-2.0-only":  true,
		"GPL-3.0-only":  true,
		"AGPL-3.0-only": true,
	}

	matrix["BSD-3-Clause"] = map[string]bool{
		"MIT":           true,
		"Apache-2.0":    true,
		"BSD-2-Clause":  true,
		"BSD-3-Clause":  true,
		"ISC":           true,
		"MPL-2.0":       true,
		"LGPL-2.1-only": true,
		"LGPL-3.0-only": true,
		"GPL-2.0-only":  true,
		"GPL-3.0-only":  true,
		"AGPL-3.0-only": true,
	}

	// ISC is compatible with most licenses
	matrix["ISC"] = map[string]bool{
		"MIT":           true,
		"Apache-2.0":    true,
		"BSD-2-Clause":  true,
		"BSD-3-Clause":  true,
		"ISC":           true,
		"MPL-2.0":       true,
		"LGPL-2.1-only": true,
		"LGPL-3.0-only": true,
		"GPL-2.0-only":  true,
		"GPL-3.0-only":  true,
		"AGPL-3.0-only": true,
	}

	// MPL-2.0 is compatible with most licenses
	matrix["MPL-2.0"] = map[string]bool{
		"MIT":           true,
		"Apache-2.0":    true,
		"BSD-2-Clause":  true,
		"BSD-3-Clause":  true,
		"ISC":           true,
		"MPL-2.0":       true,
		"LGPL-2.1-only": true,
		"LGPL-3.0-only": true,
		"GPL-2.0-only":  true,
		"GPL-3.0-only":  true,
		"AGPL-3.0-only": true,
	}

	return matrix
}

func extractLicenseID(licenses []sbom.CycloneDXLicenseChoice) string {
	if len(licenses) == 0 {
		return ""
	}

	// Try to get SPDX ID first
	for _, lc := range licenses {
		if lc.License != nil && lc.License.ID != "" {
			return normalizeLicenseID(lc.License.ID)
		}
		if lc.Expression != "" {
			// Parse SPDX expression (e.g., "MIT OR Apache-2.0")
			// For simplicity, take the first license in OR expressions
			parts := strings.Split(lc.Expression, " OR ")
			if len(parts) > 0 {
				return normalizeLicenseID(strings.TrimSpace(parts[0]))
			}
			return normalizeLicenseID(lc.Expression)
		}
		if lc.License != nil && lc.License.Name != "" {
			return normalizeLicenseID(lc.License.Name)
		}
	}

	return ""
}

func normalizeLicenseID(license string) string {
	license = strings.TrimSpace(license)

	// Common mappings
	licenseMap := map[string]string{
		"MIT":                "MIT",
		"Apache-2.0":         "Apache-2.0",
		"Apache 2.0":         "Apache-2.0",
		"Apache License 2.0": "Apache-2.0",
		"GPL-2.0":            "GPL-2.0-only",
		"GPL-2.0-only":       "GPL-2.0-only",
		"GPL-3.0":            "GPL-3.0-only",
		"GPL-3.0-only":       "GPL-3.0-only",
		"LGPL-2.1":           "LGPL-2.1-only",
		"LGPL-2.1-only":      "LGPL-2.1-only",
		"LGPL-3.0":           "LGPL-3.0-only",
		"LGPL-3.0-only":      "LGPL-3.0-only",
		"BSD-2-Clause":       "BSD-2-Clause",
		"BSD-3-Clause":       "BSD-3-Clause",
		"BSD":                "BSD-3-Clause", // Default to 3-clause
		"ISC":                "ISC",
		"MPL-2.0":            "MPL-2.0",
		"EPL-1.0":            "EPL-1.0",
		"EPL-2.0":            "EPL-2.0",
		"AGPL-3.0":           "AGPL-3.0-only",
		"AGPL-3.0-only":      "AGPL-3.0-only",
	}

	// Try exact match
	if normalized, ok := licenseMap[license]; ok {
		return normalized
	}

	// Try case-insensitive match
	licenseUpper := strings.ToUpper(license)
	for key, normalized := range licenseMap {
		if strings.ToUpper(key) == licenseUpper {
			return normalized
		}
	}

	// Return as-is if no match
	return license
}
