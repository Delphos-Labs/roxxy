package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/sbom"
)

func main() {
	var (
		sbomFile = flag.String("sbom", "", "Path to SBOM JSON file (required)")
		analyze  = flag.String("analyze", "go", "Package type to analyze: go, apk, apt, npm, pypi, all (default: go)")
	)
	flag.Parse()

	if *sbomFile == "" {
		fmt.Fprintf(os.Stderr, "Error: -sbom is required\n")
		flag.Usage()
		os.Exit(1)
	}

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

	// Filter components by requested type
	var filteredComponents []sbom.CycloneDXComponent

	for _, comp := range bom.Components {
		// Determine package type
		pkgType := ""
		for _, prop := range comp.Properties {
			if prop.Name == "packageType" {
				pkgType = prop.Value
				break
			}
		}

		// Also check sourceUrl for .mod files (legacy)
		if pkgType == "" {
			for _, prop := range comp.Properties {
				if prop.Name == "sourceUrl" && strings.HasSuffix(prop.Value, ".mod") {
					pkgType = "go"
					break
				}
			}
		}

		// Filter based on analyze flag
		if *analyze == "all" {
			filteredComponents = append(filteredComponents, comp)
		} else if pkgType == *analyze {
			filteredComponents = append(filteredComponents, comp)
		}
	}

	fmt.Printf("SBOM Analysis\n")
	fmt.Printf("=============\n\n")
	fmt.Printf("Total Components: %d\n", len(bom.Components))

	if *analyze != "all" {
		fmt.Printf("Filtered (%s): %d\n", *analyze, len(filteredComponents))
	}
	fmt.Println()

	// Analyze based on type
	if *analyze == "go" || *analyze == "all" {
		goComps := filterByType(filteredComponents, "go")
		if len(goComps) > 0 {
			analyzeGoComponents(goComps)
		}
	}

	if *analyze == "apk" || *analyze == "all" {
		apkComps := filterByType(filteredComponents, "apk")
		if len(apkComps) > 0 {
			analyzeOtherComponents(apkComps)
		}
	}

	if *analyze == "apt" || *analyze == "all" {
		aptComps := filterByType(filteredComponents, "apt")
		if len(aptComps) > 0 {
			analyzeOtherComponents(aptComps)
		}
	}

	if *analyze == "npm" || *analyze == "all" {
		npmComps := filterByType(filteredComponents, "npm")
		if len(npmComps) > 0 {
			analyzeOtherComponents(npmComps)
		}
	}

	if *analyze == "pypi" || *analyze == "all" {
		pypiComps := filterByType(filteredComponents, "pypi")
		if len(pypiComps) > 0 {
			analyzeOtherComponents(pypiComps)
		}
	}
}

