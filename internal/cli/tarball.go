package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// debugLog logs a message only if debug is enabled, writing to stderr
func (tr *TarballReader) debugLog(format string, args ...interface{}) {
	if tr.debug {
		fmt.Fprintf(os.Stderr, "Tarball: "+format+"\n", args...)
	}
}

// TarballReader provides read-only access to a roxxy cache tarball
// Uses streaming approach with section indexing for faster access
type TarballReader struct {
	filePath   string
	isGzip     bool
	sectionIdx *sectionIndex
	indexBuilt bool
	debug      bool
}

// sectionIndex records byte offsets for major sections in the tarball
// This allows seeking to specific sections instead of reading everything sequentially
type sectionIndex struct {
	manifestSectionStart int64            // Where docker-manifests/ section starts
	blobSectionStart     int64            // Where docker-blobs/ section starts
	fileOffsets          map[string]int64 // Path -> byte offset (only for non-gzipped)
}

// tarballIndex is the index file written at the end of the tarball
// It contains block numbers (512-byte aligned) for all entries, allowing direct seeking
// Block numbers are stored as 32-bit integers (offset >> 10), supporting tarballs up to 2TB
type tarballIndex struct {
	FileOffsets map[string]int32 `json:"file_offsets"` // Path -> block number (offset / 512)
	Version     int              `json:"version"`      // Index format version
}

// OpenTarball opens a roxxy cache tarball for reading
func OpenTarball(tarballPath string) (*TarballReader, error) {
	return OpenTarballWithDebug(tarballPath, false)
}

// OpenTarballWithDebug opens a roxxy cache tarball for reading with debug logging control
func OpenTarballWithDebug(tarballPath string, debug bool) (*TarballReader, error) {
	isGzip := strings.HasSuffix(tarballPath, ".gz") || strings.HasSuffix(tarballPath, ".tgz")

	return &TarballReader{
		filePath:   tarballPath,
		isGzip:     isGzip,
		sectionIdx: &sectionIndex{fileOffsets: make(map[string]int64)},
		indexBuilt: false,
		debug:      debug,
	}, nil
}

// Close closes the tarball (no-op for streaming approach)
func (tr *TarballReader) Close() error {
	return nil
}

