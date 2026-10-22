package packages

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/sbom"
)

// getClientIP extracts the client IP address from the request
// Normalizes localhost IPs (127.0.0.1 and ::1) to "127.0.0.1" for consistency
func getClientIP(r *http.Request) string {
	var ip string

	// Check X-Forwarded-For header first (for proxies)
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded != "" {
		// X-Forwarded-For can contain multiple IPs, take the first one
		ips := strings.Split(forwarded, ",")
		if len(ips) > 0 {
			ip = strings.TrimSpace(ips[0])
			// Remove port if present
			if host, _, err := net.SplitHostPort(ip); err == nil {
				ip = host
			}
		}
	}

	// Fall back to RemoteAddr if no X-Forwarded-For
	if ip == "" {
		ip = r.RemoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}
	}

	// Normalize localhost IPs: both IPv4 (127.0.0.1) and IPv6 (::1) map to 127.0.0.1
	if ip == "::1" || ip == "[::1]" {
		ip = "127.0.0.1"
	}

	return ip
}

// trackArtifact tracks a package artifact for SBOM if there's an active session
func (b *Base) trackArtifact(r *http.Request, urlPath string, isIndex bool) {
	if b.sbomManager == nil || isIndex {
		return // No SBOM manager or index file, skip
	}

	// For Go packages, skip .mod files - they're dependency manifests, not actual packages
	// Only track .zip files which are the actual packages
	if b.config.Type == TypeGo && strings.HasSuffix(urlPath, ".mod") {
		return // Skip .mod files for Go packages
	}

	clientIP := getClientIP(r)
	session := b.sbomManager.GetSession(clientIP)
	if session == nil {
		// Try to get list of active sessions for debugging
		if listSessions, ok := b.sbomManager.(interface{ ListSessions() map[string][]string }); ok {
			activeSessions := listSessions.ListSessions()
			if len(activeSessions) > 0 {
				log.Printf("[sbom] No active session found for IP %s (path: %s). Active sessions: %v", clientIP, urlPath, activeSessions)
			} else {
				log.Printf("[sbom] No active session found for IP %s (path: %s). No active sessions at all.", clientIP, urlPath)
			}
		} else {
			log.Printf("[sbom] No active session found for IP %s (path: %s)", clientIP, urlPath)
		}
		return // No active session for this IP
	}
	// Found active session, proceed with tracking

	// Load metadata to get package information
	cachePath := b.getCachePath(urlPath)
	metadataPath := cachePath + ".meta"

	metaData, err := os.ReadFile(metadataPath)
	if err != nil {
		// No metadata file found, try to parse from path
		metadata := b.parsePackageInfo(urlPath)
		metadata.Type = string(b.config.Type)
		metadata.URLPath = urlPath
		// Try to load from cache if file exists
		if _, err := os.Stat(cachePath); err == nil {
			// File exists, try to extract more info
			metadata = b.enrichMetadataFromCache(metadata, cachePath)
		}
		if metadata.Name == "" {
			log.Printf("[sbom] Warning: Could not extract name from path %s, skipping tracking", urlPath)
			return
		}

		// Try to get enriched metadata from index database
		metadata = b.enrichMetadataFromIndex(metadata)

		// If metadata is missing license/description and we have name/version, enqueue for enrichment
		if b.enricher != nil && metadata.Name != "" && metadata.Version != "" {
			if metadata.License == "" || metadata.Description == "" {
				// Enqueue for enrichment if file exists
				if _, err := os.Stat(cachePath); err == nil {
					b.enricher.Enqueue(PackageToEnrich{
						Type:         b.config.Type,
						CachePath:    cachePath,
						Name:         metadata.Name,
						Version:      metadata.Version,
						Architecture: metadata.Architecture,
					})
					log.Printf("[sbom] Enqueued %s@%s for enrichment (missing license or description)", metadata.Name, metadata.Version)
				}
			}
		}

		b.trackArtifactFromMetadata(clientIP, metadata)
		return
	}

	var metadata Metadata
	if err := json.Unmarshal(metaData, &metadata); err != nil {
		// Fallback to path parsing if metadata file is corrupted
		metadata := b.parsePackageInfo(urlPath)
		metadata.Type = string(b.config.Type)
		metadata.URLPath = urlPath
		if metadata.Name == "" {
			log.Printf("[sbom] Warning: Could not extract name from path %s, skipping tracking", urlPath)
			return
		}

		// Try to get enriched metadata from index database
		metadata = b.enrichMetadataFromIndex(metadata)

		// If metadata is missing license/description and we have name/version, enqueue for enrichment
		if b.enricher != nil && metadata.Name != "" && metadata.Version != "" {
			if metadata.License == "" || metadata.Description == "" {
				// Enqueue for enrichment if file exists
				if _, err := os.Stat(cachePath); err == nil {
					b.enricher.Enqueue(PackageToEnrich{
						Type:         b.config.Type,
						CachePath:    cachePath,
						Name:         metadata.Name,
						Version:      metadata.Version,
						Architecture: metadata.Architecture,
					})
					log.Printf("[sbom] Enqueued %s@%s for enrichment (missing license or description)", metadata.Name, metadata.Version)
				}
			}
		}

		b.trackArtifactFromMetadata(clientIP, metadata)
		return
	}

	// Ensure URLPath is set (might be missing in old metadata files)
	if metadata.URLPath == "" {
		metadata.URLPath = urlPath
	}

	// Enrich with additional info from cache if available
	metadata = b.enrichMetadataFromCache(metadata, cachePath)

	// First, check if the .meta file already has enriched license/description
	// The enricher may have updated the .meta file directly
	if metadata.License == "" || metadata.Description == "" {
		// Re-read the .meta file in case it was updated by the enricher
		if metaData, err := os.ReadFile(metadataPath); err == nil {
			var updatedMeta Metadata
			if err := json.Unmarshal(metaData, &updatedMeta); err == nil {
				if updatedMeta.License != "" && metadata.License == "" {
					metadata.License = updatedMeta.License
				}
				if updatedMeta.Description != "" && metadata.Description == "" {
					metadata.Description = updatedMeta.Description
				}
			}
		}
	}

	// Try to get enriched metadata from index database (license, description)
	// This checks version-specific metadata from the index
	metadata = b.enrichMetadataFromIndex(metadata)

	// If metadata is missing license/description and we have name/version, enqueue for enrichment
	if b.enricher != nil && metadata.Name != "" && metadata.Version != "" {
		if metadata.License == "" || metadata.Description == "" {
			// Enqueue for enrichment if file exists
			if _, err := os.Stat(cachePath); err == nil {
				b.enricher.Enqueue(PackageToEnrich{
					Type:         b.config.Type,
					CachePath:    cachePath,
					Name:         metadata.Name,
					Version:      metadata.Version,
					Architecture: metadata.Architecture,
				})
				log.Printf("[sbom] Enqueued %s@%s for enrichment (missing license or description)", metadata.Name, metadata.Version)
			}
		}
	}

	b.trackArtifactFromMetadata(clientIP, metadata)
}

