package packages

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// discardResponseWriter implements http.ResponseWriter but discards all writes
// Used for background refresh where we only care about caching, not serving
type discardResponseWriter struct {
	header http.Header
}

func (d *discardResponseWriter) Header() http.Header {
	if d.header == nil {
		d.header = make(http.Header)
	}
	return d.header
}

func (d *discardResponseWriter) Write(p []byte) (int, error) {
	return len(p), nil // Discard
}

func (d *discardResponseWriter) WriteHeader(statusCode int) {
	// Discard
}

// Proxy is a package manager proxy that uses the shared base
type Proxy struct {
	*Base
	upstreamURL     string
	refreshingFiles map[string]bool // Track files currently being refreshed
	refreshingMutex sync.Mutex      // Protect refreshingFiles map
}

// NewProxy creates a new package manager proxy
func NewProxy(config Config, statsTracker StatsTrackerInterface, indexDB IndexDBInterface, sbomManager SBOMManagerInterface, enricher PackageEnricherInterface) *Proxy {
	return &Proxy{
		Base:            NewBase(config, statsTracker, indexDB, sbomManager, enricher),
		upstreamURL:     config.UpstreamURL,
		refreshingFiles: make(map[string]bool),
	}
}

// formatLogPath formats the URL path for logging, extracting module name for Go modules
func (p *Proxy) formatLogPath(path string) string {
	if p.config.Type == TypeGo {
		// For Go modules, extract module name from path like /github.com/user/repo/@v/v1.2.3.info
		parts := strings.Split(path, "/")
		for i, part := range parts {
			if part == "@v" && i > 0 {
				modulePath := strings.Join(parts[1:i], "/")
				filename := filepath.Base(path)
				return fmt.Sprintf("%s/%s", modulePath, filename)
			}
		}
		// Fallback if @v not found
		return path
	}
	// For other package managers, just use the filename
	return filepath.Base(path)
}

// ServeHTTP handles HTTP requests using the handler's IsIndex method
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request, handler Handler) {
	// Loop detection: if this request has our upstream marker, we're in a loop
	if r.Header.Get("X-Roxxy-Upstream") == "true" {
		log.Printf("[%s] LOOP DETECTED - request has X-Roxxy-Upstream header, rejecting", p.config.Type)
		http.Error(w, "Proxy loop detected", http.StatusLoopDetected)
		return
	}

	startTime := time.Now()
	urlPath := r.URL.Path // Capture for use in goroutines
	upstreamURL := util.BuildUpstreamURL(p.upstreamURL, urlPath, r.URL.RawQuery)
	isIndex := handler.IsIndex(urlPath)
	logPath := p.formatLogPath(urlPath)

	// Check cache first
	cached, expired, err := p.GetCached(urlPath, isIndex)
	if err == nil {
		p.handleCacheHit(w, r, cached, expired, isIndex, urlPath, upstreamURL, logPath, startTime)
		return
	}

	// Check if airgapped mode is enabled
	if p.config.Airgapped {
		log.Printf("[%s] %s %s - 404 (airgapped mode, cache miss)", p.config.Type, r.Method, logPath)
		http.Error(w, "Not found (airgapped mode)", http.StatusNotFound)
		return
	}

	// Fetch from upstream
	p.handleCacheMiss(w, r, urlPath, upstreamURL, logPath, isIndex, startTime)
}

// handleCacheHit serves content from cache and triggers background refresh if needed
func (p *Proxy) handleCacheHit(w http.ResponseWriter, r *http.Request, cached *os.File, expired bool, isIndex bool, urlPath string, upstreamURL string, logPath string, startTime time.Time) {
	defer util.DeferClose(cached)

	stat, _ := cached.Stat()
	duration := time.Since(startTime)
	cacheStatus := "cache"

	if expired && isIndex {
		// Check if already refreshing
		p.refreshingMutex.Lock()
		isRefreshing := p.refreshingFiles[urlPath]
		if !isRefreshing {
			// Mark as refreshing and start background refresh
			p.refreshingFiles[urlPath] = true
			p.refreshingMutex.Unlock()
			cacheStatus = "cache (stale, refreshing)"
			go p.startBackgroundRefresh(urlPath, upstreamURL, logPath)
		} else {
			p.refreshingMutex.Unlock()
			cacheStatus = "cache (stale, refresh queued)"
		}
	}

	log.Printf("[%s] %s %s - 200 %s %dms (%s)", p.config.Type, r.Method, logPath, FormatBytes(stat.Size()), duration.Milliseconds(), cacheStatus)

	// Track artifact for SBOM if not an index file
	if !isIndex {
		p.trackArtifact(r, urlPath, isIndex)
	}

	http.ServeContent(w, r, filepath.Base(r.URL.Path), time.Time{}, cached)
}

