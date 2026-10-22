package index

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// APTParser parses APT Packages files (supports .gz, .xz, and uncompressed)
type APTParser struct{}

// CanParse returns true if this is an APT Packages file
func (p *APTParser) CanParse(indexPath string) bool {
	// Check for explicit Packages file names
	if strings.HasSuffix(indexPath, "Packages.gz") ||
		strings.HasSuffix(indexPath, "Packages.xz") ||
		strings.HasSuffix(indexPath, "Packages") {
		return true
	}

	// Modern APT uses hash-based access: /dists/{suite}/{component}/binary-{arch}/by-hash/SHA256/...
	// These are Packages files accessed by hash, not by name
	if strings.Contains(indexPath, "/binary-") && strings.Contains(indexPath, "/by-hash/") {
		return true
	}

	// Also check for /binary-{arch}/Packages patterns
	if strings.Contains(indexPath, "/binary-") && (strings.Contains(indexPath, "Packages") || strings.Contains(indexPath, "by-hash")) {
		return true
	}

	return false
}

// Parse extracts package metadata from APT Packages format
// Format: Key: Value pairs separated by blank lines
// Supports .gz, .xz, and uncompressed formats
func (p *APTParser) Parse(data []byte, indexURL string, contentType string, cachePath string) ([]IndexPackage, error) {
	log.Printf("[apt-parser] Parsing index file: %s", indexURL)
	// Decompress based on Content-Type, URL extension, or magic bytes
	var err error
	originalSize := len(data)

	// Check Content-Type first (most reliable)
	if strings.Contains(contentType, "application/x-xz") || strings.Contains(contentType, "application/xz") {
		log.Printf("[apt-parser] Decompressing xz based on Content-Type: %s", contentType)
		data, err = readXZ(data)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress xz: %w", err)
		}
	} else if strings.Contains(contentType, "application/gzip") || strings.Contains(contentType, "application/x-gzip") {
		log.Printf("[apt-parser] Decompressing gzip based on Content-Type: %s", contentType)
		data, err = readGzip(data)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress gzip: %w", err)
		}
	} else if strings.HasSuffix(indexURL, ".gz") {
		// Fallback to URL extension
		log.Printf("[apt-parser] Decompressing gzip based on URL extension")
		data, err = readGzip(data)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress gzip: %w", err)
		}
	} else if strings.HasSuffix(indexURL, ".xz") {
		// Fallback to URL extension
		log.Printf("[apt-parser] Decompressing xz based on URL extension")
		data, err = readXZ(data)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress xz: %w", err)
		}
	} else {
		// No Content-Type or extension - detect compression from magic bytes
		// xz magic: FD 37 7A 58 5A 00
		// gzip magic: 1F 8B
		if len(data) >= 6 && data[0] == 0xFD && data[1] == 0x37 && data[2] == 0x7A && data[3] == 0x58 && data[4] == 0x5A && data[5] == 0x00 {
			// xz compressed
			log.Printf("[apt-parser] Decompressing xz based on magic bytes (Content-Type: '%s', URL: %s)", contentType, indexURL)
			data, err = readXZ(data)
			if err != nil {
				return nil, fmt.Errorf("failed to decompress xz: %w", err)
			}
		} else if len(data) >= 2 && data[0] == 0x1F && data[1] == 0x8B {
			// gzip compressed
			log.Printf("[apt-parser] Decompressing gzip based on magic bytes (Content-Type: '%s', URL: %s)", contentType, indexURL)
			data, err = readGzip(data)
			if err != nil {
				return nil, fmt.Errorf("failed to decompress gzip: %w", err)
			}
		} else {
			log.Printf("[apt-parser] No compression detected, using as-is (Content-Type: '%s', URL: %s, size: %d)", contentType, indexURL, originalSize)
		}
		// If no magic bytes match, assume uncompressed
	}

	var packages []IndexPackage
	entries := strings.Split(string(data), "\n\n") // Entries separated by blank lines
	log.Printf("[apt-parser] Split into %d entries (decompressed size: %d)", len(entries), len(data))

	for _, entry := range entries {
		if strings.TrimSpace(entry) == "" {
			continue
		}

		pkg := IndexPackage{
			Type:      "apt",
			IndexedAt: parseTimeFromIndex(entry),
		}

		lines := strings.Split(entry, "\n")
		inDescription := false
		var descriptionLines []string

		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				inDescription = false
				continue
			}

			// Check if this is a continuation of a multi-line field (starts with space)
			if inDescription && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
				// Continuation line - add to description
				descriptionLines = append(descriptionLines, strings.TrimSpace(line))
				continue
			}

			// Reset description state
			inDescription = false

			// Parse key-value pairs
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				continue
			}

			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])

			switch key {
			case "Package":
				pkg.Name = value
			case "Version":
				pkg.Version = value
			case "Architecture":
				pkg.Architecture = value
			case "Filename":
				pkg.Filename = value
				// Construct URL from index URL and filename
				baseURL := strings.TrimSuffix(indexURL, "/Packages.gz")
				baseURL = strings.TrimSuffix(baseURL, "/Packages.xz")
				baseURL = strings.TrimSuffix(baseURL, "/Packages")
				// Handle hash-based URLs: remove /by-hash/SHA256/... or /by-hash/SHA512/...
				if strings.Contains(baseURL, "/by-hash/") {
					// Extract everything before /by-hash/
					if idx := strings.Index(baseURL, "/by-hash/"); idx > 0 {
						baseURL = baseURL[:idx]
					}
				}
				// Extract repository structure from path
				// Format: /{base-path}/dists/{distribution}/{component}/binary-{arch}
				// Example: /debian-security/dists/bookworm-security/main/binary-amd64
				// Or: /debian/dists/bookworm/main/binary-amd64
				// Or: /debian/dists/bookworm-updates/main/binary-amd64
				if strings.Contains(baseURL, "/dists/") {
					// Split into base path and dists path
					parts := strings.Split(baseURL, "/dists/")
					if len(parts) > 1 {
						// Extract base path (e.g., /debian-security or /debian)
						basePath := parts[0]

						// Parse dists path: {distribution}/{component}/binary-{arch}
						// Or: {distribution}/updates/{component}/binary-{arch}
						distParts := strings.Split(parts[1], "/")

						// Build group string with hierarchy: basePath:distribution:component
						// This will be parsed in the UI to create the hierarchy
						var groupParts []string

						if len(distParts) >= 1 {
							// Add base path (e.g., debian-security or debian)
							basePathName := strings.TrimPrefix(basePath, "/")
							if basePathName != "" {
								groupParts = append(groupParts, basePathName)
							}

							// Add distribution (e.g., bookworm-security, bookworm, bookworm-updates)
							distribution := distParts[0]
							if distribution != "" {
								groupParts = append(groupParts, distribution)
							}

							// Extract component (e.g., "main", "contrib", "non-free")
							component := ""
							if len(distParts) >= 2 {
								// Check if this is a security/updates repository
								if distParts[1] == "updates" && len(distParts) >= 3 {
									// Security repository: suite/updates/component
									component = distParts[2]
								} else {
									// Regular repository: suite/component
									component = distParts[1]
								}
							}

							if component != "" {
								groupParts = append(groupParts, component)
							}

							// Join with : to create hierarchical group
							pkg.Group = strings.Join(groupParts, ":")
						}

						// Extract base URL (everything before /dists/)
						baseURL = basePath
					} else {
						// This index file doesn't have the expected /dists/ structure
						// Packages from this index will end up in the 'default' group
						log.Printf("[apt-parser] WARNING: Index URL does not contain /dists/: %s (packages will be in 'default' group)", baseURL)
					}
				}
				pkg.URL = baseURL + "/" + value
			case "Size":
				if size, err := strconv.ParseInt(value, 10, 64); err == nil {
					pkg.Size = size
				}
			case "Description":
				// Description can be multi-line: first line is summary, subsequent indented lines are full description
				descriptionLines = []string{value}
				inDescription = true
				// Continuation lines (starting with space/tab) will be handled in next iteration
			case "License":
				pkg.License = value
			case "Depends", "Pre-Depends":
				// Parse dependencies (comma-separated, with version constraints)
				deps := parseDependencies(value)
				pkg.Dependencies = append(pkg.Dependencies, deps...)
			}
		}

		// Join description lines
		if len(descriptionLines) > 0 {
			pkg.Description = strings.Join(descriptionLines, " ")
		}

		// Only add if we have at least name and version
		if pkg.Name != "" && pkg.Version != "" {
			packages = append(packages, pkg)
		} else {
			log.Printf("[apt-parser] Skipping entry: name='%s', version='%s'", pkg.Name, pkg.Version)
		}
	}

	// Log summary of groups
	groupCounts := make(map[string]int)
	for _, pkg := range packages {
		group := pkg.Group
		if group == "" {
			group = "default"
		}
		groupCounts[group]++
	}
	log.Printf("[apt-parser] Extracted %d packages from %s", len(packages), indexURL)
	for group, count := range groupCounts {
		log.Printf("[apt-parser]   - %d packages in group '%s'", count, group)
	}
	return packages, nil
}

// parseDependencies parses dependency strings like "libc6 (>= 2.14), libssl1.1"
func parseDependencies(depStr string) []string {
	var deps []string
	parts := strings.Split(depStr, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		// Remove version constraints like "(>= 2.14)"
		if idx := strings.Index(part, " ("); idx > 0 {
			part = part[:idx]
		}
		// Handle alternatives like "libfoo | libbar"
		if idx := strings.Index(part, " | "); idx > 0 {
			part = part[:idx]
		}
		if part != "" {
			deps = append(deps, strings.TrimSpace(part))
		}
	}
	return deps
}

// parseTimeFromIndex extracts timestamp from index entry (if available)
func parseTimeFromIndex(entry string) time.Time {
	// APT Packages files don't typically have timestamps in the entry itself
	// We'll use current time as a fallback
	return time.Now()
}