// enrichMetadataFromCache tries to extract additional metadata from the cached file
func (b *Base) enrichMetadataFromCache(metadata Metadata, cachePath string) Metadata {
	// Try to load license/description from index if available
	// This is a best-effort attempt - full metadata extraction is done via API
	return metadata
}

// enrichMetadataFromIndex tries to get enriched metadata (license, description) from the index database
// For v2 index, it checks the version-specific cache path and loads metadata from the .meta file
// This ensures we get version-specific license/description, not package-level
func (b *Base) enrichMetadataFromIndex(metadata Metadata) Metadata {
	if b.indexDB == nil || metadata.Name == "" || metadata.Version == "" {
		return metadata
	}

	// Get all packages of this type from index
	indexPkgs, err := b.indexDB.GetPackages(b.config.Type)
	if err != nil {
		return metadata // Can't access index, return as-is
	}

	// For Go packages, merge versions from /vN variants (the index may have separate entries)
	// The index may store packages with full paths (github.com/Microsoft/go-winio/v5)
	// but we need to merge them by base path (github.com/Microsoft/go-winio)
	if b.config.Type == TypeGo {
		indexPkgs = b.mergeGoPackageVersions(indexPkgs)
	}

	// For Go modules, normalize the name by stripping /vN suffix for comparison
	// The index stores Go modules with base path (without /vN) after merging, but tracking uses full path
	lookupName := metadata.Name
	if b.config.Type == TypeGo {
		lookupName = b.getGoBaseModulePath(metadata.Name)
		log.Printf("[sbom] Looking up Go package: original=%q, normalized=%q, version=%q", metadata.Name, lookupName, metadata.Version)
	}

	// Search for matching package by name and version
	for _, pkgInterface := range indexPkgs {
		// Use reflection to access fields without importing concrete types
		v := reflect.ValueOf(pkgInterface)
		if v.Kind() == reflect.Pointer {
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			continue
		}

		// Get Name field
		nameField := v.FieldByName("Name")
		if !nameField.IsValid() || nameField.Kind() != reflect.String {
			continue
		}

		pkgName := nameField.String()

		// For Go modules, also normalize the index name for comparison
		// After MergeGoPackageVersions, index names should already be base paths, but normalize anyway
		compareName := pkgName
		if b.config.Type == TypeGo {
			compareName = b.getGoBaseModulePath(pkgName)
		}

		if compareName != lookupName {
			if b.config.Type == TypeGo {
				log.Printf("[sbom] Go package name mismatch: lookup=%q, index=%q (normalized=%q)", lookupName, pkgName, compareName)
			}
			continue // Name doesn't match
		}

		if b.config.Type == TypeGo {
			log.Printf("[sbom] Found matching Go package: %q (index name: %q), checking versions...", lookupName, pkgName)
		}

		// For v2 index, check if version is in Versions array
		if v.FieldByName("Versions").IsValid() {
			// This is v2 format - check versions array for the specific version
			versionsField := v.FieldByName("Versions")
			if versionsField.Kind() == reflect.Slice {
				for i := 0; i < versionsField.Len(); i++ {
					versionElem := versionsField.Index(i)
					if versionElem.Kind() == reflect.Struct {
						versionValueField := versionElem.FieldByName("Version")
						cachePathField := versionElem.FieldByName("CachePath")

						if versionValueField.IsValid() && versionValueField.Kind() == reflect.String {
							if versionValueField.String() == metadata.Version {
								// Found matching version! Check for version-specific license/description in the index
								versionLicenseField := versionElem.FieldByName("License")
								versionDescField := versionElem.FieldByName("Description")

								if b.config.Type == TypeGo {
									log.Printf("[sbom] Found matching Go version %q for %q", metadata.Version, lookupName)
								}

								// Use version-specific license/description from index if available
								if versionLicenseField.IsValid() && versionLicenseField.Kind() == reflect.String && versionLicenseField.String() != "" {
									metadata.License = versionLicenseField.String()
									if b.config.Type == TypeGo {
										log.Printf("[sbom] Found version-specific license in index: %q", metadata.License)
									}
								}
								if versionDescField.IsValid() && versionDescField.Kind() == reflect.String && versionDescField.String() != "" {
									metadata.Description = versionDescField.String()
									if b.config.Type == TypeGo {
										log.Printf("[sbom] Found version-specific description in index (len=%d)", len(metadata.Description))
									}
								}

								// If we found version-specific metadata in index, return it
								if metadata.License != "" || metadata.Description != "" {
									if b.config.Type == TypeGo {
										log.Printf("[sbom] Returning enriched metadata for %q@%q: license=%v, description=%v", lookupName, metadata.Version, metadata.License != "", metadata.Description != "")
									}
									return metadata
								}

								// Fall back to checking .meta file for version-specific metadata
								// Note: cachePath in index might point to .zip file, but we might be tracking .mod file
								// Try both the index cachePath and our current cachePath
								if cachePathField.IsValid() && cachePathField.Kind() == reflect.String {
									cachePath := cachePathField.String()
									if cachePath != "" {
										// Load metadata from .meta file for this specific version
										// Try the cache path from index first (usually .zip file)
										metaPath := cachePath + ".meta"
										if metaData, err := os.ReadFile(metaPath); err == nil {
											var versionMeta Metadata
											if err := json.Unmarshal(metaData, &versionMeta); err == nil {
												// Use version-specific license/description if available
												if versionMeta.License != "" {
													metadata.License = versionMeta.License
												}
												if versionMeta.Description != "" {
													metadata.Description = versionMeta.Description
												}
												// If we found version-specific metadata, return it
												if metadata.License != "" || metadata.Description != "" {
													if b.config.Type == TypeGo {
														log.Printf("[sbom] Found version-specific metadata in .meta file for %q@%q (from index cachePath)", lookupName, metadata.Version)
													}
													return metadata
												}
											}
										}

										// Also try our current cachePath (might be .mod file, but .zip has the metadata)
										// Convert .mod path to .zip path for lookup
										if strings.HasSuffix(metadata.URLPath, ".mod") {
											zipPath := strings.TrimSuffix(metadata.URLPath, ".mod") + ".zip"
											zipCachePath := b.getCachePath(zipPath)
											zipMetaPath := zipCachePath + ".meta"
											if zipMetaData, err := os.ReadFile(zipMetaPath); err == nil {
												var zipMeta Metadata
												if err := json.Unmarshal(zipMetaData, &zipMeta); err == nil {
													if zipMeta.License != "" {
														metadata.License = zipMeta.License
													}
													if zipMeta.Description != "" {
														metadata.Description = zipMeta.Description
													}
													if metadata.License != "" || metadata.Description != "" {
														if b.config.Type == TypeGo {
															log.Printf("[sbom] Found version-specific metadata in .meta file for %q@%q (from .zip file)", lookupName, metadata.Version)
														}
														return metadata
													}
												}
											}
										}
									}
								}

								// Fall back to package-level license/description if version-specific not found
								licenseField := v.FieldByName("License")
								descField := v.FieldByName("Description")
								if licenseField.IsValid() && licenseField.Kind() == reflect.String && licenseField.String() != "" {
									metadata.License = licenseField.String()
								}
								if descField.IsValid() && descField.Kind() == reflect.String && descField.String() != "" {
									metadata.Description = descField.String()
								}
								return metadata
							}
						}
					}
				}
			}
		} else {
			// v1 format - direct name/version match
			versionField := v.FieldByName("Version")
			if versionField.IsValid() && versionField.Kind() == reflect.String {
				if versionField.String() == metadata.Version {
					// Found matching package, use enriched metadata
					licenseField := v.FieldByName("License")
					descField := v.FieldByName("Description")
					if licenseField.IsValid() && licenseField.Kind() == reflect.String && licenseField.String() != "" {
						metadata.License = licenseField.String()
					}
					if descField.IsValid() && descField.Kind() == reflect.String && descField.String() != "" {
						metadata.Description = descField.String()
					}
					return metadata
				}
			}
		}
	}

	return metadata
}