// startBackgroundRefresh refreshes cached index files in the background
func (p *Proxy) startBackgroundRefresh(urlPath string, upstreamURL string, logPath string) {
	defer func() {
		// Remove from refreshing map when done
		p.refreshingMutex.Lock()
		delete(p.refreshingFiles, urlPath)
		p.refreshingMutex.Unlock()
	}()

	log.Printf("[%s] Background refresh: %s", p.config.Type, logPath)

	// Use streaming to refresh cache (don't buffer in memory)
	refreshCachePath := p.getCachePath(urlPath)
	headers := http.Header{}
	headers.Set("X-Roxxy-Upstream", "true")
	headers.Set("User-Agent", "roxxy/1.0")

	// Create a discard response writer - we only care about caching
	discardWriter := &discardResponseWriter{}

	_, err := util.StreamAndCache(util.StreamAndCacheOptions{
		URL:            upstreamURL,
		CachePath:      refreshCachePath,
		Client:         getUpstreamClient(),
		RequestHeaders: headers,
		ResponseWriter: discardWriter,
		LogPrefix:      string(p.config.Type),
		ChunkTimeout:   2 * time.Minute,
	})

	if err != nil {
		log.Printf("[%s] Background refresh failed for %s: %v", p.config.Type, logPath, err)
	} else {
		log.Printf("[%s] Background refresh completed for %s", p.config.Type, logPath)
	}
}

// handleCacheMiss fetches from upstream and caches the result
func (p *Proxy) handleCacheMiss(w http.ResponseWriter, r *http.Request, urlPath string, upstreamURL string, logPath string, isIndex bool, startTime time.Time) {
	cachePath := p.getCachePath(urlPath)
	headers := http.Header{}
	headers.Set("X-Roxxy-Upstream", "true")
	headers.Set("User-Agent", "roxxy/1.0")

	n, err := util.StreamAndCache(util.StreamAndCacheOptions{
		URL:            upstreamURL,
		CachePath:      cachePath,
		Client:         getUpstreamClient(),
		RequestHeaders: headers,
		ResponseWriter: w,
		LogPrefix:      string(p.config.Type),
		ChunkTimeout:   2 * time.Minute, // Activity timeout: fail if no data for 2 minutes
	})

	totalDuration := time.Since(startTime)

	if err != nil {
		log.Printf("[%s] %s %s - error %dms (streaming fetch failed: %v)", p.config.Type, r.Method, logPath, totalDuration.Milliseconds(), err)
		// Error response already written to client by StreamAndCache
		return
	}

	log.Printf("[%s] %s %s - 200 %s %dms (streaming fetch, cached)", p.config.Type, r.Method, logPath, FormatBytes(n), totalDuration.Milliseconds())

	// Track artifact for SBOM if not an index file
	if !isIndex {
		p.trackArtifact(r, r.URL.Path, isIndex)
	}
}

// APKHandler handles Alpine APK requests
type APKHandler struct {
	*Proxy
}

// NewAPKHandler creates a new APK handler
func NewAPKHandler(config Config, statsTracker StatsTrackerInterface, indexDB IndexDBInterface, sbomManager SBOMManagerInterface, enricher PackageEnricherInterface) *APKHandler {
	return &APKHandler{
		Proxy: NewProxy(config, statsTracker, indexDB, sbomManager, enricher),
	}
}

// IsIndex determines if the path is an index file
func (h *APKHandler) IsIndex(path string) bool {
	return strings.HasSuffix(path, "APKINDEX.tar.gz")
}

// ServeHTTP handles HTTP requests for APK
func (h *APKHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Proxy.ServeHTTP(w, r, h)
}

// APTHandler handles Debian/Ubuntu APT requests
type APTHandler struct {
	*Proxy
}

