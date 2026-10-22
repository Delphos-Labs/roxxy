package packages

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// Shared HTTP client for upstream fetches - reuses connections
var (
	upstreamClient     *http.Client
	upstreamClientOnce sync.Once
)

// getUpstreamClient returns a shared HTTP client configured for direct upstream connections
// This client bypasses any proxy by resolving hostnames to IPs and connecting directly
func getUpstreamClient() *http.Client {
	upstreamClientOnce.Do(func() {
		dialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		// Custom dial function that resolves hostname to IP and connects directly
		// This bypasses Docker's hostname-based transparent proxy
		dialFunc := func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}

			// Resolve hostname to IP addresses
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				log.Printf("[upstream] DNS lookup failed for %s: %v", host, err)
				return nil, err
			}

			// Try each IP until one works
			var lastErr error
			for _, ip := range ips {
				if ip.To4() == nil {
					continue // Skip IPv6 for now
				}
				ipAddr := net.JoinHostPort(ip.String(), port)
				log.Printf("[upstream] Connecting directly to IP: %s (hostname: %s)", ipAddr, host)
				conn, err := dialer.DialContext(ctx, network, ipAddr)
				if err != nil {
					lastErr = err
					continue
				}
				log.Printf("[upstream] Connected: %s -> %s", conn.LocalAddr(), conn.RemoteAddr())
				return conn, nil
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("no usable IP addresses for %s", host)
		}
		// Use system cert pool to trust certificates from /usr/local/share/ca-certificates/
		systemCertPool, err := x509.SystemCertPool()
		if err != nil {
			// Fallback to empty pool if system pool is unavailable
			systemCertPool = x509.NewCertPool()
		}
		transport := &http.Transport{
			Proxy:       config.GetUpstreamProxyFunc(), // Use configurable proxy (or nil if not set)
			DialContext: dialFunc,
			TLSClientConfig: &tls.Config{
				RootCAs:            systemCertPool,
				InsecureSkipVerify: false,
			},
			DisableKeepAlives:     false,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
		upstreamClient = &http.Client{
			Transport: transport,
			// No overall timeout - we use activity-based timeout per chunk instead
			// This allows unlimited download time as long as data keeps flowing
		}
		log.Printf("[upstream] Initialized shared HTTP client (direct IP connections, bypasses proxy)")
	})
	return upstreamClient
}

// Type identifies different package manager protocols
type Type string

const (
	TypeAPK  Type = "apk"
	TypeAPT  Type = "apt"
	TypeNPM  Type = "npm"
	TypePyPI Type = "pypi"
	TypeGo   Type = "go"
)

// Config holds configuration for a package manager proxy
type Config struct {
	Type           Type
	UpstreamURL    string        // Base URL for upstream (e.g., "https://dl-cdn.alpinelinux.org")
	CacheDir       string        // Local cache directory
	IndexTTL       time.Duration // TTL for index files
	MinCacheSize   int64         // Minimum size to cache
	AllowedOrigins []string      // For CORS if needed
	Airgapped      bool          // No external access, return 404 for missing artifacts
}

// SBOMManagerInterface is an interface for SBOM tracking
type SBOMManagerInterface interface {
	GetSession(ip string) interface{}            // Returns session if exists, nil otherwise
	AddArtifact(ip string, artifact interface{}) // Artifact should be sbom.Artifact
}

// PackageEnricherInterface is an interface for the package enricher
type PackageEnricherInterface interface {
	Enqueue(pkg PackageToEnrich)
}

// Base provides shared functionality for package manager proxies
type Base struct {
	config       Config
	statsTracker StatsTrackerInterface    // Optional stats tracker for central stats
	indexDB      IndexDBInterface         // Optional index database for parsing index files
	sbomManager  SBOMManagerInterface     // Optional SBOM session manager
	enricher     PackageEnricherInterface // Optional package enricher for background processing
}

// StatsTrackerInterface is an interface for tracking access statistics
// This allows Base to work with or without a stats tracker
type StatsTrackerInterface interface {
	IncrementAccessCount(key string)
}

// IndexDBInterface is an interface for parsing and storing index file metadata
type IndexDBInterface interface {
	ParseIndexFile(cachePath string, indexURL string, pmType Type, contentType string) error
	GetPackages(pmType Type) ([]interface{}, error) // Returns []IndexPackage but using interface{} to avoid import cycle
	UpdatePackageMetadata(pmType Type, name, version string, updates func(interface{})) error
}

