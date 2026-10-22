package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// createTestTarball creates a test tarball with known content and an index
// This simulates the export process to verify offset tracking is correct
func createTestTarball(t *testing.T, tarballPath string) map[string]int64 {
	t.Helper()

	file, err := os.Create(tarballPath)
	if err != nil {
		t.Fatalf("Failed to create test tarball: %v", err)
	}
	// Note: file and tarWriter are explicitly closed later in this function
	// before verification, so we don't use defer here

	tarWriter := tar.NewWriter(file)

	// Track offsets as we write (simulating export process)
	currentOffset := int64(0)
	fileOffsets := make(map[string]int64)

	// Test data: manifests and blobs
	testEntries := []struct {
		path    string
		content string
		isDir   bool
	}{
		{"docker-manifests/", "", true},
		{"docker-manifests/docker.io/", "", true},
		{"docker-manifests/docker.io/library_test/", "", true},
		{"docker-manifests/docker.io/library_test/1.0", `{"config":{"digest":"sha256:config123"},"layers":[{"digest":"sha256:blob1"},{"digest":"sha256:blob2"}]}`, false},
		{"docker-blobs/", "", true},
		{"docker-blobs/sha256:blob1", "blob1 content here", false},
		{"docker-blobs/sha256:blob2", "blob2 content here", false},
		{"docker-blobs/sha256:config123", "config content here", false},
	}

	// Write all entries
	for _, entry := range testEntries {
		// Ensure offset is 512-byte aligned
		if currentOffset%512 != 0 {
			currentOffset = ((currentOffset + 511) / 512) * 512
		}

		// Record offset before writing header
		entryOffset := currentOffset
		fileOffsets[entry.path] = entryOffset

		var header *tar.Header
		if entry.isDir {
			header = &tar.Header{
				Name:     entry.path,
				Typeflag: tar.TypeDir,
				Mode:     0755,
			}
		} else {
			header = &tar.Header{
				Name:     entry.path,
				Size:     int64(len(entry.content)),
				Typeflag: tar.TypeReg,
				Mode:     0644,
			}
		}

		// Write header (always 512 bytes)
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("Failed to write header for %s: %v", entry.path, err)
		}
		currentOffset += 512

		// Write content if not a directory
		if !entry.isDir {
			written, err := tarWriter.Write([]byte(entry.content))
			if err != nil {
				t.Fatalf("Failed to write content for %s: %v", entry.path, err)
			}
			currentOffset += int64(written)
			// Add padding to next 512-byte boundary
			padding := (512 - (int64(written) % 512)) % 512
			if padding > 0 {
				// tar.Writer handles padding automatically, but we need to track it
				currentOffset += padding
			}
		}
	}

	// Write index file at the end
	if currentOffset%512 != 0 {
		currentOffset = ((currentOffset + 511) / 512) * 512
	}

	// Convert byte offsets to block numbers (divide by 512)
	blockOffsets := make(map[string]int32)
	for path, offset := range fileOffsets {
		blockNum := int32(offset / 512)
		blockOffsets[path] = blockNum
	}
	index := map[string]interface{}{
		"version":      2, // Version 2: block numbers
		"file_offsets": blockOffsets,
	}
	indexData, err := json.Marshal(index)
	if err != nil {
		t.Fatalf("Failed to marshal index: %v", err)
	}

	// Compress the index with gzip
	var compressedData bytes.Buffer
	gzWriter := gzip.NewWriter(&compressedData)
	if _, err := gzWriter.Write(indexData); err != nil {
		t.Fatalf("Failed to write to gzip writer: %v", err)
	}
	if err := gzWriter.Close(); err != nil {
		t.Fatalf("Failed to close gzip writer: %v", err)
	}
	compressedBytes := compressedData.Bytes()

	indexHeader := &tar.Header{
		Name:     ".roxxy-index.json.gz",
		Size:     int64(len(compressedBytes)),
		Typeflag: tar.TypeReg,
		Mode:     0644,
	}

	if err := tarWriter.WriteHeader(indexHeader); err != nil {
		t.Fatalf("Failed to write index header: %v", err)
	}
	currentOffset += 512

	if _, err := tarWriter.Write(compressedBytes); err != nil {
		t.Fatalf("Failed to write index data: %v", err)
	}
	currentOffset += int64(len(compressedBytes))
	padding := (512 - (int64(len(indexData)) % 512)) % 512
	if padding > 0 {
		currentOffset += padding
		t.Logf("Added %d bytes of padding to index entry", padding)
	}
	t.Logf("Final tarball offset: %d bytes", currentOffset)

	// Flush and close
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("Failed to close tar writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Failed to close file: %v", err)
	}

	// Verify the index was written by reading the tarball directly
	verifyTarball, err := os.Open(tarballPath)
	if err != nil {
		t.Fatalf("Failed to open tarball for verification: %v", err)
	}
	defer util.DeferClose(verifyTarball)

	tarReader := tar.NewReader(verifyTarball)
	foundIndex := false
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Failed to read tarball: %v", err)
		}
		// Check for both .roxxy-index.json and .roxxy-index.json.gz
		normalizedName := strings.TrimSuffix(header.Name, ".gz")
		if normalizedName == ".roxxy-index.json" {
			foundIndex = true
			indexData := make([]byte, header.Size)
			if _, err := io.ReadFull(tarReader, indexData); err != nil {
				t.Fatalf("Failed to read index data: %v", err)
			}
			// Decompress if gzipped
			if strings.HasSuffix(header.Name, ".gz") {
				gzReader, err := gzip.NewReader(bytes.NewReader(indexData))
				if err != nil {
					t.Fatalf("Failed to create gzip reader: %v", err)
				}
				decompressedData, err := io.ReadAll(gzReader)
				util.Logerr(gzReader.Close())
				if err != nil {
					t.Fatalf("Failed to decompress index: %v", err)
				}
				indexData = decompressedData
			}
			var idx map[string]interface{}
			if err := json.Unmarshal(indexData, &idx); err != nil {
				t.Fatalf("Failed to parse index: %v", err)
			}
			t.Logf("Index found in tarball with %d entries", len(idx["file_offsets"].(map[string]interface{})))
			break
		}
		// Skip entry body
		if header.Typeflag == tar.TypeReg {
			util.LogerrT(io.Copy(io.Discard, tarReader))
		}
	}

	if !foundIndex {
		t.Fatal("Index file not found in tarball!")
	}

	return fileOffsets
}

