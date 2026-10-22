package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/cli"
	"github.com/Delphos-Labs/roxxy/internal/packages"
)

// handleURLCache handles the /api/v1/cache/url endpoint
func (a *API) handleURLCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hostname := r.URL.Query().Get("hostname")
	search := r.URL.Query().Get("search")

	cfg := cli.Config{
		CacheDir:     a.cacheDir,
		StatsTracker: a.statsTracker,
	}

	entries, err := listCacheEntries(cfg, hostname)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error listing URL cache: %v", err), http.StatusInternalServerError)
		return
	}

	// Filter by search term if provided
	if search != "" {
		filtered := []cli.CacheEntry{}
		for _, entry := range entries {
			if strings.Contains(strings.ToLower(entry.Hostname), strings.ToLower(search)) ||
				strings.Contains(strings.ToLower(entry.Path), strings.ToLower(search)) {
				filtered = append(filtered, entry)
			}
		}
		entries = filtered
	}

	writeJSON(w, map[string]interface{}{
		"entries": entries,
		"total":   len(entries),
	})
}

// handleURLCacheDelete handles DELETE /api/v1/cache/url for single or bulk deletion
func (a *API) handleURLCacheDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Entries []struct {
			Hostname string `json:"hostname"`
			Path     string `json:"path"`
		} `json:"entries"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	if len(req.Entries) == 0 {
		http.Error(w, "No entries specified", http.StatusBadRequest)
		return
	}

	deleted := []string{}
	failed := []string{}

	for _, entry := range req.Entries {
		// Build the cache file path using the same structure as cache.GetURLPath
		// Path format: cacheDir/url/hostname/hash[0:2]/hash[2:4]/hash
		pathHash := cache.PathHash(entry.Path)
		cachePath := cache.GetURLPath(a.cacheDir, entry.Hostname, pathHash)

		// Delete the cache file and its metadata
		entryKey := fmt.Sprintf("%s%s", entry.Hostname, entry.Path)
		if err := os.Remove(cachePath); err != nil {
			if !os.IsNotExist(err) {
				log.Printf("Failed to delete URL cache file %s: %v", cachePath, err)
				failed = append(failed, entryKey)
				continue
			}
		}

		// Also delete the .meta file
		metaPath := cachePath + ".meta"
		if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
			log.Printf("Warning: Failed to delete metadata file %s: %v", metaPath, err)
		}

		deleted = append(deleted, entryKey)
	}

	writeJSON(w, map[string]interface{}{
		"deleted": deleted,
		"failed":  failed,
	})
}

// handleFlushCache handles the /api/v1/cache/flush endpoint
func (a *API) handleFlushCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Type     string `json:"type"`
		Hostname string `json:"hostname"`
	}

	// Decode request body if present (allow empty body)
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
			return
		}
	}

	// Determine which package types to flush
	pmTypes := []packages.Type{packages.TypeAPK, packages.TypeAPT, packages.TypeNPM, packages.TypePyPI, packages.TypeGo}
	if req.Type != "" {
		pmTypes = []packages.Type{packages.Type(req.Type)}
	}

	// Step 1: Delete cached files for the specified type(s)
	packagesDir := filepath.Join(a.cacheDir, "packages")
	totalDeletedFiles := 0
	for _, pmType := range pmTypes {
		typeDir := filepath.Join(packagesDir, string(pmType))
		if _, err := os.Stat(typeDir); os.IsNotExist(err) {
			continue // No cache directory for this type
		}

		// Walk the directory and delete all files (but not .meta files separately, as they'll be deleted with their files)
		typeDeletedFiles := 0
		err := filepath.Walk(typeDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}

			// Skip .meta files - they'll be handled when we delete the main file
			if strings.HasSuffix(path, ".meta") {
				return nil
			}

			// Delete the file and its metadata
			if err := deletePackage(path); err != nil {
				log.Printf("Warning: Failed to delete cached file %s: %v", path, err)
			} else {
				typeDeletedFiles++
			}
			return nil
		})

		if err != nil {
			log.Printf("Warning: Error walking cache directory for %s: %v", pmType, err)
		} else {
			log.Printf("Deleted %d cached files for %s", typeDeletedFiles, pmType)
			totalDeletedFiles += typeDeletedFiles
		}
	}

	// Step 2: Clear the index database for the specified type(s)
	// This ensures the index doesn't have stale entries after cache flush
	if a.indexDB != nil {
		indexDir := filepath.Join(a.cacheDir, "index")
		for _, pmType := range pmTypes {
			typeDir := filepath.Join(indexDir, string(pmType))
			packagesPath := filepath.Join(typeDir, "packages.json")
			if err := os.Remove(packagesPath); err != nil && !os.IsNotExist(err) {
				log.Printf("Warning: Failed to clear index database for %s: %v", pmType, err)
			} else {
				log.Printf("Cleared index database for %s", pmType)
			}
		}
	}

	writeJSON(w, map[string]interface{}{
		"message":       "Cache flush completed",
		"type":          req.Type,
		"hostname":      req.Hostname,
		"deleted_files": totalDeletedFiles,
	})
}

// handleParseIndexes handles the /api/v1/index/parse endpoint
func (a *API) handleParseIndexes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// If indexDB is nil, we're likely using v2 index system
	// In v2, parsing is done via CLI command: roxxy package parse
	// Return success with a message indicating this
	if a.indexDB == nil {
		writeJSON(w, map[string]interface{}{
			"message":      "Index parsing is now done via CLI command: roxxy package parse",
			"total_parsed": 0,
			"results":      map[string]interface{}{},
		})
		return
	}

	var req struct {
		Type string `json:"type"` // Optional: parse specific type, or all if empty
	}

	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
			return
		}
	}

	pmTypes := []packages.Type{packages.TypeAPK, packages.TypeAPT, packages.TypeNPM, packages.TypePyPI, packages.TypeGo}
	if req.Type != "" {
		// Parse only the requested type
		pmTypes = []packages.Type{packages.Type(req.Type)}
	}

	results := make(map[string]interface{})
	totalParsed := 0

	for _, pmType := range pmTypes {
		count, err := a.indexDB.ParseCachedIndexFiles(a.cacheDir, pmType)
		if err != nil {
			results[string(pmType)] = map[string]interface{}{
				"error": err.Error(),
				"count": count,
			}
		} else {
			results[string(pmType)] = map[string]interface{}{
				"count": count,
			}
			totalParsed += count
		}
	}

	writeJSON(w, map[string]interface{}{
		"message":      "Index parsing completed",
		"total_parsed": totalParsed,
		"results":      results,
	})
}
