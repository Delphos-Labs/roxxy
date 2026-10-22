package registry

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/sbom"
)

// getClientIP extracts the client IP address from the request
// Normalizes localhost IPs (127.0.0.1 and ::1) to "127.0.0.1" for consistency
func getClientIP(r *http.Request) string {
	var ip string

	// Check X-Forwarded-For header first (for proxies)
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded != "" {
		// X-Forwarded-For can contain multiple IPs, take the first one
		ips := strings.Split(forwarded, ",")
		if len(ips) > 0 {
			ip = strings.TrimSpace(ips[0])
			// Remove port if present
			if host, _, err := net.SplitHostPort(ip); err == nil {
				ip = host
			}
		}
	}

	// Fall back to RemoteAddr if no X-Forwarded-For
	if ip == "" {
		ip = r.RemoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}
	}

	// Normalize localhost IPs: both IPv4 (127.0.0.1) and IPv6 (::1) map to 127.0.0.1
	if ip == "::1" || ip == "[::1]" {
		return "127.0.0.1"
	}

	return ip
}

// trackDockerManifest tracks a Docker manifest for SBOM if there's an active session
func (d *DockerRegistryProxy) trackDockerManifest(r *http.Request, path string, manifestData []byte, digest string) {
	if d.sbomManager == nil {
		return // No SBOM manager, skip
	}

	clientIP := getClientIP(r)
	session := d.sbomManager.GetSession(clientIP)
	if session == nil {
		log.Printf("[sbom] No active session found for IP %s (Docker manifest: %s)", clientIP, path)
		return // No active session for this IP
	}
	log.Printf("[sbom] Found active session for IP %s, tracking Docker manifest: %s", clientIP, path)

	// Parse path: /v2/<name>/manifests/<reference>
	parts := strings.Split(path, "/")
	if len(parts) < 5 {
		return // Invalid path
	}

	repoName := strings.Join(parts[2:len(parts)-2], "/")
	reference := parts[len(parts)-1]

	// Determine if reference is a tag or digest
	isTag := !strings.HasPrefix(reference, "sha256:")
	tag := ""
	if isTag {
		tag = reference
	}

	// Parse manifest to extract additional metadata
	var manifestJSON map[string]interface{}
	mediaType := ""
	layers := []string{}
	if err := json.Unmarshal(manifestData, &manifestJSON); err == nil {
		if mt, ok := manifestJSON["mediaType"].(string); ok {
			mediaType = mt
		}
		// Extract layer digests
		if layersArray, ok := manifestJSON["layers"].([]interface{}); ok {
			for _, layer := range layersArray {
				if layerMap, ok := layer.(map[string]interface{}); ok {
					if layerDigest, ok := layerMap["digest"].(string); ok {
						layers = append(layers, layerDigest)
					}
				}
			}
		}
		// For OCI Image Index, extract manifests
		if manifestsArray, ok := manifestJSON["manifests"].([]interface{}); ok {
			for _, manifest := range manifestsArray {
				if manifestMap, ok := manifest.(map[string]interface{}); ok {
					if manifestDigest, ok := manifestMap["digest"].(string); ok {
						layers = append(layers, manifestDigest)
					}
				}
			}
		}
	}

	// Create artifact
	artifact := sbom.Artifact{
		Type:        "docker",
		Name:        repoName,
		Version:     tag,
		Digest:      digest,
		Description: mediaType,
		URL:         path,
	}

	// Add registry information as description if available
	if d.upstreamURL != "" {
		registryInfo := d.upstreamURL
		if tag != "" {
			registryInfo += ":" + tag
		}
		if artifact.Description != "" {
			artifact.Description = registryInfo + " (" + artifact.Description + ")"
		} else {
			artifact.Description = registryInfo
		}
	}

	d.sbomManager.AddArtifact(clientIP, artifact)
	log.Printf("[sbom] Tracked Docker manifest: %s@%s (%s)", repoName, tag, digest[:12])

	// Also track layers/blobs if they're referenced
	// Note: These will be tracked separately when they're pulled
	// But we can note them in the description
	if len(layers) > 0 {
		log.Printf("[sbom] Manifest %s references %d layers/blobs", digest[:12], len(layers))
	}
}

// trackDockerBlob tracks a Docker blob for SBOM if there's an active session
func (d *DockerRegistryProxy) trackDockerBlob(r *http.Request, path string, digest string, size int64) {
	if d.sbomManager == nil {
		return // No SBOM manager, skip
	}

	clientIP := getClientIP(r)
	session := d.sbomManager.GetSession(clientIP)
	if session == nil {
		log.Printf("[sbom] No active session found for IP %s (Docker manifest: %s)", clientIP, path)
		return // No active session for this IP
	}
	log.Printf("[sbom] Found active session for IP %s, tracking Docker manifest: %s", clientIP, path)

	// Parse path: /v2/<name>/blobs/<digest>
	parts := strings.Split(path, "/")
	if len(parts) < 5 {
		return // Invalid path
	}

	repoName := strings.Join(parts[2:len(parts)-2], "/")

	// Create artifact for blob
	// Blobs are raw image layers or configs, so we track them with minimal metadata
	// Manual post-processing may be needed to extract package information from layers
	artifact := sbom.Artifact{
		Type:        "docker",
		Name:        repoName + "@blob",
		Digest:      digest,
		Description: fmt.Sprintf("Docker blob (layer/config) - %d bytes - may require manual post-processing", size),
		URL:         path,
	}

	// Add registry information
	if d.upstreamURL != "" {
		if artifact.Description != "" {
			artifact.Description = d.upstreamURL + " - " + artifact.Description
		} else {
			artifact.Description = d.upstreamURL
		}
	}

	d.sbomManager.AddArtifact(clientIP, artifact)
	log.Printf("[sbom] Tracked Docker blob: %s (%s, %d bytes)", digest[:12], repoName, size)
}