func TestTarballIndexReading(t *testing.T) {
	tmpDir := t.TempDir()
	tarballPath := filepath.Join(tmpDir, "test.tar")

	// Create test tarball with known offsets
	expectedOffsets := createTestTarball(t, tarballPath)

	// Open and read the tarball
	reader, err := OpenTarball(tarballPath)
	if err != nil {
		t.Fatalf("Failed to open tarball: %v", err)
	}
	defer util.DeferClose(reader)

	// Build the index
	if err := reader.buildSectionIndex(); err != nil {
		t.Fatalf("Failed to build section index: %v", err)
	}

	// Verify all expected offsets are present and correct
	for path, expectedOffset := range expectedOffsets {
		// Skip the index file itself (both .json and .json.gz versions)
		if path == ".roxxy-index.json" || path == ".roxxy-index.json.gz" {
			continue
		}

		actualOffset, found := reader.sectionIdx.fileOffsets[path]
		if !found {
			t.Errorf("Path %s not found in index", path)
			continue
		}

		// Verify offset is 512-byte aligned
		if actualOffset%512 != 0 {
			t.Errorf("Offset for %s is not 512-byte aligned: %d", path, actualOffset)
		}

		// Verify offset matches expected (should be exact match)
		if actualOffset != expectedOffset {
			t.Errorf("Offset mismatch for %s: expected %d, got %d", path, expectedOffset, actualOffset)
		}
	}
}

