package commands

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// DockerImageMetadata contains extracted metadata from a Docker image
type DockerImageMetadata struct {
	License       string `json:"license,omitempty"`
	Description   string `json:"description,omitempty"`
	Title         string `json:"title,omitempty"`
	Documentation string `json:"documentation,omitempty"`
}

// extractDockerMetadata extracts license and description from a Docker/OCI image
// It checks both the config blob labels and manifest annotations
func extractDockerMetadata(cacheDir string, manifestPath string) (*DockerImageMetadata, error) {
	// Read manifest
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}

	// Parse manifest to get config digest and annotations
	var manifest struct {
		MediaType string `json:"mediaType"`
		Config    struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}

	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("failed to parse manifest: %w", err)
	}

	metadata := &DockerImageMetadata{}

	// First, check manifest annotations (OCI standard)
	if manifest.Annotations != nil {
		// OCI standard annotation keys
		if license, ok := manifest.Annotations["org.opencontainers.image.licenses"]; ok && license != "" {
			metadata.License = license
		}
		if desc, ok := manifest.Annotations["org.opencontainers.image.description"]; ok && desc != "" {
			metadata.Description = desc
		}
		if title, ok := manifest.Annotations["org.opencontainers.image.title"]; ok && title != "" {
			metadata.Title = title
		}
		if doc, ok := manifest.Annotations["org.opencontainers.image.documentation"]; ok && doc != "" {
			metadata.Documentation = doc
		}
	}

	// Also check legacy annotation keys in annotations
	if manifest.Annotations != nil {
		if license, ok := manifest.Annotations["license"]; ok && license != "" && metadata.License == "" {
			metadata.License = license
		}
		if desc, ok := manifest.Annotations["description"]; ok && desc != "" && metadata.Description == "" {
			metadata.Description = desc
		}
	}

	// Then, check config blob labels (Docker format)
	if manifest.Config.Digest != "" {
		configBlobPath := util.DockerBlobPathWithFallback(cacheDir, manifest.Config.Digest)
		configData, err := os.ReadFile(configBlobPath)
		if err != nil {
			// Config blob might not be cached yet, that's okay
			log.Printf("[docker-metadata] Config blob not found for %s: %v", manifest.Config.Digest[:12], err)
		} else {
			var config struct {
				Config struct {
					Labels map[string]string `json:"Labels,omitempty"`
				} `json:"config,omitempty"`
			}

			if err := json.Unmarshal(configData, &config); err == nil {
				if config.Config.Labels != nil {
					// Check standard OCI label keys
					if license, ok := config.Config.Labels["org.opencontainers.image.licenses"]; ok && license != "" && metadata.License == "" {
						metadata.License = license
					}
					if desc, ok := config.Config.Labels["org.opencontainers.image.description"]; ok && desc != "" && metadata.Description == "" {
						metadata.Description = desc
					}
					if title, ok := config.Config.Labels["org.opencontainers.image.title"]; ok && title != "" && metadata.Title == "" {
						metadata.Title = title
					}
					if doc, ok := config.Config.Labels["org.opencontainers.image.documentation"]; ok && doc != "" && metadata.Documentation == "" {
						metadata.Documentation = doc
					}

					// Check legacy label keys
					if license, ok := config.Config.Labels["license"]; ok && license != "" && metadata.License == "" {
						metadata.License = license
					}
					if desc, ok := config.Config.Labels["description"]; ok && desc != "" && metadata.Description == "" {
						metadata.Description = desc
					}
				}
			}
		}
	}

	return metadata, nil
}

// processDockerManifest processes a single Docker manifest file and extracts metadata
func processDockerManifest(cacheDir string, manifestPath string, force bool) error {
	// Check if metadata already exists (unless force is true)
	metaPath := manifestPath + ".metadata"
	if !force {
		if _, err := os.Stat(metaPath); err == nil {
			// Metadata already exists, skip
			return nil
		}
	}

	// Extract metadata
	metadata, err := extractDockerMetadata(cacheDir, manifestPath)
	if err != nil {
		return fmt.Errorf("failed to extract metadata: %w", err)
	}

	// Only save if we found any metadata
	if metadata.License == "" && metadata.Description == "" && metadata.Title == "" && metadata.Documentation == "" {
		return nil // No metadata found, skip saving
	}

	// Save metadata to .metadata file as a map for easier reading by API
	metaMap := make(map[string]string)
	if metadata.License != "" {
		metaMap["license"] = metadata.License
	}
	if metadata.Description != "" {
		metaMap["description"] = metadata.Description
	}
	if metadata.Title != "" {
		metaMap["title"] = metadata.Title
	}
	if metadata.Documentation != "" {
		metaMap["documentation"] = metadata.Documentation
	}

	metaJSON, err := json.MarshalIndent(metaMap, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	if err := os.WriteFile(metaPath, metaJSON, 0644); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}

	// Log what we found
	fields := []string{}
	if metadata.License != "" {
		fields = append(fields, fmt.Sprintf("license=%q", metadata.License))
	}
	if metadata.Description != "" {
		preview := metadata.Description
		if len(preview) > 50 {
			preview = preview[:50] + "..."
		}
		fields = append(fields, fmt.Sprintf("description=%q", preview))
	}
	if metadata.Title != "" {
		fields = append(fields, fmt.Sprintf("title=%q", metadata.Title))
	}
	if len(fields) > 0 {
		relPath, _ := filepath.Rel(filepath.Join(cacheDir, "docker-manifests"), manifestPath)
		log.Printf("[docker-metadata] Extracted metadata for %s: %s", relPath, strings.Join(fields, ", "))
	}

	return nil
}

// enrichDockerImages processes all Docker manifests in the cache and extracts metadata
func enrichDockerImages(cacheDir string, force bool) (int, error) {
	manifestDir := filepath.Join(cacheDir, "docker-manifests")
	if _, err := os.Stat(manifestDir); os.IsNotExist(err) {
		return 0, nil // No Docker manifests directory
	}

	processedCount := 0
	errorCount := 0

	// Walk all manifest files (skip .meta and .metadata files)
	err := filepath.Walk(manifestDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Continue on error
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Skip metadata files
		if strings.HasSuffix(path, ".meta") || strings.HasSuffix(path, ".metadata") {
			return nil
		}

		// Process manifest file
		if err := processDockerManifest(cacheDir, path, force); err != nil {
			log.Printf("[docker-metadata] Error processing %s: %v", path, err)
			errorCount++
			return nil // Continue processing other files
		}

		processedCount++
		return nil
	})

	if err != nil {
		return processedCount, fmt.Errorf("error walking manifest directory: %w", err)
	}

	log.Printf("[docker-metadata] Processed %d manifests, %d errors", processedCount, errorCount)
	return processedCount, nil
}