// createTarReader creates a new tar reader from the tarball file
// If offset is > 0 and not gzipped, seeks to that position first
// Tar headers are 512-byte aligned, so we align the offset to the nearest block boundary
func (tr *TarballReader) createTarReader(offset int64) (*os.File, *tar.Reader, error) {
	file, err := os.Open(tr.filePath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open tarball: %w", err)
	}

	// For non-gzipped tarballs, we can seek to a specific position
	// Tar format uses 512-byte blocks, so we align to block boundaries
	if !tr.isGzip && offset > 0 {
		// Align offset to 512-byte boundary (round down)
		alignedOffset := (offset / 512) * 512
		tr.debugLog("Seeking to offset %d (aligned from %d to %d)", offset, offset, alignedOffset)
		if _, err := file.Seek(alignedOffset, io.SeekStart); err != nil {
			util.Logerr(file.Close())
			return nil, nil, fmt.Errorf("failed to seek to offset %d (aligned to %d): %w", offset, alignedOffset, err)
		}
		tr.debugLog("Successfully seeked to offset %d", alignedOffset)
	} else if offset > 0 {
		tr.debugLog("Offset %d requested but file is gzipped, starting from beginning", offset)
	} else {
		tr.debugLog("No offset specified, starting from beginning")
	}

	var tarReader *tar.Reader
	if tr.isGzip {
		gzReader, err := gzip.NewReader(file)
		if err != nil {
			util.Logerr(file.Close())
			return nil, nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		tarReader = tar.NewReader(gzReader)
	} else {
		tarReader = tar.NewReader(file)
	}

	// If we seeked to an offset, the caller handles scanning forward to find the correct entry
	// since the offset might be slightly before the actual header due to alignment

	return file, tarReader, nil
}

// buildSectionIndex reads the index file from the end of the tarball
// The index file is written at the end and contains byte offsets for all entries
func (tr *TarballReader) buildSectionIndex() error {
	if tr.indexBuilt {
		tr.debugLog("Index already built, skipping")
		return nil
	}

	tr.debugLog("Building section index for %s (gzip: %v)", tr.filePath, tr.isGzip)

	// For gzipped files, we can't seek, so skip index
	if tr.isGzip {
		tr.debugLog("Gzipped file, skipping index (can't seek)")
		tr.indexBuilt = true
		return nil
	}

	file, err := os.Open(tr.filePath)
	if err != nil {
		return fmt.Errorf("failed to open tarball: %w", err)
	}
	defer util.DeferClose(file)

	// Get file size
	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat tarball: %w", err)
	}
	fileSize := fileInfo.Size()
	tr.debugLog("File size: %d bytes", fileSize)

	// Try to read index from the end
	// The index file is named ".roxxy-index.json" or ".roxxy-index.json.gz" and is at the end
	// We'll read backwards in 512-byte blocks (tar header size) looking for the index file entry
	// Start by reading the last 1MB, which should be enough for the index file and its header
	maxReadSize := int64(1024 * 1024) // 1MB
	readSize := int64(64 * 1024)      // Start with 64KB
	blockSize := int64(512)           // Tar block size

	// If file is smaller than readSize, use file size
	if readSize > fileSize {
		readSize = fileSize
	}

	for readSize <= maxReadSize && readSize > 0 {
		// Seek to position to read from
		seekPos := fileSize - readSize
		if seekPos < 0 {
			seekPos = 0
		}

		tr.debugLog("Searching for index, reading last %d bytes (seek to %d)", readSize, seekPos)
		if _, err := file.Seek(seekPos, io.SeekStart); err != nil {
			return fmt.Errorf("failed to seek: %w", err)
		}

		// Read the tail of the file
		tail := make([]byte, readSize)
		n, err := file.Read(tail)
		if err != nil && err != io.EOF {
			return fmt.Errorf("failed to read tail: %w", err)
		}
		tail = tail[:n]
		tr.debugLog("Read %d bytes from tail", n)

		// Scan backwards through the tail looking for tar headers
		// Tar headers are 512 bytes, and the name field is at offset 0-99 (or 0-255 for USTAR)
		// We'll check every 512-byte boundary
		for offset := len(tail) - int(blockSize); offset >= 0; offset -= int(blockSize) {
			// Check if this looks like a tar header (starts with a filename)
			// The name field in tar is at bytes 0-99 (or 0-255 for USTAR)
			headerStart := offset
			if headerStart+512 > len(tail) {
				continue
			}

			// Manually check the name field in the tar header
			// Tar header name is at bytes 0-99 (null-terminated)
			nameBytes := tail[headerStart : headerStart+100]
			nameEnd := 0
			for i, b := range nameBytes {
				if b == 0 {
					nameEnd = i
					break
				}
			}
			if nameEnd == 0 {
				// Try USTAR format (255 bytes)
				if headerStart+255 <= len(tail) {
					nameBytes = tail[headerStart : headerStart+255]
					for i, b := range nameBytes {
						if b == 0 {
							nameEnd = i
							break
						}
					}
				}
			}

			if nameEnd > 0 {
				headerName := string(tail[headerStart : headerStart+nameEnd])
				// Check for both new format (no cache/ prefix) and old format (with cache/ prefix)
				// Also check for gzipped version (.gz suffix)
				normalizedName := strings.TrimSuffix(headerName, ".gz")
				normalizedName = strings.TrimPrefix(normalizedName, "cache/")
				// Debug logging for index-related headers
				if strings.Contains(headerName, "roxxy-index") {
					tr.debugLog("Found potential index header: '%s' -> normalized: '%s'", headerName, normalizedName)
				}
				if normalizedName == ".roxxy-index.json" {
					// Found it! Now read the actual tar entry from the file
					actualHeaderStart := seekPos + int64(headerStart)
					// Align to 512-byte boundary (should already be aligned, but ensure it)
					alignedHeaderStart := (actualHeaderStart / 512) * 512

					if _, err := file.Seek(alignedHeaderStart, io.SeekStart); err != nil {
						tr.debugLog("Failed to seek to index header: %v", err)
						continue
					}

					tarReader := tar.NewReader(file)
					header, err := tarReader.Next()
					if err != nil {
						tr.debugLog("Failed to read index header: %v", err)
						continue
					}

					// Double-check the name matches (support both .json and .json.gz)
					headerName := header.Name
					isGzipped := strings.HasSuffix(headerName, ".gz")
					// Strip .gz suffix and cache/ prefix for comparison
					normalizedName := strings.TrimSuffix(headerName, ".gz")
					normalizedName = strings.TrimPrefix(normalizedName, "cache/")

					if normalizedName == ".roxxy-index.json" {
						tr.debugLog("Found index file header at offset %d (aligned from %d)", alignedHeaderStart, actualHeaderStart)
						tr.debugLog("Reading index file, size: %d bytes (gzipped: %v)", header.Size, isGzipped)

						// Read the index data
						indexData := make([]byte, header.Size)
						if _, err := io.ReadFull(tarReader, indexData); err != nil {
							return fmt.Errorf("failed to read index data: %w", err)
						}

						// Decompress if gzipped
						if isGzipped {
							gzReader, err := gzip.NewReader(bytes.NewReader(indexData))
							if err != nil {
								return fmt.Errorf("failed to create gzip reader for index: %w", err)
							}
							decompressedData, err := io.ReadAll(gzReader)
							util.Logerr(gzReader.Close())
							if err != nil {
								return fmt.Errorf("failed to decompress index: %w", err)
							}
							indexData = decompressedData
							tr.debugLog("Decompressed index from %d to %d bytes", header.Size, len(indexData))
						}

						// Parse the index - handle both version 1 (byte offsets) and version 2 (block numbers)
						var rawIndex map[string]interface{}
						if err := json.Unmarshal(indexData, &rawIndex); err != nil {
							return fmt.Errorf("failed to parse index: %w", err)
						}

						version, _ := rawIndex["version"].(float64)
						fileOffsetsRaw := rawIndex["file_offsets"]

						// Convert offsets to byte offsets (handling both versions)
						alignedOffsets := make(map[string]int64)

						if version == 1 {
							// Version 1: byte offsets stored as int64
							offsets, ok := fileOffsetsRaw.(map[string]interface{})
							if !ok {
								return fmt.Errorf("invalid index format: file_offsets is not a map")
							}
							for path, offsetVal := range offsets {
								offset, ok := offsetVal.(float64)
								if !ok {
									continue
								}
								// Align to 512-byte boundary
								byteOffset := int64(offset)
								alignedOffset := (byteOffset / 512) * 512
								alignedOffsets[path] = alignedOffset
							}
							tr.debugLog("Parsed index with %d file offsets (version 1: byte offsets)", len(alignedOffsets))
						} else {
							// Version 2+: block numbers stored as int32
							var idx tarballIndex
							if err := json.Unmarshal(indexData, &idx); err != nil {
								return fmt.Errorf("failed to parse index: %w", err)
							}
							tr.debugLog("Parsed index with %d file offsets (version %d: block numbers)", len(idx.FileOffsets), idx.Version)

							// Convert block numbers back to byte offsets (multiply by 512)
							for path, blockNum := range idx.FileOffsets {
								byteOffset := int64(blockNum) * 512
								alignedOffsets[path] = byteOffset
							}
						}

						tr.sectionIdx.fileOffsets = alignedOffsets
						tr.indexBuilt = true

						// Log sample of offsets - check for blob entries specifically
						sampleCount := 0
						blobSampleCount := 0
						for path, offset := range alignedOffsets {
							if sampleCount < 5 {
								tr.debugLog("Index sample - %s -> offset %d (aligned)", path, offset)
								sampleCount++
							}
							// Also log blob entries specifically
							if strings.HasPrefix(path, "docker-blobs/") && blobSampleCount < 3 {
								tr.debugLog("Index blob sample - %s -> offset %d (aligned)", path, offset)
								blobSampleCount++
							}
						}
						tr.debugLog("Total index entries: %d", len(alignedOffsets))
						return nil
					}
				}
			}
		}

		// Index not found in this chunk, try larger
		tr.debugLog("Index not found in last %d bytes, trying larger chunk", readSize)
		readSize *= 4
	}

	// Index not found - that's okay, we'll read sequentially
	tr.indexBuilt = true
	return nil
}

