package cache

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// Stats tracks access statistics for cached content
type Stats struct {
	AccessCount  int       `json:"access_count"`
	LastAccessed time.Time `json:"last_accessed"`
}

// StatsTracker tracks access statistics for cached content
type StatsTracker struct {
	stats         map[string]*Stats
	statsMu       sync.RWMutex
	statsFile     string
	lastSavedHash string // Hash of last saved stats to detect changes
}

// NewStatsTracker creates a new stats tracker
func NewStatsTracker(statsFile string) *StatsTracker {
	return &StatsTracker{
		stats:     make(map[string]*Stats),
		statsFile: statsFile,
	}
}

// Load loads access statistics, trying API first if server is running, then falling back to disk
// verbose controls whether to log loading messages.
func (st *StatsTracker) Load(verbose bool) error {
	st.statsMu.Lock()
	defer st.statsMu.Unlock()

	// Initialize map
	st.stats = make(map[string]*Stats)

	// Check if stats file exists and is recent (within last 5 minutes)
	// If file is recent, prefer disk (server might have just shut down)
	// If file is old or doesn't exist, try API first (server might be running with fresh stats)
	fileInfo, fileExists := os.Stat(st.statsFile)
	fileIsRecent := false
	if fileExists == nil {
		age := time.Since(fileInfo.ModTime())
		fileIsRecent = age < 5*time.Minute
	}

	// Try API first if file doesn't exist or is old
	if !fileIsRecent {
		if stats, err := st.loadFromAPI(); err == nil && stats != nil {
			st.stats = stats
			if verbose {
				log.Printf("Loaded access stats for %d resources from API", len(st.stats))
			}
			return nil
		}
	}

	// Fallback to disk
	data, err := os.ReadFile(st.statsFile)
	if err != nil {
		if os.IsNotExist(err) {
			if verbose {
				log.Printf("No existing access stats file, starting fresh")
			}
			return nil
		}
		return fmt.Errorf("failed to read access stats: %w", err)
	}

	if err := json.Unmarshal(data, &st.stats); err != nil {
		return fmt.Errorf("failed to parse access stats: %w", err)
	}

	if verbose {
		log.Printf("Loaded access stats for %d resources from disk", len(st.stats))
	}
	return nil
}

// loadFromAPI attempts to fetch stats from the running server API
// Returns stats on success, error if server is not available or request fails
func (st *StatsTracker) loadFromAPI() (map[string]*Stats, error) {
	// Try common server addresses (in order of likelihood)
	// Default is :3128, but could be configured differently
	addresses := []string{
		"http://localhost:3128", // Default proxy port
		"http://127.0.0.1:3128", // Default proxy port (IP)
		"http://localhost:8080", // Alternative port
		"http://127.0.0.1:8080", // Alternative port (IP)
	}

	// Check LISTEN_ADDR environment variable
	if listenAddr := os.Getenv("LISTEN_ADDR"); listenAddr != "" {
		// Parse listen address (format: ":3128" or "localhost:3128")
		host := "localhost"
		port := listenAddr
		if strings.Contains(listenAddr, ":") {
			parts := strings.Split(listenAddr, ":")
			if len(parts) == 2 {
				if parts[0] != "" {
					host = parts[0]
				}
				port = ":" + parts[1]
			}
		}
		// Prepend to addresses list
		addresses = append([]string{fmt.Sprintf("http://%s%s", host, port)}, addresses...)
	}

	// Create HTTP client with very short timeout (100ms)
	client := &http.Client{
		Timeout: 100 * time.Millisecond,
	}

	for _, addr := range addresses {
		url := addr + "/api/v1/stats/access"

		// Create request with Accept-Encoding: gzip header
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept-Encoding", "gzip")

		resp, err := client.Do(req)
		if err != nil {
			continue // Try next address
		}
		defer util.DeferClose(resp.Body)

		if resp.StatusCode != http.StatusOK {
			continue // Try next address
		}

		// Handle gzip decompression if needed
		var reader io.Reader = resp.Body
		if resp.Header.Get("Content-Encoding") == "gzip" {
			gzipReader, err := gzip.NewReader(resp.Body)
			if err != nil {
				continue // Try next address
			}
			defer util.DeferClose(gzipReader)
			reader = gzipReader
		}

		// Parse response
		var apiResponse struct {
			Stats map[string]*Stats `json:"stats"`
		}
		if err := json.NewDecoder(reader).Decode(&apiResponse); err != nil {
			continue // Try next address
		}

		return apiResponse.Stats, nil
	}

	return nil, fmt.Errorf("server not available")
}

// Save persists access statistics to disk
// Only saves if stats have changed since last save
func (st *StatsTracker) Save() error {
	// Acquire write lock immediately to ensure atomicity
	// We need to marshal, compare hash, write, and update hash all atomically
	st.statsMu.Lock()
	defer st.statsMu.Unlock()

	// Marshal to get current state (while holding write lock)
	// Use Marshal instead of MarshalIndent for better performance
	data, err := json.Marshal(st.stats)
	if err != nil {
		return fmt.Errorf("failed to marshal access stats: %w", err)
	}

	// Compute hash of current stats (while holding write lock)
	hashBytes := sha256.Sum256(data)
	hash := hex.EncodeToString(hashBytes[:])
	statsCount := len(st.stats)

	// Compare hash while holding write lock (prevents race with IncrementAccessCount)
	if hash == st.lastSavedHash {
		return nil // No changes, skip save
	}

	// Write file (while holding write lock to ensure consistency)
	if err := os.WriteFile(st.statsFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write access stats: %w", err)
	}

	// Update last saved hash (while holding write lock)
	st.lastSavedHash = hash

	log.Printf("Saved access stats for %d resources", statsCount)
	return nil
}

// IncrementAccessCount increments the access count for a content hash
func (st *StatsTracker) IncrementAccessCount(contentHash string) {
	st.statsMu.Lock()
	defer st.statsMu.Unlock()

	stats, exists := st.stats[contentHash]
	if !exists {
		stats = &Stats{
			AccessCount:  0,
			LastAccessed: time.Time{},
		}
		st.stats[contentHash] = stats
	}

	stats.AccessCount++
	stats.LastAccessed = time.Now()
}

// GetAccessCount returns the access count for a content hash
func (st *StatsTracker) GetAccessCount(contentHash string) (int, time.Time) {
	st.statsMu.RLock()
	defer st.statsMu.RUnlock()

	if stats, exists := st.stats[contentHash]; exists {
		return stats.AccessCount, stats.LastAccessed
	}
	return 0, time.Time{}
}

// GetAllStats returns a copy of all stats (for read-only access)
func (st *StatsTracker) GetAllStats() map[string]*Stats {
	st.statsMu.RLock()
	defer st.statsMu.RUnlock()

	// Return a copy to avoid race conditions
	result := make(map[string]*Stats)
	for k, v := range st.stats {
		result[k] = &Stats{
			AccessCount:  v.AccessCount,
			LastAccessed: v.LastAccessed,
		}
	}
	return result
}
