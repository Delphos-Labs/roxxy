package api

import (
	"log"
	"net/http"
	"os"

	"github.com/Delphos-Labs/roxxy/internal/cache"
)

// NewAPI creates a new API handler
func NewAPI(cacheDir string, statsTracker *cache.StatsTracker, indexDB interface{}) *API {
	var db IndexDBInterface
	if idx, ok := indexDB.(IndexDBInterface); ok {
		db = idx
	}
	return &API{
		cacheDir:     cacheDir,
		statsTracker: statsTracker,
		indexDB:      db,
	}
}

// SetCertsDir sets the certificates directory for the API
func (a *API) SetCertsDir(certsDir string) {
	a.certsDir = certsDir
	// Ensure the directory exists
	if certsDir != "" {
		if err := os.MkdirAll(certsDir, 0755); err != nil {
			log.Printf("Warning: Failed to create certs directory %s: %v", certsDir, err)
		}
	}
}

// RegisterRoutes registers API routes with the given mux
func (a *API) RegisterRoutes(mux *http.ServeMux) {
	a.registerNonPackageRoutes(mux)
	a.registerPackageRoutes(mux)
}

// RegisterRoutesExceptPackages registers API routes except package endpoints (for use with v2 API)
func (a *API) RegisterRoutesExceptPackages(mux *http.ServeMux) {
	a.registerNonPackageRoutes(mux)
}

// registerNonPackageRoutes registers all routes except package endpoints
func (a *API) registerNonPackageRoutes(mux *http.ServeMux) {
	// Stats endpoints
	mux.HandleFunc("/api/v1/stats/summary", a.corsMiddleware(a.handleStatsSummary))
	mux.HandleFunc("/api/v1/stats/access", a.corsMiddleware(a.handleAccessStats))

	// Build info endpoint
	mux.HandleFunc("/api/v1/buildinfo", a.corsMiddleware(a.handleBuildInfo))

	// Docker registry endpoints
	mux.HandleFunc("/api/v1/docker/repositories", a.corsMiddleware(a.handleDockerRepositories))
	mux.HandleFunc("/api/v1/docker/repositories/", a.corsMiddleware(a.handleDockerRepositoryDetail))
	mux.HandleFunc("/api/v1/docker/bulk-delete", a.corsMiddleware(a.handleDockerBulkDelete))

	// URL cache endpoints
	mux.HandleFunc("/api/v1/cache/url", a.corsMiddleware(a.handleURLCache))
	mux.HandleFunc("/api/v1/cache/url/delete", a.corsMiddleware(a.handleURLCacheDelete))

	// Management endpoints
	mux.HandleFunc("/api/v1/cache/flush", a.corsMiddleware(a.handleFlushCache))
	mux.HandleFunc("/api/v1/index/parse", a.corsMiddleware(a.handleParseIndexes))

	// Export endpoints (register more specific routes first)
	// Note: Export endpoints don't use CORS middleware because they stream large files
	// but we do add logging to track requests
	mux.HandleFunc("/api/v1/export/docker", a.loggingMiddleware(a.handleExportDocker))
	mux.HandleFunc("/api/v1/export/archive", a.loggingMiddleware(a.handleExportArchive))
	mux.HandleFunc("/api/v1/export/packages", a.loggingMiddleware(a.handleExportPackages))
	mux.HandleFunc("/api/v1/export/raw", a.loggingMiddleware(a.handleExportRaw))
	mux.HandleFunc("/api/v1/export", a.loggingMiddleware(a.handleExportFull))

	// Certificate endpoints
	mux.HandleFunc("/api/v1/certs/ca", a.corsMiddleware(a.handleCACertificate))
	mux.HandleFunc("/api/v1/certs/test", a.corsMiddleware(a.handleCertTest))

	// SBOM endpoints
	mux.HandleFunc("/api/v1/sbom/start", a.corsMiddleware(a.handleSBOMStart))
	mux.HandleFunc("/api/v1/sbom/stop", a.corsMiddleware(a.handleSBOMStop))
	mux.HandleFunc("/api/v1/sbom/output", a.corsMiddleware(a.handleSBOMOutput))
	mux.HandleFunc("/api/v1/sbom/list", a.corsMiddleware(a.handleSBOMList))
	mux.HandleFunc("/api/v1/sbom/rm", a.corsMiddleware(a.handleSBOMRemove))
	log.Printf("API: Registered SBOM endpoints: /api/v1/sbom/start, /api/v1/sbom/stop, /api/v1/sbom/output, /api/v1/sbom/list, /api/v1/sbom/rm")

	// Image override endpoints
	mux.HandleFunc("/api/v1/overrides", a.corsMiddleware(a.handleOverrides))
	mux.HandleFunc("/api/v1/overrides/", a.corsMiddleware(a.handleOverrides))
}

// registerPackageRoutes registers package endpoints
func (a *API) registerPackageRoutes(mux *http.ServeMux) {
	// Package endpoints (register more specific routes first)
	mux.HandleFunc("/api/v1/packages/go/", a.corsMiddleware(a.handleGoMetadata))
	mux.HandleFunc("/api/v1/packages/apk/", a.corsMiddleware(a.handleAPKMetadata))
	mux.HandleFunc("/api/v1/packages/pypi/", a.corsMiddleware(a.handlePyPIMetadata))
	mux.HandleFunc("/api/v1/packages/npm/", a.corsMiddleware(a.handleNPMTarballMetadata))
	mux.HandleFunc("/api/v1/packages/groups", a.corsMiddleware(a.handlePackageGroups))
	mux.HandleFunc("/api/v1/packages/bulk-delete", a.corsMiddleware(a.handleBulkDeletePackages))
	mux.HandleFunc("/api/v1/packages", a.corsMiddleware(a.handlePackages))
	mux.HandleFunc("/api/v1/packages/", a.corsMiddleware(a.handlePackageDetail))
}

// corsMiddleware adds CORS headers to API responses
// CORS is set to "*" to allow same-origin requests from the embedded UI
// The UI is served from the same server, so this is safe
// In production with separate frontend, restrict to specific origins
func (a *API) corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers - allow all origins since UI is served from same server
		// Frontend and backend agree on using "*" for same-origin requests
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "3600")

		// Handle preflight requests
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Call the next handler
		next(w, r)
	}
}

// loggingMiddleware logs API requests
func (a *API) loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("API: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		next(w, r)
	}
}
