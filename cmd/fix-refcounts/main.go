package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/cli"
	"github.com/Delphos-Labs/roxxy/internal/config"
)

func main() {
	var correctCacheDir = flag.String("cache-dir", "", "Cache directory (defaults to config or platform default)")
	var dryRun = flag.Bool("dry-run", false, "Show what would be done without making changes")
	flag.Parse()

	// Determine correct cache directory
	if *correctCacheDir == "" {
		// Try to load from config
		cfgFile := config.FindConfigFile(false)
		if cfgFile != "" {
			globalConfig, err := config.LoadGlobalConfig(cfgFile)
			if err == nil && globalConfig.CacheDir != "" {
				*correctCacheDir = globalConfig.CacheDir
				fmt.Printf("Using cache directory from config: %s\n", *correctCacheDir)
			}
		}

		// Fall back to platform default
		if *correctCacheDir == "" {
			*correctCacheDir = config.GetDefaultCacheDir()
			fmt.Printf("Using platform default cache directory: %s\n", *correctCacheDir)
		}
	}

	refcountDir := filepath.Join(*correctCacheDir, "refcount")
	manifestDir := filepath.Join(*correctCacheDir, "docker-manifests")

	fmt.Printf("Rebuilding refcounts from manifests:\n")
	fmt.Printf("  Cache directory: %s\n", *correctCacheDir)
	fmt.Printf("  Manifest directory: %s\n", manifestDir)
	fmt.Printf("  Refcount directory: %s\n", refcountDir)
	if *dryRun {
		fmt.Printf("  Mode: DRY RUN (no changes will be made)\n")
	}
	fmt.Println()

	// Check if manifest directory exists
	if _, err := os.Stat(manifestDir); os.IsNotExist(err) {
		fmt.Printf("No manifests found in: %s\n", manifestDir)
		os.Exit(0)
	}

	// Ensure refcount directory exists
	if !*dryRun {
		if err := os.MkdirAll(refcountDir, 0755); err != nil {
			fmt.Printf("Error: Failed to create refcount directory: %v\n", err)
			os.Exit(1)
		}
	}

	// Map to count blob references: contentHash -> count
	refcounts := make(map[string]int)
	var manifestCount, errorCount int

	fmt.Println("Scanning manifests...")

	// Walk through all manifest files
	err := filepath.Walk(manifestDir, func(manifestPath string, info fs.FileInfo, err error) error {
		if err != nil {
			return nil // Continue on errors
		}

		// Skip directories and .meta files
		if info.IsDir() || strings.HasSuffix(manifestPath, ".meta") {
			return nil
		}

		// Read manifest file
		manifestData, err := os.ReadFile(manifestPath)
		if err != nil {
			errorCount++
			return nil // Continue on read errors
		}

		// Check if this is a multi-platform index
		var indexCheck struct {
			MediaType string `json:"mediaType"`
			Manifests []struct {
				Digest   string `json:"digest"`
				Platform struct {
					Architecture string `json:"architecture"`
					OS           string `json:"os"`
				} `json:"platform,omitempty"`
			} `json:"manifests"`
		}

		isIndex := false
		if err := json.Unmarshal(manifestData, &indexCheck); err == nil {
			isIndex = indexCheck.MediaType == "application/vnd.oci.image.index.v1+json" ||
				indexCheck.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
		}

		if isIndex {
			// Multi-platform index - need to process each platform manifest
			// Extract registry and repo from path to find platform manifests
			relPath, err := filepath.Rel(manifestDir, manifestPath)
			if err != nil {
				return nil
			}

			parts := strings.Split(relPath, string(filepath.Separator))
			if len(parts) < 3 {
				return nil
			}

			registryID := parts[0]
			safeRepo := parts[1]

			// For each platform manifest in the index, we need to find and process it
			// Platform manifests are stored by digest, so we need to look them up
			for _, m := range indexCheck.Manifests {
				// Skip attestations/SBOMs
				if m.Platform.OS == "unknown" || m.Platform.Architecture == "unknown" {
					continue
				}
				if m.Platform.OS == "" || m.Platform.Architecture == "" {
					continue
				}

				// Try to find platform manifest by digest
				platformManifestPath := filepath.Join(manifestDir, registryID, safeRepo, m.Digest)
				platformData, err := os.ReadFile(platformManifestPath)
				if err != nil {
					// Platform manifest not found - skip it
					continue
				}

				// Extract blob digests from platform manifest
				configDigest, layerDigests, err := cli.ExtractBlobDigestsFromManifest(platformData)
				if err != nil {
					continue
				}

				// Increment refcounts for all blobs
				if configDigest != "" {
					contentHash := strings.TrimPrefix(configDigest, "sha256:")
					refcounts[contentHash]++
				}
				for _, layerDigest := range layerDigests {
					if layerDigest != "" {
						contentHash := strings.TrimPrefix(layerDigest, "sha256:")
						refcounts[contentHash]++
					}
				}
			}
		} else {
			// Single-platform manifest - extract blob digests directly
			configDigest, layerDigests, err := cli.ExtractBlobDigestsFromManifest(manifestData)
			if err != nil {
				errorCount++
				return nil
			}

			// Increment refcounts for all blobs
			if configDigest != "" {
				contentHash := strings.TrimPrefix(configDigest, "sha256:")
				refcounts[contentHash]++
			}
			for _, layerDigest := range layerDigests {
				if layerDigest != "" {
					contentHash := strings.TrimPrefix(layerDigest, "sha256:")
					refcounts[contentHash]++
				}
			}
		}

		manifestCount++
		if manifestCount%100 == 0 {
			fmt.Printf("  Processed %d manifests, found %d unique blobs...\n", manifestCount, len(refcounts))
		}

		return nil
	})

	if err != nil {
		fmt.Printf("Error walking manifest directory: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("  Processed %d manifests, found %d unique blobs\n", manifestCount, len(refcounts))
	fmt.Println()

	// Write refcounts
	fmt.Println("Writing refcounts...")
	var writtenCount, writeErrorCount int

	for contentHash, count := range refcounts {
		refcountPath := cache.GetRefcountPath(*correctCacheDir, contentHash)

		if !*dryRun {
			// Ensure directory exists
			dir := filepath.Dir(refcountPath)
			if err := os.MkdirAll(dir, 0755); err != nil {
				fmt.Printf("  ✗ Error creating directory for %s: %v\n", contentHash[:12], err)
				writeErrorCount++
				continue
			}

			// Write refcount
			if err := os.WriteFile(refcountPath, []byte(fmt.Sprintf("%d", count)), 0644); err != nil {
				fmt.Printf("  ✗ Error writing refcount for %s: %v\n", contentHash[:12], err)
				writeErrorCount++
				continue
			}
		}

		writtenCount++
		if writtenCount%1000 == 0 {
			fmt.Printf("  Written %d refcounts...\n", writtenCount)
		}
	}

	fmt.Println()
	fmt.Printf("Summary:\n")
	fmt.Printf("  Manifests processed: %d\n", manifestCount)
	fmt.Printf("  Unique blobs found: %d\n", len(refcounts))
	fmt.Printf("  Refcounts written: %d\n", writtenCount)
	fmt.Printf("  Errors:            %d\n", errorCount+writeErrorCount)

	if *dryRun {
		fmt.Println()
		fmt.Println("This was a dry run. Run without --dry-run to write refcounts.")
	} else {
		fmt.Println()
		fmt.Println("✓ Refcount rebuild complete!")
	}
}
