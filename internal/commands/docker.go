package commands

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/cli"
	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/registry"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// HandleDocker routes docker subcommands
func HandleDocker(cfg Config, args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: docker <command>")
		fmt.Println("Commands:")
		fmt.Println("  ls              List Docker images")
		fmt.Println("  pull <image> [--platform <os/arch>]... [--file <file>]  Pull image(s) from registry into roxxy cache")
		fmt.Println("  rmi <image>     Remove Docker image from cache")
		fmt.Println("  tag -d <tag>    Delete a specific tag (keeps manifest by digest)")
		fmt.Println("  push <registry> <image> [--from-tarball <tarball>] [--file <file>]  Push image(s) to registry")
		fmt.Println("  export <image> [output.tar] [--file <file>]  Export image(s) as OCI tarball")
		fmt.Println("  prune           Remove unreferenced blobs")
		fmt.Println("  prune --manifests  Remove digest-only manifests with no tags")
		fmt.Println("  prune --migrate  Migrate old manifest structure to registry-based structure")
		fmt.Println("  prune --audit   Heal refcount drift, then prune")
		fmt.Println("  audit           Reconcile refcount files against manifest contents")
		fmt.Println("  discover        Discover tags for manifests stored by digest only")
		fmt.Println("  find <digest>   Find which repository/tag a manifest digest belongs to")
		fmt.Println("  parse           Extract metadata (license, description) from Docker/OCI images")
		fmt.Println("  parse --force   Force re-processing of images that already have metadata")
		fmt.Println("  override <image> <alias>    Override: serve <image> when <alias> is pulled")
		fmt.Println("  override --list             List all image overrides")
		fmt.Println("  override --rm <alias>       Remove an image override")
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
		// docker ls → cache ls --docker
		cli.Run(cliCfg, append([]string{"--docker"}, subargs...))
	case "pull":
		// docker pull <image> [--platform <os/arch>]... [--file <file>] [--force] [--ca-cert <path>] [--continue-on-error]
		// docker pull --file <file> [--force] [--ca-cert <path>] [--continue-on-error]
		var imageSpec string
		var platforms []string
		var filePath string
		var force bool
		var caCertPath string
		var continueOnError bool

		// Parse arguments
		for i := 0; i < len(subargs); i++ {
			if subargs[i] == "--file" && i+1 < len(subargs) {
				filePath = subargs[i+1]
				i++ // Skip the file path
			} else if subargs[i] == "--platform" && i+1 < len(subargs) {
				platforms = append(platforms, subargs[i+1])
				i++ // Skip the platform value
			} else if subargs[i] == "--force" {
				force = true
			} else if subargs[i] == "--ca-cert" && i+1 < len(subargs) {
				caCertPath = subargs[i+1]
				i++ // Skip the cert path
			} else if subargs[i] == "--continue-on-error" {
				continueOnError = true
			} else if imageSpec == "" {
				imageSpec = subargs[i]
			}
		}

		if filePath != "" {
			// Batch mode: read from file
			specs, err := parseImageListFile(filePath)
			if err != nil {
				fmt.Printf("Error reading image list file: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Pulling %d images from file...\n", len(specs))
			var errors []string
			for i, spec := range specs {
				fmt.Printf("\n[%d/%d] Pulling %s", i+1, len(specs), spec.Image)
				if len(spec.Platforms) > 0 {
					fmt.Printf(" (platforms: %s)", strings.Join(spec.Platforms, ", "))
				}
				fmt.Println()

				err := pullDockerImage(cfg, spec.Image, spec.Platforms, force, caCertPath)
				if err != nil {
					errorMsg := fmt.Sprintf("Failed to pull %s: %v", spec.Image, err)
					errors = append(errors, errorMsg)
					fmt.Printf("Error: %s\n", errorMsg)
					if !continueOnError {
						fmt.Printf("\nStopping on first error (use --continue-on-error to continue)\n")
						os.Exit(1)
					}
				}
			}
			fmt.Printf("\nCompleted pulling %d images", len(specs))
			if len(errors) > 0 {
				fmt.Printf(" (%d errors)", len(errors))
			}
			fmt.Println()
			if len(errors) > 0 {
				fmt.Println("\nErrors encountered:")
				for _, err := range errors {
					fmt.Printf("  - %s\n", err)
				}
				if !continueOnError {
					os.Exit(1)
				}
			}
		} else {
			// Single image mode
			if imageSpec == "" {
				fmt.Println("Error: image name or --file required")
				fmt.Println("Usage: docker pull <repo:tag> [--platform <os/arch>]... [--force] [--ca-cert <path>]")
				fmt.Println("       docker pull --file <file> [--force] [--ca-cert <path>] [--continue-on-error]")
				fmt.Println("Example: docker pull library/golang:1.25-alpine")
				fmt.Println("Example: docker pull library/golang:1.25-alpine --platform linux/amd64 --platform linux/arm64")
				fmt.Println("Example: docker pull library/golang:latest --force  # Force check for newer version")
				fmt.Println("Example: docker pull library/golang:1-alpine --force  # Force update convenience tag")
				fmt.Println("Example: docker pull --file images.txt")
				fmt.Println("Example: docker pull --file images.txt --ca-cert /path/to/roxxy-ca.crt  # For remote execution")
				fmt.Println("Example: docker pull --file images.txt --continue-on-error  # Continue on errors")
				fmt.Println()
				fmt.Println("Note: 'latest' tags are automatically checked for updates after 24 hours.")
				fmt.Println("      Use --force to check any tag for updates regardless of cache age.")
				fmt.Println("      Use --ca-cert to specify CA certificate for TLS verification (required for remote execution).")
				fmt.Println("      Use --continue-on-error to continue processing remaining images when using --file.")
				os.Exit(1)
			}
			err := pullDockerImage(cfg, imageSpec, platforms, force, caCertPath)
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
		}
	case "prune":
		// docker prune                  → prune unreferenced blobs
		// docker prune --manifests      → prune digest-only manifests with no tags
		// docker prune --migrate        → migrate old manifest structure to new registry-based structure
		// docker prune --audit          → heal refcount drift before pruning
		// docker prune --audit --repair → also re-fetch missing blobs from their upstream
		if len(subargs) > 0 && subargs[0] == "--manifests" {
			pruneDockerManifests(cfg.CacheDir)
		} else if len(subargs) > 0 && subargs[0] == "--migrate" {
			migrateDockerManifests(cfg.CacheDir)
		} else {
			if len(subargs) > 0 && subargs[0] == "--audit" {
				repair := false
				for _, a := range subargs[1:] {
					if a == "--repair" {
						repair = true
					}
				}
				runDockerAuditWithRepair(cfg.CacheDir, repair)
			}
			pruneDockerBlobs(cfg.CacheDir)
		}
	case "audit":
		// docker audit → reconcile refcount files against manifest contents
		runDockerAudit(cfg.CacheDir)
	case "discover":
		// docker discover → discover tags for digest-only manifests
		discoverDockerTags(cfg.CacheDir)
	case "tag":
		// docker tag -d <repo:tag> → delete a specific tag
		if len(subargs) < 2 || subargs[0] != "-d" {
			fmt.Println("Usage: docker tag -d <repo:tag>")
			fmt.Println("Example: docker tag -d library/golang:1.25-alpine")
			os.Exit(1)
		}
		tagSpec := subargs[1]
		deleteDockerTag(cfg.CacheDir, tagSpec)
	case "rmi":
		// docker rmi <image> → cache ls --docker --delete <image>
		if len(subargs) == 0 {
			fmt.Println("Error: image name required")
			fmt.Println("Usage: docker rmi <repo:tag> or docker rmi <repo@digest>")
			os.Exit(1)
		}
		imageSpec := subargs[0]
		cli.Run(cliCfg, []string{"--docker", "--delete", imageSpec})
	case "push":
		// docker push <registry> <image> [--from-tarball <tarball>] [--file <file>]
		// docker push <registry> --file <file> [--from-tarball <tarball>]
		var registry string
		var imageSpec string
		var tarballPath string
		var filePath string

		// Parse arguments
		for i := 0; i < len(subargs); i++ {
			if subargs[i] == "--file" && i+1 < len(subargs) {
				filePath = subargs[i+1]
				i++ // Skip the file path
			} else if subargs[i] == "--from-tarball" && i+1 < len(subargs) {
				tarballPath = subargs[i+1]
				i++ // Skip the tarball path
			} else if registry == "" {
				registry = subargs[i]
			} else if imageSpec == "" {
				imageSpec = subargs[i]
			}
		}

		if filePath != "" {
			// Batch mode: read from file
			if registry == "" {
				fmt.Println("Error: registry required when using --file")
				fmt.Println("Usage: docker push <registry> --file <file> [--from-tarball <tarball>]")
				os.Exit(1)
			}

			specs, err := parseImageListFile(filePath)
			if err != nil {
				fmt.Printf("Error reading image list file: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Pushing %d images from file to %s...\n", len(specs), registry)
			for i, spec := range specs {
				fmt.Printf("\n[%d/%d] Pushing %s", i+1, len(specs), spec.Image)
				if len(spec.Platforms) > 0 {
					fmt.Printf(" (platforms: %s)", strings.Join(spec.Platforms, ", "))
				}
				fmt.Println()
				pushFromTarball(cfg, registry, spec.Image, tarballPath, "", DefaultPushWorkers, false)
			}
			fmt.Printf("\nCompleted pushing %d images\n", len(specs))
		} else {
			// Single image mode
			if registry == "" || imageSpec == "" {
				fmt.Println("Error: registry and image name required")
				fmt.Println("Usage: docker push <registry> <repo:tag> [--from-tarball <tarball>]")
				fmt.Println("       docker push <registry> --file <file> [--from-tarball <tarball>]")
				fmt.Println("Example: docker push docker.io library/golang:1.25-alpine")
				fmt.Println("Example: docker push ghcr.io myorg/myimage:v1.0 --from-tarball cache.tar")
				fmt.Println("Example: docker push docker.io --file images.txt")
				os.Exit(1)
			}
			pushFromTarball(cfg, registry, imageSpec, tarballPath, "", DefaultPushWorkers, false)
		}
	case "export":
		// docker export <image> [output.tar] [--from-tarball <tarball>] [--file <file>]
		// docker export --file <file> [--from-tarball <tarball>]
		var imageSpec string
		var outputFile string
		var tarballPath string
		var filePath string

		// Parse arguments
		for i := 0; i < len(subargs); i++ {
			if subargs[i] == "--file" && i+1 < len(subargs) {
				filePath = subargs[i+1]
				i++ // Skip the file path
			} else if subargs[i] == "--from-tarball" && i+1 < len(subargs) {
				tarballPath = subargs[i+1]
				i++ // Skip the tarball path
			} else if imageSpec == "" {
				imageSpec = subargs[i]
			} else if outputFile == "" {
				outputFile = subargs[i]
			}
		}

		if filePath != "" {
			// Batch mode: read from file
			specs, err := parseImageListFile(filePath)
			if err != nil {
				fmt.Printf("Error reading image list file: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Exporting %d images from file...\n", len(specs))
			for i, spec := range specs {
				fmt.Printf("\n[%d/%d] Exporting %s", i+1, len(specs), spec.Image)
				if len(spec.Platforms) > 0 {
					fmt.Printf(" (platforms: %s)", strings.Join(spec.Platforms, ", "))
				}
				fmt.Println()

				// Generate output filename if not provided
				exportOutputFile := outputFile
				if exportOutputFile == "" {
					// Use image name as base for filename
					safeName := strings.ReplaceAll(spec.Image, "/", "_")
					safeName = strings.ReplaceAll(safeName, ":", "_")
					exportOutputFile = safeName + ".tar"
				} else if len(specs) > 1 {
					// Multiple images: append index or image name
					ext := filepath.Ext(exportOutputFile)
					base := strings.TrimSuffix(exportOutputFile, ext)
					safeName := strings.ReplaceAll(spec.Image, "/", "_")
					safeName = strings.ReplaceAll(safeName, ":", "_")
					exportOutputFile = fmt.Sprintf("%s_%s%s", base, safeName, ext)
				}

				// Use OCI format for multi-image compatibility
				ExportMultipleDockerImagesToOCITarball(cfg, []string{spec.Image}, exportOutputFile, tarballPath, "")
			}
			fmt.Printf("\nCompleted exporting %d images\n", len(specs))
		} else {
			// Single image mode
			if imageSpec == "" {
				fmt.Println("Error: image name or --file required")
				fmt.Println("Usage: docker export <repo:tag> [output.tar] [--from-tarball <tarball>]")
				fmt.Println("       docker export --file <file> [--from-tarball <tarball>]")
				fmt.Println("Example: docker export library/golang:1.25-alpine")
				fmt.Println("Example: docker export library/golang:1.25-alpine image.tar --from-tarball cache.tar")
				fmt.Println("Example: docker export --file images.txt")
				os.Exit(1)
			}
			// Use OCI format for multi-image compatibility (works with podman load and newer docker load)
			ExportMultipleDockerImagesToOCITarball(cfg, []string{imageSpec}, outputFile, tarballPath, "")
		}
	case "find":
		// docker find <digest>
		if len(subargs) == 0 {
			fmt.Println("Error: digest required")
			fmt.Println("Usage: docker find <digest>")
			fmt.Println("Example: docker find sha256:bb9a62ad9605f7c6426a86a8a80e0a5500b5bb10a1d039621e235c2b407ca74d")
			os.Exit(1)
		}
		digest := subargs[0]
		if err := FindManifestByDigest(cfg.CacheDir, digest); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
	case "parse":
		// docker parse → extract metadata from Docker/OCI images
		var force bool
		for _, arg := range subargs {
			if arg == "--force" {
				force = true
			}
		}

		fmt.Println("Processing Docker/OCI images to extract metadata...")
		fmt.Println()
		fmt.Printf("Cache directory: %s\n", cfg.CacheDir)

		count, err := enrichDockerImages(cfg.CacheDir, force)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("✓ Processed %d Docker image(s)\n", count)
	case "override":
		handleDockerOverride(cfg, subargs)
	default:
		fmt.Printf("Unknown docker command: %s\n", subcmd)
		fmt.Println("Use 'docker ls' or 'docker rmi <image>'")
		os.Exit(1)
	}
}

// handleDockerOverride manages image overrides via the roxxy API.
// The CLI talks to the running roxxy server so overrides take effect immediately.
//
// Uses docker-tag semantics: existing image first, alias second.
//
//	roxxy docker override delphos/node:22-slim node:22-slim
//
// means "the real image is delphos/node:22-slim; when someone pulls node:22-slim, serve it"
func handleDockerOverride(cfg Config, args []string) {
	apiBase := "http://localhost:3128/api/v1/overrides"

	if len(args) == 0 {
		fmt.Println("Usage: docker override <image> <alias>")
		fmt.Println("       docker override --list")
		fmt.Println("       docker override --rm <alias>")
		fmt.Println()
		fmt.Println("Manage image overrides (docker-tag semantics: existing image first, alias second).")
		fmt.Println("When a client pulls <alias>, roxxy transparently serves <image> instead.")
		fmt.Println()
		fmt.Println("Examples:")
		fmt.Println("  docker override delphos/docker-dind:27 docker:27-dind")
		fmt.Println("  docker override delphos/node:22-slim node:22-slim")
		fmt.Println("  docker override --list")
		fmt.Println("  docker override --rm docker:27-dind")
		os.Exit(1)
	}

	client := &http.Client{Timeout: 10 * time.Second}

	switch args[0] {
	case "--list", "-l":
		resp, err := client.Get(apiBase)
		if err != nil {
			fmt.Printf("Error: cannot reach roxxy at %s: %v\n", apiBase, err)
			os.Exit(1)
		}
		defer util.DeferClose(resp.Body)
		var overrides []registry.Override
		if err := json.NewDecoder(resp.Body).Decode(&overrides); err != nil {
			fmt.Printf("Error decoding response: %v\n", err)
			os.Exit(1)
		}
		if len(overrides) == 0 {
			fmt.Println("No image overrides configured.")
			return
		}
		fmt.Printf("%-50s  %s\n", "SOURCE", "TARGET")
		for _, o := range overrides {
			fmt.Printf("%-50s  %s\n", o.Source, o.Target)
		}

	case "--rm", "--remove", "-d":
		if len(args) < 2 {
			fmt.Println("Usage: docker override --rm <source>")
			os.Exit(1)
		}
		source := args[1]
		req, _ := http.NewRequest("DELETE", apiBase+"/"+url.PathEscape(source), nil)
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("Error: cannot reach roxxy: %v\n", err)
			os.Exit(1)
		}
		defer util.DeferClose(resp.Body)
		if resp.StatusCode == http.StatusNotFound {
			fmt.Printf("No override found for %s\n", source)
			os.Exit(1)
		}
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			fmt.Printf("Error: server returned %s\n", resp.Status)
			os.Exit(1)
		}
		fmt.Printf("Removed override for %s\n", source)

	default:
		// Create/update: override <image> <alias>
		// image = real image to serve, alias = what client requests
		if len(args) < 2 {
			fmt.Println("Usage: docker override <image> <alias>")
			os.Exit(1)
		}
		target := args[0] // real image
		source := args[1] // alias that gets overridden
		body, _ := json.Marshal(registry.Override{Source: source, Target: target})

		// Try PUT first (update), fall back to POST (create)
		req, _ := http.NewRequest("PUT", apiBase+"/"+url.PathEscape(source), strings.NewReader(`{"target":"`+target+`"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("Error: cannot reach roxxy: %v\n", err)
			os.Exit(1)
		}
		util.Logerr(resp.Body.Close())

		if resp.StatusCode >= 400 {
			// Fall back to POST
			resp, err = client.Post(apiBase, "application/json", strings.NewReader(string(body)))
			if err != nil {
				fmt.Printf("Error: cannot reach roxxy: %v\n", err)
				os.Exit(1)
			}
			defer util.DeferClose(resp.Body)
			if resp.StatusCode >= 400 {
				fmt.Printf("Error: server returned %s\n", resp.Status)
				os.Exit(1)
			}
		}
		fmt.Printf("Override set: %s -> %s\n", source, target)
	}
}

// runDockerAudit calls runDockerAuditWithRepair without repair (back-compat
// for callers that don't need the repair path).
func runDockerAudit(cacheDir string) {
	runDockerAuditWithRepair(cacheDir, false)
}

// runDockerAuditWithRepair reconciles refcount files against manifest contents,
// heals drift, and (if repair=true) attempts to re-fetch missing blobs from
// their upstream. Two passes: manifest audit (compute refcounts from tag/index
// references, cascade-remove orphan manifests) then blob audit (recompute
// blob refcounts from surviving manifests, prune orphan blobs).
func runDockerAuditWithRepair(cacheDir string, repair bool) {
	fmt.Println("Auditing Docker manifest refcounts against tags/index references...")
	mrep, err := cache.AuditManifests(cacheDir)
	if err != nil {
		fmt.Printf("Manifest audit error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Manifest files scanned:           %d\n", mrep.ManifestsScanned)
	fmt.Printf("  Tag references counted:           %d\n", mrep.TagRefs)
	fmt.Printf("  Index sub-manifest references:    %d\n", mrep.IndexSubRefs)
	fmt.Printf("  Orphan manifests removed:         %d (freed %s)\n", mrep.ManifestsRemoved, util.FormatBytes(mrep.ManifestBytesFreed))
	fmt.Printf("  Stale manifest refcounts cleared: %d\n", mrep.StaleManifestRefcounts)
	fmt.Printf("  Orphan sidecars removed:          %d (freed %s)\n", mrep.OrphanSidecarsRemoved, util.FormatBytes(mrep.OrphanSidecarBytesFreed))
	fmt.Printf("  Fresh manifest refcounts written: %d\n", mrep.ManifestRefcountsWritten)

	fmt.Println("Auditing Docker blob refcounts against manifest contents...")
	rep, err := cache.AuditRefcounts(cacheDir)
	if err != nil {
		fmt.Printf("Blob audit error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Manifests scanned: %d\n", rep.ManifestsScanned)
	fmt.Printf("  Blobs scanned:     %d\n", rep.BlobsScanned)
	fmt.Printf("  Orphan blobs deleted: %d (freed %s)\n", rep.BlobsRemoved, util.FormatBytes(rep.BytesFreed))
	fmt.Printf("  Refcounts healed:  %d\n", rep.RefcountsHealed)
	fmt.Printf("  Stale refcount files removed: %d\n", rep.StaleRefcountsRemoved)
	if rep.SpeculativePlatformBlobsMissing > 0 {
		fmt.Printf("  Speculative platform blobs missing: %d (expected — sub-manifests of indexes whose platform was never pulled)\n",
			rep.SpeculativePlatformBlobsMissing)
	}
	if rep.ManifestOnlyFetchBlobsMissing > 0 {
		fmt.Printf("  Manifest-only fetch blobs missing:  %d (expected — manifest inspected/probed but layers never pulled)\n",
			rep.ManifestOnlyFetchBlobsMissing)
	}
	for _, w := range rep.MissingBlobWarnings {
		fmt.Printf("  WARNING: %s\n", w)
	}

	// Surface per-repo attribution for refcount drift + orphan blobs.
	// The actual leaker (the manifest that was deleted without
	// decrementing) is gone, but its repo path tends to recur across
	// current referrers — so high counts point at the buggy code path
	// (per-repo push/rmi handler). "(unattributed)" tallies orphans
	// whose entire referrer chain is gone — high values here mean
	// audit-time inference can't reach them and an rmi-time journal
	// is needed.
	if len(rep.LeakAttributionByRepo) > 0 {
		// Sort by count desc, then by repo name asc for stable output.
		type repoCount struct {
			repo  string
			count int
		}
		ranked := make([]repoCount, 0, len(rep.LeakAttributionByRepo))
		for repo, c := range rep.LeakAttributionByRepo {
			ranked = append(ranked, repoCount{repo, c})
		}
		sort.Slice(ranked, func(i, j int) bool {
			if ranked[i].count != ranked[j].count {
				return ranked[i].count > ranked[j].count
			}
			return ranked[i].repo < ranked[j].repo
		})
		fmt.Println("  Leaked-ref attribution (blobs with refcount drift, by current referrer repo):")
		for _, rc := range ranked {
			fmt.Printf("    %-50s %d\n", rc.repo, rc.count)
		}
	}

	if repair && len(rep.RepairTargets) > 0 {
		fmt.Printf("\nAttempting to re-fetch %d missing blob(s) from upstream...\n", len(rep.RepairTargets))
		ok, failed := repairMissingBlobs(cacheDir, rep.RepairTargets)
		fmt.Printf("  Re-fetched: %d   Failed: %d\n", ok, failed)
	}
}

// repairMissingBlobs groups RepairTargets by (upstream, repo) and re-fetches
// each blob via the same logic the docker pull command uses (handles bearer
// token auth challenges; honors existing cache-dir structure).
func repairMissingBlobs(cacheDir string, targets []cache.RepairTarget) (int, int) {
	type key struct{ upstream, repo string }
	grouped := make(map[key][]string)
	for _, t := range targets {
		k := key{t.UpstreamURL, t.RepoName}
		grouped[k] = append(grouped[k], t.Digest)
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	ok, failed := 0, 0
	for k, digests := range grouped {
		fmt.Printf("  %s/%s: %d blob(s)\n", k.upstream, k.repo, len(digests))
		token := ""
		if err := pullBlobs(client, cacheDir, k.upstream, k.repo, digests, &token); err != nil {
			fmt.Printf("    failed: %v\n", err)
			failed += len(digests)
			continue
		}
		ok += len(digests)
	}
	return ok, failed
}

// pruneDockerBlobs removes unreferenced Docker blobs
func pruneDockerBlobs(cacheDir string) {
	blobDir := filepath.Join(cacheDir, "docker-blobs")

	fmt.Println("Pruning unreferenced Docker blobs...")

	// Read all blobs (walk recursively to handle subdirectories)
	prunedCount := 0
	prunedSize := int64(0)

	err := filepath.WalkDir(blobDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // Continue on errors
		}
		if d.IsDir() {
			return nil // Skip directories
		}

		blobName := filepath.Base(path)
		// Blob names are digests like "sha256:abc123..."
		if !strings.HasPrefix(blobName, "sha256:") {
			return nil
		}

		// Extract content hash (without "sha256:" prefix)
		contentHash := strings.TrimPrefix(blobName, "sha256:")

		// Reclaim if either:
		//   - the refcount file is missing (legacy "never seen" / explicitly
		//     gone), OR
		//   - the refcount file is present at value 0 (DecrementRefcount no
		//     longer removes the file when count hits 0; it parks it at 0
		//     so a quick re-pull can rebuild without re-fetching the blob).
		refcountPath := cache.GetRefcountPath(cacheDir, contentHash)
		unreferenced := false
		if data, err := os.ReadFile(refcountPath); err != nil {
			if os.IsNotExist(err) {
				unreferenced = true
			}
		} else {
			count := 0
			if _, scanErr := fmt.Sscanf(string(data), "%d", &count); scanErr == nil && count == 0 {
				unreferenced = true
			}
		}
		if !unreferenced {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil
		}
		if err := os.Remove(path); err != nil {
			fmt.Printf("  Warning: Failed to delete blob %s: %v\n", blobName[:19], err)
			return nil
		}
		// Also drop the now-meaningless refcount=0 file. Missing-file is the
		// canonical "blob and refcount both gone" state.
		_ = os.Remove(refcountPath)
		prunedCount++
		prunedSize += info.Size()
		fmt.Printf("  Pruned unreferenced blob: %s (%s)\n", blobName[:19], util.FormatBytes(info.Size()))
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No Docker blobs directory found")
			return
		}
		fmt.Printf("Error reading blob directory: %v\n", err)
		os.Exit(1)
	}

	if prunedCount == 0 {
		fmt.Println("No unreferenced blobs found")
	} else {
		fmt.Printf("\nPruned %d unreferenced blob(s), freed %s\n", prunedCount, util.FormatBytes(prunedSize))
	}

	// Also sweep download temp files that lingered after an aborted /
	// crashed / killed transfer. Long-running daemons accumulate these
	// otherwise — startup is too rare for cleanup. The operator runs
	// prune when they want to reclaim space, and these are part of
	// that bucket. Live downloads with temp files actively being
	// written are at risk if prune runs concurrently with traffic;
	// this is operator-invoked, so that's their call to make.
	fmt.Println("\nPruning orphan download temp files...")
	rep, err := cache.PruneOrphanTemps(cacheDir)
	if err != nil {
		fmt.Printf("  Warning: temp-file sweep failed: %v\n", err)
	} else if rep.Filesystem == 0 {
		fmt.Println("No orphan temp files found")
	} else {
		fmt.Printf("Pruned %d orphan temp file(s), freed %s (%d delete errors)\n",
			rep.Filesystem, util.FormatBytes(rep.BytesFreed), rep.Errors)
	}
}

// pruneDockerManifests removes digest-only manifests that have no tags
func pruneDockerManifests(cacheDir string) {
	manifestDir := filepath.Join(cacheDir, "docker-manifests")

	fmt.Println("Pruning digest-only manifests with no tags...")

	prunedCount := 0
	var prunedSize int64 = 0

	err := filepath.WalkDir(manifestDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		if strings.HasSuffix(path, ".meta") {
			return nil
		}

		relPath, err := filepath.Rel(manifestDir, path)
		if err != nil {
			return nil
		}

		parts := strings.Split(relPath, string(filepath.Separator))
		// New structure: <registry>/<repo>/<reference>
		if len(parts) != 3 {
			return nil
		}

		registryID := parts[0]
		repoName := parts[1]
		reference := parts[2]

		// Only process digest-only manifests
		if !strings.HasPrefix(reference, "sha256:") {
			return nil
		}

		// Check if this repo directory has any tag files
		repoDir := filepath.Dir(path)
		entries, err := os.ReadDir(repoDir)
		if err != nil {
			return nil
		}

		hasTag := false
		for _, entry := range entries {
			if !entry.IsDir() && !strings.HasSuffix(entry.Name(), ".meta") {
				if !strings.HasPrefix(entry.Name(), "sha256:") {
					hasTag = true
					break
				}
			}
		}

		// If repo has tags, keep the digest-only manifest (it might be referenced)
		if hasTag {
			return nil
		}

		// No tags found - this is an orphaned digest-only manifest
		info, err := os.Stat(path)
		if err != nil {
			return nil
		}

		manifestSize := info.Size()

		if _, err := cache.RemoveManifest(cacheDir, path); err != nil {
			return nil
		}

		prunedCount++
		prunedSize += manifestSize

		repoNameNormalized := strings.ReplaceAll(repoName, "_", "/")
		fmt.Printf("  Pruned digest-only manifest: [%s] %s/%s (manifest %s; orphan blobs reclaimed by 'docker prune-blobs')\n",
			registryID, repoNameNormalized, reference[:12],
			util.FormatBytes(manifestSize))

		return nil
	})

	if err != nil {
		fmt.Printf("Error walking manifest directory: %v\n", err)
		os.Exit(1)
	}

	if prunedCount == 0 {
		fmt.Println("No orphaned digest-only manifests found")
	} else {
		fmt.Printf("\nPruned %d orphaned manifest(s), freed %s\n", prunedCount, util.FormatBytes(prunedSize))
	}
}

// migrateDockerManifests migrates manifests from old structure to new registry-based structure
// Old: docker-manifests/<repo>/<reference>
// New: docker-manifests/<registry>/<repo>/<reference>
func migrateDockerManifests(cacheDir string) {
	manifestDir := filepath.Join(cacheDir, "docker-manifests")

	fmt.Println("Migrating Docker manifests to registry-based structure...")
	fmt.Println("This will move manifests from docker-manifests/<repo>/ to docker-manifests/<registry>/<repo>/")

	migratedCount := 0
	var migratedSize int64 = 0

	err := filepath.WalkDir(manifestDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		if strings.HasSuffix(path, ".meta") {
			return nil
		}

		relPath, err := filepath.Rel(manifestDir, path)
		if err != nil {
			return nil
		}

		parts := strings.Split(relPath, string(filepath.Separator))

		// Skip if already in new structure (3 parts: registry/repo/reference)
		if len(parts) == 3 {
			return nil
		}

		// Old structure: repo/reference (2 parts)
		if len(parts) != 2 {
			return nil
		}

		repoName := parts[0]
		reference := parts[1]

		// Read metadata to get upstream URL
		metaPath := path + ".meta"
		upstreamURL := ""
		if metaData, err := os.ReadFile(metaPath); err == nil {
			var meta struct {
				UpstreamURL string `json:"upstream_url"`
			}
			if json.Unmarshal(metaData, &meta) == nil && meta.UpstreamURL != "" {
				upstreamURL = meta.UpstreamURL
			}
		}

		// Determine registry from upstream URL or repo name
		registryID := "docker.io" // default
		if upstreamURL != "" {
			registryID = NormalizeRegistryURLForPath(upstreamURL)
		} else {
			// Try to guess from repo name
			// If it's not library/* and has org/user pattern, might be ghcr.io
			normalizedRepo := UnsafeRepoName(repoName)
			// Could be ghcr.io, but we default to docker.io for safety. User can re-run discover to fix tags
			_ = normalizedRepo // Used for potential future registry detection logic
		}

		// Skip if this registry directory already exists and has this repo
		newRepoDir := filepath.Join(manifestDir, registryID, repoName)
		if _, err := os.Stat(newRepoDir); err == nil {
			// Check if this specific file already exists
			newPath := filepath.Join(newRepoDir, reference)
			if _, err := os.Stat(newPath); err == nil {
				// Already migrated, delete old file
				util.Logerr(os.Remove(path))
				util.Logerr(os.Remove(metaPath))
				return nil
			}
		}

		// Read the manifest file
		info, err := os.Stat(path)
		if err != nil {
			return nil
		}
		manifestSize := info.Size()

		manifestData, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		// Read metadata
		metaData, _ := os.ReadFile(metaPath)

		// Create new directory structure
		if err := os.MkdirAll(newRepoDir, 0755); err != nil {
			fmt.Printf("  ✗ Failed to create directory %s: %v\n", newRepoDir, err)
			return nil
		}

		// Write to new location
		newPath := filepath.Join(newRepoDir, reference)
		if err := os.WriteFile(newPath, manifestData, 0644); err != nil {
			fmt.Printf("  ✗ Failed to write %s: %v\n", newPath, err)
			return nil
		}

		// Write metadata to new location
		if len(metaData) > 0 {
			newMetaPath := newPath + ".meta"
			util.Logerr(os.WriteFile(newMetaPath, metaData, 0644))
		}

		// Delete old files
		util.Logerr(os.Remove(path))
		util.Logerr(os.Remove(metaPath))

		migratedCount++
		migratedSize += manifestSize

		normalizedRepo := UnsafeRepoName(repoName)
		refDisplay := reference
		if len(reference) > 12 {
			refDisplay = reference[:12]
		}
		fmt.Printf("  Migrated [%s] %s/%s (size: %s)\n", registryID, normalizedRepo, refDisplay, util.FormatBytes(manifestSize))

		return nil
	})

	if err != nil {
		fmt.Printf("Error walking manifest directory: %v\n", err)
		os.Exit(1)
	}

	if migratedCount == 0 {
		fmt.Println("No manifests to migrate (all are already in the new structure)")
	} else {
		fmt.Printf("\nMigrated %d manifest(s), moved %s\n", migratedCount, util.FormatBytes(migratedSize))
		fmt.Println("Note: You may want to run 'docker discover' to find tags for migrated manifests")
	}
}

// deleteDockerTag deletes a specific tag from the cache (keeps the manifest by digest)
func deleteDockerTag(cacheDir, tagSpec string) {
	// Parse tag spec: repo:tag
	parts := strings.Split(tagSpec, ":")
	if len(parts) != 2 {
		fmt.Printf("Error: Invalid tag specification: %s (expected repo:tag)\n", tagSpec)
		os.Exit(1)
	}

	repoName := parts[0]
	tagName := parts[1]

	// Normalize repo name (e.g., "golang" -> "library/golang")
	normalizedRepo := NormalizeRepoName(repoName)

	// Find the manifest - it could be in any registry directory
	// We need to search for it since we don't know which registry it came from
	manifestDir := filepath.Join(cacheDir, "docker-manifests")
	var tagPath string
	var found bool

	// Walk through registry directories to find the tag
	err := filepath.WalkDir(manifestDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		// Check if this is the tag we're looking for
		relPath, err := filepath.Rel(manifestDir, path)
		if err != nil {
			return nil
		}

		parts := strings.Split(relPath, string(filepath.Separator))
		if len(parts) == 3 {
			// Structure: <registry>/<safeRepo>/<reference>
			safeRepo := parts[1]
			reference := parts[2]
			if UnsafeRepoName(safeRepo) == normalizedRepo && reference == tagName {
				tagPath = path
				found = true
				return filepath.SkipAll
			}
		}

		return nil
	})

	if !found || err != nil {
		fmt.Printf("Tag not found: %s:%s\n", normalizedRepo, tagName)
		os.Exit(1)
	}

	// Check if tag exists
	if _, err := os.Stat(tagPath); os.IsNotExist(err) {
		fmt.Printf("Tag not found: %s:%s\n", repoName, tagName)
		os.Exit(1)
	}

	// Use cache.RemoveManifest so blob refcounts are decremented in lockstep
	// with the tag-file removal. The previous raw os.Remove path leaked
	// refcounts: blobs the deleted tag-file referenced kept their inflated
	// count, so a later prune would either undercount (orphan blobs stay
	// forever) or, if it ran after the centralized inc fix, overcount and
	// reach 0 too late.
	if _, err := cache.RemoveManifest(cacheDir, tagPath); err != nil {
		fmt.Printf("Error deleting tag: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Deleted tag: %s:%s (manifest by digest is preserved; run 'docker prune-blobs' to reclaim now-unreferenced blob storage)\n", normalizedRepo, tagName)
}

// discoverDockerTags scans manifests stored by digest and tries to discover their tags
// It also processes tagged manifests to fetch missing platform manifests
func discoverDockerTags(cacheDir string) {
	fmt.Println("Discovering tags for manifests stored by digest and fetching missing platform manifests...")

	manifestDir := filepath.Join(cacheDir, "docker-manifests")
	discoveredCount := 0
	platformFetchedCount := 0
	var wg sync.WaitGroup

	err := filepath.WalkDir(manifestDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		if strings.HasSuffix(path, ".meta") {
			return nil
		}

		relPath, err := filepath.Rel(manifestDir, path)
		if err != nil {
			return nil
		}

		parts := strings.Split(relPath, string(filepath.Separator))
		// New structure: <registry>/<repo>/<reference>
		if len(parts) != 3 {
			return nil
		}

		registryID := parts[0]
		repoName := parts[1]
		reference := parts[2]

		// Check if we already have a tag file
		repoDir := filepath.Dir(path)
		entries, err := os.ReadDir(repoDir)
		if err != nil {
			return nil
		}

		hasTag := false
		for _, entry := range entries {
			if !entry.IsDir() && !strings.HasSuffix(entry.Name(), ".meta") {
				if !strings.HasPrefix(entry.Name(), "sha256:") {
					hasTag = true
					break
				}
			}
		}

		// If this is a tag file (not a digest), check if we need to fetch platform manifests
		if !strings.HasPrefix(reference, "sha256:") {
			// This is a tag file - check if it's a multi-platform index missing platform manifests
			manifestBody, err := os.ReadFile(path)
			if err != nil {
				return nil
			}

			// Check if this is a multi-platform index
			var index struct {
				MediaType string `json:"mediaType"`
				Manifests []struct {
					Digest   string `json:"digest"`
					Platform struct {
						Architecture string `json:"architecture"`
						OS           string `json:"os"`
						Variant      string `json:"variant,omitempty"`
					} `json:"platform,omitempty"`
				} `json:"manifests"`
			}

			if err := json.Unmarshal(manifestBody, &index); err == nil {
				isIndex := index.MediaType == "application/vnd.oci.image.index.v1+json" ||
					index.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json"

				if isIndex {
					// Check which platform manifests are missing
					var missingPlatforms []string

					for _, manifest := range index.Manifests {
						// Skip attestations/SBOMs (unknown platform)
						if manifest.Platform.OS == "unknown" || manifest.Platform.Architecture == "unknown" {
							continue
						}
						if manifest.Platform.OS == "" || manifest.Platform.Architecture == "" {
							continue
						}

						// Check if platform manifest exists
						platformManifestPath := filepath.Join(repoDir, manifest.Digest)
						if _, err := os.Stat(platformManifestPath); err != nil {
							missingPlatforms = append(missingPlatforms, manifest.Digest)
						}
					}

					if len(missingPlatforms) > 0 {
						// Get upstream URL from metadata
						metaPath := path + ".meta"
						upstreamURL := ""
						authToken := ""

						if metaData, err := os.ReadFile(metaPath); err == nil {
							var meta struct {
								UpstreamURL string `json:"upstream_url"`
							}
							if json.Unmarshal(metaData, &meta) == nil && meta.UpstreamURL != "" {
								upstreamURL = meta.UpstreamURL
							}
						}

						if upstreamURL == "" {
							// Map registry ID back to upstream URL
							switch registryID {
							case "docker.io":
								upstreamURL = "https://registry-1.docker.io"
							case "quay.io":
								upstreamURL = "https://quay.io"
							case "ghcr.io":
								upstreamURL = "https://ghcr.io"
							case "registry.k8s.io":
								upstreamURL = "https://registry.k8s.io"
							case "nvcr.io":
								upstreamURL = "https://nvcr.io"
							default:
								upstreamURL = "https://" + registryID
							}
						}

						normalizedRepo := NormalizeRepoName(UnsafeRepoName(repoName))

						fmt.Printf("  Fetching %d missing platform manifest(s) for [%s] %s/%s...\n", len(missingPlatforms), registryID, normalizedRepo, reference)
						for _, platformDigest := range missingPlatforms {
							wg.Add(1)
							go func(digest string) {
								defer wg.Done()
								fetchPlatformManifestForDiscover(cacheDir, upstreamURL, normalizedRepo, digest, authToken)
							}(platformDigest)
						}
						platformFetchedCount += len(missingPlatforms)
					}
				}
			}
			return nil // Skip tag files for tag discovery (they already have tags)
		}

		// This is a digest-only manifest - check if it needs tag discovery
		if hasTag {
			return nil // Already has tags, skip tag discovery
		}

		// Check manifest age - skip very old manifests (older than 30 days)
		// These are likely from old images that no longer exist in the registry
		info, err := os.Stat(path)
		if err == nil {
			age := time.Since(info.ModTime())
			if age > 30*24*time.Hour {
				fmt.Printf("  Skipping old manifest: [%s] %s/%s (age: %v)\n", registryID, repoName, reference[:12], age.Round(24*time.Hour))
				return nil
			}
		}

		manifestBody, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		meta := ReadDockerManifestMetadata(path)
		contentType := "application/vnd.docker.distribution.manifest.v2+json"
		upstreamURL := "" // Will be read from metadata if available

		if meta != nil {
			if meta.ContentType != "" {
				contentType = meta.ContentType
			}
			if meta.UpstreamURL != "" {
				upstreamURL = meta.UpstreamURL
			}
		}

		// Extract registry and repo from path structure
		// Path is: <registry>/<repo>/<reference>
		// We already have registryID from the path, so use that to determine upstream URL
		if upstreamURL == "" {
			// Map registry ID back to upstream URL
			switch registryID {
			case "docker.io":
				upstreamURL = "https://registry-1.docker.io"
			case "quay.io":
				upstreamURL = "https://quay.io"
			case "ghcr.io":
				upstreamURL = "https://ghcr.io"
			case "registry.k8s.io":
				upstreamURL = "https://registry.k8s.io"
			case "nvcr.io":
				upstreamURL = "https://nvcr.io"
			default:
				upstreamURL = "https://" + registryID
			}
		}

		// Normalize repo name for display
		normalizedRepo := NormalizeRepoName(UnsafeRepoName(repoName))

		fmt.Printf("  Checking [%s] %s/%s (registry: %s)...\n", registryID, normalizedRepo, reference[:12], upstreamURL)
		discoverTagsForDigest(cacheDir, upstreamURL, normalizedRepo, reference, manifestBody, contentType, &wg)
		discoveredCount++

		return nil
	})

	if err != nil {
		fmt.Printf("Error scanning manifests: %v\n", err)
	} else {
		fmt.Printf("\nProcessed %d digest-only manifest(s) for tag discovery\n", discoveredCount)
		if platformFetchedCount > 0 {
			fmt.Printf("Fetching %d missing platform manifest(s)...\n", platformFetchedCount)
			wg.Wait()
			fmt.Printf("Completed fetching platform manifests\n")
		}
	}
}

// discoverTagsForDigest queries upstream to find tags for a digest
func discoverTagsForDigest(cacheDir, upstreamURL, repoName, digest string, manifestBody []byte, contentType string, wg *sync.WaitGroup) {
	tagsURL := fmt.Sprintf("%s/v2/%s/tags/list", upstreamURL, repoName)

	req, err := http.NewRequest("GET", tagsURL, nil)
	if err != nil {
		return
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy: config.GetUpstreamProxyFunc(),
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("    ✗ Failed to query tags list: %v\n", err)
		return
	}
	defer util.DeferClose(resp.Body)

	// Handle 401 Unauthorized - try to get token
	var authToken string
	if resp.StatusCode == http.StatusUnauthorized {
		authHeader := resp.Header.Get("WWW-Authenticate")
		if authHeader != "" {
			// Try to get a token (for public repos, this should work)
			token, err := getDockerTokenFromChallenge(authHeader, fmt.Sprintf("/v2/%s/tags/list", repoName))
			if err != nil {
				fmt.Printf("    ✗ Failed to get token: %v\n", err)
				return
			}
			authToken = token

			// Retry with token
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err = client.Do(req)
			if err != nil {
				fmt.Printf("    ✗ Failed to retry tags list: %v\n", err)
				return
			}
			defer util.DeferClose(resp.Body)
		}
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("    ✗ Tags list query returned %d\n", resp.StatusCode)
		return
	}

	var tagsList struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tagsList); err != nil {
		fmt.Printf("    ✗ Failed to parse tags list: %v\n", err)
		return
	}

	if len(tagsList.Tags) == 0 {
		fmt.Printf("    ✗ No tags found for %s\n", repoName)
		return
	}

	fmt.Printf("    Checking %d tags...\n", len(tagsList.Tags))

	maxChecks := 100 // Check more tags to find the most specific one
	checked := 0

	// Find all matching tags, then pick the most specific (longest) one
	var matchingTags []string

	for _, tag := range tagsList.Tags {
		if checked >= maxChecks {
			break
		}
		checked++

		manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", upstreamURL, repoName, tag)
		headReq, err := http.NewRequest("HEAD", manifestURL, nil)
		if err != nil {
			continue
		}

		// Use same token if we got one earlier
		if authToken != "" {
			headReq.Header.Set("Authorization", "Bearer "+authToken)
		}

		headResp, err := client.Do(headReq)
		if err != nil {
			continue
		}
		util.Logerr(headResp.Body.Close())

		switch headResp.StatusCode {
		case http.StatusOK:
			tagDigest := headResp.Header.Get("Docker-Content-Digest")
			if tagDigest == digest {
				matchingTags = append(matchingTags, tag)
			}
		case http.StatusUnauthorized:
			// Try to get token and retry
			authHeader := headResp.Header.Get("WWW-Authenticate")
			if authHeader != "" {
				fmt.Printf("      ⚠ Tag %s requires authentication\n", tag)
			}
		}
	}

	if len(matchingTags) == 0 {
		fmt.Printf("      ✗ No matching tag found (checked %d tags, digest: %s) - manifest may be from an old/deleted image\n", checked, digest[:12])
		return
	}

	// Store the manifest with all matching tags
	// We need to determine which registry this came from to construct the correct path
	// Since we're called from discoverDockerTags, we should use the upstreamURL to determine registry
	registryID := NormalizeRegistryURLForPath(upstreamURL)
	repoDir := DockerManifestDir(cacheDir, registryID, repoName)

	if err := os.MkdirAll(repoDir, 0755); err != nil {
		fmt.Printf("      ✗ Failed to create directory: %v\n", err)
		return
	}

	// Check if this is a multi-platform index - if so, proactively fetch platform manifests
	var index struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
				Variant      string `json:"variant,omitempty"`
			} `json:"platform,omitempty"`
		} `json:"manifests"`
	}

	isIndex := false
	var platformManifests []string
	if err := json.Unmarshal(manifestBody, &index); err == nil {
		isIndex = index.MediaType == "application/vnd.oci.image.index.v1+json" ||
			index.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json"

		if isIndex {
			// Collect platform manifest digests to fetch (fetch all, they're tiny)
			for _, manifest := range index.Manifests {
				// Skip attestations/SBOMs (unknown platform)
				if manifest.Platform.OS == "unknown" || manifest.Platform.Architecture == "unknown" {
					continue
				}
				if manifest.Platform.OS == "" || manifest.Platform.Architecture == "" {
					continue
				}
				platformManifests = append(platformManifests, manifest.Digest)
			}
		}
	}

	storedCount := 0
	for _, tag := range matchingTags {
		tagPath := filepath.Join(repoDir, tag)

		if err := os.WriteFile(tagPath, manifestBody, 0644); err != nil {
			fmt.Printf("      ✗ Failed to write tag manifest for %s: %v\n", tag, err)
			continue
		}

		meta := &DockerManifestMetadata{
			ContentType: contentType,
			StoredAt:    time.Now(),
			UpstreamURL: upstreamURL, // Store the registry URL so we know where it came from
		}
		if err := WriteDockerManifestMetadata(tagPath, meta); err != nil {
			fmt.Printf("      ⚠ Failed to write metadata for %s: %v\n", tag, err)
		}
		storedCount++
	}

	// Proactively fetch platform manifests (but not blobs)
	if isIndex && len(platformManifests) > 0 {
		fmt.Printf("      → Proactively fetching %d platform manifest(s) (manifests only, no blobs)\n", len(platformManifests))
		for _, platformDigest := range platformManifests {
			if wg != nil {
				wg.Add(1)
				go func(digest string) {
					defer wg.Done()
					fetchPlatformManifestForDiscover(cacheDir, upstreamURL, repoName, digest, authToken)
				}(platformDigest)
			} else {
				go fetchPlatformManifestForDiscover(cacheDir, upstreamURL, repoName, platformDigest, authToken)
			}
		}
	}

	if storedCount > 0 {
		if len(matchingTags) > 1 {
			fmt.Printf("      ✓ Found and stored %d matching tags: %s (digest: %s)\n", storedCount, strings.Join(matchingTags, ", "), digest[:12])
		} else {
			fmt.Printf("      ✓ Found and stored tag: %s/%s (digest: %s)\n", repoName, matchingTags[0], digest[:12])
		}
	}
}

// fetchPlatformManifestForDiscover proactively fetches a platform manifest (but not blobs) during discover
func fetchPlatformManifestForDiscover(cacheDir, upstreamURL, repoName, digest, authToken string) {
	registryID := NormalizeRegistryURLForPath(upstreamURL)
	manifestPath := DockerManifestPath(cacheDir, registryID, repoName, digest)

	// Check if already cached
	if _, err := os.Stat(manifestPath); err == nil {
		return // Already cached
	}

	// Fetch manifest from upstream
	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", upstreamURL, repoName, digest)
	req, err := http.NewRequest("GET", manifestURL, nil)
	if err != nil {
		return
	}

	// Add Accept headers
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json")

	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy: config.GetUpstreamProxyFunc(),
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("        ✗ Failed to fetch platform manifest %s: %v\n", digest[:12], err)
		return
	}
	defer util.DeferClose(resp.Body)

	// Handle 401 Unauthorized - try to get token
	if resp.StatusCode == http.StatusUnauthorized {
		authHeader := resp.Header.Get("WWW-Authenticate")
		if authHeader != "" {
			// Try to get a token
			token, err := getDockerTokenFromChallenge(authHeader, fmt.Sprintf("/v2/%s/manifests/%s", repoName, digest))
			if err == nil && token != "" {
				// Retry with token
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err = client.Do(req)
				if err != nil {
					fmt.Printf("        ✗ Failed to retry platform manifest %s: %v\n", digest[:12], err)
					return
				}
				defer util.DeferClose(resp.Body)
			}
		}
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("        ✗ Platform manifest %s returned %d\n", digest[:12], resp.StatusCode)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/vnd.docker.distribution.manifest.v2+json"
	}

	// Store manifest
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0755); err != nil {
		fmt.Printf("        ✗ Failed to create directory for platform manifest %s: %v\n", digest[:12], err)
		return
	}

	if err := os.WriteFile(manifestPath, body, 0644); err != nil {
		fmt.Printf("        ✗ Failed to write platform manifest %s: %v\n", digest[:12], err)
		return
	}

	// Store metadata
	metaPath := manifestPath + ".meta"
	meta := struct {
		ContentType string    `json:"content_type"`
		StoredAt    time.Time `json:"stored_at"`
		UpstreamURL string    `json:"upstream_url,omitempty"`
	}{
		ContentType: contentType,
		StoredAt:    time.Now(),
		UpstreamURL: upstreamURL,
	}
	metaData, _ := json.Marshal(meta)
	util.Logerr(os.WriteFile(metaPath, metaData, 0644))

	fmt.Printf("        ✓ Stored platform manifest %s\n", digest[:12])
}

// getDockerTokenFromChallenge gets a Bearer token from Docker registry auth challenge
func getDockerTokenFromChallenge(wwwAuth, path string) (string, error) {
	return getTokenFromChallengeHelper(wwwAuth, path)
}

// exportDockerImage exports a Docker image as an OCI tarball
func exportDockerImage(cacheDir, imageSpec, outputFile string) {
	// Parse image spec: repo:tag or repo@digest
	var repoName, reference string
	var isDigest bool

	if strings.Contains(imageSpec, "@") {
		// Digest format: repo@sha256:...
		parts := strings.SplitN(imageSpec, "@", 2)
		repoName = parts[0]
		reference = parts[1]
		isDigest = true
	} else if strings.Contains(imageSpec, ":") {
		// Tag format: repo:tag
		parts := strings.SplitN(imageSpec, ":", 2)
		repoName = parts[0]
		reference = parts[1]
		isDigest = false
	} else {
		fmt.Printf("Error: Invalid image specification: %s (expected repo:tag or repo@digest)\n", imageSpec)
		os.Exit(1)
	}

	// Normalize repo name (e.g., "golang" -> "library/golang")
	if !strings.Contains(repoName, "/") {
		repoName = "library/" + repoName
	}

	// Find manifest path
	// Structure: docker-manifests/<registry>/<repo>/<reference>
	// We need to find which registry this repo is from
	var manifestPath string

	err := filepath.Walk(filepath.Join(cacheDir, "docker-manifests"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		// Skip .meta files
		if strings.HasSuffix(path, ".meta") {
			return nil
		}

		relPath, err := filepath.Rel(filepath.Join(cacheDir, "docker-manifests"), path)
		if err != nil {
			return nil
		}

		parts := strings.Split(relPath, string(filepath.Separator))
		if len(parts) != 3 {
			return nil
		}

		safeRepo := parts[1]
		ref := parts[2]

		// Check if this matches our repo and reference
		unsafeRepo := strings.ReplaceAll(safeRepo, "_", "/")
		repoMatch := unsafeRepo == repoName
		refMatch := ref == reference

		if repoMatch && refMatch {
			manifestPath = path
			return filepath.SkipAll // Found it, stop walking
		}

		return nil
	})

	if manifestPath == "" || err != nil {
		fmt.Printf("Error: Image not found in cache: %s\n", imageSpec)
		os.Exit(1)
	}

	// Read manifest
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		fmt.Printf("Error reading manifest: %v\n", err)
		os.Exit(1)
	}

	// Parse manifest to get blob digests
	var manifest struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		Config        struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}

	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		fmt.Printf("Error parsing manifest: %v\n", err)
		os.Exit(1)
	}

	// Collect all blob digests
	var blobDigests []string
	if manifest.Config.Digest != "" {
		blobDigests = append(blobDigests, manifest.Config.Digest)
	}
	for _, layer := range manifest.Layers {
		if layer.Digest != "" {
			blobDigests = append(blobDigests, layer.Digest)
		}
	}

	// Verify all blobs exist
	for _, digest := range blobDigests {
		blobPath := util.DockerBlobPathWithFallback(cacheDir, digest)
		if _, err := os.Stat(blobPath); os.IsNotExist(err) {
			fmt.Printf("Error: Blob not found: %s\n", digest)
			os.Exit(1)
		}
	}

	// Determine output filename
	if outputFile == "" {
		// Generate filename from repo and tag
		safeName := strings.ReplaceAll(repoName, "/", "_")
		if isDigest {
			// Use first 12 chars of digest
			digestShort := strings.TrimPrefix(reference, "sha256:")[:12]
			outputFile = fmt.Sprintf("%s_%s.tar", safeName, digestShort)
		} else {
			outputFile = fmt.Sprintf("%s_%s.tar", safeName, reference)
		}
	}

	// Create OCI tarball
	fmt.Printf("Exporting %s to %s...\n", imageSpec, outputFile)

	file, err := os.Create(outputFile)
	if err != nil {
		fmt.Printf("Error creating output file: %v\n", err)
		os.Exit(1)
	}
	defer util.DeferClose(file)

	tw := tar.NewWriter(file)
	defer util.DeferClose(tw)

	// Write oci-layout file (optional but recommended for OCI format)
	ociLayout := `{"imageLayoutVersion":"1.0.0"}`
	if err := writeTarEntry(tw, "oci-layout", []byte(ociLayout), 0644); err != nil {
		fmt.Printf("Error writing oci-layout: %v\n", err)
		os.Exit(1)
	}

	// Write index.json (OCI image index)
	// For single image, we create a simple index pointing to the manifest
	manifestDigest := calculateDigest(manifestData)
	index := map[string]interface{}{
		"schemaVersion": 2,
		"manifests": []map[string]interface{}{
			{
				"mediaType": manifest.MediaType,
				"digest":    manifestDigest,
				"size":      len(manifestData),
			},
		},
	}
	indexData, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		fmt.Printf("Error creating index: %v\n", err)
		os.Exit(1)
	}
	if err := writeTarEntry(tw, "index.json", indexData, 0644); err != nil {
		fmt.Printf("Error writing index.json: %v\n", err)
		os.Exit(1)
	}

	// Write manifest to blobs directory (OCI format: blobs/sha256/<digest-without-prefix>)
	digestWithoutPrefix := strings.TrimPrefix(manifestDigest, "sha256:")
	manifestBlobPath := fmt.Sprintf("blobs/sha256/%s", digestWithoutPrefix)
	if err := writeTarEntry(tw, manifestBlobPath, manifestData, 0644); err != nil {
		fmt.Printf("Error writing manifest blob: %v\n", err)
		os.Exit(1)
	}

	// Write all blobs by streaming from disk — never buffer entire layers in memory
	for _, digest := range blobDigests {
		blobPath := util.DockerBlobPathWithFallback(cacheDir, digest)
		info, err := os.Stat(blobPath)
		if err != nil {
			fmt.Printf("Error stat'ing blob %s: %v\n", digest, err)
			os.Exit(1)
		}
		blobFile, err := os.Open(blobPath)
		if err != nil {
			fmt.Printf("Error opening blob %s: %v\n", digest, err)
			os.Exit(1)
		}

		digestWithoutPrefix := strings.TrimPrefix(digest, "sha256:")
		blobTarPath := fmt.Sprintf("blobs/sha256/%s", digestWithoutPrefix)

		if err := writeTarEntryStream(tw, blobTarPath, info.Size(), blobFile, 0644); err != nil {
			util.DeferClose(blobFile)
			fmt.Printf("Error writing blob %s: %v\n", digest, err)
			os.Exit(1)
		}
		util.DeferClose(blobFile)
		fmt.Printf("  Added blob: %s\n", digest[:19])
	}

	fmt.Printf("Successfully exported %s to %s\n", imageSpec, outputFile)
}

// writeTarEntry writes a file entry to a tar archive
func writeTarEntry(tw *tar.Writer, name string, data []byte, mode int64) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    mode,
		Size:    int64(len(data)),
		ModTime: time.Now(),
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// writeTarEntryStream writes a tar entry by streaming `size` bytes from r,
// avoiding the buffer-the-whole-blob memory cost of writeTarEntry for large layers.
func writeTarEntryStream(tw *tar.Writer, name string, size int64, r io.Reader, mode int64) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    mode,
		Size:    size,
		ModTime: time.Now(),
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := io.Copy(tw, r)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("short blob write: expected %d bytes, wrote %d", size, n)
	}
	return nil
}

// calculateDigest calculates the sha256 digest of data
func calculateDigest(data []byte) string {
	hash := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(hash[:]))
}

// getTokenFromChallengeHelper is a helper that duplicates the minimal token fetching logic
func getTokenFromChallengeHelper(wwwAuth, path string) (string, error) {
	if wwwAuth == "" {
		return "", fmt.Errorf("no WWW-Authenticate header")
	}

	// Reuse parseWWWAuthenticate logic - but it's not exported, so we'll duplicate the parsing
	realmRegex := regexp.MustCompile(`realm="([^"]+)"`)
	serviceRegex := regexp.MustCompile(`service="([^"]+)"`)
	scopeRegex := regexp.MustCompile(`scope="([^"]+)"`)

	realmMatch := realmRegex.FindStringSubmatch(wwwAuth)
	if len(realmMatch) < 2 {
		return "", fmt.Errorf("no realm found in WWW-Authenticate header")
	}
	realm := realmMatch[1]

	serviceMatch := serviceRegex.FindStringSubmatch(wwwAuth)
	service := "registry.docker.io"
	if len(serviceMatch) > 1 {
		service = serviceMatch[1]
	}

	scopeMatch := scopeRegex.FindStringSubmatch(wwwAuth)
	scope := ""
	if len(scopeMatch) > 1 {
		scope = scopeMatch[1]
	} else {
		parts := strings.Split(strings.TrimPrefix(path, "/v2/"), "/")
		if len(parts) >= 2 {
			repoName := strings.Join(parts[:len(parts)-1], "/")
			scope = fmt.Sprintf("repository:%s:pull", repoName)
		} else {
			scope = "repository:*:pull"
		}
	}

	tokenURL := realm
	if !strings.Contains(tokenURL, "?") {
		tokenURL += "?"
	} else {
		tokenURL += "&"
	}
	tokenURL += "service=" + url.QueryEscape(service)
	tokenURL += "&scope=" + url.QueryEscape(scope)

	req, err := http.NewRequest("GET", tokenURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create token request: %v", err)
	}

	// Parse token URL to check if it's an auth endpoint
	parsedTokenURL, err := url.Parse(tokenURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse token URL: %v", err)
	}

	// Bypass proxy for authentication endpoints (auth.docker.io, ghcr.io/token, etc.)
	// These endpoints should always be accessed directly to avoid authentication issues
	// when going through proxy chains
	authHostname := parsedTokenURL.Hostname()
	isAuthEndpoint := strings.Contains(authHostname, "auth.") ||
		strings.HasSuffix(authHostname, ".docker.io") ||
		strings.Contains(parsedTokenURL.Path, "/token")

	var proxyFunc func(*http.Request) (*url.URL, error)
	if isAuthEndpoint {
		proxyFunc = nil // Direct connection, no proxy
	} else {
		proxyFunc = config.GetUpstreamProxyFunc()
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: proxyFunc,
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch token: %v", err)
	}
	defer util.DeferClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token service returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read token response: %v", err)
	}

	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("failed to parse token response: %v", err)
	}

	token := tokenResp.Token
	if token == "" {
		token = tokenResp.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("no token in response")
	}

	return token, nil
}