func analyzeGoComponents(components []sbom.CycloneDXComponent) {
	fmt.Printf("Go Dependencies Analysis\n")
	fmt.Printf("========================\n\n")

	// Separate .zip files and identify direct dependencies
	var zipFiles []sbom.CycloneDXComponent
	var directDeps []sbom.CycloneDXComponent

	// Check go.mod to identify direct dependencies
	goModDeps := getDirectDependencies()

	for _, comp := range components {
		// Skip .mod files (they shouldn't be in SBOM anymore, but check just in case)
		isMod := false
		for _, prop := range comp.Properties {
			if prop.Name == "sourceUrl" && strings.HasSuffix(prop.Value, ".mod") {
				isMod = true
				break
			}
		}

		if !isMod {
			zipFiles = append(zipFiles, comp)
			// Check if this is a direct dependency
			if isDirectDependency(comp.Name, goModDeps) {
				directDeps = append(directDeps, comp)
			}
		}
	}

	fmt.Printf("📦 Package Files (.zip): %d\n", len(zipFiles))
	fmt.Printf("🎯 Direct Dependencies: %d\n", len(directDeps))
	fmt.Printf("📚 Transitive Dependencies: %d\n", len(zipFiles)-len(directDeps))
	fmt.Println()

	// Group by module path (base path without /vN)
	moduleGroups := make(map[string][]sbom.CycloneDXComponent)
	for _, comp := range zipFiles {
		basePath := getBaseModulePath(comp.Name)
		moduleGroups[basePath] = append(moduleGroups[basePath], comp)
	}

	// Find modules with multiple versions
	var multiVersionModules []string
	for basePath, comps := range moduleGroups {
		if len(comps) > 1 {
			multiVersionModules = append(multiVersionModules, basePath)
		}
	}

	if len(multiVersionModules) > 0 {
		fmt.Printf("📊 Modules with Multiple Versions: %d\n", len(multiVersionModules))
		sort.Strings(multiVersionModules)
		for _, mod := range multiVersionModules[:min(10, len(multiVersionModules))] {
			versions := moduleGroups[mod]
			fmt.Printf("   - %s (%d versions)\n", mod, len(versions))
			for _, v := range versions {
				fmt.Printf("     • %s\n", v.Version)
			}
		}
		if len(multiVersionModules) > 10 {
			fmt.Printf("   ... and %d more\n", len(multiVersionModules)-10)
		}
		fmt.Println()
	}

	// Analyze dependency tree
	fmt.Printf("🔍 Dependency Analysis:\n")
	fmt.Printf("======================\n\n")

	// Group by dependency source
	dependencySources := make(map[string][]sbom.CycloneDXComponent)
	for _, comp := range zipFiles {
		source := getDependencySource(comp.Name, directDeps)
		dependencySources[source] = append(dependencySources[source], comp)
	}

	fmt.Printf("Direct Dependencies:\n")
	if deps, ok := dependencySources["direct"]; ok {
		for _, dep := range deps {
			fmt.Printf("  ✅ %s@%s\n", dep.Name, dep.Version)
		}
	}

	fmt.Printf("\nTransitive Dependencies by Source:\n")
	for source, deps := range dependencySources {
		if source == "direct" {
			continue
		}
		fmt.Printf("  📦 %s: %d dependencies\n", source, len(deps))
		if len(deps) <= 5 {
			for _, dep := range deps {
				fmt.Printf("     - %s@%s\n", dep.Name, dep.Version)
			}
		} else {
			for _, dep := range deps[:5] {
				fmt.Printf("     - %s@%s\n", dep.Name, dep.Version)
			}
			fmt.Printf("     ... and %d more\n", len(deps)-5)
		}
	}

	// Recommendations
	fmt.Printf("\n💡 Recommendations:\n")
	fmt.Printf("===================\n\n")

	// Check for large dependency trees
	largestSource := ""
	largestCount := 0
	for source, deps := range dependencySources {
		if source != "direct" && len(deps) > largestCount {
			largestCount = len(deps)
			largestSource = source
		}
	}

	if largestCount > 10 {
		fmt.Printf("2. Consider alternatives for %s\n", largestSource)
		fmt.Printf("   Impact: %d transitive dependencies come from this package\n", largestCount)
		fmt.Printf("   Action: Evaluate if a lighter alternative exists\n\n")
	}
}

func filterByType(components []sbom.CycloneDXComponent, pkgType string) []sbom.CycloneDXComponent {
	var filtered []sbom.CycloneDXComponent
	for _, comp := range components {
		compType := ""
		for _, prop := range comp.Properties {
			if prop.Name == "packageType" {
				compType = prop.Value
				break
			}
		}
		// Also check sourceUrl for .mod files (legacy)
		if compType == "" {
			for _, prop := range comp.Properties {
				if prop.Name == "sourceUrl" && strings.HasSuffix(prop.Value, ".mod") {
					compType = "go"
					break
				}
			}
		}
		if compType == pkgType {
			filtered = append(filtered, comp)
		}
	}
	return filtered
}

