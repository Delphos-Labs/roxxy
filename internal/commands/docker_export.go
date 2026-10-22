package commands

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/cli"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// exportSink owns an export's destination (a real file or stdout) plus the tar
// writer layered over it, and centralizes the two things every exporter must
// get right: (1) on ANY failure after the file is created, remove the partial
// output so a truncated tarball is never left behind to be mistaken for a good
// one; (2) on success, flush the tar trailer + fsync the file and check those
// errors BEFORE printing "Successfully exported". A close error after a
// half-written body is a real failure, not a log-only afterthought.
//
// stdout is special: it can't be removed or re-opened, so partial-output
// cleanup is skipped for it (the caller already streamed bytes downstream).
type exportSink struct {
	file     *os.File
	tw       *tar.Writer
	path     string // destination path; "" when writing to stdout
	toStdout bool
}

// newExportSink opens the destination (creating the file when not stdout) and
// wraps it in a tar.Writer. On a create error it prints and exits — there's no
// partial file to clean up yet.
func newExportSink(outputFile string, toStdout bool) *exportSink {
	s := &exportSink{toStdout: toStdout}
	if toStdout {
		s.file = os.Stdout
	} else {
		f, err := os.Create(outputFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating output file: %v\n", err)
			os.Exit(1)
		}
		s.file = f
		s.path = outputFile
	}
	s.tw = tar.NewWriter(s.file)
	return s
}

// fail prints the error, removes the partial output file (unless writing to
// stdout), and exits non-zero. Use for every error after the sink is created.
func (s *exportSink) fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
	s.removePartial()
	os.Exit(1)
}

func (s *exportSink) removePartial() {
	if s.toStdout || s.path == "" {
		return
	}
	util.DeferRemove(s.path)
}

// finish flushes the tar trailer and (for a real file) fsyncs + closes. On any
// failure it removes the partial output and exits non-zero; it returns only when
// the output is durably complete, so the caller may print success immediately
// after. The error-bearing core is finishErr, separated so the failure path is
// assertable without intercepting os.Exit.
func (s *exportSink) finish() {
	if err := s.finishErr(); err != nil {
		s.fail("Error: %v\n", err)
	}
}

// finishErr flushes + fsyncs + closes, checking each step, and returns the first
// error (nil only on durable completion). A tar Close / Sync / Close failure here
// is exactly the truncated-tarball-as-success bug the exportSink guards against.
func (s *exportSink) finishErr() error {
	if err := s.tw.Close(); err != nil {
		return fmt.Errorf("finalizing tarball: %w", err)
	}
	if s.toStdout {
		return nil
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("flushing output file: %w", err)
	}
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("closing output file: %w", err)
	}
	return nil
}

