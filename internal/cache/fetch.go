package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// FetchOptions configures how to fetch and cache content
type FetchOptions struct {
	URL            string
	ExpectedHash   string // Optional, for validation (e.g., "sha256:abc123...")
	ExpectedSize   int64  // Optional, -1 if unknown
	ContentType    string
	MinCacheSize   int64
	CacheDir       string
	StatsTracker   *StatsTracker
	RequestedRange *ByteRange // Optional, if client requested specific range

	// HTTP client to use for upstream requests
	Client *http.Client

	// Original request headers (for auth, etc.)
	RequestHeaders http.Header

	// ChunkTimeout is the inactivity timeout - if no data arrives within this duration, fail
	// If zero, defaults to 2 minutes. This allows large downloads as long as data keeps flowing.
	ChunkTimeout time.Duration
}

// FetchResult contains the outcome of a fetch operation
type FetchResult struct {
	Served       int64       // Bytes served to client
	Cached       int64       // Bytes written to cache
	FromCache    int64       // Bytes served from cache
	FromUpstream int64       // Bytes fetched from upstream
	StatusCode   int         // HTTP status code to return
	Headers      http.Header // Response headers
	Complete     bool        // True if file is now complete in cache
	PartialPath  string      // Path to partial cache dir if incomplete
	CompletePath string      // Path to complete cache file if complete
}

// StreamingFetch fetches content with partial caching and resumability
// Returns FetchResult and error
func StreamingFetch(opts FetchOptions, respWriter io.Writer) (*FetchResult, error) {
	result := &FetchResult{
		Headers: make(http.Header),
	}

	// Hash the URL for cache lookups
	urlHash := PathHash(opts.URL)

	// Step 1: Check complete cache (content-addressed)
	if opts.ExpectedHash != "" {
		// We know the expected hash, check if we have it
		contentPath := GetContentPath(opts.CacheDir, opts.ExpectedHash)
		if data, err := os.ReadFile(contentPath); err == nil {
			// Complete file found!
			log.Printf("[fetch] Complete cache hit: %s (%d bytes)", opts.ExpectedHash[:12], len(data))

			// Serve the requested range (or full content)
			if opts.RequestedRange != nil {
				return serveRangeFromData(data, opts.RequestedRange, opts.ContentType, respWriter, result)
			}

			// Serve full content
			result.Headers.Set("Content-Type", opts.ContentType)
			result.Headers.Set("Content-Length", fmt.Sprintf("%d", len(data)))
			result.Headers.Set("Accept-Ranges", "bytes")
			result.StatusCode = http.StatusOK

			n, err := respWriter.Write(data)
			result.Served = int64(n)
			result.FromCache = int64(n)
			result.Complete = true
			result.CompletePath = contentPath

			if opts.StatsTracker != nil {
				opts.StatsTracker.IncrementAccessCount(opts.ExpectedHash)
			}

			return result, err
		}
	}

	// Step 2: Check partial cache (URL-addressed)
	metadata, err := LoadPartialMetadata(opts.CacheDir, urlHash)
	if err != nil {
		// No partial cache, start fresh
		metadata = &PartialCacheMetadata{
			URL:          opts.URL,
			TotalSize:    opts.ExpectedSize,
			ContentType:  opts.ContentType,
			ExpectedHash: opts.ExpectedHash,
			Ranges:       []ByteRange{},
		}
	} else {
		log.Printf("[fetch] Partial cache found: %s (%.1f%% complete)", urlHash[:12], metadata.GetCompletionPercentage())
	}

	// Step 3: Determine what range to fetch
	var fetchStart, fetchEnd int64
	var serveFromCache bool

	if opts.RequestedRange != nil {
		// Client requested specific range
		fetchStart = opts.RequestedRange.Start
		fetchEnd = opts.RequestedRange.End

		// Check if we have any of this range cached
		if metadata.HasRange(fetchStart, fetchEnd) {
			// We have the entire requested range!
			log.Printf("[fetch] Serving range %d-%d entirely from partial cache", fetchStart, fetchEnd)
			return serveRangeFromPartialCache(metadata, opts.RequestedRange, opts.CacheDir, urlHash, opts.ContentType, respWriter, result)
		}

		// Check if we have the start of the range
		for _, r := range metadata.Ranges {
			if r.Start <= fetchStart && r.End >= fetchStart {
				// We have the beginning, can serve partial from cache
				serveFromCache = true
				break
			}
		}
	} else {
		// No specific range requested, fetch entire file
		fetchStart = 0
		fetchEnd = opts.ExpectedSize - 1
		if opts.ExpectedSize < 0 {
			fetchEnd = -1 // Unknown size
		}

		// Check if we have the entire file cached
		if opts.ExpectedSize > 0 && metadata.HasRange(0, opts.ExpectedSize-1) {
			log.Printf("[fetch] Serving entire file from partial cache (0-%d)", opts.ExpectedSize-1)

			// Set up result headers for full content (200 OK, not 206)
			result.Headers.Set("Content-Type", opts.ContentType)
			result.Headers.Set("Content-Length", fmt.Sprintf("%d", opts.ExpectedSize))
			result.Headers.Set("Accept-Ranges", "bytes")
			result.StatusCode = http.StatusOK

			// Serve from cache
			bytesServed, err := readRangeFromChunks(metadata, 0, opts.ExpectedSize-1, opts.CacheDir, urlHash, respWriter)
			result.Served = bytesServed
			result.FromCache = bytesServed
			result.Complete = true
			result.PartialPath = GetPartialCachePath(opts.CacheDir, urlHash)

			return result, err
		}

		// Check if we have the start of the file cached
		for _, r := range metadata.Ranges {
			if r.Start == 0 {
				// We have the beginning, can serve partial from cache
				serveFromCache = true
				break
			}
		}
	}

	// Step 4: Fetch from upstream
	return fetchAndStreamWithPartialCache(opts, metadata, urlHash, fetchStart, fetchEnd, serveFromCache, respWriter, result)
}