// extractRegistryAndRepo extracts registry and repo from a full repo name
// Examples:
//
//	"golang" -> ("docker.io", "library/golang")
//	"library/golang" -> ("docker.io", "library/golang")
//	"ghcr.io/cloudnative-pg/cloudnative-pg" -> ("ghcr.io", "cloudnative-pg/cloudnative-pg")
func extractRegistryAndRepo(fullRepoName string) (registryID, repoName string) {
	// Check if repo name contains a registry (has a dot and slash, like "ghcr.io/...")
	parts := strings.SplitN(fullRepoName, "/", 2)
	if len(parts) == 2 && strings.Contains(parts[0], ".") {
		// First part looks like a registry (contains a dot)
		registryID = parts[0]
		repoName = parts[1]
		return
	}

	// Default to docker.io
	registryID = "docker.io"
	repoName = fullRepoName

	// Normalize repo name (e.g., "golang" -> "library/golang")
	if !strings.Contains(repoName, "/") {
		repoName = "library/" + repoName
	}

	return
}

// FindManifestByRepoTag finds a manifest by repository and tag, streaming through the tarball
// Returns the manifest data and blob digests needed
// If platform is specified (format: "os/arch", e.g., "linux/amd64"), it will try to find that specific platform.
// If platform is empty, it defaults to "linux/amd64" or the first available platform.
func (tr *TarballReader) FindManifestByRepoTag(repoName, tag, platform string) ([]byte, string, []string, error) {
	// Extract registry and repo from the full repo name
	registryID, actualRepoName := extractRegistryAndRepo(repoName)

	safeRepo := util.SafeRepoName(actualRepoName)
	// Build manifest path with the correct registry ID
	manifestPath := fmt.Sprintf("docker-manifests/%s/%s/%s", registryID, safeRepo, tag)

	// Build index if not already built
	if !tr.indexBuilt {
		if err := tr.buildSectionIndex(); err != nil {
			return nil, "", nil, fmt.Errorf("failed to build section index: %w", err)
		}
	}

	// Try to use index file offset if available
	var startOffset int64
	var useIndex bool
	if !tr.isGzip {
		if offset, ok := tr.sectionIdx.fileOffsets[manifestPath]; ok {
			startOffset = offset
			useIndex = true
			tr.debugLog("Found manifest offset in index: %s -> %d", manifestPath, offset)
		} else {
			tr.debugLog("Manifest offset NOT found in index for: %s (have %d entries)", manifestPath, len(tr.sectionIdx.fileOffsets))
			// Log available keys for debugging
			sampleCount := 0
			for key := range tr.sectionIdx.fileOffsets {
				if sampleCount < 5 {
					tr.debugLog("Available index key sample: %s", key)
					sampleCount++
				}
			}
		}
	}

	file, tarReader, err := tr.createTarReader(startOffset)
	if err != nil {
		return nil, "", nil, err
	}
	defer util.DeferClose(file)

	// Stream through tarball looking for the manifest
	// If we used an index offset, we might need to scan forward a bit to find the correct entry
	// since tar headers are 512-byte aligned and we might have aligned the offset
	entriesScanned := 0
	maxScanAttempts := 10 // If we used index, try scanning up to 10 entries forward
	scanAttempts := 0
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			// If we got an invalid header error and we used an index offset,
			// try scanning forward a few 512-byte blocks to find a valid header
			if useIndex && strings.Contains(err.Error(), "invalid tar header") && scanAttempts < maxScanAttempts {
				scanAttempts++
				// Try seeking forward by one 512-byte block
				currentPos, _ := file.Seek(0, io.SeekCurrent)
				newPos := currentPos + 512
				if _, seekErr := file.Seek(newPos, io.SeekStart); seekErr == nil {
					// Create a new tar reader at the new position
					util.Logerr(file.Close())
					file, tarReader, err = tr.createTarReader(newPos)
					if err != nil {
						return nil, "", nil, err
					}
					continue
				}
			}
			// If scanning forward didn't work, fall back to reading from start
			if useIndex && strings.Contains(err.Error(), "invalid tar header") {
				util.Logerr(file.Close())
				// Retry from the beginning
				file, tarReader, err = tr.createTarReader(0)
				if err != nil {
					return nil, "", nil, err
				}
				// Continue to read sequentially
				useIndex = false // Don't try index again
				continue
			}
			return nil, "", nil, fmt.Errorf("failed to read tar entry: %w", err)
		}
		entriesScanned++
		scanAttempts = 0 // Reset scan attempts on successful read

		// Strip cache/ prefix if present (for backwards compatibility with old tarballs)
		// New tarballs don't have cache/ prefix, but old ones might
		path := stripCachePrefix(header.Name)

		// Check if this is our manifest
		if path != manifestPath {
			// Skip this entry's body
			if header.Typeflag == tar.TypeReg {
				if _, err := io.Copy(io.Discard, tarReader); err != nil {
					return nil, "", nil, fmt.Errorf("failed to skip entry: %w", err)
				}
			}
			continue
		}

		// Found it - read manifest data
		if header.Typeflag != tar.TypeReg {
			continue
		}
		// Read manifest data
		manifestData := make([]byte, header.Size)
		if _, err := io.ReadFull(tarReader, manifestData); err != nil {
			return nil, "", nil, fmt.Errorf("failed to read manifest: %w", err)
		}

		// Check if this is an OCI Image Index (multi-platform)
		var indexCheck struct {
			Manifests []interface{} `json:"manifests"`
		}
		if json.Unmarshal(manifestData, &indexCheck) == nil && len(indexCheck.Manifests) > 0 {
			// This is an index - we need to find a platform-specific manifest
			var index struct {
				Manifests []struct {
					Digest   string `json:"digest"`
					Platform struct {
						Architecture string `json:"architecture"`
						OS           string `json:"os"`
					} `json:"platform,omitempty"`
				} `json:"manifests"`
			}
			if err := json.Unmarshal(manifestData, &index); err == nil {
				// Collect available platforms
				var availablePlatforms []string
				for _, m := range index.Manifests {
					// Skip attestations/SBOMs (unknown platform)
					if m.Platform.OS == "unknown" || m.Platform.Architecture == "unknown" {
						continue
					}
					if m.Platform.OS == "" || m.Platform.Architecture == "" {
						continue
					}
					platformStr := fmt.Sprintf("%s/%s", m.Platform.OS, m.Platform.Architecture)
					availablePlatforms = append(availablePlatforms, platformStr)
				}

				// Parse platform if specified (format: "os/arch")
				var targetOS, targetArch string
				if platform != "" {
					parts := strings.Split(platform, "/")
					if len(parts) == 2 {
						targetOS = parts[0]
						targetArch = parts[1]
					}
				}

				// If no platform specified, try to match current CPU architecture
				if targetOS == "" && targetArch == "" {
					// Map Go's runtime architecture to Docker's architecture names
					currentOS := runtime.GOOS
					currentArch := runtime.GOARCH

					// Docker uses "linux" for Linux, "darwin" for macOS, "windows" for Windows
					// For architecture, Docker uses: amd64, arm64, arm, etc.
					// Go uses: amd64, arm64, arm, 386, etc.
					if currentOS == "darwin" {
						currentOS = "linux" // Docker images on macOS are typically linux-based
					}
					// Architecture names are mostly the same, but handle edge cases
					if currentArch == "386" {
						currentArch = "i386"
					}

					targetOS = currentOS
					targetArch = currentArch
				}

				// Find matching platform manifest
				var selectedDigest string

				for _, m := range index.Manifests {
					// Skip attestations/SBOMs (unknown platform)
					if m.Platform.OS == "unknown" || m.Platform.Architecture == "unknown" {
						continue
					}
					if m.Platform.OS == "" || m.Platform.Architecture == "" {
						continue
					}

					// Try to match the target platform
					if targetOS != "" && targetArch != "" {
						if m.Platform.OS == targetOS && m.Platform.Architecture == targetArch {
							selectedDigest = m.Digest
							break
						}
					}
				}

				// If no match found and platform was specified, return error with available platforms
				if selectedDigest == "" && targetOS != "" && targetArch != "" && platform != "" {
					return nil, "", nil, fmt.Errorf("platform %s/%s not found in index. Available platforms: %s", targetOS, targetArch, strings.Join(availablePlatforms, ", "))
				}

				// Fallback: prefer linux/amd64, then first available
				if selectedDigest == "" {
					for _, m := range index.Manifests {
						if m.Platform.Architecture == "amd64" && m.Platform.OS == "linux" {
							selectedDigest = m.Digest
							break
						}
					}
				}
				if selectedDigest == "" && len(index.Manifests) > 0 {
					// Get first non-attestation manifest
					for _, m := range index.Manifests {
						if m.Platform.OS != "unknown" && m.Platform.Architecture != "unknown" && m.Platform.OS != "" && m.Platform.Architecture != "" {
							selectedDigest = m.Digest
							break
						}
					}
				}

				if selectedDigest != "" {
					// Hand off to FindManifestByDigest, which opens its own reader.
					// The outer reader stays open and is released by the function-scoped
					// defer; closing it here would race with that defer and log a
					// spurious "file already closed" warning.
					platformManifestData, _, _, err := tr.FindManifestByDigest(repoName, selectedDigest)
					if err != nil {
						return nil, "", nil, fmt.Errorf("platform-specific manifest %s not found: %w", selectedDigest, err)
					}

					// Extract blob digests from the platform-specific manifest
					configDigest, layerDigests, err := ExtractBlobDigestsFromManifest(platformManifestData)
					if err != nil {
						return nil, "", nil, fmt.Errorf("failed to parse platform manifest: %w", err)
					}

					// Calculate manifest digest from the index (for reference)
					hash := sha256.Sum256(manifestData)
					manifestDigest := fmt.Sprintf("sha256:%x", hash)

					// Return the platform-specific manifest data, not the index
					// The caller needs the actual manifest with config and layers
					return platformManifestData, manifestDigest, append([]string{configDigest}, layerDigests...), nil
				}
			}
		}

		// Single-platform manifest - extract blob digests directly
		configDigest, layerDigests, err := ExtractBlobDigestsFromManifest(manifestData)
		if err != nil {
			return nil, "", nil, fmt.Errorf("failed to parse manifest: %w", err)
		}

		// Calculate manifest digest
		hash := sha256.Sum256(manifestData)
		manifestDigest := fmt.Sprintf("sha256:%x", hash)

		return manifestData, manifestDigest, append([]string{configDigest}, layerDigests...), nil
	}

	return nil, "", nil, fmt.Errorf("manifest not found: %s:%s", repoName, tag)
}