// NewBase creates a new base package manager proxy
func NewBase(config Config, statsTracker StatsTrackerInterface, indexDB IndexDBInterface, sbomManager SBOMManagerInterface, enricher PackageEnricherInterface) *Base {
	// Ensure cache directory exists
	// Only try to create if CacheDir is not empty
	if config.CacheDir != "" {
		// Ensure parent directory exists first (MkdirAll should handle this, but be explicit on Windows)
		if err := os.MkdirAll(config.CacheDir, 0755); err != nil {
			log.Printf("Warning: Failed to create cache dir %s: %v (type: %s)", config.CacheDir, err, config.Type)
		}
	} else {
		log.Printf("Warning: CacheDir is empty in config for type %s - cache operations may fail", config.Type)
	}

	return &Base{
		config:       config,
		statsTracker: statsTracker,
		indexDB:      indexDB,
		sbomManager:  sbomManager,
		enricher:     enricher,
	}
}

// GetCached retrieves a cached file
// isIndex indicates if this is an index file (subject to TTL)
// Returns the file and a boolean indicating if the cache is expired (for index files)
func (b *Base) GetCached(path string, isIndex bool) (*os.File, bool, error) {
	cachePath := b.getCachePath(path)

	// Check if file exists
	stat, err := os.Stat(cachePath)
	if err != nil {
		return nil, false, err
	}

	// Check if expired
	expired := false
	age := time.Since(stat.ModTime())

	if isIndex {
		// For index files, implement stale-while-revalidate with hard cutoff:
		// - If age > TTL: fully expired, don't serve stale (return error)
		// - If age > TTL/2: partially expired, serve stale and refresh in background
		if age > b.config.IndexTTL {
			// Fully expired - metadata may reference packages that no longer exist
			log.Printf("  Cache fully expired (age: %v, TTL: %v): %s (blocking, must fetch fresh)", age, b.config.IndexTTL, path)
			return nil, false, fmt.Errorf("cache fully expired")
		} else if age > b.config.IndexTTL/2 {
			// Partially expired - serve stale, refresh in background
			expired = true
			log.Printf("  Cache partially expired (age: %v, TTL/2: %v): %s (serving stale, refreshing in background)", age, b.config.IndexTTL/2, path)
		}
	}

	// Increment access count for non-index files
	if !isIndex {
		b.incrementAccessCount(cachePath)
	}

	file, err := os.Open(cachePath)
	return file, expired, err
}

// PutCached stores a file in the cache
func (b *Base) PutCached(path string, data []byte, isIndex bool, fetchDuration time.Duration, contentType string) error {
	cachePath := b.getCachePath(path)

	// Create directory structure
	dir := filepath.Dir(cachePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create cache dir: %w", err)
	}

	// Log cache path for debugging
	log.Printf("[%s] Caching file: path=%s, cachePath=%s, size=%d, isIndex=%v, cacheDir=%s",
		b.config.Type, path, cachePath, len(data), isIndex, b.config.CacheDir)

	// Write file
	if err := os.WriteFile(cachePath, data, 0644); err != nil {
		log.Printf("[%s] ERROR: Failed to write cache file %s: %v", b.config.Type, cachePath, err)
		return fmt.Errorf("failed to write cache file: %w", err)
	}

	log.Printf("[%s] Successfully cached file: %s", b.config.Type, cachePath)

	// Save metadata for all files (both index and packages)
	// This allows us to determine after the fact if a file is an index file
	metadata := b.parsePackageInfo(path)
	metadata.Type = string(b.config.Type)
	metadata.CachedAt = time.Now()
	metadata.FetchDuration = fetchDuration
	metadata.IsIndex = isIndex
	metadata.ContentType = contentType
	if !isIndex {
		metadata.AccessCount = 1 // First access (the fetch itself)
	} else {
		metadata.AccessCount = 0 // Index files don't track access count
	}
	util.Logerr(b.saveMetadata(cachePath, metadata))

	// Index parsing is now done on-demand via UI API, not automatically

	// Enqueue package for enrichment if it's not an index file and we have name/version
	if !isIndex && b.enricher != nil && metadata.Name != "" && metadata.Version != "" {
		b.enricher.Enqueue(PackageToEnrich{
			Type:         b.config.Type,
			CachePath:    cachePath,
			Name:         metadata.Name,
			Version:      metadata.Version,
			Architecture: metadata.Architecture,
		})
	}

	return nil
}