// serveRangeFromData serves a range from in-memory data
func serveRangeFromData(data []byte, requestedRange *ByteRange, contentType string, w io.Writer, result *FetchResult) (*FetchResult, error) {
	start := requestedRange.Start
	end := requestedRange.End

	// Validate range
	if start < 0 || start >= int64(len(data)) {
		result.StatusCode = http.StatusRequestedRangeNotSatisfiable
		result.Headers.Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
		return result, fmt.Errorf("invalid range: %d-%d (file size: %d)", start, end, len(data))
	}

	if end < 0 || end >= int64(len(data)) {
		end = int64(len(data)) - 1
	}

	// Serve range
	rangeData := data[start : end+1]
	result.Headers.Set("Content-Type", contentType)
	result.Headers.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	result.Headers.Set("Content-Length", fmt.Sprintf("%d", len(rangeData)))
	result.Headers.Set("Accept-Ranges", "bytes")
	result.StatusCode = http.StatusPartialContent

	n, err := w.Write(rangeData)
	result.Served = int64(n)
	result.FromCache = int64(n)
	result.Complete = true

	return result, err
}

// serveRangeFromPartialCache serves a range that's entirely in partial cache
func serveRangeFromPartialCache(metadata *PartialCacheMetadata, requestedRange *ByteRange, cacheDir, urlHash, contentType string, w io.Writer, result *FetchResult) (*FetchResult, error) {
	start := requestedRange.Start
	end := requestedRange.End

	result.Headers.Set("Content-Type", contentType)
	result.Headers.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, metadata.TotalSize))
	result.Headers.Set("Content-Length", fmt.Sprintf("%d", end-start+1))
	result.Headers.Set("Accept-Ranges", "bytes")
	result.StatusCode = http.StatusPartialContent

	// Read from chunk files
	bytesServed, err := readRangeFromChunks(metadata, start, end, cacheDir, urlHash, w)
	result.Served = bytesServed
	result.FromCache = bytesServed
	result.PartialPath = GetPartialCachePath(cacheDir, urlHash)

	return result, err
}