// ExportDockerImageToOCITarball exports a Docker image as an OCI tarball
// This creates the OCI image layout format
func ExportDockerImageToOCITarball(cfg Config, imageSpec, outputFile, tarballPath, platform string) {
	// Parse image spec: repo:tag
	var repoName, tag string
	if strings.Contains(imageSpec, ":") {
		parts := strings.SplitN(imageSpec, ":", 2)
		repoName = parts[0]
		tag = parts[1]
	} else {
		fmt.Fprintf(os.Stderr, "Error: Image specification must include tag: %s (expected repo:tag)\n", imageSpec)
		os.Exit(1)
	}

	// Normalize repo name (e.g., "golang" -> "library/golang")
	if !strings.Contains(repoName, "/") {
		repoName = "library/" + repoName
	}

	reader, err := cli.OpenTarball(tarballPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening tarball: %v\n", err)
		os.Exit(1)
	}
	defer util.DeferClose(reader)

	// Find manifest (with platform support for multi-platform images)
	manifestData, _, blobDigests, err := reader.FindManifestByRepoTag(repoName, tag, platform)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error finding manifest: %v\n", err)
		os.Exit(1)
	}

	// Determine output filename
	writeToStdout := outputFile == "-"
	if !writeToStdout && outputFile == "" {
		safeName := strings.ReplaceAll(repoName, "/", "_")
		outputFile = fmt.Sprintf("%s_%s-oci.tar", safeName, tag)
	}

	if writeToStdout {
		fmt.Fprintf(os.Stderr, "Creating OCI tarball (stdout)\n")
	} else {
		fmt.Fprintf(os.Stderr, "Creating OCI tarball: %s\n", outputFile)
	}

	// Create output file or use stdout
	sink := newExportSink(outputFile, writeToStdout)
	tw := sink.tw

	// Write oci-layout file
	ociLayout := `{"imageLayoutVersion":"1.0.0"}`
	hdr := &tar.Header{
		Name:    "oci-layout",
		Size:    int64(len(ociLayout)),
		Mode:    0644,
		ModTime: time.Now(),
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		sink.fail("Error writing oci-layout: %v\n", err)
	}
	if _, err := tw.Write([]byte(ociLayout)); err != nil {
		sink.fail("Error writing oci-layout: %v\n", err)
	}

	// Calculate manifest digest
	hash := sha256.Sum256(manifestData)
	manifestDigest := fmt.Sprintf("sha256:%x", hash)

	// Write index.json (OCI image index)
	// For now, create a simple index pointing to the manifest
	index := map[string]interface{}{
		"schemaVersion": 2,
		"manifests": []map[string]interface{}{
			{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest":    manifestDigest,
				"size":      len(manifestData),
			},
		},
	}
	indexData, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		sink.fail("Error creating index.json: %v\n", err)
	}

	hdr = &tar.Header{
		Name:    "index.json",
		Size:    int64(len(indexData)),
		Mode:    0644,
		ModTime: time.Now(),
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		sink.fail("Error writing index.json header: %v\n", err)
	}
	if _, err := tw.Write(indexData); err != nil {
		sink.fail("Error writing index.json: %v\n", err)
	}

	// Write manifest as a blob first
	manifestDigestPart := strings.TrimPrefix(manifestDigest, "sha256:")
	manifestBlobPath := fmt.Sprintf("blobs/sha256/%s", manifestDigestPart)
	hdr = &tar.Header{
		Name:    manifestBlobPath,
		Size:    int64(len(manifestData)),
		Mode:    0644,
		ModTime: time.Now(),
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		sink.fail("Error writing manifest blob header: %v\n", err)
	}
	if _, err := tw.Write(manifestData); err != nil {
		sink.fail("Error writing manifest blob: %v\n", err)
	}
	fmt.Fprintf(os.Stderr, "  Added manifest blob: %s\n", manifestDigest[:19])

	// Write all blobs to blobs/sha256/ directory
	blobSet := make(map[string]bool)
	for _, digest := range blobDigests {
		blobSet[digest] = true
	}

	blobIndex := 0
	err = reader.StreamBlobs(blobSet, func(digest string, data io.Reader, size int64) error {
		blobIndex++
		fmt.Fprintf(os.Stderr, "  [%d/%d] Adding blob %s (%s)...", blobIndex, len(blobDigests), digest[:12], formatBytes(size))

		// OCI format: blobs/sha256/<digest without sha256:>
		digestPart := strings.TrimPrefix(digest, "sha256:")
		blobPath := fmt.Sprintf("blobs/sha256/%s", digestPart)

		if err := writeTarEntryStream(tw, blobPath, size, data, 0644); err != nil {
			return fmt.Errorf("failed to write blob: %w", err)
		}

		fmt.Fprintf(os.Stderr, " done\n")
		return nil
	})

	if err != nil {
		sink.fail("Error streaming blobs: %v\n", err)
	}

	// Write optional metadata JSON at the end (after all OCI-required files)
	// This metadata helps identify the source and export details
	metadata := map[string]interface{}{
		"exportedAt":    time.Now().Format(time.RFC3339),
		"exportedBy":    "roxxy",
		"imageCount":    1,
		"images":        []string{imageSpec},
		"sourceTarball": tarballPath,
		"format":        "oci",
		"version":       "1.0.0",
	}
	metadataData, err := json.MarshalIndent(metadata, "", "  ")
	if err == nil {
		// Write metadata.json (optional, doesn't affect OCI compliance)
		hdr := &tar.Header{
			Name:    "metadata.json",
			Size:    int64(len(metadataData)),
			Mode:    0644,
			ModTime: time.Now(),
			Format:  tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err == nil {
			if _, err := tw.Write(metadataData); err == nil {
				fmt.Fprintf(os.Stderr, "  Added metadata.json\n")
			}
		}
		// Note: Errors writing metadata are non-fatal - OCI format is complete without it
	}

	sink.finish()
	if writeToStdout {
		fmt.Fprintf(os.Stderr, "Successfully exported %s:%s to stdout (OCI format)\n", repoName, tag)
	} else {
		fmt.Fprintf(os.Stderr, "Successfully exported %s:%s to %s (OCI format)\n", repoName, tag, outputFile)
	}
}