// incrementAccessCount increments the access count for a cached package
func (b *Base) incrementAccessCount(cachePath string) {
	metadataPath := cachePath + ".meta"

	// Load existing metadata
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return // No metadata file, skip
	}

	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return // Failed to parse, skip
	}

	// Increment count
	metadata.AccessCount++

	// Save updated metadata
	util.Logerr(b.saveMetadata(cachePath, metadata))

	// Also track in central stats tracker if available
	// Use a key that includes package type and identifier for uniqueness
	if b.statsTracker != nil {
		// Create a unique key: {type}:{name}@{version} or {type}:{urlPath}
		statsKey := fmt.Sprintf("%s:%s", b.config.Type, metadata.URLPath)
		if metadata.Name != "" && metadata.Version != "" {
			statsKey = fmt.Sprintf("%s:%s@%s", b.config.Type, metadata.Name, metadata.Version)
		}
		b.statsTracker.IncrementAccessCount(statsKey)
	}
}

// parsePackageInfo extracts package name, version, and architecture from URL path
func (b *Base) parsePackageInfo(urlPath string) Metadata {
	metadata := Metadata{
		URLPath: urlPath,
	}

	filename := filepath.Base(urlPath)

	switch b.config.Type {
	case TypeAPK:
		// Format: package-1.0-r0.apk
		// Extract from path: /alpine/v3.19/main/x86_64/package-1.0-r0.apk
		parts := strings.Split(urlPath, "/")
		if len(parts) >= 5 {
			metadata.Architecture = parts[len(parts)-2]
		}
		// Parse filename: package-name-version-release.apk
		// APK format: {name}-{version}-{release}.apk
		// Version can contain dashes, but release is typically a single segment like "r0" or "r1"
		// We need to find the last dash that separates version-release from the name
		if strings.HasSuffix(filename, ".apk") {
			name := strings.TrimSuffix(filename, ".apk")
			// Try to find version pattern: look for a dash followed by a digit (version starts with digit)
			// Then find the next dash which separates version from release
			// Example: "alpine-baselayout-3.6.0-r0" -> name="alpine-baselayout", version="3.6.0-r0"
			// Strategy: find the last dash, check if what follows looks like a release (starts with 'r' and digits)
			// If so, find the previous dash to get version-release together
			idx := strings.LastIndex(name, "-")
			if idx > 0 {
				releasePart := name[idx+1:]
				// Check if this looks like a release (starts with 'r' followed by digits, or just digits)
				if len(releasePart) > 0 && (releasePart[0] == 'r' || (releasePart[0] >= '0' && releasePart[0] <= '9')) {
					// This is likely the release part, so find the previous dash for version
					remaining := name[:idx]
					idx2 := strings.LastIndex(remaining, "-")
					if idx2 > 0 {
						// Check if what follows looks like a version (starts with digit)
						versionPart := remaining[idx2+1:]
						if len(versionPart) > 0 && (versionPart[0] >= '0' && versionPart[0] <= '9') {
							metadata.Name = remaining[:idx2]
							metadata.Version = remaining[idx2+1:] + "-" + releasePart
						} else {
							// Fallback: treat everything before last dash as name, last part as version
							metadata.Name = remaining
							metadata.Version = versionPart + "-" + releasePart
						}
					} else {
						// Only one dash, treat as name-version
						metadata.Name = remaining
						metadata.Version = releasePart
					}
				} else {
					// Doesn't look like a release, treat as name-version
					metadata.Name = name[:idx]
					metadata.Version = name[idx+1:]
				}
			} else {
				metadata.Name = name
			}
		}

	case TypeAPT:
		// Format: package_1.18.0-0ubuntu1_amd64.deb
		if strings.HasSuffix(filename, ".deb") {
			name := strings.TrimSuffix(filename, ".deb")
			parts := strings.Split(name, "_")
			if len(parts) >= 3 {
				metadata.Name = parts[0]
				metadata.Version = parts[1]
				metadata.Architecture = parts[2]
			} else if len(parts) == 2 {
				metadata.Name = parts[0]
				metadata.Version = parts[1]
			}
		}

	case TypeNPM:
		// Format: /express/-/express-4.18.2.tgz
		// Format (scoped): /@apidevtools/swagger-parser/-/swagger-parser-10.0.3.tgz
		parts := strings.Split(strings.Trim(urlPath, "/"), "/")
		if len(parts) >= 2 {
			// Check if this is a scoped package (starts with @)
			if strings.HasPrefix(parts[0], "@") {
				// Scoped package: @scope/package
				if len(parts) >= 2 {
					metadata.Name = parts[0] + "/" + parts[1]
				} else {
					metadata.Name = parts[0]
				}
			} else {
				// Regular package
				metadata.Name = parts[0]
			}
		}
		// Parse version from filename: package-version.tgz
		if strings.HasSuffix(filename, ".tgz") {
			name := strings.TrimSuffix(filename, ".tgz")
			idx := strings.LastIndex(name, "-")
			if idx > 0 {
				metadata.Version = name[idx+1:]
			}
		}

	case TypePyPI:
		// PyPI supports multiple formats:
		// - Wheel: {distribution}-{version}(-{build})?-{python}-{abi}-{platform}.whl
		// - Source: {distribution}-{version}.tar.gz
		// - Zip: {distribution}-{version}.zip
		// - Egg: {distribution}-{version}(-{build})?.egg

		// Try to extract name and version from various formats
		var baseName string
		var ext string

		if strings.HasSuffix(filename, ".whl") {
			ext = ".whl"
			baseName = strings.TrimSuffix(filename, ".whl")
			// Wheel format: {distribution}-{version}(-{build})?-{python}-{abi}-{platform}
			parts := strings.Split(baseName, "-")
			if len(parts) >= 2 {
				metadata.Name = parts[0]
				metadata.Version = parts[1]
			}
			if len(parts) >= 5 {
				metadata.Architecture = parts[len(parts)-1]
			}
		} else if strings.HasSuffix(filename, ".tar.gz") {
			ext = ".tar.gz"
			baseName = strings.TrimSuffix(filename, ".tar.gz")
		} else if strings.HasSuffix(filename, ".zip") {
			ext = ".zip"
			baseName = strings.TrimSuffix(filename, ".zip")
		} else if strings.HasSuffix(filename, ".egg") {
			ext = ".egg"
			baseName = strings.TrimSuffix(filename, ".egg")
		} else {
			// Unknown format, try to extract from filename anyway
			baseName = filename
		}

		// For non-wheel formats, try to extract name-version pattern
		if metadata.Name == "" && ext != ".whl" {
			// Try to find the last dash that separates name from version
			// Version typically starts with a digit
			idx := strings.LastIndex(baseName, "-")
			if idx > 0 {
				// Check if the part after the dash looks like a version (starts with digit)
				potentialVersion := baseName[idx+1:]
				if len(potentialVersion) > 0 && (potentialVersion[0] >= '0' && potentialVersion[0] <= '9') {
					metadata.Name = baseName[:idx]
					metadata.Version = potentialVersion
				} else {
					// Might be a build tag or other suffix, try second-to-last dash
					remaining := baseName[:idx]
					idx2 := strings.LastIndex(remaining, "-")
					if idx2 > 0 {
						potentialVersion = remaining[idx2+1:]
						if len(potentialVersion) > 0 && (potentialVersion[0] >= '0' && potentialVersion[0] <= '9') {
							metadata.Name = remaining[:idx2]
							metadata.Version = potentialVersion
						}
					}
				}
			}
		}

		// If still no name, use filename as fallback
		if metadata.Name == "" {
			metadata.Name = baseName
		}

	case TypeGo:
		// Format: /module/@v/v1.2.3.zip or /module/@v/v1.2.3.info
		// Extract module name from path: /github.com/user/repo/@v/v1.2.3.zip
		parts := strings.Split(urlPath, "/")
		if len(parts) >= 2 {
			// Module path is everything before /@v/
			modulePath := ""
			for i, part := range parts {
				if part == "@v" {
					if i > 0 {
						modulePath = strings.Join(parts[1:i], "/")
					}
					break
				}
			}
			if modulePath != "" {
				// First URL decode (handles %XX encoding)
				decodedPath, err := url.PathUnescape(modulePath)
				if err == nil {
					modulePath = decodedPath
				}
				// Then decode Go module proxy encoding (!lowercase -> Uppercase)
				metadata.Name = decodeGoModulePath(modulePath)
				// For Go packages, don't set Group here - it will be derived in listPackagesByType
				// using the parent path with / separator
			} else {
				// Fallback: use first non-empty part
				for _, part := range parts[1:] {
					if part != "" && part != "@v" {
						metadata.Name = part
						break
					}
				}
			}
		}
		// Extract version from filename: v1.2.3.zip, v1.2.3.info, v1.2.3.mod
		if strings.HasSuffix(filename, ".zip") || strings.HasSuffix(filename, ".info") || strings.HasSuffix(filename, ".mod") {
			version := strings.TrimSuffix(filename, ".zip")
			version = strings.TrimSuffix(version, ".info")
			version = strings.TrimSuffix(version, ".mod")
			if strings.HasPrefix(version, "v") {
				metadata.Version = version
			}
		}
	}

	return metadata
}