// readRangeFromChunks reads a byte range from chunk files
func readRangeFromChunks(metadata *PartialCacheMetadata, start, end int64, cacheDir, urlHash string, w io.Writer) (int64, error) {
	var totalRead int64
	currentPos := start

	// Find which chunks contain our data
	for currentPos <= end {
		// Find the chunk that contains currentPos
		var chunkStart, chunkEnd int64
		found := false

		for _, r := range metadata.Ranges {
			if r.Start <= currentPos && r.End >= currentPos {
				chunkStart = r.Start
				chunkEnd = r.End
				found = true
				break
			}
		}

		if !found {
			return totalRead, fmt.Errorf("missing range at position %d", currentPos)
		}

		// Read from chunk file
		chunkPath := GetPartialChunkPath(cacheDir, urlHash, chunkStart, chunkEnd)
		data, err := os.ReadFile(chunkPath)
		if err != nil {
			return totalRead, fmt.Errorf("error reading chunk %s: %w", chunkPath, err)
		}

		// Calculate offsets within chunk
		offsetInChunk := currentPos - chunkStart
		bytesToRead := min(chunkEnd-currentPos+1, end-currentPos+1)

		// Write data to response
		n, err := w.Write(data[offsetInChunk : offsetInChunk+bytesToRead])
		totalRead += int64(n)
		currentPos += int64(n)

		if err != nil {
			return totalRead, err
		}
	}

	return totalRead, nil
}

// fetchAndStreamWithPartialCache fetches from upstream with partial cache support
func fetchAndStreamWithPartialCache(opts FetchOptions, metadata *PartialCacheMetadata, urlHash string, fetchStart, fetchEnd int64, serveFromCache bool, w io.Writer, result *FetchResult) (*FetchResult, error) {
	// Try to serve from partial cache first
	updatedFetchStart, done, err := tryServeFromPartialCache(metadata, fetchStart, fetchEnd, serveFromCache, opts.CacheDir, urlHash, w, result)
	if err != nil || done {
		return result, err
	}
	fetchStart = updatedFetchStart

	// Build and execute upstream request
	resp, activityBody, err := executeUpstreamFetch(opts, metadata, fetchStart, fetchEnd)
	if err != nil {
		return result, err
	}
	defer util.DeferClose(resp.Body)
	defer util.DeferClose(activityBody)

	// Update metadata with total size from response
	updateMetadataSize(metadata, resp, &opts)

	// Stream and cache the response
	startTime := time.Now()
	bytesWritten, err := streamAndCacheChunk(opts, metadata, urlHash, fetchStart, fetchEnd, activityBody, w, result)
	if err != nil {
		return result, err
	}

	log.Printf("[fetch] Fetched and cached bytes %d-%d (%d bytes) in %v", fetchStart, fetchStart+bytesWritten-1, bytesWritten, time.Since(startTime))

	// Finalize result (check completion, set status, copy headers)
	finalizeResult(metadata, opts, urlHash, resp, result)

	return result, nil
}

// tryServeFromPartialCache attempts to serve data from partial cache
// Returns: updated fetchStart, whether request is complete, and error
func tryServeFromPartialCache(metadata *PartialCacheMetadata, fetchStart, fetchEnd int64, serveFromCache bool, cacheDir, urlHash string, w io.Writer, result *FetchResult) (int64, bool, error) {
	if !serveFromCache || len(metadata.Ranges) == 0 {
		return fetchStart, false, nil
	}

	// Find how much we can serve from cache
	cachedEnd := int64(-1)
	for _, r := range metadata.Ranges {
		if r.Start <= fetchStart && r.End >= fetchStart {
			cachedEnd = r.End
			if fetchEnd >= 0 && cachedEnd > fetchEnd {
				cachedEnd = fetchEnd
			}
			break
		}
	}

	if cachedEnd < fetchStart {
		return fetchStart, false, nil
	}

	log.Printf("[fetch] Serving bytes %d-%d from partial cache", fetchStart, cachedEnd)
	n, err := readRangeFromChunks(metadata, fetchStart, cachedEnd, cacheDir, urlHash, w)
	result.FromCache = n
	result.Served = n

	if err != nil {
		return fetchStart, false, err
	}

	// Update fetch range to start after cached portion
	fetchStart = cachedEnd + 1

	// If we've served everything requested, we're done
	if fetchEnd >= 0 && fetchStart > fetchEnd {
		result.StatusCode = http.StatusPartialContent
		result.PartialPath = GetPartialCachePath(cacheDir, urlHash)
		return fetchStart, true, nil
	}

	return fetchStart, false, nil
}

