package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/cli"
)

// HandleCache routes cache subcommands
func HandleCache(cfg Config, args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: cache <command>")
		fmt.Println("Commands:")
		fmt.Println("  ls              List cached content")
		fmt.Println("  flush           Flush all cache")
		os.Exit(1)
	}

	subcmd := args[0]
	subargs := args[1:]

	cliCfg := cli.Config{
		CacheDir:     cfg.CacheDir,
		StatsTracker: cfg.StatsTracker,
	}

	switch subcmd {
	case "ls":
		// Run lscache with remaining args
		cli.Run(cliCfg, subargs)
	case "flush":
		// Run flush command
		runFlushCommand(cfg)
	default:
		fmt.Printf("Unknown cache command: %s\n", subcmd)
		fmt.Println("Use 'cache ls' or 'cache flush'")
		os.Exit(1)
	}
}

// runFlushCommand runs the flush cache logic
func runFlushCommand(cfg Config) {
	fmt.Println("Flushing roxxy cache...")

	// Delete package manager cache
	packagesDir := filepath.Join(cfg.CacheDir, "packages")
	if err := removeCacheContents(packagesDir); err != nil {
		fmt.Printf("  Error removing package cache: %v\n", err)
	} else {
		fmt.Println("  ✓ Removed package cache")
	}

	// Delete URL cache
	urlDir := filepath.Join(cfg.CacheDir, "url")
	if err := removeCacheContents(urlDir); err != nil {
		fmt.Printf("  Error removing URL cache: %v\n", err)
	} else {
		fmt.Println("  ✓ Removed URL cache")
	}

	// Delete content cache
	contentDir := filepath.Join(cfg.CacheDir, "content")
	if err := removeCacheFiles(contentDir); err != nil {
		fmt.Printf("  Error removing content cache: %v\n", err)
	} else {
		fmt.Println("  ✓ Removed content cache")
	}

	// Delete refcount cache
	refcountDir := filepath.Join(cfg.CacheDir, "refcount")
	if err := removeCacheFiles(refcountDir); err != nil {
		fmt.Printf("  Error removing refcount cache: %v\n", err)
	} else {
		fmt.Println("  ✓ Removed refcount cache")
	}

	// Delete Docker manifests and blobs
	dockerManifestsDir := filepath.Join(cfg.CacheDir, "docker-manifests")
	if err := removeCacheContents(dockerManifestsDir); err != nil {
		fmt.Printf("  Error removing Docker manifests: %v\n", err)
	} else {
		fmt.Println("  ✓ Removed Docker manifests")
	}

	dockerBlobsDir := filepath.Join(cfg.CacheDir, "docker-blobs")
	if err := removeCacheContents(dockerBlobsDir); err != nil {
		fmt.Printf("  Error removing Docker blobs: %v\n", err)
	} else {
		fmt.Println("  ✓ Removed Docker blobs")
	}

	// Delete access stats
	statsFile := filepath.Join(cfg.CacheDir, "access-stats.json")
	if err := os.Remove(statsFile); err != nil && !os.IsNotExist(err) {
		fmt.Printf("  Error removing access stats: %v\n", err)
	} else {
		fmt.Println("  ✓ Removed access stats")
	}

	fmt.Println("Cache flushed successfully")
}

// removeCacheContents removes all contents of a directory but keeps the directory itself
func removeCacheContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}

	return nil
}

// removeCacheFiles removes all files recursively but keeps directory structure
func removeCacheFiles(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if path == dir {
			return nil
		}

		if !info.IsDir() {
			if !strings.HasSuffix(path, ".meta") {
				return os.Remove(path)
			}
		}

		return nil
	})
}
