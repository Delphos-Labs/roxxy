package registry

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/tls"
)

// Proxy handles registry proxying logic
type Proxy struct {
	certManager     *tls.CertificateManager
	statsTracker    *cache.StatsTracker
	cacheDir        string
	knownRegistries config.RegistryMap
}

// NewProxy creates a new registry proxy
func NewProxy(certManager *tls.CertificateManager, statsTracker *cache.StatsTracker, cacheDir string, knownRegistries config.RegistryMap) *Proxy {
	// Ensure tmp directory exists for streaming temp files
	tmpDir := filepath.Join(cacheDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		log.Printf("Warning: Failed to create tmp directory %s: %v", tmpDir, err)
	}

	// Clean up any orphaned temp files from previous runs
	// These are files that were left behind if roxxy crashed during streaming
	if entries, err := os.ReadDir(tmpDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tmp") {
				tmpFile := filepath.Join(tmpDir, entry.Name())
				if err := os.Remove(tmpFile); err == nil {
					log.Printf("Cleaned up orphaned temp file: %s", entry.Name())
				}
			}
		}
	}

	return &Proxy{
		certManager:     certManager,
		statsTracker:    statsTracker,
		cacheDir:        cacheDir,
		knownRegistries: knownRegistries,
	}
}