// executeUpstreamFetch builds request and fetches from upstream
func executeUpstreamFetch(opts FetchOptions, metadata *PartialCacheMetadata, fetchStart, fetchEnd int64) (*http.Response, *util.ActivityReader, error) {
	log.Printf("[fetch] Executing upstream fetch: %s (range: %d-%d)", opts.URL, fetchStart, fetchEnd)

	// Build upstream request
	upstreamReq, err := http.NewRequest("GET", opts.URL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating upstream request: %w", err)
	}

	// Copy request headers (for auth, etc.)
	if opts.RequestHeaders != nil {
		upstreamReq.Header = opts.RequestHeaders.Clone()
	}

	// Add Range header if we're fetching a specific range
	if fetchStart > 0 || (fetchEnd >= 0 && fetchEnd < metadata.TotalSize-1) {
		rangeHeader := fmt.Sprintf("bytes=%d-", fetchStart)
		if fetchEnd >= 0 {
			rangeHeader = fmt.Sprintf("bytes=%d-%d", fetchStart, fetchEnd)
		}
		upstreamReq.Header.Set("Range", rangeHeader)
		log.Printf("[fetch] Requesting range from upstream: %s", rangeHeader)
	}

	// Fetch from upstream
	resp, err := opts.Client.Do(upstreamReq)
	if err != nil {
		return nil, nil, fmt.Errorf("error fetching from upstream: %w", err)
	}

	// Check response status
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		util.Logerr(resp.Body.Close())
		return nil, nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	// Wrap response body with activity-based timeout
	activityBody := util.NewActivityReader(resp.Body, opts.ChunkTimeout)

	return resp, activityBody, nil
}

// updateMetadataSize updates metadata with total size from response headers
func updateMetadataSize(metadata *PartialCacheMetadata, resp *http.Response, opts *FetchOptions) {
	if metadata.TotalSize > 0 {
		return
	}

	if resp.StatusCode == http.StatusPartialContent {
		// Parse Content-Range header
		contentRange := resp.Header.Get("Content-Range")
		if parts := strings.Split(contentRange, "/"); len(parts) == 2 {
			if size, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				metadata.TotalSize = size
				opts.ExpectedSize = size
			}
		}
	} else if resp.ContentLength > 0 {
		metadata.TotalSize = resp.ContentLength
		opts.ExpectedSize = resp.ContentLength
	}
}

// streamAndCacheChunk streams response to client and cache file
func streamAndCacheChunk(opts FetchOptions, metadata *PartialCacheMetadata, urlHash string, fetchStart, fetchEnd int64, activityBody *util.ActivityReader, w io.Writer, result *FetchResult) (int64, error) {
	chunkStart := fetchStart
	chunkEnd := fetchEnd
	if chunkEnd < 0 && metadata.TotalSize > 0 {
		chunkEnd = metadata.TotalSize - 1
	} else if chunkEnd < 0 {
		// Use a large value - will be updated based on actual bytes read
		chunkEnd = chunkStart + 1<<30 // 1GB placeholder
	}

	// Create chunk file
	partialDir := GetPartialCachePath(opts.CacheDir, urlHash)
	if err := os.MkdirAll(partialDir, 0755); err != nil {
		return 0, fmt.Errorf("error creating partial cache dir: %w", err)
	}

	chunkPath := GetPartialChunkPath(opts.CacheDir, urlHash, chunkStart, chunkEnd)
	chunkFile, err := os.Create(chunkPath)
	if err != nil {
		log.Printf("[fetch] Warning: cannot create chunk file, streaming without cache: %v", err)
		// Stream without caching
		n, err := io.Copy(w, activityBody)
		result.Served += n
		result.FromUpstream = n
		return n, err
	}
	defer util.DeferClose(chunkFile)

	// Stream to client AND chunk file simultaneously
	multiWriter := io.MultiWriter(w, chunkFile)
	log.Printf("[fetch] Starting stream from upstream...")
	bytesWritten, err := io.Copy(multiWriter, activityBody)
	if err != nil {
		log.Printf("[fetch] Error during streaming: %v", err)
		return 0, fmt.Errorf("error streaming: %w", err)
	}
	log.Printf("[fetch] Streaming complete: %d bytes", bytesWritten)

	result.Served += bytesWritten
	result.FromUpstream = bytesWritten
	result.Cached = bytesWritten

	// Update metadata
	metadata.AddRange(chunkStart, chunkStart+bytesWritten-1)
	if err := metadata.Save(opts.CacheDir, urlHash); err != nil {
		log.Printf("[fetch] Warning: failed to save metadata: %v", err)
	}

	return bytesWritten, nil
}