// FindManifestByDigest finds a manifest by digest, streaming through the tarball
func (tr *TarballReader) FindManifestByDigest(repoName, digest string) ([]byte, string, []string, error) {
	// Extract registry and repo from the full repo name
	registryID, actualRepoName := extractRegistryAndRepo(repoName)

	safeRepo := util.SafeRepoName(actualRepoName)
	// Build manifest path with the correct registry ID
	manifestPath := fmt.Sprintf("docker-manifests/%s/%s/%s", registryID, safeRepo, digest)

	// Build index if not already built
	if !tr.indexBuilt {
		if err := tr.buildSectionIndex(); err != nil {
			return nil, "", nil, fmt.Errorf("failed to build section index: %w", err)
		}
	}

	// Try to use index file offset if available
	var startOffset int64
	if !tr.isGzip {
		if offset, ok := tr.sectionIdx.fileOffsets[manifestPath]; ok {
			startOffset = offset
			tr.debugLog("Found manifest offset in index: %s -> %d", manifestPath, offset)
		} else {
			tr.debugLog("Manifest offset NOT found in index for: %s", manifestPath)
		}
	}

	file, tarReader, err := tr.createTarReader(startOffset)
	if err != nil {
		return nil, "", nil, err
	}
	defer util.DeferClose(file)

	// Stream through tarball looking for the manifest
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", nil, fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Strip cache/ prefix if present (for backwards compatibility with old tarballs)
		// New tarballs don't have cache/ prefix, but old ones might
		path := stripCachePrefix(header.Name)

		// Check if this is our manifest
		if path != manifestPath {
			// Skip this entry's body
			if header.Typeflag == tar.TypeReg {
				if _, err := io.Copy(io.Discard, tarReader); err != nil {
					return nil, "", nil, fmt.Errorf("failed to skip entry: %w", err)
				}
			}
			continue
		}

		// Found it - read manifest data
		if header.Typeflag != tar.TypeReg {
			continue
		}
		// Read manifest data
		manifestData := make([]byte, header.Size)
		if _, err := io.ReadFull(tarReader, manifestData); err != nil {
			return nil, "", nil, fmt.Errorf("failed to read manifest: %w", err)
		}

		// Extract blob digests from manifest
		configDigest, layerDigests, err := ExtractBlobDigestsFromManifest(manifestData)
		if err != nil {
			return nil, "", nil, fmt.Errorf("failed to parse manifest: %w", err)
		}

		// Use provided digest
		return manifestData, digest, append([]string{configDigest}, layerDigests...), nil
	}

	return nil, "", nil, fmt.Errorf("manifest not found: %s@%s", repoName, digest)
}

