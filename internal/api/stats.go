package api

import (
	"compress/gzip"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/cli"
	"github.com/Delphos-Labs/roxxy/internal/packages"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// handleStatsSummary handles the stats summary endpoint
func (a *API) handleStatsSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Return early with defaults if cacheDir is not set
	if a.cacheDir == "" {
		log.Printf("Warning: cacheDir is empty in handleStatsSummary")
		response := StatsSummaryResponse{
			TotalCacheSize:     0,
			PackageCount:       0,
			PackageCountByType: make(map[string]int),
			DockerImageCount:   0,
			TotalTimeSaved:     "0s",
			CacheHitRate:       0,
			URLCacheCount:      0,
		}
		writeJSON(w, response)
		return
	}

	cfg := cli.Config{
		CacheDir:     a.cacheDir,
		StatsTracker: a.statsTracker,
	}

	// Collect package statistics (optimized - only count, don't load full details)
	packageCountByType := make(map[string]int)
	totalPackageSize := int64(0)

	// Fast path: just count cached packages without loading index metadata
	cacheRoot := filepath.Join(a.cacheDir, "packages")
	pmTypes := []packages.Type{packages.TypeAPK, packages.TypeAPT, packages.TypeNPM, packages.TypePyPI, packages.TypeGo}

	for _, pmType := range pmTypes {
		typeDir := filepath.Join(cacheRoot, string(pmType))
		if _, err := os.Stat(typeDir); os.IsNotExist(err) {
			continue
		}

		// Quick count by scanning directory structure
		count, size, err := a.countPackagesByType(typeDir, pmType)
		if err != nil {
			log.Printf("Error counting packages for type %s: %v", pmType, err)
			continue
		}
		packageCountByType[string(pmType)] = count
		totalPackageSize += size
	}

	// Collect URL cache statistics
	urlEntries, err := listCacheEntries(cfg, "")
	if err != nil {
		log.Printf("Error listing URL cache in handleStatsSummary: %v", err)
		urlEntries = []cli.CacheEntry{}
	}

	totalURLCacheSize := int64(0)
	for _, entry := range urlEntries {
		totalURLCacheSize += entry.Size
	}

	// Get Docker image count (using helper from docker.go)
	dockerCount := a.getDockerImageCount()

	// Calculate time saved and cache hit rate by scanning metadata files
	// This is reasonably fast since we're just reading small .meta files
	var totalTimeSaved time.Duration
	var totalAccessCount int
	var cacheHits int

	if a.statsTracker != nil {
		// Get all stats from the tracker
		statsMap := a.statsTracker.GetAllStats()

		// Build a map of stats by key for quick lookup
		// Stats keys are like "pypi:requests@2.31.0"
		statsByKey := make(map[string]*cache.Stats)
		for key, stat := range statsMap {
			if stat.AccessCount > 1 {
				statsByKey[key] = stat
			}
		}

		// Quick scan: read metadata files to get fetch durations
		// We'll match metadata with stats by constructing the key from package info
		for _, pmType := range pmTypes {
			typeDir := filepath.Join(cacheRoot, string(pmType))
			if _, err := os.Stat(typeDir); os.IsNotExist(err) {
				continue
			}

			// Walk metadata files (much faster than walking all files)
			if err := filepath.Walk(typeDir, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}

				if !strings.HasSuffix(path, ".meta") {
					return nil
				}

				// Read metadata
				metaData, err := os.ReadFile(path)
				if err != nil {
					return nil
				}

				var metadata struct {
					Type          string        `json:"type"`
					Name          string        `json:"name"`
					Version       string        `json:"version"`
					Architecture  string        `json:"architecture"`
					FetchDuration time.Duration `json:"fetch_duration"`
					AccessCount   int           `json:"access_count"`
					IsIndex       bool          `json:"is_index"`
				}
				if err := json.Unmarshal(metaData, &metadata); err != nil {
					return nil
				}

				// Skip index files
				if metadata.IsIndex {
					return nil
				}

				// Build stats key to match with stats tracker
				key := buildPackageKey(metadata.Type, metadata.Name, metadata.Version, metadata.Architecture)

				// Check if we have stats for this package
				if stat, exists := statsByKey[key]; exists {
					if stat.AccessCount > 1 && metadata.FetchDuration > 0 {
						// Time saved = fetch duration * (access count - 1)
						// The -1 is because the first access was the fetch itself
						timeSaved := metadata.FetchDuration * time.Duration(stat.AccessCount-1)
						totalTimeSaved += timeSaved
						cacheHits += stat.AccessCount - 1
					}
					totalAccessCount += stat.AccessCount
				} else if metadata.AccessCount > 1 && metadata.FetchDuration > 0 {
					// Fallback: use access count from metadata if stats tracker doesn't have it
					timeSaved := metadata.FetchDuration * time.Duration(metadata.AccessCount-1)
					totalTimeSaved += timeSaved
					cacheHits += metadata.AccessCount - 1
					totalAccessCount += metadata.AccessCount
				}

				return nil
			}); err != nil {
				log.Printf("Error walking directory: %v", err)
			}
		}
	}

	// Calculate cache hit rate
	cacheHitRate := 0.0
	if totalAccessCount > 0 && cacheHits > 0 {
		cacheHitRate = float64(cacheHits) / float64(totalAccessCount) * 100
	}

	// Calculate total package count
	totalPackageCount := 0
	for _, count := range packageCountByType {
		totalPackageCount += count
	}

	response := StatsSummaryResponse{
		TotalCacheSize:     totalPackageSize + totalURLCacheSize,
		PackageCount:       totalPackageCount,
		PackageCountByType: packageCountByType,
		DockerImageCount:   dockerCount,
		TotalTimeSaved:     formatDuration(totalTimeSaved),
		CacheHitRate:       cacheHitRate,
		URLCacheCount:      len(urlEntries),
	}

	// Ensure package_count_by_type is never nil
	if response.PackageCountByType == nil {
		response.PackageCountByType = make(map[string]int)
	}

	writeJSON(w, response)
}

// handleAccessStats handles the access stats endpoint
func (a *API) handleAccessStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Return access statistics from the statsTracker
	var stats map[string]*cache.Stats
	if a.statsTracker == nil {
		stats = make(map[string]*cache.Stats)
	} else {
		stats = a.statsTracker.GetAllStats()
	}

	response := map[string]interface{}{
		"stats": stats,
	}

	// Check if client accepts gzip encoding
	acceptsGzip := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")

	if acceptsGzip {
		// Use gzip compression for faster transfer
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Vary", "Accept-Encoding")

		gzipWriter := gzip.NewWriter(w)
		defer util.DeferClose(gzipWriter)

		encoder := json.NewEncoder(gzipWriter)
		if err := encoder.Encode(response); err != nil {
			log.Printf("Error encoding JSON: %v", err)
			return
		}
	} else {
		// No compression
		writeJSON(w, response)
	}
}