// mergeGoPackageVersions merges versions from /vN variants for Go packages
// Groups packages by base module path (strips /vN suffix) and merges versions
func (b *Base) mergeGoPackageVersions(pkgs []interface{}) []interface{} {
	if len(pkgs) == 0 {
		return pkgs
	}

	// Group by base module path
	grouped := make(map[string]interface{})

	for _, pkgInterface := range pkgs {
		v := reflect.ValueOf(pkgInterface)
		if v.Kind() == reflect.Pointer {
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			// Not a struct, keep as-is
			key := fmt.Sprintf("%v", pkgInterface)
			if grouped[key] == nil {
				grouped[key] = pkgInterface
			}
			continue
		}

		// Get Type and Name fields
		typeField := v.FieldByName("Type")
		nameField := v.FieldByName("Name")
		if !typeField.IsValid() || !nameField.IsValid() || nameField.Kind() != reflect.String {
			// Can't determine type/name, keep as-is
			key := fmt.Sprintf("%v", pkgInterface)
			if grouped[key] == nil {
				grouped[key] = pkgInterface
			}
			continue
		}

		pkgType := ""
		if typeField.Kind() == reflect.String {
			pkgType = typeField.String()
		}

		if pkgType != "go" {
			// Not a Go package, keep as-is
			pkgName := nameField.String()
			if grouped[pkgName] == nil {
				grouped[pkgName] = pkgInterface
			}
			continue
		}

		// Get base module path (strip /vN)
		pkgName := nameField.String()
		basePath := b.getGoBaseModulePath(pkgName)

		// Use first package with this base path (skip duplicates from /vN variants)
		// TODO: Implement merge logic to combine versions from different module path variants
		if grouped[basePath] == nil {
			grouped[basePath] = pkgInterface
		}

	}
	// Convert back to slice
	var result []interface{}
	for _, pkg := range grouped {
		result = append(result, pkg)
	}

	return result
}

