package util

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// StreamAndCacheOptions configures streaming behavior
type StreamAndCacheOptions struct {
	URL            string              // Upstream URL to fetch
	CachePath      string              // Final cache file path
	Client         *http.Client        // HTTP client to use (uses default if nil)
	RequestHeaders http.Header         // Additional request headers
	ResponseWriter http.ResponseWriter // Where to stream response
	LogPrefix      string              // Prefix for log messages
	ChunkTimeout   time.Duration       // Inactivity timeout (default: 2 minutes)
}

// StreamAndCache fetches from upstream, streams to client, and caches simultaneously
// Returns bytes written and error
func StreamAndCache(opts StreamAndCacheOptions) (int64, error) {
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}

	// Create request
	req, err := http.NewRequest("GET", opts.URL, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	// Copy request headers
	if opts.RequestHeaders != nil {
		for k, v := range opts.RequestHeaders {
			req.Header[k] = v
		}
	}

	// Fetch from upstream
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("upstream fetch failed: %w", err)
	}
	defer DeferClose(resp.Body)

	// Forward status and headers to client
	for k, v := range resp.Header {
		opts.ResponseWriter.Header()[k] = v
	}
	opts.ResponseWriter.WriteHeader(resp.StatusCode)

	// If non-200, just stream error response (don't cache)
	if resp.StatusCode != http.StatusOK {
		n, _ := io.Copy(opts.ResponseWriter, resp.Body)
		return n, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	// Wrap response body with activity timeout
	activityBody := NewActivityReader(resp.Body, opts.ChunkTimeout)
	defer DeferClose(activityBody)

	// Create temp file for caching
	var tempFile *os.File
	var tempFileName string
	var multiWriter io.Writer = opts.ResponseWriter

	if opts.CachePath != "" {
		dir := filepath.Dir(opts.CachePath)
		if err := os.MkdirAll(dir, 0755); err == nil {
			tempFile, err = os.CreateTemp(dir, ".tmp-*")
			if err == nil {
				tempFileName = tempFile.Name()
				defer DeferClose(tempFile)
				defer func() {
					// Clean up temp file only if it still exists (wasn't renamed)
					if tempFileName != "" {
						Logerr(os.Remove(tempFileName))
					}
				}()

				// Write to both client and temp file
				multiWriter = io.MultiWriter(opts.ResponseWriter, tempFile)
			} else {
				if opts.LogPrefix != "" {
					log.Printf("[%s] Failed to create temp file, streaming without cache: %v", opts.LogPrefix, err)
				}
			}
		}
	}

	// Stream response to client (and temp file if available)
	n, err := io.Copy(multiWriter, activityBody)
	if err != nil {
		return n, fmt.Errorf("streaming failed: %w", err)
	}

	// If we created a temp file, atomically move it to cache
	if tempFile != nil {
		Logerr(tempFile.Close())
		if err := os.Rename(tempFileName, opts.CachePath); err != nil {
			if opts.LogPrefix != "" {
				log.Printf("[%s] Failed to move temp file to cache: %v", opts.LogPrefix, err)
			}
			return n, err
		}
		tempFileName = "" // Mark as renamed so defer won't try to remove it
		if opts.LogPrefix != "" {
			log.Printf("[%s] Cached fresh fetch: %s (%d bytes)", opts.LogPrefix, opts.CachePath, n)
		}
	}

	return n, nil
}
