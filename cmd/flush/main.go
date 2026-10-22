package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	cacheDir := os.Getenv("CACHE_DIR")
	if cacheDir == "" {
		cacheDir = "/app/cache"
	}

	fmt.Println("Flushing registry-proxy cache...")

	// Delete package manager cache
	packagesDir := filepath.Join(cacheDir, "packages")
	if err := removeContents(packagesDir); err != nil {
		fmt.Printf("  Error removing package cache: %v\n", err)
	} else {
		fmt.Println("  ✓ Removed package cache")
	}

	// Delete URL cache (content-addressable storage metadata)
	urlDir := filepath.Join(cacheDir, "url")
	if err := removeContents(urlDir); err != nil {
		fmt.Printf("  Error removing URL cache: %v\n", err)
	} else {
		fmt.Println("  ✓ Removed URL cache")
	}

	// Delete content cache (keep structure, just clear files)
	contentDir := filepath.Join(cacheDir, "content")
	if err := removeFiles(contentDir); err != nil {
		fmt.Printf("  Error removing content cache: %v\n", err)
	} else {
		fmt.Println("  ✓ Removed content cache")
	}

	// Delete refcount cache
	refcountDir := filepath.Join(cacheDir, "refcount")
	if err := removeFiles(refcountDir); err != nil {
		fmt.Printf("  Error removing refcount cache: %v\n", err)
	} else {
		fmt.Println("  ✓ Removed refcount cache")
	}

	// Delete access stats
	statsFile := filepath.Join(cacheDir, "access-stats.json")
	if err := os.Remove(statsFile); err != nil && !os.IsNotExist(err) {
		fmt.Printf("  Error removing access stats: %v\n", err)
	} else {
		fmt.Println("  ✓ Removed access stats")
	}

	fmt.Println("Cache flushed successfully")
}

// removeContents removes all contents of a directory but keeps the directory itself
func removeContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Directory doesn't exist, nothing to do
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

// removeFiles removes all files recursively but keeps directory structure
func removeFiles(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip the root directory itself
		if path == dir {
			return nil
		}

		// Only remove files, not directories
		if !info.IsDir() {
			// Skip .meta files (they're metadata, not content)
			if strings.HasSuffix(path, ".meta") {
				return nil
			}
			return os.Remove(path)
		}

		return nil
	})
}
