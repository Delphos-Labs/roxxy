package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FindManifestByDigest searches for a manifest by its digest and returns which repository/tag it belongs to
func FindManifestByDigest(cacheDir, digest string) error {
	if cacheDir == "" {
		return fmt.Errorf("cache directory not specified")
	}

	// Normalize digest (add sha256: prefix if missing)
	searchDigest := digest
	if !strings.HasPrefix(searchDigest, "sha256:") {
		searchDigest = "sha256:" + searchDigest
	}

	// Extract hash part for comparison
	hashPart := strings.TrimPrefix(searchDigest, "sha256:")
	if len(hashPart) != 64 {
		return fmt.Errorf("invalid digest format: %s (expected 64 hex characters)", digest)
	}

	manifestDir := filepath.Join(cacheDir, "docker-manifests")
	if _, err := os.Stat(manifestDir); os.IsNotExist(err) {
		return fmt.Errorf("manifest directory not found: %s", manifestDir)
	}

	fmt.Printf("Searching for manifest digest: %s\n", searchDigest)
	fmt.Printf("Searching in: %s\n\n", manifestDir)

	var foundManifests []manifestInfo

	// Walk through all manifest files
	err := filepath.Walk(manifestDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Continue on errors
		}

		// Skip directories and .meta files
		if info.IsDir() || strings.HasSuffix(path, ".meta") {
			return nil
		}

		// Read manifest file
		manifestData, err := os.ReadFile(path)
		if err != nil {
			return nil // Continue on read errors
		}

		// Calculate digest
		hash := sha256.Sum256(manifestData)
		calculatedDigest := "sha256:" + hex.EncodeToString(hash[:])

		// Check if this matches our search digest
		if calculatedDigest == searchDigest {
			// Extract registry, repo, and reference from path
			// Path format: docker-manifests/<registry>/<safeRepo>/<reference>
			relPath, err := filepath.Rel(manifestDir, path)
			if err != nil {
				return nil
			}

			parts := strings.Split(relPath, string(filepath.Separator))
			if len(parts) != 3 {
				return nil
			}

			registryID := parts[0]
			safeRepo := parts[1]
			reference := parts[2]

			// Convert safe repo back to normal repo name
			repoName := UnsafeRepoName(safeRepo)

			// Determine if reference is a tag or digest
			isTag := !strings.HasPrefix(reference, "sha256:")

			// Read metadata if available
			meta := ReadDockerManifestMetadata(path)
			upstreamURL := ""
			if meta != nil {
				upstreamURL = meta.UpstreamURL
			}

			foundManifests = append(foundManifests, manifestInfo{
				Registry:    registryID,
				Repo:        repoName,
				Reference:   reference,
				IsTag:       isTag,
				UpstreamURL: upstreamURL,
				Path:        path,
			})
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("error walking manifest directory: %w", err)
	}

	if len(foundManifests) == 0 {
		fmt.Printf("✗ Manifest not found in cache\n")
		return nil
	}

	// Group by repository
	repoGroups := make(map[string][]manifestInfo)
	for _, m := range foundManifests {
		key := fmt.Sprintf("%s/%s", m.Registry, m.Repo)
		repoGroups[key] = append(repoGroups[key], m)
	}

	fmt.Printf("✓ Found manifest in %d repository(ies):\n\n", len(repoGroups))

	for repoKey, manifests := range repoGroups {
		fmt.Printf("Repository: %s\n", repoKey)

		// Show tags first, then digests
		var tags []string
		var digests []string
		var upstreamURL string

		for _, m := range manifests {
			if m.UpstreamURL != "" {
				upstreamURL = m.UpstreamURL
			}
			if m.IsTag {
				tags = append(tags, m.Reference)
			} else {
				digests = append(digests, m.Reference)
			}
		}

		if upstreamURL != "" {
			fmt.Printf("  Registry: %s\n", upstreamURL)
		}

		if len(tags) > 0 {
			fmt.Printf("  Tags: %s\n", strings.Join(tags, ", "))
		}

		if len(digests) > 0 {
			fmt.Printf("  Also stored as digest: %s\n", strings.Join(digests, ", "))
		}

		fmt.Printf("\n  To re-pull and export:\n")
		if len(tags) > 0 {
			// Use first tag
			repoName := manifests[0].Repo
			if manifests[0].Registry == "docker.io" {
				// For docker.io, construct full image spec
				fmt.Printf("    roxxy docker pull %s:%s\n", repoName, tags[0])
				fmt.Printf("    roar export oci %s:%s --from-tarball <tarball>\n", repoName, tags[0])
			} else {
				// For other registries, include registry in image spec
				registryHost := manifests[0].Registry
				if upstreamURL != "" {
					// Extract hostname from URL
					registryHost = strings.TrimPrefix(upstreamURL, "https://")
					registryHost = strings.TrimPrefix(registryHost, "http://")
				}
				fmt.Printf("    roxxy docker pull %s/%s:%s\n", registryHost, repoName, tags[0])
				fmt.Printf("    roar export oci %s/%s:%s --from-tarball <tarball>\n", registryHost, repoName, tags[0])
			}
		} else {
			// No tags, use digest
			repoName := manifests[0].Repo
			if manifests[0].Registry == "docker.io" {
				fmt.Printf("    roxxy docker pull %s@%s\n", repoName, searchDigest)
				fmt.Printf("    roar export oci %s@%s --from-tarball <tarball>\n", repoName, searchDigest)
			} else {
				registryHost := manifests[0].Registry
				if upstreamURL != "" {
					registryHost = strings.TrimPrefix(upstreamURL, "https://")
					registryHost = strings.TrimPrefix(registryHost, "http://")
				}
				fmt.Printf("    roxxy docker pull %s/%s@%s\n", registryHost, repoName, searchDigest)
				fmt.Printf("    roar export oci %s/%s@%s --from-tarball <tarball>\n", registryHost, repoName, searchDigest)
			}
		}
		fmt.Println()
	}

	return nil
}

type manifestInfo struct {
	Registry    string
	Repo        string
	Reference   string
	IsTag       bool
	UpstreamURL string
	Path        string
}
