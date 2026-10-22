package main

import (
	"log"
	"os"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/cli"
)

func main() {
	// Default config - can be overridden by environment variables
	cacheDir := os.Getenv("CACHE_DIR")
	if cacheDir == "" {
		cacheDir = "/app/cache"
	}

	statsPath := os.Getenv("STATS_PATH")
	if statsPath == "" {
		statsPath = "/app/cache/access-stats.json"
	}

	statsTracker := cache.NewStatsTracker(statsPath)
	if err := statsTracker.Load(false); err != nil {
		// Non-fatal - stats just won't be available
		log.Printf("Warning: Failed to load access stats: %v", err)
	}

	cfg := cli.Config{
		CacheDir:     cacheDir,
		StatsTracker: statsTracker,
	}

	cli.Run(cfg, os.Args[1:])
}