// StreamBlobs streams specific blobs from the tarball to a writer
// This is memory-efficient as it doesn't load all blobs into memory
func (tr *TarballReader) StreamBlobs(blobDigests map[string]bool, writeBlob func(digest string, data io.Reader, size int64) error) error {
	// Build index if not already built
	if !tr.indexBuilt {
		if err := tr.buildSectionIndex(); err != nil {
			return fmt.Errorf("failed to build section index: %w", err)
		}
	}

	// For non-gzipped, start from blob section if available
	var startOffset int64
	if !tr.isGzip && tr.sectionIdx.blobSectionStart > 0 {
		startOffset = tr.sectionIdx.blobSectionStart
	}

	file, tarReader, err := tr.createTarReader(startOffset)
	if err != nil {
		return err
	}
	defer util.DeferClose(file)

	// Track which blobs we've found
	found := make(map[string]bool)
	for digest := range blobDigests {
		found[digest] = false
	}

	// Stream through tarball looking for needed blobs
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Strip cache/ prefix if present (for backwards compatibility with old tarballs)
		// New tarballs don't have cache/ prefix, but old ones might
		path := stripCachePrefix(header.Name)

		// Check if this is a blob we need
		// Handle both old flat structure (docker-blobs/sha256:...) and new subdirectory structure (docker-blobs/sha256/ab/c1/sha256:...)
		if strings.HasPrefix(path, "docker-blobs/") {
			// Extract digest from path - handle both formats
			var digest string
			pathWithoutPrefix := strings.TrimPrefix(path, "docker-blobs/")

			// Check if it's the new subdirectory format: sha256/ab/c1/sha256:...
			if strings.HasPrefix(pathWithoutPrefix, "sha256/") {
				// New format: docker-blobs/sha256/ab/c1/sha256:...
				// Extract the full digest (sha256:...) from the end of the path
				parts := strings.Split(pathWithoutPrefix, "/")
				if len(parts) >= 3 && strings.HasPrefix(parts[len(parts)-1], "sha256:") {
					digest = parts[len(parts)-1]
				} else {
					// Fallback: try to reconstruct from path
					// Path format: sha256/ab/c1/sha256:abc123...
					// We need the full sha256:... part
					lastPart := parts[len(parts)-1]
					if strings.HasPrefix(lastPart, "sha256:") {
						digest = lastPart
					} else {
						continue // Skip if we can't parse it
					}
				}
			} else {
				// Old format: docker-blobs/sha256:...
				digest = pathWithoutPrefix
			}

			if blobDigests[digest] {
				// Stream this blob to the writer
				if err := writeBlob(digest, tarReader, header.Size); err != nil {
					return fmt.Errorf("failed to write blob %s: %w", digest, err)
				}
				found[digest] = true

				// Check if we've found all blobs
				allFound := true
				for _, f := range found {
					if !f {
						allFound = false
						break
					}
				}
				if allFound {
					return nil // Early exit - found all blobs
				}
				continue
			}
		}

		// Skip this entry's body if we don't need it
		if header.Typeflag == tar.TypeReg {
			if _, err := io.Copy(io.Discard, tarReader); err != nil {
				return fmt.Errorf("failed to skip entry: %w", err)
			}
		}
	}

	// Check if we found all required blobs
	var missing []string
	for digest, f := range found {
		if !f {
			missing = append(missing, digest)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required blobs: %v", missing)
	}

	return nil
}