// ExportMultipleDockerImagesToOCITarball exports multiple Docker images as a single OCI tarball
// This creates an OCI image index with multiple manifest references
func ExportMultipleDockerImagesToOCITarball(cfg Config, imageSpecs []string, outputFile, tarballPath, platform string) {
	reader, err := cli.OpenTarball(tarballPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening tarball: %v\n", err)
		os.Exit(1)
	}
	defer util.DeferClose(reader)

	// Determine output filename
	// For multiple images, default to stdout if no file specified
	writeToStdout := outputFile == "-" || (outputFile == "" && len(imageSpecs) > 1)
	if !writeToStdout && outputFile == "" {
		outputFile = "images-oci.tar"
	}

	if writeToStdout {
		fmt.Fprintf(os.Stderr, "Creating OCI tarball with %d image(s) (stdout)\n", len(imageSpecs))
	} else {
		fmt.Fprintf(os.Stderr, "Creating OCI tarball with %d image(s): %s\n", len(imageSpecs), outputFile)
	}

	// Create output file or use stdout
	sink := newExportSink(outputFile, writeToStdout)
	tw := sink.tw

	// Write oci-layout file
	ociLayout := `{"imageLayoutVersion":"1.0.0"}`
	hdr := &tar.Header{
		Name:    "oci-layout",
		Size:    int64(len(ociLayout)),
		Mode:    0644,
		ModTime: time.Now(),
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		sink.fail("Error writing oci-layout: %v\n", err)
	}
	if _, err := tw.Write([]byte(ociLayout)); err != nil {
		sink.fail("Error writing oci-layout: %v\n", err)
	}

	// Collect all manifests and their blobs
	type manifestInfo struct {
		Digest      string
		Size        int
		MediaType   string
		BlobDigests []string
		Data        []byte
		Annotations map[string]string // OCI annotations for image reference
	}

	var manifests []manifestInfo
	allBlobDigests := make(map[string]bool)

	// Process each image
	for i, imageSpec := range imageSpecs {
		// Parse image spec: repo:tag
		var repoName, tag string
		if strings.Contains(imageSpec, ":") {
			parts := strings.SplitN(imageSpec, ":", 2)
			repoName = parts[0]
			tag = parts[1]
		} else {
			sink.fail("Error: Image specification must include tag: %s (expected repo:tag)\n", imageSpec)
		}

		// Store original image reference for annotations
		originalRef := imageSpec

		// Normalize repo name (e.g., "golang" -> "library/golang")
		if !strings.Contains(repoName, "/") {
			repoName = "library/" + repoName
		}

		fmt.Fprintf(os.Stderr, "  [%d/%d] Processing %s:%s...\n", i+1, len(imageSpecs), repoName, tag)

		// Find manifest
		manifestData, contentType, blobDigests, err := reader.FindManifestByRepoTag(repoName, tag, platform)
		if err != nil {
			sink.fail("Error finding manifest for %s:%s: %v\n", repoName, tag, err)
		}

		// Calculate manifest digest
		hash := sha256.Sum256(manifestData)
		manifestDigest := fmt.Sprintf("sha256:%x", hash)

		// Determine media type
		mediaType := contentType
		if mediaType == "" {
			mediaType = "application/vnd.oci.image.manifest.v1+json"
		}

		// Add annotations to preserve original image reference
		// This helps tools identify which registry/namespace each image came from
		annotations := map[string]string{
			"org.opencontainers.image.ref.name": originalRef,
		}

		manifests = append(manifests, manifestInfo{
			Digest:      manifestDigest,
			Size:        len(manifestData),
			MediaType:   mediaType,
			BlobDigests: blobDigests,
			Data:        manifestData,
			Annotations: annotations,
		})

		// Collect all blob digests (deduplicated)
		// Note: We don't add manifestDigest here because manifests are stored in docker-manifests/
		// not docker-blobs/, and we're already writing them separately above
		for _, digest := range blobDigests {
			allBlobDigests[digest] = true
		}

		fmt.Fprintf(os.Stderr, "    Found manifest: %s (%d blobs)\n", manifestDigest[:19], len(blobDigests))
	}

	// Write all manifests as blobs
	fmt.Fprintf(os.Stderr, "Writing %d manifest(s)...\n", len(manifests))
	for _, m := range manifests {
		manifestDigestPart := strings.TrimPrefix(m.Digest, "sha256:")
		manifestBlobPath := fmt.Sprintf("blobs/sha256/%s", manifestDigestPart)
		hdr = &tar.Header{
			Name:    manifestBlobPath,
			Size:    int64(m.Size),
			Mode:    0644,
			ModTime: time.Now(),
			Format:  tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			sink.fail("Error writing manifest blob header: %v\n", err)
		}
		if _, err := tw.Write(m.Data); err != nil {
			sink.fail("Error writing manifest blob: %v\n", err)
		}
	}

	// Write index.json with all manifests
	indexManifests := make([]map[string]interface{}, 0, len(manifests))
	for _, m := range manifests {
		manifestEntry := map[string]interface{}{
			"mediaType": m.MediaType,
			"digest":    m.Digest,
			"size":      m.Size,
		}
		// Add annotations if present (helps tools identify original image references)
		if len(m.Annotations) > 0 {
			manifestEntry["annotations"] = m.Annotations
		}
		indexManifests = append(indexManifests, manifestEntry)
	}

	index := map[string]interface{}{
		"schemaVersion": 2,
		"manifests":     indexManifests,
	}
	indexData, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		sink.fail("Error creating index.json: %v\n", err)
	}

	hdr = &tar.Header{
		Name:    "index.json",
		Size:    int64(len(indexData)),
		Mode:    0644,
		ModTime: time.Now(),
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		sink.fail("Error writing index.json header: %v\n", err)
	}
	if _, err := tw.Write(indexData); err != nil {
		sink.fail("Error writing index.json: %v\n", err)
	}

	// Write all blobs to blobs/sha256/ directory
	blobSet := make(map[string]bool)
	for digest := range allBlobDigests {
		blobSet[digest] = true
	}

	blobIndex := 0
	totalBlobs := len(blobSet)
	fmt.Fprintf(os.Stderr, "Writing %d blob(s)...\n", totalBlobs)
	err = reader.StreamBlobs(blobSet, func(digest string, data io.Reader, size int64) error {
		blobIndex++
		fmt.Fprintf(os.Stderr, "  [%d/%d] Adding blob %s (%s)...", blobIndex, totalBlobs, digest[:12], formatBytes(size))

		// OCI format: blobs/sha256/<digest without sha256:>
		digestPart := strings.TrimPrefix(digest, "sha256:")
		blobPath := fmt.Sprintf("blobs/sha256/%s", digestPart)

		if err := writeTarEntryStream(tw, blobPath, size, data, 0644); err != nil {
			return fmt.Errorf("failed to write blob: %w", err)
		}

		fmt.Fprintf(os.Stderr, " done\n")
		return nil
	})

	if err != nil {
		sink.fail("Error streaming blobs: %v\n", err)
	}

	// Write optional metadata JSON at the end (after all OCI-required files)
	// This metadata helps identify the source and export details
	metadata := map[string]interface{}{
		"exportedAt":    time.Now().Format(time.RFC3339),
		"exportedBy":    "roxxy",
		"imageCount":    len(imageSpecs),
		"images":        imageSpecs,
		"sourceTarball": tarballPath,
		"format":        "oci",
		"version":       "1.0.0",
	}
	metadataData, err := json.MarshalIndent(metadata, "", "  ")
	if err == nil {
		// Write metadata.json (optional, doesn't affect OCI compliance)
		hdr := &tar.Header{
			Name:    "metadata.json",
			Size:    int64(len(metadataData)),
			Mode:    0644,
			ModTime: time.Now(),
			Format:  tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err == nil {
			if _, err := tw.Write(metadataData); err == nil {
				fmt.Fprintf(os.Stderr, "  Added metadata.json\n")
			}
		}
		// Note: Errors writing metadata are non-fatal - OCI format is complete without it
	}

	sink.finish()
	if writeToStdout {
		fmt.Fprintf(os.Stderr, "Successfully exported %d image(s) to stdout (OCI format)\n", len(imageSpecs))
	} else {
		fmt.Fprintf(os.Stderr, "Successfully exported %d image(s) to %s (OCI format)\n", len(imageSpecs), outputFile)
	}
}

