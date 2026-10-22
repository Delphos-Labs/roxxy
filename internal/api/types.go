package api

import (
	"time"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/packages"
	"github.com/Delphos-Labs/roxxy/internal/packages/index"
	"github.com/Delphos-Labs/roxxy/internal/registry"
)

// IndexDBInterface is an interface for the index database
type IndexDBInterface interface {
	ParseCachedIndexFiles(cacheDir string, pmType packages.Type) (int, error)
	GetPackages(pmType packages.Type) ([]index.IndexPackage, error)
}

// StatsSummaryResponse represents the dashboard overview
type StatsSummaryResponse struct {
	TotalCacheSize     int64          `json:"total_cache_size"`
	PackageCount       int            `json:"package_count"`
	PackageCountByType map[string]int `json:"package_count_by_type"`
	DockerImageCount   int            `json:"docker_image_count"`
	TotalTimeSaved     string         `json:"total_time_saved"`
	CacheHitRate       float64        `json:"cache_hit_rate"`
	URLCacheCount      int            `json:"url_cache_count"`
}

// PackageEntry represents a package in the cache
type PackageEntry struct {
	ID            string    `json:"id"`
	Type          string    `json:"type"`
	Name          string    `json:"name"`
	Version       string    `json:"version"`
	Architecture  string    `json:"architecture"`
	Group         string    `json:"group,omitempty"` // Component/group (e.g., "main", "security")
	Size          int64     `json:"size"`
	CachedAt      time.Time `json:"cached_at"`
	FetchDuration int64     `json:"fetch_duration_ms"`
	AccessCount   int       `json:"access_count"`
	LastAccessed  time.Time `json:"last_accessed"`
	Cached        bool      `json:"cached"`       // True if the actual package file is cached
	Description   string    `json:"description"`  // Package description (from index)
	Dependencies  []string  `json:"dependencies"` // Package dependencies (from index)
	URL           string    `json:"url"`          // Package download URL (from index)
}

// API provides REST API endpoints for the web UI
type API struct {
	cacheDir      string
	certsDir      string
	statsTracker  *cache.StatsTracker
	indexDB       IndexDBInterface        // Optional index database for parsing index files
	sbomManager   SBOMManagerInterface    // SBOM session manager
	overrideStore *registry.OverrideStore // Image override store
}