// stripCachePrefix removes the "cache/" prefix from tarball paths if present
// Kept for backwards compatibility with old tarballs that have cache/ prefix
func stripCachePrefix(path string) string {
	if strings.HasPrefix(path, "cache/") {
		return strings.TrimPrefix(path, "cache/")
	}
	return path
}

// ListManifests returns all manifest paths in the tarball (streaming)
func (tr *TarballReader) ListManifests() ([]string, error) {
	var paths []string

	// Build index if not already built
	if !tr.indexBuilt {
		if err := tr.buildSectionIndex(); err != nil {
			return nil, fmt.Errorf("failed to build section index: %w", err)
		}
	}

	// For non-gzipped, start from manifest section if available
	var startOffset int64
	if !tr.isGzip && tr.sectionIdx.manifestSectionStart > 0 {
		startOffset = tr.sectionIdx.manifestSectionStart
	}

	file, tarReader, err := tr.createTarReader(startOffset)
	if err != nil {
		return nil, err
	}
	defer util.DeferClose(file)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Strip cache/ prefix if present (for backwards compatibility with old tarballs)
		// New tarballs don't have cache/ prefix, but old ones might
		path := stripCachePrefix(header.Name)

		// Index manifests (docker-manifests/...)
		if strings.HasPrefix(path, "docker-manifests/") {
			// Skip cache-internal sidecars: .meta (legacy) and .metadata (current).
			// These live alongside real manifests as <ref>.meta / <ref>.metadata
			// and are not OCI/Docker manifests.
			if header.Typeflag == tar.TypeReg &&
				!strings.HasSuffix(path, ".meta") &&
				!strings.HasSuffix(path, ".metadata") {
				paths = append(paths, path)
			}
		}

		// Don't drain the body: Next() discards the current entry's unread
		// bytes automatically, and since the tarball is non-gzipped (the
		// underlying *os.File is an io.Seeker) it seeks past them instead of
		// reading — so we touch only headers, not the multi-GB of payloads.
	}

	return paths, nil
}