// ExportDockerImageToDockerTarball exports a Docker image as a Docker tarball (compatible with docker load)
// This creates the format used by `docker save` and loadable with `docker load`
func ExportDockerImageToDockerTarball(cfg Config, imageSpec, outputFile, tarballPath string) {
	// Parse image spec: repo:tag
	var repoName, tag string
	if strings.Contains(imageSpec, ":") {
		parts := strings.SplitN(imageSpec, ":", 2)
		repoName = parts[0]
		tag = parts[1]
	} else {
		fmt.Fprintf(os.Stderr, "Error: Image specification must include tag: %s (expected repo:tag)\n", imageSpec)
		os.Exit(1)
	}

	// Normalize repo name (e.g., "golang" -> "library/golang")
	if !strings.Contains(repoName, "/") {
		repoName = "library/" + repoName
	}

	var reader *cli.TarballReader

	// Open tarball if provided, otherwise use cache
	if tarballPath != "" {
		fmt.Fprintf(os.Stderr, "Exporting image %s:%s from tarball %s...\n", repoName, tag, tarballPath)
		var err error
		reader, err = cli.OpenTarball(tarballPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening tarball: %v\n", err)
			os.Exit(1)
		}
		defer util.DeferClose(reader)
	} else {
		fmt.Fprintf(os.Stderr, "Exporting image %s:%s from cache...\n", repoName, tag)
		// For cache-based export, use the existing function
		exportDockerImage(cfg.CacheDir, imageSpec, outputFile)
		return
	}

	// Find manifest
	manifestData, _, blobDigests, err := reader.FindManifestByRepoTag(repoName, tag, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error finding manifest: %v\n", err)
		os.Exit(1)
	}

	// Parse manifest to extract config and layers
	var manifest struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		Config        struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"layers"`
	}

	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing manifest: %v\n", err)
		os.Exit(1)
	}

	// Determine output filename
	writeToStdout := outputFile == "-" || outputFile == ""
	if writeToStdout && outputFile != "-" {
		safeName := strings.ReplaceAll(repoName, "/", "_")
		outputFile = fmt.Sprintf("%s_%s.tar", safeName, tag)
		writeToStdout = false
	}

	if writeToStdout {
		fmt.Fprintf(os.Stderr, "Creating Docker tarball (stdout)\n")
	} else {
		fmt.Fprintf(os.Stderr, "Creating Docker tarball: %s\n", outputFile)
	}

	// Create output file or use stdout
	sink := newExportSink(outputFile, writeToStdout)
	tw := sink.tw

	// Collect layer information for manifest.json
	type layerInfo struct {
		Digest string
		Size   int64
		Path   string
	}

	layers := make([]layerInfo, 0, len(manifest.Layers))
	configDigest := manifest.Config.Digest

	// Stream blobs and write them to tarball
	blobSet := make(map[string]bool)
	for _, digest := range blobDigests {
		blobSet[digest] = true
	}

	var configData []byte

	blobIndex := 0
	err = reader.StreamBlobs(blobSet, func(digest string, data io.Reader, size int64) error {
		blobIndex++
		fmt.Fprintf(os.Stderr, "  [%d/%d] Adding blob %s (%s)...", blobIndex, len(blobSet), digest[:12], formatBytes(size))

		digestFilename := strings.TrimPrefix(digest, "sha256:")
		isConfig := digest == configDigest

		// Docker tarball layout: <digest>/json for config, <digest>/layer.tar for layers
		var blobPath string
		if isConfig {
			blobPath = digestFilename + "/json"
		} else {
			blobPath = digestFilename + "/layer.tar"
		}

		// Tar entries require Size up front. The config blob is small (typically <100KB)
		// and we need its bytes later to derive the image ID — buffer it. Layers stream.
		hdr := &tar.Header{
			Name:    blobPath,
			Size:    size,
			Mode:    0644,
			ModTime: getModTime(),
			Format:  tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("failed to write blob header: %w", err)
		}

		if isConfig {
			buf, err := io.ReadAll(data)
			if err != nil {
				return fmt.Errorf("failed to read config blob: %w", err)
			}
			if int64(len(buf)) != size {
				return fmt.Errorf("short config blob: expected %d bytes, got %d", size, len(buf))
			}
			if _, err := tw.Write(buf); err != nil {
				return fmt.Errorf("failed to write config blob: %w", err)
			}
			configData = buf
		} else {
			n, err := io.Copy(tw, data)
			if err != nil {
				return fmt.Errorf("failed to write layer blob: %w", err)
			}
			if n != size {
				return fmt.Errorf("short layer blob: expected %d bytes, wrote %d", size, n)
			}
			layers = append(layers, layerInfo{
				Digest: digest,
				Size:   size,
				Path:   blobPath,
			})
		}

		fmt.Fprintf(os.Stderr, " done\n")
		return nil
	})

	if err != nil {
		sink.fail("Error streaming blobs: %v\n", err)
	}

	if configData == nil {
		sink.fail("Error: config blob not found\n")
	}

	// Parse config to get image ID and metadata
	var configJSON map[string]interface{}
	if err := json.Unmarshal(configData, &configJSON); err != nil {
		sink.fail("Error parsing config: %v\n", err)
	}

	// Extract image ID from config (config digest without sha256: prefix)
	imageID := strings.TrimPrefix(configDigest, "sha256:")

	// Build manifest.json structure
	// Docker tarball format: manifest.json is an array of image entries
	manifestEntry := map[string]interface{}{
		"Config":   strings.TrimPrefix(configDigest, "sha256:") + "/json",
		"RepoTags": []string{repoName + ":" + tag},
		"Layers":   make([]string, 0, len(layers)),
	}

	// Add layer paths
	for _, layer := range layers {
		manifestEntry["Layers"] = append(manifestEntry["Layers"].([]string), layer.Path)
	}

	manifestJSON := []map[string]interface{}{manifestEntry}
	manifestDataBytes, err := json.MarshalIndent(manifestJSON, "", "  ")
	if err != nil {
		sink.fail("Error creating manifest.json: %v\n", err)
	}

	// Write manifest.json
	hdr := &tar.Header{
		Name:    "manifest.json",
		Size:    int64(len(manifestDataBytes)),
		Mode:    0644,
		ModTime: getModTime(),
		Format:  tar.FormatPAX,
	}

	if err := tw.WriteHeader(hdr); err != nil {
		sink.fail("Error writing manifest.json header: %v\n", err)
	}

	if _, err := tw.Write(manifestDataBytes); err != nil {
		sink.fail("Error writing manifest.json: %v\n", err)
	}

	// Write repositories file (legacy format, optional but some tools expect it)
	repositories := map[string]map[string]string{
		repoName: {
			tag: imageID,
		},
	}

	repositoriesData, err := json.MarshalIndent(repositories, "", "  ")
	if err != nil {
		sink.fail("Error creating repositories: %v\n", err)
	}

	hdr = &tar.Header{
		Name:    "repositories",
		Size:    int64(len(repositoriesData)),
		Mode:    0644,
		ModTime: getModTime(),
		Format:  tar.FormatPAX,
	}

	if err := tw.WriteHeader(hdr); err != nil {
		sink.fail("Error writing repositories header: %v\n", err)
	}

	if _, err := tw.Write(repositoriesData); err != nil {
		sink.fail("Error writing repositories: %v\n", err)
	}

	sink.finish()
	if writeToStdout {
		fmt.Fprintf(os.Stderr, "Successfully exported %s:%s to stdout\n", repoName, tag)
		fmt.Fprintf(os.Stderr, "You can load this with: docker load -i -\n")
	} else {
		fmt.Fprintf(os.Stderr, "Successfully exported %s:%s to %s\n", repoName, tag, outputFile)
		fmt.Fprintf(os.Stderr, "You can load this with: docker load -i %s\n", outputFile)
	}
}

// getModTime returns a consistent modification time for tarball entries
func getModTime() time.Time {
	// Use a fixed time for reproducibility
	return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
}