// finalizeResult checks if download is complete and sets result status/headers
func finalizeResult(metadata *PartialCacheMetadata, opts FetchOptions, urlHash string, resp *http.Response, result *FetchResult) {
	partialDir := GetPartialCachePath(opts.CacheDir, urlHash)

	// Check if complete
	if metadata.IsComplete() {
		log.Printf("[fetch] Download complete! Assembling full file...")
		completePath, err := assembleAndValidate(metadata, opts.CacheDir, urlHash, opts.ExpectedHash, opts.StatsTracker)
		if err != nil {
			log.Printf("[fetch] Error assembling complete file: %v", err)
		} else {
			result.Complete = true
			result.CompletePath = completePath
			log.Printf("[fetch] File complete and validated: %s", completePath)
		}
	} else {
		log.Printf("[fetch] Partial download: %.1f%% complete", metadata.GetCompletionPercentage())
		result.PartialPath = partialDir
	}

	// Set status code
	result.StatusCode = http.StatusOK
	if opts.RequestedRange != nil {
		result.StatusCode = http.StatusPartialContent
	}

	// Copy response headers
	for k, v := range resp.Header {
		result.Headers[k] = v
	}
}

// assembleAndValidate assembles a complete file from chunks and validates hash
func assembleAndValidate(metadata *PartialCacheMetadata, cacheDir, urlHash, expectedHash string, statsTracker *StatsTracker) (string, error) {
	// Create temp file for assembly
	tempFile, err := os.CreateTemp(cacheDir, "assemble-*.tmp")
	if err != nil {
		return "", err
	}
	tempFileName := tempFile.Name()
	defer util.DeferRemove(tempFileName)
	defer util.DeferClose(tempFile)

	// Hash while assembling
	hasher := sha256.New()
	multiWriter := io.MultiWriter(tempFile, hasher)

	// Read all chunks in order
	for _, r := range metadata.Ranges {
		chunkPath := GetPartialChunkPath(cacheDir, urlHash, r.Start, r.End)
		data, err := os.ReadFile(chunkPath)
		if err != nil {
			return "", fmt.Errorf("error reading chunk: %w", err)
		}

		if _, err := multiWriter.Write(data); err != nil {
			return "", fmt.Errorf("error writing to temp file: %w", err)
		}
	}

	util.Logerr(tempFile.Close())

	// Calculate hash
	computedHash := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	// Validate if expected hash provided
	if expectedHash != "" && computedHash != expectedHash {
		return "", fmt.Errorf("hash mismatch: expected %s, got %s", expectedHash, computedHash)
	}

	// Move to content-addressed storage
	contentPath := GetContentPath(cacheDir, computedHash)
	if err := os.MkdirAll(filepath.Dir(contentPath), 0755); err != nil {
		return "", err
	}

	if err := os.Rename(tempFileName, contentPath); err != nil {
		return "", err
	}

	// Update refcount
	if statsTracker != nil {
		util.Logerr(IncrementRefcount(cacheDir, computedHash))
	}

	// Clean up partial cache
	partialDir := GetPartialCachePath(cacheDir, urlHash)
	if err := os.RemoveAll(partialDir); err != nil {
		log.Printf("[fetch] Warning: failed to clean up partial cache: %v", err)
	}

	log.Printf("[fetch] Assembled complete file: %s (%d bytes, hash validated)", contentPath, metadata.TotalSize)

	return contentPath, nil
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