func analyzeOtherComponents(components []sbom.CycloneDXComponent) {
	if len(components) == 0 {
		return
	}

	// Determine package type from first component
	pkgType := "unknown"
	for _, prop := range components[0].Properties {
		if prop.Name == "packageType" {
			pkgType = prop.Value
			break
		}
	}

	fmt.Printf("%s Dependencies Analysis\n", strings.ToUpper(pkgType))
	fmt.Printf("========================\n\n")
	fmt.Printf("Total Components: %d\n\n", len(components))

	// Group by package name
	byName := make(map[string][]sbom.CycloneDXComponent)
	for _, comp := range components {
		byName[comp.Name] = append(byName[comp.Name], comp)
	}

	// Show packages with multiple versions
	var multiVersion []string
	for name, comps := range byName {
		if len(comps) > 1 {
			multiVersion = append(multiVersion, name)
		}
	}

	if len(multiVersion) > 0 {
		fmt.Printf("📊 Packages with Multiple Versions: %d\n", len(multiVersion))
		sort.Strings(multiVersion)
		for _, name := range multiVersion[:min(10, len(multiVersion))] {
			versions := byName[name]
			fmt.Printf("   - %s (%d versions)\n", name, len(versions))
			for _, v := range versions {
				fmt.Printf("     • %s\n", v.Version)
			}
		}
		if len(multiVersion) > 10 {
			fmt.Printf("   ... and %d more\n", len(multiVersion)-10)
		}
		fmt.Println()
	}

	// Show all packages with their versions and licenses
	fmt.Printf("📦 Packages:\n")
	fmt.Printf("============\n\n")

	var packageList []string
	for name := range byName {
		packageList = append(packageList, name)
	}
	sort.Strings(packageList)

	withLicense := 0
	withoutLicense := 0

	for _, name := range packageList {
		comps := byName[name]
		for _, comp := range comps {
			licenseInfo := "No license"
			if len(comp.Licenses) > 0 {
				if comp.Licenses[0].License != nil && comp.Licenses[0].License.ID != "" {
					licenseInfo = comp.Licenses[0].License.ID
				} else if comp.Licenses[0].License != nil && comp.Licenses[0].License.Name != "" {
					licenseInfo = comp.Licenses[0].License.Name
				} else if comp.Licenses[0].Expression != "" {
					licenseInfo = comp.Licenses[0].Expression
				}
				withLicense++
			} else {
				withoutLicense++
			}

			fmt.Printf("  %s@%s", comp.Name, comp.Version)
			if licenseInfo != "No license" {
				fmt.Printf(" - License: %s", licenseInfo)
			} else {
				fmt.Printf(" - ⚠️  No license")
			}
			fmt.Println()
		}
	}

	if withoutLicense > 0 {
		fmt.Printf("\n⚠️  Packages without license: %d\n", withoutLicense)
	}
	if withLicense > 0 {
		fmt.Printf("✅ Packages with license: %d\n", withLicense)
	}
}

func getDirectDependencies() map[string]bool {
	// Read go.mod to get direct dependencies
	deps := make(map[string]bool)

	data, err := os.ReadFile("go.mod")
	if err != nil {
		return deps
	}

	lines := strings.Split(string(data), "\n")
	inRequire := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "require (" {
			inRequire = true
			continue
		}
		if inRequire && line == ")" {
			break
		}
		if inRequire && !strings.HasPrefix(line, "//") && line != "" {
			// Parse: module path version
			parts := strings.Fields(line)
			if len(parts) >= 1 {
				modulePath := parts[0]
				// Remove /vN suffix for matching
				basePath := getBaseModulePath(modulePath)
				deps[basePath] = true
			}
		}
	}

	return deps
}

func isDirectDependency(modulePath string, directDeps map[string]bool) bool {
	basePath := getBaseModulePath(modulePath)
	return directDeps[basePath]
}

func getDependencySource(modulePath string, directDeps []sbom.CycloneDXComponent) string {
	basePath := getBaseModulePath(modulePath)

	// Check if it's a direct dependency
	for _, dep := range directDeps {
		if getBaseModulePath(dep.Name) == basePath {
			return "direct"
		}
	}

	// Try to infer from module path
	// Most transitive deps come from go-git
	if strings.Contains(modulePath, "go-git") {
		return "github.com/go-git/go-git/v5"
	}
	if strings.Contains(modulePath, "golang.org/x/") {
		return "golang.org/x/net (or other x/ packages)"
	}

	return "unknown"
}

func getBaseModulePath(modulePath string) string {
	// Remove /vN suffix
	parts := strings.Split(modulePath, "/")
	if len(parts) == 0 {
		return modulePath
	}

	lastPart := parts[len(parts)-1]
	if strings.HasPrefix(lastPart, "v") && len(lastPart) > 1 {
		// Check if it's a version suffix (v followed by digits)
		isVersion := true
		for i := 1; i < len(lastPart); i++ {
			if lastPart[i] < '0' || lastPart[i] > '9' {
				isVersion = false
				break
			}
		}
		if isVersion {
			return strings.Join(parts[:len(parts)-1], "/")
		}
	}

	return modulePath
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