// getGoBaseModulePath strips /vN suffix from Go module path for matching
// e.g., "github.com/Microsoft/go-winio/v5" -> "github.com/Microsoft/go-winio"
// e.g., "github.com/Microsoft/go-winio" -> "github.com/Microsoft/go-winio"
func (b *Base) getGoBaseModulePath(modulePath string) string {
	parts := strings.Split(modulePath, "/")
	if len(parts) == 0 {
		return modulePath
	}

	lastPart := parts[len(parts)-1]
	// Check if last part matches /vN pattern (where N is a digit)
	if strings.HasPrefix(lastPart, "v") && len(lastPart) > 1 {
		isVersionSuffix := true
		for i := 1; i < len(lastPart); i++ {
			if lastPart[i] < '0' || lastPart[i] > '9' {
				isVersionSuffix = false
				break
			}
		}
		if isVersionSuffix {
			// Remove the /vN part
			return strings.Join(parts[:len(parts)-1], "/")
		}
	}
	return modulePath
}

// trackArtifactFromMetadata creates an artifact from metadata and tracks it
func (b *Base) trackArtifactFromMetadata(clientIP string, metadata Metadata) {
	artifact := sbom.Artifact{
		Type:         "package",
		PackageType:  string(b.config.Type),
		Name:         metadata.Name,
		Version:      metadata.Version,
		Architecture: metadata.Architecture,
		License:      metadata.License,
		Description:  metadata.Description,
		URL:          metadata.URLPath,
	}

	// Only track if we have at least a name
	if artifact.Name == "" {
		// Try to extract name from URL path as fallback
		filename := filepath.Base(metadata.URLPath)
		if filename != "" && filename != "/" {
			artifact.Name = filename
		} else {
			return // Can't track without a name
		}
	}

	b.sbomManager.AddArtifact(clientIP, artifact)
	if artifact.License != "" || artifact.Description != "" {
		log.Printf("[sbom] Tracked %s@%s (%s) with metadata (license: %v, description: %v)", artifact.Name, artifact.Version, artifact.PackageType, artifact.License != "", artifact.Description != "")
	} else {
		log.Printf("[sbom] Tracked %s@%s (%s) - enrichment queued", artifact.Name, artifact.Version, artifact.PackageType)
	}
}