// NewAPTHandler creates a new APT handler
func NewAPTHandler(config Config, statsTracker StatsTrackerInterface, indexDB IndexDBInterface, sbomManager SBOMManagerInterface, enricher PackageEnricherInterface) *APTHandler {
	return &APTHandler{
		Proxy: NewProxy(config, statsTracker, indexDB, sbomManager, enricher),
	}
}

// IsIndex determines if the path is an index file
func (h *APTHandler) IsIndex(path string) bool {
	// Check for common APT index file patterns
	filename := filepath.Base(path)

	// Common index file names
	indexPatterns := []string{
		"Packages.gz",
		"Packages",
		"Release",
		"Release.gpg",
		"InRelease",
		"Sources.gz",
		"Sources",
		"Contents-",
		"Translation-",
	}

	for _, pattern := range indexPatterns {
		if strings.HasSuffix(filename, pattern) || strings.HasPrefix(filename, pattern) {
			return true
		}
	}

	// Check if path contains /dists/ (APT repository structure)
	if strings.Contains(path, "/dists/") {
		return true
	}

	// If it's not a .deb file, it's likely an index file
	if !strings.HasSuffix(filename, ".deb") {
		return true
	}

	return false
}

// ServeHTTP handles HTTP requests for APT
func (h *APTHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Proxy.ServeHTTP(w, r, h)
}

// NPMHandler handles NPM registry requests
type NPMHandler struct {
	*Proxy
}

// NewNPMHandler creates a new NPM handler
func NewNPMHandler(config Config, statsTracker StatsTrackerInterface, indexDB IndexDBInterface, sbomManager SBOMManagerInterface, enricher PackageEnricherInterface) *NPMHandler {
	return &NPMHandler{
		Proxy: NewProxy(config, statsTracker, indexDB, sbomManager, enricher),
	}
}

// IsIndex determines if the path is an index file (not a tarball)
func (h *NPMHandler) IsIndex(path string) bool {
	return !strings.Contains(path, "/-/") || !strings.HasSuffix(path, ".tgz")
}

// ServeHTTP handles HTTP requests for NPM
func (h *NPMHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Proxy.ServeHTTP(w, r, h)
}

// PyPIHandler handles Python PyPI requests
type PyPIHandler struct {
	*Proxy
}

// NewPyPIHandler creates a new PyPI handler
func NewPyPIHandler(config Config, statsTracker StatsTrackerInterface, indexDB IndexDBInterface, sbomManager SBOMManagerInterface, enricher PackageEnricherInterface) *PyPIHandler {
	return &PyPIHandler{
		Proxy: NewProxy(config, statsTracker, indexDB, sbomManager, enricher),
	}
}

// IsIndex determines if the path is an index file
func (h *PyPIHandler) IsIndex(path string) bool {
	// Index files: simple HTML pages and .metadata files (not actual packages)
	return strings.HasPrefix(path, "/simple/") || strings.HasSuffix(path, ".metadata")
}

// ServeHTTP handles HTTP requests for PyPI
func (h *PyPIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Proxy.ServeHTTP(w, r, h)
}

// GoHandler handles Go module proxy requests
type GoHandler struct {
	*Proxy
}

// NewGoHandler creates a new Go handler
func NewGoHandler(config Config, statsTracker StatsTrackerInterface, indexDB IndexDBInterface, sbomManager SBOMManagerInterface, enricher PackageEnricherInterface) *GoHandler {
	return &GoHandler{
		Proxy: NewProxy(config, statsTracker, indexDB, sbomManager, enricher),
	}
}

// IsIndex determines if the path is an index file (list, info) vs a zip file
// Note: .mod files are dependency manifests, not index files that we parse
// For Go, .info and /list files are index files, but they have minimal data
// The actual package data (LICENSE, README) is in .zip files which are processed separately
func (h *GoHandler) IsIndex(path string) bool {
	// Index files: /module/@v/list, /module/@v/version.info
	// Package files: /module/@v/version.zip (processed by enricher, not as index)
	// Dependency files: /module/@v/version.mod (not parsed as index)
	return strings.HasSuffix(path, "/list") ||
		strings.HasSuffix(path, ".info")
}

// ServeHTTP handles HTTP requests for Go modules
func (h *GoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Proxy.ServeHTTP(w, r, h)
}