// decodeGoModulePath decodes Go module proxy path encoding
// The Go module proxy encodes uppercase letters as ! followed by lowercase
// e.g., !proton!mail -> ProtonMail, !microsoft -> Microsoft
func decodeGoModulePath(path string) string {
	var result strings.Builder
	i := 0
	for i < len(path) {
		if path[i] == '!' && i+1 < len(path) {
			// Check if next character is lowercase
			next := path[i+1]
			if next >= 'a' && next <= 'z' {
				// Convert to uppercase
				result.WriteByte(next - ('a' - 'A'))
				i += 2
				continue
			}
		}
		result.WriteByte(path[i])
		i++
	}
	return result.String()
}

// saveMetadata saves package metadata to a .meta file
func (b *Base) saveMetadata(cachePath string, metadata Metadata) error {
	metaPath := cachePath + ".meta"
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath, data, 0644)
}

// FixCachedMetadata fixes existing cached metadata files for Go packages
// This fixes issues like URL-encoded characters in package names and incorrect group fields
func FixCachedMetadata(cacheDir string, pmType Type) (int, error) {
	if pmType != TypeGo {
		return 0, nil // Only fix Go packages for now
	}

	typeDir := filepath.Join(cacheDir, "packages", string(pmType))
	if _, err := os.Stat(typeDir); os.IsNotExist(err) {
		return 0, nil // No cache directory for this type
	}

	var fixedCount int
	err := filepath.Walk(typeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		// Only process .meta files
		if !strings.HasSuffix(path, ".meta") {
			return nil
		}

		// Read metadata
		data, err := os.ReadFile(path)
		if err != nil {
			return nil // Skip files we can't read
		}

		var metadata Metadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return nil // Skip invalid metadata
		}

		// Only process Go packages
		if metadata.Type != string(TypeGo) {
			return nil
		}

		// Skip index files
		if metadata.IsIndex {
			return nil
		}

		// Check if Name needs fixing
		needsFix := false
		fixedName := metadata.Name

		// If Name contains !, decode Go module proxy encoding
		if strings.Contains(metadata.Name, "!") {
			fixedName = decodeGoModulePath(metadata.Name)
			if fixedName != metadata.Name {
				needsFix = true
			}
		}

		// Check if Group needs to be cleared (it will be derived correctly in listPackagesByType)
		if metadata.Group != "" && strings.Contains(metadata.Group, ":") {
			// Old format with : separator, clear it
			needsFix = true
		}

		if needsFix {
			// Update metadata
			metadata.Name = fixedName
			metadata.Group = "" // Clear group, it will be derived correctly

			// Write back
			newData, err := json.Marshal(metadata)
			if err != nil {
				return nil // Skip if we can't marshal
			}

			if err := os.WriteFile(path, newData, 0644); err != nil {
				log.Printf("Warning: Failed to fix metadata file %s: %v", path, err)
				return nil
			}

			fixedCount++
			if fixedCount <= 10 {
				log.Printf("[fix-metadata] Fixed %s: Name=%s (was: %s)", filepath.Base(path), fixedName, metadata.Name)
			}
		}

		return nil
	})

	if err != nil {
		return fixedCount, fmt.Errorf("error walking cache directory: %w", err)
	}

	return fixedCount, nil
}

