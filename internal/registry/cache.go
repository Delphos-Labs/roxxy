package registry

import (
	"bufio"
	"crypto/sha256"
	cryptotls "crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/tls"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// responseWriter is an interface for writing HTTP responses
// This allows us to use the same caching logic for both HTTP and HTTPS
type responseWriter interface {
	Write([]byte) (int, error)
}

// handleLocalCacheRequest handles the core caching logic for a registry request
// It works with both HTTP and HTTPS by accepting a generic responseWriter interface
func (p *Proxy) handleLocalCacheRequest(req *http.Request, hostname string, registryConfig config.RegistryConfig, scheme string, respWriter responseWriter) {
	startTime := time.Now()

	// Debug logging
	log.Printf("[cache] handleLocalCacheRequest: method=%s hostname=%q path=%q", req.Method, hostname, req.URL.Path)

	// OPTIONS requests should be passed through to upstream without caching
	// This includes OPTIONS * (server capabilities query)
	if req.Method == "OPTIONS" {
		log.Printf("[cache] Passing OPTIONS request to upstream: %s %s", hostname, req.URL.Path)

		// For OPTIONS *, we need to make an OPTIONS * request to upstream
		// Construct the upstream URL differently for OPTIONS *
		var upstreamURL string

		if req.URL.Path == "*" {
			// OPTIONS * - request applies to server as a whole
			upstreamURL = scheme + "://" + hostname
		} else {
			// OPTIONS /path - request applies to specific resource
			upstreamURL = scheme + "://" + hostname + req.URL.Path
			if req.URL.RawQuery != "" {
				upstreamURL += "?" + req.URL.RawQuery
			}
		}

		// Create OPTIONS request
		upstreamReq, err := http.NewRequest("OPTIONS", upstreamURL, nil)
		if err != nil {
			log.Printf("Error creating OPTIONS request: %v", err)
			return
		}

		// For OPTIONS *, set the path explicitly
		if req.URL.Path == "*" {
			upstreamReq.URL.Opaque = "*"
		}

		// Copy headers
		upstreamReq.Header = req.Header.Clone()

		// Make request
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(upstreamReq)
		if err != nil {
			log.Printf("Error making OPTIONS request: %v", err)
			return
		}
		defer util.DeferClose(resp.Body)

		// Forward response to client
		if httpRespWriter, ok := respWriter.(http.ResponseWriter); ok {
			for k, v := range resp.Header {
				httpRespWriter.Header()[k] = v
			}
			httpRespWriter.WriteHeader(resp.StatusCode)
			if _, err := io.Copy(httpRespWriter, resp.Body); err != nil {
				log.Printf("Error copying response body: %v", err)
			}
		} else {
			// For raw connections
			if err := resp.Write(respWriter); err != nil {
				log.Printf("Error writing response: %v", err)
			}
		}
		return
	}

	// Build full URL for upstream fetch (what the client requested)
	// Use req.URL.Path + RawQuery instead of RequestURI() to avoid issues with encoded characters
	fullURL := scheme + "://" + hostname + req.URL.Path
	if req.URL.RawQuery != "" {
		fullURL += "?" + req.URL.RawQuery
	}

	// Hash the path to get cache key (not the full URL)
	pathHashStr := cache.PathHash(req.URL.Path)
	urlCachePath := cache.GetURLPath(p.cacheDir, hostname, pathHashStr)

	// Handle DELETE request (cache eviction)
	if req.Method == "DELETE" {
		// Read URL metadata to get content hash
		if metadataBytes, err := os.ReadFile(urlCachePath); err == nil {
			var metadata cache.Metadata
			if err := json.Unmarshal(metadataBytes, &metadata); err == nil {
				// Delete URL mapping
				util.Logerr(os.Remove(urlCachePath))
				log.Printf("  [DELETE %s] -> Removed URL from cache", req.URL.Path)

				// Decrement refcount
				if shouldDelete, err := cache.DecrementRefcount(p.cacheDir, metadata.ContentHash); err == nil && shouldDelete {
					// Refcount reached 0, delete content
					contentPath := cache.GetContentPath(p.cacheDir, metadata.ContentHash)
					util.Logerr(os.Remove(contentPath))
					log.Printf("  ✓ Deleted content %s... (refcount=0)", metadata.ContentHash[:8])
				}

				// Send 204 No Content response
				// For http.ResponseWriter, we need to write headers separately
				// For raw connections (net.Conn), we can use http.Response.Write()
				if httpRespWriter, ok := respWriter.(http.ResponseWriter); ok {
					httpRespWriter.WriteHeader(http.StatusNoContent)
				} else {
					// For raw connections (TLS), use http.Response.Write()
					resp := &http.Response{
						StatusCode: http.StatusNoContent,
						ProtoMajor: 1,
						ProtoMinor: 1,
						Header:     make(http.Header),
					}
					if err := resp.Write(respWriter); err != nil {
						log.Printf("Error writing response: %v", err)
					}
				}
				return
			}
		}

		// URL not found in cache
		// For http.ResponseWriter, we need to write headers separately
		// For raw connections (net.Conn), we can use http.Response.Write()
		if httpRespWriter, ok := respWriter.(http.ResponseWriter); ok {
			httpRespWriter.WriteHeader(http.StatusNotFound)
		} else {
			// For raw connections (TLS), use http.Response.Write()
			resp := &http.Response{
				StatusCode: http.StatusNotFound,
				ProtoMajor: 1,
				ProtoMinor: 1,
				Header:     make(http.Header),
			}
			if err := resp.Write(respWriter); err != nil {
				log.Printf("Error writing response: %v", err)
			}
		}
		return
	}

	// Handle PUT request (direct file upload into cache)
	// This allows storing arbitrary files (e.g., database dumps, parquet files)
	// that can be retrieved later via GET on the same path.
	if req.Method == "PUT" {
		if req.Body == nil {
			if httpRespWriter, ok := respWriter.(http.ResponseWriter); ok {
				http.Error(httpRespWriter, "PUT requires a body", http.StatusBadRequest)
			}
			return
		}
		defer util.DeferClose(req.Body)

		// Stream body to temp file while computing hash
		tempFile, err := os.CreateTemp(filepath.Join(p.cacheDir, "tmp"), "put-*.tmp")
		if err != nil {
			log.Printf("[put] Error creating temp file: %v", err)
			if httpRespWriter, ok := respWriter.(http.ResponseWriter); ok {
				http.Error(httpRespWriter, "internal error", http.StatusInternalServerError)
			}
			return
		}
		tempFileName := tempFile.Name()
		defer util.DeferRemove(tempFileName)

		hasher := sha256.New()
		written, err := io.Copy(io.MultiWriter(hasher, tempFile), req.Body)
		util.Logerr(tempFile.Close())
		if err != nil {
			log.Printf("[put] Error writing upload: %v", err)
			if httpRespWriter, ok := respWriter.(http.ResponseWriter); ok {
				http.Error(httpRespWriter, "upload failed", http.StatusInternalServerError)
			}
			return
		}

		contentHashStr := hex.EncodeToString(hasher.Sum(nil))
		contentPath := cache.GetContentPath(p.cacheDir, contentHashStr)

		// Move temp file to content-addressed storage (if not already there)
		if _, err := os.Stat(contentPath); err != nil {
			if err := os.MkdirAll(filepath.Dir(contentPath), 0755); err != nil {
				log.Printf("[put] Error creating content dir: %v", err)
			} else if err := os.Rename(tempFileName, contentPath); err != nil {
				log.Printf("[put] Error moving to content store: %v", err)
			}
		}

		// Increment refcount
		util.Logerr(cache.IncrementRefcount(p.cacheDir, contentHashStr))

		// Create URL metadata
		contentType := req.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		metadata := cache.Metadata{
			ContentHash: contentHashStr,
			URL:         fullURL,
			ContentType: contentType,
			Size:        written,
			CachedAt:    time.Now(),
		}
		if metadataBytes, err := json.Marshal(metadata); err == nil {
			if err := os.MkdirAll(filepath.Dir(urlCachePath), 0755); err == nil {
				metadataTempFile, err := os.CreateTemp(filepath.Dir(urlCachePath), ".metadata-*.tmp")
				if err == nil {
					if _, err := metadataTempFile.Write(metadataBytes); err == nil {
						util.Logerr(metadataTempFile.Close())
						util.Logerr(os.Rename(metadataTempFile.Name(), urlCachePath))
					} else {
						util.Logerr(metadataTempFile.Close())
						util.Logerr(os.Remove(metadataTempFile.Name()))
					}
				}
			}
		}

		// Track access
		if p.statsTracker != nil {
			p.statsTracker.IncrementAccessCount(contentHashStr)
		}

		log.Printf("[put] %s -> %s (%s, sha256:%s…)", req.URL.Path, hostname, util.FormatBytes(written), contentHashStr[:12])

		if httpRespWriter, ok := respWriter.(http.ResponseWriter); ok {
			httpRespWriter.Header().Set("Content-Type", "application/json")
			httpRespWriter.WriteHeader(http.StatusCreated)
			util.LogerrT(fmt.Fprintf(httpRespWriter, `{"sha256":"%s","size":%d}`+"\n", contentHashStr, written))
		} else {
			resp := &http.Response{
				StatusCode: http.StatusCreated,
				ProtoMajor: 1,
				ProtoMinor: 1,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"sha256":"%s","size":%d}`+"\n", contentHashStr, written))),
			}
			resp.Header.Set("Content-Type", "application/json")
			if err := resp.Write(respWriter); err != nil {
				log.Printf("Error writing response: %v", err)
			}
		}
		return
	}

	// Check if file should be cached based on extension
	shouldCache := true
	for _, ext := range registryConfig.ExcludeExtensions {
		if strings.HasSuffix(req.URL.Path, ext) {
			shouldCache = false
			break
		}
	}

	// Check URL cache for metadata
	if metadataBytes, err := os.ReadFile(urlCachePath); err == nil {
		// URL cache hit - read metadata
		var metadata cache.Metadata
		if err := json.Unmarshal(metadataBytes, &metadata); err == nil {
			// Build content path using helper
			contentPath := cache.GetContentPath(p.cacheDir, metadata.ContentHash)

			// Check if content exists
			if contentBytes, err := os.ReadFile(contentPath); err == nil {
				// Increment access count
				if p.statsTracker != nil {
					p.statsTracker.IncrementAccessCount(metadata.ContentHash)
				}

				duration := time.Since(startTime)
				log.Printf("[binary] %s %s - 200 %s %dms (cache)", req.Method, filepath.Base(req.URL.Path), util.FormatBytes(metadata.Size), duration.Milliseconds())

				// Send response with Range support
				// For http.ResponseWriter, we need to write headers and body separately
				// For raw connections (net.Conn), we can use http.Response.Write()
				if httpRespWriter, ok := respWriter.(http.ResponseWriter); ok {
					httpRespWriter.Header().Set("Content-Type", metadata.ContentType)
					httpRespWriter.Header().Set("Accept-Ranges", "bytes")

					// Handle Range requests
					rangeHeader := req.Header.Get("Range")
					if rangeHeader != "" {
						p.handleRangeRequest(httpRespWriter, req, contentBytes, rangeHeader, metadata.ContentType)
					} else {
						httpRespWriter.Header().Set("Content-Length", fmt.Sprintf("%d", len(contentBytes)))
						httpRespWriter.WriteHeader(http.StatusOK)
						if _, err := httpRespWriter.Write(contentBytes); err != nil {
							log.Printf("Error writing content: %v", err)
						}
					}
				} else {
					// For raw connections (TLS), use http.Response.Write()
					// TODO: Add range support for raw connections if needed
					resp := &http.Response{
						StatusCode: http.StatusOK,
						ProtoMajor: 1,
						ProtoMinor: 1,
						Header:     make(http.Header),
					}
					resp.Header.Set("Content-Type", metadata.ContentType)
					resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(contentBytes)))
					resp.Header.Set("Accept-Ranges", "bytes")
					resp.Body = io.NopCloser(strings.NewReader(string(contentBytes)))
					if err := resp.Write(respWriter); err != nil {
						log.Printf("Error writing response: %v", err)
					}
				}
				return
			}
		}
	}

	// Cache miss — if hostname is localhost, there's no upstream to fetch from.
	// Return 404 so callers know the file hasn't been PUT yet.
	if util.IsLocalhost(hostname) {
		log.Printf("[cache] Local-only cache miss: %s%s", hostname, req.URL.Path)
		if httpRespWriter, ok := respWriter.(http.ResponseWriter); ok {
			http.Error(httpRespWriter, "not found in local cache", http.StatusNotFound)
		} else {
			resp := &http.Response{
				StatusCode: http.StatusNotFound,
				ProtoMajor: 1,
				ProtoMinor: 1,
				Header:     make(http.Header),
			}
			if err := resp.Write(respWriter); err != nil {
				log.Printf("Error writing response: %v", err)
			}
		}
		return
	}

	// Cache miss - fetch from upstream
	// Build upstream request
	upstreamReq, err := http.NewRequest(req.Method, fullURL, req.Body)
	if err != nil {
		log.Printf("Error creating upstream request: %v", err)
		return
	}

	// Copy headers
	upstreamReq.Header = req.Header.Clone()

	// Fetch from upstream with configurable proxy (supports proxy chaining)
	// Use system cert pool to trust certificates from /usr/local/share/ca-certificates/
	systemCertPool, err := x509.SystemCertPool()
	if err != nil {
		// Fallback to empty pool if system pool is unavailable
		systemCertPool = x509.NewCertPool()
	}
	transport := &http.Transport{
		Proxy: config.GetUpstreamProxyFunc(), // Use configurable proxy (or nil if not set)
		TLSClientConfig: &cryptotls.Config{
			RootCAs:            systemCertPool,
			InsecureSkipVerify: false,
		},
		IdleConnTimeout:       300 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
		ExpectContinueTimeout: 10 * time.Second,
	}
	client := &http.Client{
		Timeout:   300 * time.Second,
		Transport: transport,
	}

	// Measure fetch time
	fetchStart := time.Now()
	upstreamResp, err := client.Do(upstreamReq)
	if err != nil {
		log.Printf("Error fetching from upstream: %v", err)
		return
	}
	defer util.DeferClose(upstreamResp.Body)

	// Check Content-Length to determine if we should stream or buffer
	const maxBufferSize = 100 * 1024 * 1024 // 100MB - stream files larger than this
	contentLength := upstreamResp.ContentLength
	shouldStreamCache := contentLength > maxBufferSize || contentLength < 0 // Stream if > 100MB or unknown size

	var body []byte
	var fileSize int64
	var contentHashStr string
	cached := false

	if shouldStreamCache {
		if shouldCache {
			// Stream large files: hash + cache + send to client simultaneously
			// Create temp file for caching as we stream
			tempFile, err := os.CreateTemp(filepath.Join(p.cacheDir, "tmp"), "stream-*.tmp")
			if err != nil {
				log.Printf("Error creating temp file for streaming: %v, streaming without cache", err)
				// Fall through to non-caching stream below
			} else {
				tempFileName := tempFile.Name()
				defer util.DeferRemove(tempFileName) // Clean up temp file on exit (unless moved)
				defer util.DeferClose(tempFile)

				// Extract expected digest from URL if present (Docker blobs have sha256:digest in path)
				var expectedDigest string
				if strings.Contains(req.URL.Path, "/blobs/sha256:") {
					parts := strings.Split(req.URL.Path, "/blobs/sha256:")
					if len(parts) == 2 {
						expectedDigest = parts[1]
					}
				}

				// Create SHA256 hasher
				hasher := sha256.New()

				// Create multi-writer: hash + temp file + client
				var writer io.Writer
				if httpRespWriter, ok := respWriter.(http.ResponseWriter); ok {
					// Copy headers first
					for k, v := range upstreamResp.Header {
						for _, val := range v {
							httpRespWriter.Header().Add(k, val)
						}
					}
					httpRespWriter.WriteHeader(upstreamResp.StatusCode)
					writer = io.MultiWriter(hasher, tempFile, httpRespWriter)
				} else {
					// For raw connections, we need to write headers manually later
					writer = io.MultiWriter(hasher, tempFile)
				}

				// Stream from upstream through multi-writer
				bytesWritten, copyErr := io.Copy(writer, upstreamResp.Body)
				fetchDuration := time.Since(fetchStart)
				totalDuration := time.Since(startTime)

				// Check for errors during streaming
				if copyErr != nil {
					log.Printf("[binary] %s %s - ERROR streaming %s after %dms: %v", req.Method, filepath.Base(req.URL.Path), util.FormatBytes(bytesWritten), totalDuration.Milliseconds(), copyErr)
					// Temp file will be cleaned up by defer
					return
				}

				// Verify content length if provided
				if contentLength >= 0 && bytesWritten != contentLength {
					log.Printf("[binary] %s %s - ERROR incomplete download: got %s, expected %s", req.Method, filepath.Base(req.URL.Path), util.FormatBytes(bytesWritten), util.FormatBytes(contentLength))
					return
				}

				fileSize = bytesWritten
				contentHashStr = hex.EncodeToString(hasher.Sum(nil))

				// Validate hash matches expected digest (for Docker blobs)
				if expectedDigest != "" && contentHashStr != expectedDigest {
					log.Printf("[binary] %s %s - ERROR hash mismatch: computed %s, expected %s", req.Method, filepath.Base(req.URL.Path), contentHashStr[:12], expectedDigest[:12])
					return
				}

				// Check if we should cache based on size
				if fileSize >= registryConfig.MinCacheSize {
					// Move temp file to content path
					contentPath := cache.GetContentPath(p.cacheDir, contentHashStr)

					contentExists := false
					// Check if content already exists
					if _, err := os.Stat(contentPath); err == nil {
						contentExists = true
					} else {
						// Content doesn't exist, move temp file
						if err := os.MkdirAll(filepath.Dir(contentPath), 0755); err != nil {
							log.Printf("[binary] %s %s - ERROR creating cache dir: %v", req.Method, filepath.Base(req.URL.Path), err)
						} else {
							util.Logerr(tempFile.Close()) // Close before moving
							if err := os.Rename(tempFileName, contentPath); err != nil {
								log.Printf("[binary] %s %s - ERROR moving to cache: %v", req.Method, filepath.Base(req.URL.Path), err)
							} else {
								contentExists = true
							}
						}
					}

					// If we have content (either existing or just moved), create metadata
					if contentExists {
						// Increment refcount
						util.Logerr(cache.IncrementRefcount(p.cacheDir, contentHashStr))

						// Create URL metadata
						metadata := cache.Metadata{
							ContentHash:   contentHashStr,
							URL:           fullURL,
							ContentType:   upstreamResp.Header.Get("Content-Type"),
							Size:          fileSize,
							CachedAt:      time.Now(),
							FetchDuration: fetchDuration,
						}

						// Store URL mapping atomically using temp file + rename
						if metadataBytes, err := json.Marshal(metadata); err == nil {
							if err := os.MkdirAll(filepath.Dir(urlCachePath), 0755); err == nil {
								// Write to temp file first
								metadataTempFile, err := os.CreateTemp(filepath.Dir(urlCachePath), ".metadata-*.tmp")
								if err == nil {
									if _, err := metadataTempFile.Write(metadataBytes); err == nil {
										util.Logerr(metadataTempFile.Close())
										// Atomic rename
										if err := os.Rename(metadataTempFile.Name(), urlCachePath); err == nil {
											cached = true
										} else {
											log.Printf("[binary] %s %s - ERROR writing metadata: %v", req.Method, filepath.Base(req.URL.Path), err)
											util.Logerr(os.Remove(metadataTempFile.Name()))
										}
									} else {
										util.Logerr(metadataTempFile.Close())
										util.Logerr(os.Remove(metadataTempFile.Name()))
									}
								}
							}
						}
					}
				}

				// For raw connections, we need to send the response now
				if _, ok := respWriter.(http.ResponseWriter); !ok {
					// Reopen temp file and send to client
					if _, err := tempFile.Seek(0, 0); err != nil {
						log.Printf("Error seeking temp file: %v", err)
					}
					upstreamResp.Body = io.NopCloser(tempFile)
					if err := upstreamResp.Write(respWriter); err != nil {
						log.Printf("Error writing upstream response: %v", err)
					}
				}

				// Log the result
				if cached {
					log.Printf("[binary] %s %s - 200 %s %dms (streamed+cached)", req.Method, filepath.Base(req.URL.Path), util.FormatBytes(fileSize), totalDuration.Milliseconds())
				} else {
					log.Printf("[binary] %s %s - 200 %s %dms (streamed, no-cache: too small)", req.Method, filepath.Base(req.URL.Path), util.FormatBytes(fileSize), totalDuration.Milliseconds())
				}
				return
			}
		}

		// Stream without caching (temp file creation failed or caching disabled for large files)
		// Just stream directly to client without hashing or caching
		totalDuration := time.Since(startTime)
		if httpRespWriter, ok := respWriter.(http.ResponseWriter); ok {
			// Copy headers
			for k, v := range upstreamResp.Header {
				for _, val := range v {
					httpRespWriter.Header().Add(k, val)
				}
			}
			httpRespWriter.WriteHeader(upstreamResp.StatusCode)
			// Stream body
			bytesWritten, err := io.Copy(httpRespWriter, upstreamResp.Body)
			if err != nil {
				log.Printf("[binary] %s %s - ERROR streaming without cache: %v", req.Method, filepath.Base(req.URL.Path), err)
				return
			}
			log.Printf("[binary] %s %s - 200 %s %dms (streamed, no-cache)", req.Method, filepath.Base(req.URL.Path), util.FormatBytes(bytesWritten), totalDuration.Milliseconds())
		} else {
			// For raw connections, use http.Response.Write()
			if err := upstreamResp.Write(respWriter); err != nil {
				log.Printf("Error writing upstream response: %v", err)
			}
			sizeStr := util.FormatBytes(contentLength)
			if contentLength < 0 {
				sizeStr = "unknown"
			}
			log.Printf("[binary] %s %s - 200 %s %dms (streamed, no-cache)", req.Method, filepath.Base(req.URL.Path), sizeStr, totalDuration.Milliseconds())
		}
		return
	}

	// Buffer small files (existing code path)
	body, err = io.ReadAll(upstreamResp.Body)
	fetchDuration := time.Since(fetchStart)
	totalDuration := time.Since(startTime)

	if err != nil {
		log.Printf("Error reading upstream response: %v", err)
		return
	}

	// Check if we should cache this file
	fileSize = int64(len(body))
	if shouldCache && fileSize >= registryConfig.MinCacheSize {
		// Hash the content
		contentHash := sha256.Sum256(body)
		contentHashStr = hex.EncodeToString(contentHash[:])
		contentPath := cache.GetContentPath(p.cacheDir, contentHashStr)

		// Write content file if it doesn't exist (deduplication)
		if _, err := os.Stat(contentPath); err != nil {
			// Content doesn't exist, write it
			if err := os.MkdirAll(filepath.Dir(contentPath), 0755); err == nil {
				util.Logerr(os.WriteFile(contentPath, body, 0644))
			}
		}

		// Increment refcount (whether content existed or not)
		util.Logerr(cache.IncrementRefcount(p.cacheDir, contentHashStr))

		// Create URL metadata
		metadata := cache.Metadata{
			ContentHash:   contentHashStr,
			URL:           fullURL,
			ContentType:   upstreamResp.Header.Get("Content-Type"),
			Size:          fileSize,
			CachedAt:      time.Now(),
			FetchDuration: fetchDuration,
		}

		// Store URL mapping
		if metadataBytes, err := json.Marshal(metadata); err == nil {
			if err := os.MkdirAll(filepath.Dir(urlCachePath), 0755); err != nil {
				log.Printf("Error creating cache directory: %v", err)
			}
			if err := os.WriteFile(urlCachePath, metadataBytes, 0644); err == nil {
				cached = true
			}
		}
	}

	// Log the result
	if cached {
		log.Printf("[binary] %s %s - 200 %s %dms (fetch)", req.Method, filepath.Base(req.URL.Path), util.FormatBytes(fileSize), totalDuration.Milliseconds())
	} else if fileSize < registryConfig.MinCacheSize {
		log.Printf("[binary] %s %s - 200 %s %dms (fetch, no-cache: too small)", req.Method, filepath.Base(req.URL.Path), util.FormatBytes(fileSize), totalDuration.Milliseconds())
	} else {
		log.Printf("[binary] %s %s - 200 %s %dms (fetch, no-cache: excluded)", req.Method, filepath.Base(req.URL.Path), util.FormatBytes(fileSize), totalDuration.Milliseconds())
	}

	// Return response to client
	// For http.ResponseWriter, we need to write headers and body separately
	// For raw connections (net.Conn), we can use http.Response.Write()
	if httpRespWriter, ok := respWriter.(http.ResponseWriter); ok {
		// Copy headers
		for k, v := range upstreamResp.Header {
			for _, val := range v {
				httpRespWriter.Header().Add(k, val)
			}
		}
		// Write status code
		httpRespWriter.WriteHeader(upstreamResp.StatusCode)
		// Write body
		if _, err := httpRespWriter.Write(body); err != nil {
			log.Printf("Error writing body: %v", err)
		}
	} else {
		// For raw connections (TLS), use http.Response.Write()
		upstreamResp.Body = io.NopCloser(strings.NewReader(string(body)))
		if err := upstreamResp.Write(respWriter); err != nil {
			log.Printf("Error writing upstream response: %v", err)
		}
	}
}

// HandleLocalCache handles local cache for registry requests via CONNECT (HTTPS)
// This hijacks the connection, performs TLS handshake, and then handles the request
func (p *Proxy) HandleLocalCache(w http.ResponseWriter, r *http.Request, hostname string, registryConfig config.RegistryConfig, certManager *tls.CertificateManager) {
	// Send 200 Connection Established
	w.WriteHeader(http.StatusOK)

	// Hijack the connection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		log.Printf("Error hijacking connection: %v", err)
		return
	}
	defer util.DeferClose(clientConn)

	// Generate certificate for this hostname
	cert, err := certManager.GenerateCertForHost(hostname)
	if err != nil {
		log.Printf("Error generating certificate for %s: %v", hostname, err)
		return
	}

	// Perform TLS handshake with client
	// Only advertise HTTP/1.1 since we don't implement HTTP/2 framing
	tlsConfig := &cryptotls.Config{
		Certificates: []cryptotls.Certificate{*cert},
		NextProtos:   []string{"http/1.1"},   // Only HTTP/1.1, no h2
		MinVersion:   cryptotls.VersionTLS12, // Explicitly require TLS 1.2 minimum
	}

	tlsClientConn := cryptotls.Server(clientConn, tlsConfig)
	if err := tlsClientConn.Handshake(); err != nil {
		log.Printf("TLS handshake error: %v", err)
		return
	}
	defer util.DeferClose(tlsClientConn)

	// Read HTTP request from client
	bufReader := bufio.NewReader(tlsClientConn)
	req, err := http.ReadRequest(bufReader)
	if err != nil {
		log.Printf("Error reading request: %v", err)
		return
	}

	// Use the shared caching logic with HTTPS scheme
	p.handleLocalCacheRequest(req, hostname, registryConfig, "https", tlsClientConn)
}

// handleRangeRequest handles HTTP Range requests for cached content
func (p *Proxy) handleRangeRequest(w http.ResponseWriter, r *http.Request, data []byte, rangeHeader string, contentType string) {
	// Parse Range header: "bytes=start-end" or "bytes=start-"
	// We only support simple single-range requests for now
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		// Invalid range, return full content
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(data); err != nil {
			log.Printf("Error writing response: %v", err)
		}
		return
	}

	rangeSpec := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.Split(rangeSpec, "-")
	if len(parts) != 2 {
		// Invalid range format
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
		return
	}

	var start, end int64
	var err error

	// Parse start
	if parts[0] != "" {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || start < 0 || start >= int64(len(data)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			return
		}
	}

	// Parse end
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start || end >= int64(len(data)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			return
		}
	} else {
		// "bytes=start-" means from start to end of file
		end = int64(len(data)) - 1
	}

	// Return partial content
	rangeData := data[start : end+1]
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(rangeData)))
	w.WriteHeader(http.StatusPartialContent)
	if _, err := w.Write(rangeData); err != nil {
		log.Printf("Error writing response: %v", err)
	}

	log.Printf("[binary] Served range request: bytes %d-%d/%d (%d bytes)", start, end, len(data), len(rangeData))
}

// HandleLocalCacheHTTP handles local cache for plain HTTP requests
// This is used when the request comes in as plain HTTP (not CONNECT)
func (p *Proxy) HandleLocalCacheHTTP(w http.ResponseWriter, r *http.Request, hostname string, registryConfig config.RegistryConfig) {
	// Use the shared caching logic with HTTP scheme
	p.handleLocalCacheRequest(r, hostname, registryConfig, "http", w)
}