func TestTarballBlobLookup(t *testing.T) {
	tmpDir := t.TempDir()
	tarballPath := filepath.Join(tmpDir, "test.tar")

	// Create test tarball
	createTestTarball(t, tarballPath)

	// Open and read the tarball
	reader, err := OpenTarball(tarballPath)
	if err != nil {
		t.Fatalf("Failed to open tarball: %v", err)
	}
	defer util.DeferClose(reader)

	// Test blob lookups
	testBlobs := []string{
		"sha256:blob1",
		"sha256:blob2",
		"sha256:config123",
	}

	for _, digest := range testBlobs {
		hasBlob, err := reader.HasBlob(digest)
		if err != nil {
			t.Fatalf("Error checking blob %s: %v", digest, err)
		}
		if !hasBlob {
			t.Errorf("Blob %s not found in tarball", digest)
		}
	}

	// Test non-existent blob
	hasBlob, err := reader.HasBlob("sha256:nonexistent")
	if err != nil {
		t.Fatalf("Error checking non-existent blob: %v", err)
	}
	if hasBlob {
		t.Error("Non-existent blob was found (should not be)")
	}
}

func TestTarballManifestLookup(t *testing.T) {
	tmpDir := t.TempDir()
	tarballPath := filepath.Join(tmpDir, "test.tar")

	// Create test tarball
	createTestTarball(t, tarballPath)

	// Open and read the tarball
	reader, err := OpenTarball(tarballPath)
	if err != nil {
		t.Fatalf("Failed to open tarball: %v", err)
	}
	defer util.DeferClose(reader)

	// Test manifest lookup by repo:tag
	manifestData, manifestDigest, blobDigests, err := reader.FindManifestByRepoTag("docker.io/library/test", "1.0", "")
	if err != nil {
		t.Fatalf("Failed to find manifest: %v", err)
	}

	if manifestData == nil {
		t.Error("Manifest data is nil")
	}

	if manifestDigest == "" {
		t.Error("Manifest digest is empty")
	}

	// Verify blob digests are extracted correctly
	expectedBlobs := []string{"sha256:config123", "sha256:blob1", "sha256:blob2"}
	if len(blobDigests) != len(expectedBlobs) {
		t.Errorf("Expected %d blob digests, got %d", len(expectedBlobs), len(blobDigests))
	}

	// Verify all expected blobs are present
	for _, expectedBlob := range expectedBlobs {
		found := false
		for _, actualBlob := range blobDigests {
			if actualBlob == expectedBlob {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected blob %s not found in manifest digests", expectedBlob)
		}
	}
}

func TestTarballOffsetAlignment(t *testing.T) {
	tmpDir := t.TempDir()
	tarballPath := filepath.Join(tmpDir, "test.tar")

	// Create test tarball
	expectedOffsets := createTestTarball(t, tarballPath)

	// Open and read the tarball
	reader, err := OpenTarball(tarballPath)
	if err != nil {
		t.Fatalf("Failed to open tarball: %v", err)
	}
	defer util.DeferClose(reader)

	// Build the index
	if err := reader.buildSectionIndex(); err != nil {
		t.Fatalf("Failed to build section index: %v", err)
	}

	// Verify all offsets are 512-byte aligned
	for path, offset := range reader.sectionIdx.fileOffsets {
		if offset%512 != 0 {
			t.Errorf("Offset for %s is not 512-byte aligned: %d (remainder: %d)", path, offset, offset%512)
		}
	}

	// Verify we can seek to each offset and read the correct entry
	for path, offset := range expectedOffsets {
		// Skip directories and the index file (both .json and .json.gz versions)
		if path == ".roxxy-index.json" || path == ".roxxy-index.json.gz" || strings.HasSuffix(path, "/") {
			continue
		}

		file, tarReader, err := reader.createTarReader(offset)
		if err != nil {
			t.Fatalf("Failed to create tar reader at offset %d for %s: %v", offset, path, err)
		}
		defer util.DeferClose(file)

		// Read the entry
		header, err := tarReader.Next()
		if err != nil {
			t.Fatalf("Failed to read entry at offset %d for %s: %v", offset, path, err)
		}

		// Verify the path matches
		actualPath := stripCachePrefix(header.Name)
		if actualPath != path {
			t.Errorf("Path mismatch at offset %d: expected %s, got %s", offset, path, actualPath)
		}
	}
}

func TestTarballStreamBlobs(t *testing.T) {
	tmpDir := t.TempDir()
	tarballPath := filepath.Join(tmpDir, "test.tar")

	// Create test tarball
	createTestTarball(t, tarballPath)

	// Open and read the tarball
	reader, err := OpenTarball(tarballPath)
	if err != nil {
		t.Fatalf("Failed to open tarball: %v", err)
	}
	defer util.DeferClose(reader)

	// Test streaming specific blobs
	blobDigests := map[string]bool{
		"sha256:blob1": true,
		"sha256:blob2": true,
	}

	receivedBlobs := make(map[string][]byte)

	err = reader.StreamBlobs(blobDigests, func(digest string, data io.Reader, size int64) error {
		content, err := io.ReadAll(data)
		if err != nil {
			return err
		}
		receivedBlobs[digest] = content
		return nil
	})

	if err != nil {
		t.Fatalf("Failed to stream blobs: %v", err)
	}

	// Verify we received the expected blobs
	if len(receivedBlobs) != 2 {
		t.Errorf("Expected 2 blobs, got %d", len(receivedBlobs))
	}

	if string(receivedBlobs["sha256:blob1"]) != "blob1 content here" {
		t.Errorf("Blob1 content mismatch: got %q", string(receivedBlobs["sha256:blob1"]))
	}

	if string(receivedBlobs["sha256:blob2"]) != "blob2 content here" {
		t.Errorf("Blob2 content mismatch: got %q", string(receivedBlobs["sha256:blob2"]))
	}
}

func TestTarballIndexAtEnd(t *testing.T) {
	tmpDir := t.TempDir()
	tarballPath := filepath.Join(tmpDir, "test.tar")

	// Create test tarball
	createTestTarball(t, tarballPath)

	// Open the tarball file directly to verify index is at the end
	file, err := os.Open(tarballPath)
	if err != nil {
		t.Fatalf("Failed to open tarball: %v", err)
	}
	defer util.DeferClose(file)

	// Get file size
	fileInfo, err := file.Stat()
	if err != nil {
		t.Fatalf("Failed to stat file: %v", err)
	}
	fileSize := fileInfo.Size()

	// Read the last 64KB to find the index
	tailSize := int64(64 * 1024)
	if tailSize > fileSize {
		tailSize = fileSize
	}

	tail := make([]byte, tailSize)
	if _, err := file.Seek(fileSize-tailSize, io.SeekStart); err != nil {
		t.Fatalf("Failed to seek to tail: %v", err)
	}

	if _, err := io.ReadFull(file, tail); err != nil {
		t.Fatalf("Failed to read tail: %v", err)
	}

	// Look for index file header in the tail
	foundIndex := false
	for i := len(tail) - 512; i >= 0; i -= 512 {
		if i+512 > len(tail) {
			continue
		}

		// Try to parse as tar header
		headerBytes := tail[i : i+512]
		// Check if name field contains ".roxxy-index.json"
		nameBytes := headerBytes[0:100]
		nameEnd := 0
		for j, b := range nameBytes {
			if b == 0 {
				nameEnd = j
				break
			}
		}
		if nameEnd > 0 {
			name := string(nameBytes[:nameEnd])
			// Check for both .roxxy-index.json and .roxxy-index.json.gz
			normalizedName := strings.TrimSuffix(name, ".gz")
			if normalizedName == ".roxxy-index.json" {
				foundIndex = true
				break
			}
		}
	}

	if !foundIndex {
		t.Error("Index file not found in tail of tarball")
	}
}