// getCachePath returns the filesystem path for a cached URL path
func (b *Base) getCachePath(urlPath string) string {
	// Use content-addressable storage for non-index files
	// Index files are stored by path for easy TTL checks
	hash := sha256.Sum256([]byte(urlPath))
	hashStr := hex.EncodeToString(hash[:])

	// Create directory structure: cache/type/hash[0:2]/hash[2:4]/hash
	return filepath.Join(
		b.config.CacheDir,
		string(b.config.Type),
		hashStr[0:2],
		hashStr[2:4],
		hashStr,
	)
}

// FormatBytes formats a byte size as a human-readable string
func FormatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// Metadata holds package metadata
type Metadata struct {
	Type          string        `json:"type"`
	Name          string        `json:"name"`
	Version       string        `json:"version"`
	Architecture  string        `json:"architecture"`
	Group         string        `json:"group,omitempty"` // Component/group (e.g., "main", "security", "contrib", or hierarchical like "hostname:group:package")
	Description   string        `json:"description,omitempty"`
	License       string        `json:"license,omitempty"`
	URLPath       string        `json:"url_path"`
	ContentType   string        `json:"content_type,omitempty"` // HTTP Content-Type header
	CachedAt      time.Time     `json:"cached_at"`
	FetchDuration time.Duration `json:"fetch_duration"`
	AccessCount   int           `json:"access_count"`
	IsIndex       bool          `json:"is_index"` // True if this is an index/metadata file, not an actual package
}

// Handler is the interface for package manager handlers
type Handler interface {
	// IsIndex determines if the given path is an index file
	IsIndex(path string) bool
	// ServeHTTP handles HTTP requests
	ServeHTTP(w http.ResponseWriter, r *http.Request)
}