// ListBlobs returns all blob digests in the tarball (streaming)
func (tr *TarballReader) ListBlobs() ([]string, error) {
	var digests []string

	// Build index if not already built
	if !tr.indexBuilt {
		if err := tr.buildSectionIndex(); err != nil {
			return nil, fmt.Errorf("failed to build section index: %w", err)
		}
	}

	// For non-gzipped, start from blob section if available
	var startOffset int64
	if !tr.isGzip && tr.sectionIdx.blobSectionStart > 0 {
		startOffset = tr.sectionIdx.blobSectionStart
	}

	file, tarReader, err := tr.createTarReader(startOffset)
	if err != nil {
		return nil, err
	}
	defer util.DeferClose(file)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Strip cache/ prefix if present (for backwards compatibility with old tarballs)
		// New tarballs don't have cache/ prefix, but old ones might
		path := stripCachePrefix(header.Name)

		// Index blobs (docker-blobs/...)
		if strings.HasPrefix(path, "docker-blobs/") {
			digest := strings.TrimPrefix(path, "docker-blobs/")
			if strings.HasPrefix(digest, "sha256:") {
				digests = append(digests, digest)
			}
		}

		// Don't drain the body: Next() discards the current entry's unread
		// bytes automatically, and since the tarball is non-gzipped (the
		// underlying *os.File is an io.Seeker) it seeks past them instead of
		// reading — so we touch only headers, not the multi-GB of payloads.
	}

	return digests, nil
}

// BlobSizes returns a digest→size map by walking the tarball headers only.
// Bodies are skipped, so this is fast even for multi-GB tarballs and is
// suitable for computing a denominator for byte-based progress reporting.
func (tr *TarballReader) BlobSizes() (map[string]int64, error) {
	sizes := make(map[string]int64)

	if !tr.indexBuilt {
		if err := tr.buildSectionIndex(); err != nil {
			return nil, fmt.Errorf("failed to build section index: %w", err)
		}
	}

	var startOffset int64
	if !tr.isGzip && tr.sectionIdx.blobSectionStart > 0 {
		startOffset = tr.sectionIdx.blobSectionStart
	}

	file, tarReader, err := tr.createTarReader(startOffset)
	if err != nil {
		return nil, err
	}
	defer util.DeferClose(file)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar entry: %w", err)
		}

		path := stripCachePrefix(header.Name)
		if !strings.HasPrefix(path, "docker-blobs/") {
			continue
		}
		// Same digest extraction as StreamBlobs — accept old flat layout and
		// new sha256/<ab>/<cd>/sha256:... subdirectory layout.
		rest := strings.TrimPrefix(path, "docker-blobs/")
		var digest string
		if strings.HasPrefix(rest, "sha256/") {
			parts := strings.Split(rest, "/")
			if len(parts) >= 4 && strings.HasPrefix(parts[len(parts)-1], "sha256:") {
				digest = parts[len(parts)-1]
			}
		} else if strings.HasPrefix(rest, "sha256:") {
			digest = rest
		}
		if digest != "" {
			sizes[digest] = header.Size
		}
		// Don't drain the body: Next() discards the current entry's unread
		// bytes automatically, and since the tarball is non-gzipped (the
		// underlying *os.File is an io.Seeker) it seeks past them instead of
		// reading — so we touch only headers, not the multi-GB of payloads.
	}

	return sizes, nil
}

// getBlobPathForTarball constructs blob paths for tarball lookup
// Tries new subdirectory format first, then falls back to old flat format
func getBlobPathForTarball(digest string) []string {
	var paths []string

	// New format: docker-blobs/sha256/<first2>/<next2>/sha256:...
	if strings.HasPrefix(digest, "sha256:") {
		hashPart := digest[7:] // Skip "sha256:" prefix
		if len(hashPart) >= 4 {
			newPath := fmt.Sprintf("docker-blobs/sha256/%s/%s/%s", hashPart[0:2], hashPart[2:4], digest)
			paths = append(paths, newPath)
		}
	}

	// Old format: docker-blobs/sha256:...
	oldPath := "docker-blobs/" + digest
	paths = append(paths, oldPath)

	// Also try with cache/ prefix (for backwards compatibility)
	var pathsWithCache []string
	for _, p := range paths {
		pathsWithCache = append(pathsWithCache, "cache/"+p)
	}
	paths = append(paths, pathsWithCache...)

	return paths
}

