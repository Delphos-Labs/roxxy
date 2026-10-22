package index

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// APKParser parses APK APKINDEX.tar.gz files
type APKParser struct{}

// CanParse returns true if this is an APK APKINDEX.tar.gz file
func (p *APKParser) CanParse(indexPath string) bool {
	return strings.HasSuffix(indexPath, "APKINDEX.tar.gz")
}

// Parse extracts package metadata from APKINDEX.tar.gz
// Format: Each package entry starts with C: (checksum) and contains fields like:
//
//	C: checksum
//	P: Package name
//	V: Version
//	A: Architecture
//	S: Size
//	I: Installed size
//	T: Description
//	U: URL
//	L: License
//	o: Origin
//	m: Maintainer
//	t: Timestamp
//	c: Commit hash
//	D: Dependencies
//	p: Provides
//	i: Install if (for subpackages)
func (p *APKParser) Parse(data []byte, indexURL string, contentType string, cachePath string) ([]IndexPackage, error) {
	log.Printf("[apk-parser] Parsing index file: %s", indexURL)

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

	var packages []IndexPackage
	content := string(apkindexData)

	// Split entries by C: line (each entry starts with C:)
	entries := strings.Split(content, "\nC:")
	if len(entries) == 0 {
		return packages, nil
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

		pkg := IndexPackage{
			Type:      "apk",
			IndexedAt: time.Now(),
		}

		lines := strings.Split(entry, "\n")
		for _, line := range lines {
			if len(line) < 2 || line[1] != ':' {
				continue
			}

			key := line[0:1]
			value := strings.TrimSpace(line[2:])

			switch key {
			case "P": // Package name
				pkg.Name = value
			case "V": // Version
				pkg.Version = value
			case "A": // Architecture
				pkg.Architecture = value
			case "S": // Size
				if size, err := strconv.ParseInt(value, 10, 64); err == nil {
					pkg.Size = size
				}
			case "T": // Description
				pkg.Description = value
			case "L": // License
				pkg.License = value
			case "U": // URL
				pkg.URL = value
			case "D": // Dependencies
				// Dependencies are space-separated, format: "so:libc.musl-x86_64.so.1 so:libxcb.so.1"
				// or "cmd:2bwm=0.3-r3" for command dependencies
				deps := strings.Fields(value)
				for _, dep := range deps {
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
						pkg.Dependencies = append(pkg.Dependencies, dep)
					}
				}
			}
		}

		// Only add if we have at least name and version
		if pkg.Name != "" && pkg.Version != "" {
			// Construct the actual package download URL from the index file path
			// Index file: /alpine/v3.23/main/x86_64/APKINDEX.tar.gz
			// Package: /alpine/v3.23/main/x86_64/package-version-release.apk
			// Extract the base path from the index URL (everything before APKINDEX.tar.gz)
			basePath := strings.TrimSuffix(indexURL, "/APKINDEX.tar.gz")
			if basePath != indexURL {
				// Construct package filename: name-version-release.apk
				// Version format is typically "version-release" (e.g., "2.45.1-r0")
				packageFilename := fmt.Sprintf("%s-%s.apk", pkg.Name, pkg.Version)
				// Construct full download URL
				downloadURL := basePath + "/" + packageFilename
				// Store download URL in URL field (overwrite homepage URL)
				pkg.URL = downloadURL
				if pkg.Filename == "" {
					pkg.Filename = packageFilename
				}
			} else {
				// Fallback: construct filename if we can't determine from index URL
				if pkg.Filename == "" {
					pkg.Filename = fmt.Sprintf("%s-%s-%s.apk", pkg.Name, pkg.Version, pkg.Architecture)
				}
			}
			packages = append(packages, pkg)
		}
	}

	log.Printf("[apk-parser] Extracted %d packages from %s", len(packages), indexURL)
	return packages, nil
}