// OpenBlob returns a streaming reader for a single blob's body, seeking to
// it via the section index. Each call opens its own file handle, so it's
// safe to call concurrently from multiple goroutines.
//
// Only works on uncompressed tarballs — gzipped streams need a linear scan
// and aren't worth random-access support.
func (tr *TarballReader) OpenBlob(digest string) (io.ReadCloser, int64, error) {
	if !tr.indexBuilt {
		if err := tr.buildSectionIndex(); err != nil {
			return nil, 0, fmt.Errorf("build section index: %w", err)
		}
	}
	if tr.isGzip {
		return nil, 0, fmt.Errorf("OpenBlob requires an uncompressed tarball")
	}

	var offset int64
	var found bool
	for _, p := range getBlobPathForTarball(digest) {
		if o, ok := tr.sectionIdx.fileOffsets[p]; ok {
			offset = o
			found = true
			break
		}
	}
	if !found {
		return nil, 0, fmt.Errorf("blob %s not in tarball section index", digest)
	}

	file, tarReader, err := tr.createTarReader(offset)
	if err != nil {
		return nil, 0, err
	}

	// The seek lands us at the start of the entry; Next() reads the header
	// and positions tarReader at the start of the body.
	header, err := tarReader.Next()
	if err != nil {
		util.Logerr(file.Close())
		return nil, 0, fmt.Errorf("read blob header at offset %d: %w", offset, err)
	}

	return &blobBodyReader{file: file, tarReader: tarReader}, header.Size, nil
}

// blobBodyReader wraps a tar.Reader so closing also closes the underlying
// file. The tar.Reader itself doesn't own the file handle.
type blobBodyReader struct {
	file      *os.File
	tarReader *tar.Reader
}

func (b *blobBodyReader) Read(p []byte) (int, error) { return b.tarReader.Read(p) }
func (b *blobBodyReader) Close() error               { return b.file.Close() }

// HasBlob checks if a blob exists in the tarball (streaming check)
func (tr *TarballReader) HasBlob(digest string) (bool, error) {
	// Build index if not already built
	if !tr.indexBuilt {
		if err := tr.buildSectionIndex(); err != nil {
			return false, fmt.Errorf("failed to build section index: %w", err)
		}
	}

	// Try to use index file offset if available
	var startOffset int64
	if !tr.isGzip {
		// Try multiple path formats (new subdirectory format first, then old flat format)
		blobPaths := getBlobPathForTarball(digest)
		found := false
		for _, path := range blobPaths {
			if offset, ok := tr.sectionIdx.fileOffsets[path]; ok {
				startOffset = offset
				tr.debugLog("Found blob offset in index: %s -> %d", path, offset)
				found = true
				break
			}
		}

		if !found {
			tr.debugLog("Blob offset NOT found in index for: %s or %s", blobPaths[0], digest)
			// Check if there are any similar keys (for debugging)
			digestShort := digest
			if len(digest) > 20 {
				digestShort = digest[:20]
			}
			matchCount := 0
			for key := range tr.sectionIdx.fileOffsets {
				if strings.Contains(key, digestShort) {
					tr.debugLog("Found similar key in index: %s", key)
					matchCount++
					if matchCount >= 3 {
						break
					}
				}
			}
			if matchCount == 0 {
				// Show a few blob keys from the index to see the pattern
				blobCount := 0
				for key := range tr.sectionIdx.fileOffsets {
					if strings.HasPrefix(key, "docker-blobs/") {
						tr.debugLog("Sample blob key from index: %s", key)
						blobCount++
						if blobCount >= 3 {
							break
						}
					}
				}
			}
		}
	}

	file, tarReader, err := tr.createTarReader(startOffset)
	if err != nil {
		return false, err
	}
	defer util.DeferClose(file)

	// Try all possible blob path formats when streaming
	blobPaths := getBlobPathForTarball(digest)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Strip cache/ prefix if present (for backwards compatibility with old tarballs)
		// New tarballs don't have cache/ prefix, but old ones might
		path := stripCachePrefix(header.Name)

		// Check if this matches any of the possible blob path formats
		for _, possiblePath := range blobPaths {
			if path == possiblePath || path == stripCachePrefix(possiblePath) {
				// Found it - skip the body and return
				if header.Typeflag == tar.TypeReg {
					util.LogerrT(io.Copy(io.Discard, tarReader))
				}
				return true, nil
			}
		}

		// Skip this entry's body
		if header.Typeflag == tar.TypeReg {
			if _, err := io.Copy(io.Discard, tarReader); err != nil {
				return false, fmt.Errorf("failed to skip entry: %w", err)
			}
		}
	}
}

// ExtractBlobDigestsFromManifest parses a manifest and returns all blob digests
// This function expects a single-platform manifest, not an OCI Image Index.
// If an index is passed, it will return an error. Use FindManifestByRepoTag
// which handles indexes by resolving to platform-specific manifests.
func ExtractBlobDigestsFromManifest(manifestData []byte) (string, []string, error) {
	// Quick check: if this looks like an index, return an error
	// (This should not happen if FindManifestByRepoTag is used correctly)
	var quickCheck struct {
		Manifests []interface{} `json:"manifests"`
		Layers    []interface{} `json:"layers"`
	}
	if err := json.Unmarshal(manifestData, &quickCheck); err == nil {
		if len(quickCheck.Manifests) > 0 && len(quickCheck.Layers) == 0 {
			return "", nil, fmt.Errorf("manifest is an OCI Image Index - use FindManifestByRepoTag which handles indexes")
		}
	}

	// Parse as single-platform manifest
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
		return "", nil, fmt.Errorf("failed to parse manifest: %w", err)
	}

	var layerDigests []string
	for _, layer := range manifest.Layers {
		if layer.Digest != "" {
			layerDigests = append(layerDigests, layer.Digest)
		}
	}

	return manifest.Config.Digest, layerDigests, nil
}
