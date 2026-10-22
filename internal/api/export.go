package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/archive"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// countingWriter tracks the byte count of everything that passes
// through it. Used by walkAndAddToTar to learn the actual current tar
// position rather than trusting hand-rolled `currentOffset += 512`
// arithmetic, which silently miscounts under PAX extended headers
// (Format=PAX + Size > ~8GB OR long file name): WriteHeader emits 1024
// bytes, not 512, and every subsequent index entry lands 1024 bytes
// behind reality. roar then seeks mid-padding and fails with
// "invalid tar header". Snap counter.n at each capture site instead.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// handleExportFull exports the entire cache
func (a *API) handleExportFull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.handleExport(w, r, "full")
}

// handleExportDocker exports only Docker-related cache
func (a *API) handleExportDocker(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.handleExport(w, r, "docker")
}

// handleExportArchive exports Docker cache plus locally-cached plain files (db dumps, etc.)
func (a *API) handleExportArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.handleExport(w, r, "archive")
}

// handleExportPackages exports only package manager artifacts
func (a *API) handleExportPackages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.handleExport(w, r, "packages")
}

// handleExportRaw exports only raw file cache
func (a *API) handleExportRaw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.handleExport(w, r, "raw")
}

// exportFilter prunes orphan (untagged) manifests and their blobs from
// docker/archive exports. Built once per request by walking docker-manifests/
// before streaming: tagged manifests + manifests pointed to by tagged OCI
// indexes form the keep-set, along with the blobs those manifests reference.
// active=false means no orphan filtering — used for full/packages/raw exports.
type exportFilter struct {
	active        bool
	keepManifests map[string]bool // relPath under docker-manifests/, base name only (no .meta/.metadata)
	keepBlobs     map[string]bool // hash without "sha256:" prefix
}

// newDockerExportFilter builds the keep-set for docker/archive exports.
// For other export types it returns an inactive filter (passes everything).
func newDockerExportFilter(cacheDir, exportType string) (*exportFilter, error) {
	f := &exportFilter{}
	if exportType != "docker" && exportType != "archive" {
		return f, nil
	}
	f.active = true
	f.keepManifests = make(map[string]bool)
	f.keepBlobs = make(map[string]bool)

	manifestDir := filepath.Join(cacheDir, "docker-manifests")
	type entry struct {
		relPath string // docker-manifests/<reg>/<safe-repo>/<ref>, slash-form
		repoDir string // docker-manifests/<reg>/<safe-repo>
		ref     string // basename (tag or sha256:...)
		path    string // absolute on-disk path
	}
	var all []entry
	err := filepath.Walk(manifestDir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return nil
		}
		name := info.Name()
		if strings.HasSuffix(name, ".meta") || strings.HasSuffix(name, ".metadata") {
			return nil
		}
		rel, relErr := filepath.Rel(cacheDir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		all = append(all, entry{
			relPath: rel,
			repoDir: filepath.ToSlash(filepath.Dir(rel)),
			ref:     name,
			path:    path,
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("walk manifests: %w", err)
	}

	byRepoDir := make(map[string]map[string]entry) // repoDir -> ref -> entry
	for _, e := range all {
		if byRepoDir[e.repoDir] == nil {
			byRepoDir[e.repoDir] = make(map[string]entry)
		}
		byRepoDir[e.repoDir][e.ref] = e
	}

	// Seed the queue with tagged manifests (reference is a tag, not a digest).
	var queue []entry
	for _, e := range all {
		if strings.HasPrefix(e.ref, "sha256:") {
			continue
		}
		f.keepManifests[e.relPath] = true
		queue = append(queue, e)
	}

	// Walk the queue: image manifests contribute blobs; OCI indexes /
	// manifest lists drag in their per-platform sub-manifests (which live
	// alongside as docker-manifests/<reg>/<safe-repo>/sha256:<digest>).
	visited := make(map[string]bool)
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		if visited[e.relPath] {
			continue
		}
		visited[e.relPath] = true

		body, rerr := os.ReadFile(e.path)
		if rerr != nil {
			continue
		}
		for _, sub := range exportExtractSubManifestDigests(body) {
			if sibling, ok := byRepoDir[e.repoDir][sub]; ok {
				if !f.keepManifests[sibling.relPath] {
					f.keepManifests[sibling.relPath] = true
					queue = append(queue, sibling)
				}
			}
		}
		for _, dg := range exportExtractManifestBlobDigests(body) {
			if hash, ok := strings.CutPrefix(dg, "sha256:"); ok {
				f.keepBlobs[hash] = true
			}
		}
	}

	log.Printf("Export: docker filter built — %d manifests kept, %d blobs kept (type=%s)",
		len(f.keepManifests), len(f.keepBlobs), exportType)
	return f, nil
}

// allow returns true if a path passes orphan filtering. Inactive filters
// (full/packages/raw exports) always pass.
func (f *exportFilter) allow(relPath string, isDir bool) bool {
	if !f.active {
		return true
	}
	// Always descend directories so we can reach the files we want to keep.
	if isDir {
		return true
	}
	switch {
	case strings.HasPrefix(relPath, "docker-manifests/"):
		base := relPath
		switch {
		case strings.HasSuffix(base, ".meta"):
			base = strings.TrimSuffix(base, ".meta")
		case strings.HasSuffix(base, ".metadata"):
			base = strings.TrimSuffix(base, ".metadata")
		}
		return f.keepManifests[base]
	case strings.HasPrefix(relPath, "docker-blobs/"):
		base := filepath.Base(relPath)
		if hash, ok := strings.CutPrefix(base, "sha256:"); ok {
			return f.keepBlobs[hash]
		}
		return true
	case strings.HasPrefix(relPath, "refcount/"):
		// Refcount files are named by the blob hash (no "sha256:" prefix).
		// Drop refcounts for blobs we're dropping; otherwise the importer
		// would see dangling refcount entries.
		return f.keepBlobs[filepath.Base(relPath)]
	}
	return true
}

// exportExtractSubManifestDigests mirrors cache.extractSubManifestDigests
// without dragging in the cache package. Returns digests referenced from
// an OCI image index or Docker manifest list; empty for image manifests.
func exportExtractSubManifestDigests(body []byte) []string {
	var idx struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil
	}
	if idx.MediaType != "application/vnd.oci.image.index.v1+json" &&
		idx.MediaType != "application/vnd.docker.distribution.manifest.list.v2+json" {
		return nil
	}
	out := make([]string, 0, len(idx.Manifests))
	for _, m := range idx.Manifests {
		if m.Digest != "" {
			out = append(out, m.Digest)
		}
	}
	return out
}

// exportExtractManifestBlobDigests mirrors cache.extractManifestBlobDigests:
// config + layer digests from a single-platform manifest.
func exportExtractManifestBlobDigests(body []byte) []string {
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	digests := make([]string, 0, 1+len(m.Layers))
	if m.Config.Digest != "" {
		digests = append(digests, m.Config.Digest)
	}
	for _, l := range m.Layers {
		if l.Digest != "" {
			digests = append(digests, l.Digest)
		}
	}
	return digests
}

// emitOCILayouts wipes <cacheDir>/archive/oci-layouts and rebuilds it with
// one hardlinked OCI image-layout per tagged manifest in the export's
// keep-set. Idempotent re-runs are fine — wipe always happens first.
//
// We walk the *same* manifest set the filter already enumerated, so the
// layouts and the tarball describe the same content by construction. The
// filter's keep set is computed by following indexes to their per-platform
// sub-manifests, so emitting one layout per *tagged* ref (skipping
// digest-only manifests pulled in as index children) captures every
// shipped image without duplication.
func emitOCILayouts(cacheDir string, filter *exportFilter) error {
	if err := archive.WipeLayouts(cacheDir); err != nil {
		return err
	}
	if filter == nil || !filter.active || len(filter.keepManifests) == 0 {
		return nil
	}
	emitter := archive.NewLayoutEmitter(cacheDir)
	manifestDir := filepath.Join(cacheDir, "docker-manifests")
	for relPath := range filter.keepManifests {
		// relPath: docker-manifests/<registryID>/<safeRepo>/<reference>.
		// Skip digest-only refs (index children) — those get materialized
		// transitively when we emit their parent index's layout.
		rest := strings.TrimPrefix(relPath, "docker-manifests/")
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) != 3 {
			continue
		}
		registryID, safeRepo, ref := parts[0], parts[1], parts[2]
		if strings.HasPrefix(ref, "sha256:") {
			continue
		}
		manifestPath := filepath.Join(manifestDir, registryID, safeRepo, ref)
		body, err := os.ReadFile(manifestPath)
		if err != nil {
			log.Printf("Export: skip layout for %s/%s/%s: %v", registryID, safeRepo, ref, err)
			continue
		}
		if err := emitter.EmitRef(registryID, safeRepo, ref, body); err != nil {
			log.Printf("Export: layout emit failed for %s/%s/%s: %v", registryID, safeRepo, ref, err)
			continue
		}
	}
	rep := emitter.Report()
	log.Printf("Export: oci-layouts emitted — %d refs, %d blobs hardlinked, %d copied, %d missed, %d errors",
		rep.Refs, rep.BlobsLinked, rep.BlobsCopied, rep.BlobsMissed, rep.Errors)
	return nil
}

// handleExport handles the actual export logic
func (a *API) handleExport(w http.ResponseWriter, r *http.Request, exportType string) {
	// Log the request immediately and flush to ensure it appears in logs
	log.Printf("Export API: %s %s (type: %s) from %s", r.Method, r.URL.Path, exportType, r.RemoteAddr)
	// Force flush log output (if using a buffered logger)
	if flusher, ok := w.(http.Flusher); ok {
		// This ensures logs are visible even if the request fails early
		_ = flusher
	}

	// Validate cache directory before starting
	if a.cacheDir == "" {
		log.Printf("Export failed: cache directory not set")
		http.Error(w, "Cache directory not configured", http.StatusInternalServerError)
		return
	}

	// Check if cache directory exists
	if _, err := os.Stat(a.cacheDir); os.IsNotExist(err) {
		log.Printf("Export failed: cache directory does not exist: %s", a.cacheDir)
		http.Error(w, fmt.Sprintf("Cache directory not found: %s", a.cacheDir), http.StatusNotFound)
		return
	}

	// Diagnostic: list what's actually in the cache directory
	log.Printf("Export: Cache directory contents check for type %s:", exportType)
	if entries, err := os.ReadDir(a.cacheDir); err == nil {
		log.Printf("Export: Cache dir %s contains %d top-level entries", a.cacheDir, len(entries))
		for i, entry := range entries {
			if i < 20 {
				info, _ := entry.Info()
				log.Printf("Export:   - %s (dir=%v, size=%d)", entry.Name(), entry.IsDir(), info.Size())
			}
		}
		// Check specifically for docker-blobs
		for _, entry := range entries {
			if entry.Name() == "docker-blobs" {
				blobPath := filepath.Join(a.cacheDir, "docker-blobs")
				if blobEntries, err := os.ReadDir(blobPath); err == nil {
					log.Printf("Export: docker-blobs directory contains %d entries", len(blobEntries))
					for i, blobEntry := range blobEntries {
						if i < 10 {
							log.Printf("Export:   docker-blobs/%s (dir=%v)", blobEntry.Name(), blobEntry.IsDir())
						}
					}
				}
				break
			}
		}
	} else {
		log.Printf("Export: ERROR - failed to read cache directory: %v", err)
	}

	// Check if compression is requested (default: no compression for Docker blobs)
	// Docker blobs are already compressed, so gzip adds overhead without benefit
	compress := r.URL.Query().Get("compress") == "true"

	// Build the orphan filter once. For docker/archive exports this drops
	// untagged manifests (and blobs/refcounts only they referenced); for
	// other types it's a no-op pass-through.
	filter, err := newDockerExportFilter(a.cacheDir, exportType)
	if err != nil {
		log.Printf("Export failed: error building filter: %v", err)
		http.Error(w, fmt.Sprintf("Error building export filter: %v", err), http.StatusInternalServerError)
		return
	}

	// For docker/archive exports, materialize a hardlinked OCI image-layout
	// tree under <cacheDir>/archive/oci-layouts/ before streaming the tar.
	// Wipe + rebuild gives SBOM tooling a read-in-place view that is
	// content-addressed identical to what we're about to put in the
	// tarball (same digests, same inodes). See docs/PROVENANCE.md.
	//
	// Done before any tarball bytes hit the wire so a layout-build failure
	// can still fail the request cleanly with a 500 instead of leaving a
	// half-streamed body the client can't recover from.
	if exportType == "docker" || exportType == "archive" {
		if err := emitOCILayouts(a.cacheDir, filter); err != nil {
			log.Printf("Export failed: oci-layout emission: %v", err)
			http.Error(w, fmt.Sprintf("OCI layout emission failed: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Quick check: verify there are files to export before setting headers
	// This prevents sending headers if there's nothing to export
	hasFiles, err := a.hasFilesToExport(exportType, filter)
	if err != nil {
		log.Printf("Export failed: error checking for files: %v", err)
		http.Error(w, fmt.Sprintf("Error checking cache: %v", err), http.StatusInternalServerError)
		return
	}
	if !hasFiles {
		log.Printf("Export failed: no files found for export type %s in cache dir %s", exportType, a.cacheDir)
		http.Error(w, fmt.Sprintf("No files found to export for type: %s", exportType), http.StatusNotFound)
		return
	}

	// Set headers
	var filename string
	if compress {
		filename = fmt.Sprintf("roxxy-cache-%s.tar.gz", exportType)
		w.Header().Set("Content-Type", "application/gzip")
	} else {
		filename = fmt.Sprintf("roxxy-cache-%s.tar", exportType)
		w.Header().Set("Content-Type", "application/x-tar")
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))

	var tarWriter *tar.Writer
	var gzipWriter *gzip.Writer

	// Counter sits immediately below the tar.Writer; counter.n is the
	// canonical "current tar position". See countingWriter type doc
	// for why we don't trust the manual +=512 scheme.
	counter := &countingWriter{}
	if compress {
		// Create gzip writer (streams to response)
		gzipWriter = gzip.NewWriter(w)
		defer util.DeferClose(gzipWriter)
		counter.w = gzipWriter
		tarWriter = tar.NewWriter(counter)
	} else {
		// Write tar directly (no compression)
		counter.w = w
		tarWriter = tar.NewWriter(counter)
	}
	defer util.DeferClose(tarWriter)

	// Normalize cache directory path (resolve symlinks, make absolute)
	absCacheDir, err := filepath.Abs(a.cacheDir)
	if err != nil {
		log.Printf("Export failed: error resolving cache directory path %s: %v", a.cacheDir, err)
		http.Error(w, fmt.Sprintf("Error resolving cache directory: %v", err), http.StatusInternalServerError)
		return
	}
	absCacheDir = filepath.Clean(absCacheDir)
	log.Printf("Export: Using cache directory: %s (normalized from %s)", absCacheDir, a.cacheDir)

	// Walk cache directory and stream files
	if err := a.walkAndAddToTar(tarWriter, counter, absCacheDir, exportType, compress, filter); err != nil {
		log.Printf("Export failed during walk: %v", err)
		// Try to close writers gracefully even on error
		// This ensures the tarball is at least valid (even if incomplete)
		if closeErr := tarWriter.Close(); closeErr != nil {
			log.Printf("Error closing tar writer after failure: %v", closeErr)
		}
		if gzipWriter != nil {
			if closeErr := gzipWriter.Close(); closeErr != nil {
				log.Printf("Error closing gzip writer after failure: %v", closeErr)
			}
		}
		// Note: Can't change status code after streaming started
		// The client will receive a partial/invalid tarball
		return
	}

	// Flush tar writer (this writes the final tar EOF markers)
	if err := tarWriter.Close(); err != nil {
		log.Printf("Error closing tar writer: %v", err)
		return
	}

	// Flush gzip writer if compression was used
	if gzipWriter != nil {
		if err := gzipWriter.Close(); err != nil {
			log.Printf("Error closing gzip writer: %v", err)
			return
		}
	}

	log.Printf("Export completed successfully for type %s", exportType)
}

// hasFilesToExport quickly checks if there are any files matching the export type
// This is used to validate before setting HTTP headers
func (a *API) hasFilesToExport(exportType string, filter *exportFilter) (bool, error) {
	// Normalize cache directory path (resolve symlinks, make absolute)
	absCacheDir, err := filepath.Abs(a.cacheDir)
	if err != nil {
		return false, fmt.Errorf("error resolving cache directory path %s: %w", a.cacheDir, err)
	}
	absCacheDir = filepath.Clean(absCacheDir)

	// Quick check: look for at least one matching file/directory
	found := false
	err = filepath.Walk(absCacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Continue on errors
		}
		if found {
			return filepath.SkipAll // Already found one, stop searching
		}

		relPath, err := filepath.Rel(absCacheDir, path)
		if err != nil {
			return nil
		}

		if relPath == "." {
			return nil
		}

		included := shouldIncludePath(relPath, exportType) && filter.allow(relPath, info.IsDir())
		if included && !info.IsDir() {
			// Require at least one real file — directories alone aren't worth
			// streaming a tarball for, especially after orphan filtering.
			found = true
			return filepath.SkipAll
		}

		// If it's a directory that wouldn't pass the type whitelist, skip it.
		// (Don't skip on the filter alone: orphan-filtered dirs may still
		// contain kept files we want to descend into.)
		if info.IsDir() && !shouldIncludePath(relPath, exportType) {
			return filepath.SkipDir
		}

		return nil
	})
	return found, err
}

// shouldIncludePath determines if a path should be included in the export based on export type
func shouldIncludePath(relPath string, exportType string) bool {
	switch exportType {
	case "full":
		return true // Include everything

	case "docker":
		// Check if path is the directory itself or starts with the directory path
		// Exclude index/ and access-stats.json as they can be regenerated
		return relPath == "docker-blobs" ||
			strings.HasPrefix(relPath, "docker-blobs/") ||
			relPath == "docker-manifests" ||
			strings.HasPrefix(relPath, "docker-manifests/") ||
			relPath == "refcount" ||
			strings.HasPrefix(relPath, "refcount/")

	case "archive":
		// Docker images + locally-cached plain files (db dumps, seeds, etc.)
		return relPath == "docker-blobs" ||
			strings.HasPrefix(relPath, "docker-blobs/") ||
			relPath == "docker-manifests" ||
			strings.HasPrefix(relPath, "docker-manifests/") ||
			relPath == "content" ||
			strings.HasPrefix(relPath, "content/") ||
			relPath == "url" ||
			relPath == "url/localhost" ||
			strings.HasPrefix(relPath, "url/localhost/") ||
			relPath == "refcount" ||
			strings.HasPrefix(relPath, "refcount/")

	case "packages":
		// Exclude index/ and access-stats.json as they can be regenerated
		return relPath == "packages" ||
			strings.HasPrefix(relPath, "packages/") ||
			relPath == "refcount" ||
			strings.HasPrefix(relPath, "refcount/")

	case "raw":
		// Exclude index/ and access-stats.json as they can be regenerated
		return relPath == "raw" ||
			strings.HasPrefix(relPath, "raw/") ||
			relPath == "refcount" ||
			strings.HasPrefix(relPath, "refcount/")

	default:
		return false
	}
}

// walkAndAddToTar walks the cache directory and adds files to the tar archive
// Files are written in a specific order to optimize tarball reading:
// 1. Directories first
// 2. Manifests (files that reference other files) before blobs (leaf files)
// 3. Within each category, alphabetical order
func (a *API) walkAndAddToTar(tarWriter *tar.Writer, counter *countingWriter, cacheDir string, exportType string, compress bool, filter *exportFilter) error {
	// Collect all entries first, then sort and write in order
	type tarEntry struct {
		path          string
		relPath       string
		info          os.FileInfo
		isDir         bool
		isManifest    bool
		isIndex       bool // OCI Image Index (multi-platform)
		isManifestDir bool // Directory that contains manifests
		isBlobDir     bool // Directory that contains blobs
	}

	var entries []tarEntry

	// First pass: collect all entries
	log.Printf("Export: Starting filepath.Walk on cacheDir: %s", cacheDir)
	err := filepath.Walk(cacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Log but continue - some files may not be accessible
			log.Printf("Export: Error accessing %s: %v", path, err)
			return nil
		}

		// Calculate relative path from cache directory
		relPath, err := filepath.Rel(cacheDir, path)
		if err != nil {
			log.Printf("Export: Warning - failed to calculate relative path for %s (from %s): %v (skipping)", path, cacheDir, err)
			// Don't fail entire export if one path calculation fails
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip the root directory itself
		if relPath == "." {
			return nil
		}

		// Debug: log first 20 paths we encounter
		if len(entries) < 20 {
			log.Printf("Export: Walk found: %s (relPath: %s, isDir: %v, size: %d)", path, relPath, info.IsDir(), info.Size())
		}

		// Check if this path should be included
		shouldInclude := shouldIncludePath(relPath, exportType)
		if !shouldInclude {
			// If it's a directory, skip it entirely
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Apply orphan filter: drop untagged manifests and the blobs /
		// refcounts only they referenced. Directories always pass so we
		// keep descending into them.
		if !filter.allow(relPath, info.IsDir()) {
			return nil
		}

		// Debug: log what we're including (first few entries, and all blobs)
		if len(entries) < 10 || strings.HasPrefix(relPath, "docker-blobs/") {
			log.Printf("Export: Including %s (dir=%v)", relPath, info.IsDir())
		}

		// Determine if this is a manifest (references other files)
		isManifest := strings.HasPrefix(relPath, "docker-manifests/") &&
			!strings.HasSuffix(relPath, ".meta") &&
			!info.IsDir()

		// Check if this is an OCI Image Index (multi-platform manifest)
		// We need to peek at the file to determine if it's an index
		isIndex := false
		if isManifest {
			// Quick check: read first few bytes to see if it contains "manifests" array
			// This is a heuristic - indexes have "manifests" field, single-platform have "layers"
			if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
				// Check if it's JSON and contains "manifests" (index) vs "layers" (single-platform)
				dataStr := string(data)
				if strings.Contains(dataStr, `"manifests"`) && !strings.Contains(dataStr, `"layers"`) {
					isIndex = true
				}
			}
		}

		// Determine if this is a manifest directory
		// Note: Blobs are stored flat in docker-blobs/ (no subdirectories), so isBlobDir is only true for docker-blobs itself
		isManifestDir := info.IsDir() && strings.HasPrefix(relPath, "docker-manifests/")
		isBlobDir := info.IsDir() && (relPath == "docker-blobs" || strings.HasPrefix(relPath, "docker-blobs/"))

		entries = append(entries, tarEntry{
			path:          path,
			relPath:       relPath,
			info:          info,
			isDir:         info.IsDir(),
			isManifest:    isManifest,
			isIndex:       isIndex,
			isManifestDir: isManifestDir,
			isBlobDir:     isBlobDir,
		})

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to walk cache directory: %w", err)
	}

	log.Printf("Export: Collected %d entries for export type %s (cache dir: %s)", len(entries), exportType, cacheDir)

	// Count directories vs files for debugging
	dirCount := 0
	fileCount := 0
	for _, entry := range entries {
		if entry.isDir {
			dirCount++
		} else {
			fileCount++
		}
	}
	log.Printf("Export: Breakdown - %d directories, %d files", dirCount, fileCount)

	if len(entries) == 0 {
		log.Printf("Export: No entries found! Cache dir: %s, export type: %s", cacheDir, exportType)
		// List what's actually in the cache directory for debugging
		if entries, err := os.ReadDir(cacheDir); err == nil {
			log.Printf("Export: Cache directory contains: %d entries", len(entries))
			for i, entry := range entries {
				if i < 10 {
					log.Printf("Export:   - %s (dir=%v)", entry.Name(), entry.IsDir())
				}
			}
		}
		return fmt.Errorf("no files found to export for type %s", exportType)
	}

	// Sort entries with proper ordering using priority:
	// Priority 1: Manifest directories (docker-manifests/)
	// Priority 2: Other directories
	// Priority 3: Blob directories (docker-blobs/) - after other dirs but before files
	// Priority 4: Multi-platform manifests (OCI Image Index) - come first
	// Priority 5: Single-platform manifests - come after indexes
	// Priority 6: Blob files
	// Priority 7: Everything else
	// Within same priority, sort alphabetically
	getPriority := func(e tarEntry) int {
		if e.isManifestDir {
			return 1
		}
		if e.isDir && !e.isBlobDir {
			return 2
		}
		if e.isBlobDir {
			return 3
		}
		if e.isIndex {
			return 4 // Multi-platform indexes come first
		}
		if e.isManifest {
			return 5 // Single-platform manifests come after indexes
		}
		if strings.HasPrefix(e.relPath, "docker-blobs/") {
			return 6
		}
		return 7
	}

	sort.Slice(entries, func(i, j int) bool {
		ei, ej := entries[i], entries[j]
		pi, pj := getPriority(ei), getPriority(ej)

		if pi != pj {
			return pi < pj
		}

		// Same priority - sort alphabetically
		return ei.relPath < ej.relPath
	})

	// Track current position in the tarball (for offset calculation)
	// Start at 0, all tar entries are 512-byte aligned
	currentOffset := int64(0)
	log.Printf("Export: Starting offset tracking at position 0")

	// Track offsets for index file (only for non-compressed exports)
	// For HTTP streaming, we can't write the index at the end, but we can still track
	fileOffsets := make(map[string]int64)

	// Second pass: write entries in sorted order
	var filesWritten, dirsWritten int
	for _, entry := range entries {
		// Create tar header
		header, err := tar.FileInfoHeader(entry.info, "")
		if err != nil {
			return fmt.Errorf("failed to create tar header for %s: %w", entry.path, err)
		}

		// Set the name in the archive (use relPath directly, no cache/ prefix)
		header.Name = entry.relPath

		// Record offset BEFORE writing header - this is where the tar entry starts.
		// Flush pending padding from the previous entry then read the byte
		// counter — gives the exact next-entry offset. The previous
		// `currentOffset += 512` scheme silently miscounted under PAX
		// extended headers (long file name OR Size > ~8GB): WriteHeader
		// emits 1024 bytes not 512, so every subsequent entry's recorded
		// offset trailed reality by 1024 bytes and roar landed mid-blob.
		_ = tarWriter.Flush()
		entryOffset := counter.n
		currentOffset = entryOffset
		log.Printf("Export: [%s] Recording offset BEFORE WriteHeader: %d", entry.relPath, entryOffset)

		// Write header - tar headers are always 512 bytes
		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("failed to write tar header for %s: %w", entry.path, err)
		}
		// Update tracked offset - WriteHeader writes exactly 512 bytes
		currentOffset += 512
		log.Printf("Export: [%s] After WriteHeader: offset updated to %d (wrote 512 bytes)", entry.relPath, currentOffset)

		// Record offset for this entry (the position where the header starts)
		fileOffsets[entry.relPath] = entryOffset
		log.Printf("Export: [%s] Stored offset %d in fileOffsets map", entry.relPath, entryOffset)

		// If it's a directory, we're done
		if entry.isDir {
			dirsWritten++
			log.Printf("Export: [%s] Directory entry complete, final offset: %d", entry.relPath, currentOffset)
			continue
		}

		// Open and copy file contents
		file, err := os.Open(entry.path)
		if err != nil {
			log.Printf("Export: ERROR - failed to open file %s: %v (skipping this file, continuing export)", entry.path, err)
			// Check if file still exists
			if _, statErr := os.Stat(entry.path); os.IsNotExist(statErr) {
				log.Printf("Export: File %s does not exist (may have been deleted)", entry.path)
			} else if statErr != nil {
				log.Printf("Export: File %s stat error: %v", entry.path, statErr)
			}
			continue // Continue with other files - don't fail entire export
		}

		// Get file size for logging
		fileInfo, _ := file.Stat()
		fileSize := fileInfo.Size()
		log.Printf("Export: [%s] Starting file copy, file size: %d bytes, offset before copy: %d", entry.relPath, fileSize, currentOffset)

		// Copy file contents to tar
		// tar.Writer automatically handles padding to 512-byte boundaries
		written, err := io.Copy(tarWriter, file)
		if err != nil {
			util.Logerr(file.Close())
			log.Printf("Export: Error writing file %s to tar: %v (stopping export)", entry.path, err)
			return fmt.Errorf("failed to write file %s to tar: %w", entry.path, err)
		}
		util.Logerr(file.Close())

		// Flush after each file to ensure data is written (especially important for HTTP streaming)
		if err := tarWriter.Flush(); err != nil {
			log.Printf("Export: Error flushing tar writer after %s: %v (stopping export)", entry.relPath, err)
			return fmt.Errorf("failed to flush tar writer: %w", err)
		}

		// Update tracked offset: add data written, then padding to next 512-byte boundary
		// Tar format pads file data to 512-byte boundaries
		currentOffset += written
		padding := (512 - (written % 512)) % 512
		if padding > 0 {
			currentOffset += padding
		}
		log.Printf("Export: [%s] After file copy: wrote %d bytes, padding: %d bytes, offset updated to %d (512-byte aligned)",
			entry.relPath, written, padding, currentOffset)

		filesWritten++
		log.Printf("Export: [%s] File entry complete, final tracked offset: %d", entry.relPath, currentOffset)
	}

	// Write index file at the end (for all exports, including HTTP streaming)
	// The index allows fast seeking when reading the tarball later
	if len(fileOffsets) > 0 {
		// Ensure current offset is 512-byte aligned before writing index header
		if currentOffset%512 != 0 {
			currentOffset = ((currentOffset + 511) / 512) * 512
		}

		log.Printf("Export: Writing index file, current tracked offset: %d, %d entries to index", currentOffset, len(fileOffsets))
		// Convert byte offsets to block numbers (divide by 512) to save space
		// Block numbers are stored as 32-bit integers, supporting tarballs up to 2TB
		blockOffsets := make(map[string]int32)
		for path, offset := range fileOffsets {
			// Convert byte offset to block number (offset / 512)
			// Offsets are already 512-byte aligned, so this is exact
			blockNum := int32(offset / 512)
			blockOffsets[path] = blockNum
		}
		index := map[string]interface{}{
			"version":      2, // Version 2: block numbers instead of byte offsets
			"file_offsets": blockOffsets,
		}
		indexData, err := json.Marshal(index)
		if err == nil {
			log.Printf("Export: Index JSON size: %d bytes (uncompressed)", len(indexData))

			// Compress the index with gzip
			var compressedData bytes.Buffer
			gzWriter := gzip.NewWriter(&compressedData)
			if _, err := gzWriter.Write(indexData); err == nil {
				if err := gzWriter.Close(); err == nil {
					compressedBytes := compressedData.Bytes()
					log.Printf("Export: Index compressed size: %d bytes (%.1f%% reduction)",
						len(compressedBytes), 100.0*(1.0-float64(len(compressedBytes))/float64(len(indexData))))
					indexData = compressedBytes
				}
			}

			// Write index file entry
			indexHeader := &tar.Header{
				Name:     ".roxxy-index.json.gz",
				Size:     int64(len(indexData)),
				Mode:     0644,
				ModTime:  time.Now(),
				Typeflag: tar.TypeReg,
			}
			indexOffsetBeforeHeader := currentOffset
			log.Printf("Export: Writing index header at offset %d (512-byte aligned)", indexOffsetBeforeHeader)
			if err := tarWriter.WriteHeader(indexHeader); err == nil {
				currentOffset += 512 // Header size
				log.Printf("Export: After index WriteHeader: offset updated to %d", currentOffset)
				if _, err := tarWriter.Write(indexData); err == nil {
					currentOffset += int64(len(indexData))
					// Add padding to next 512-byte boundary
					padding := (512 - (int64(len(indexData)) % 512)) % 512
					if padding > 0 {
						currentOffset += padding
					}
					log.Printf("Export: After index data write: wrote %d bytes, padding: %d bytes, offset updated to %d (512-byte aligned)",
						len(indexData), padding, currentOffset)
					log.Printf("Export: Wrote index file with %d entries, final tracked offset: %d", len(fileOffsets), currentOffset)

					// Log a sample of the offsets stored in the index for debugging
					sampleCount := 0
					blobCount := 0
					blobPaths := []string{}
					for path, offset := range fileOffsets {
						if sampleCount < 5 {
							log.Printf("Export: Index sample - %s -> offset %d", path, offset)
							sampleCount++
						}
						if strings.HasPrefix(path, "docker-blobs/") && !strings.HasSuffix(path, "/") {
							blobCount++
							blobPaths = append(blobPaths, path)
							if blobCount <= 10 {
								log.Printf("Export: Blob in index - %s -> offset %d", path, offset)
							}
						}
					}
					log.Printf("Export: Total blob entries in index: %d", blobCount)
					if blobCount > 0 {
						log.Printf("Export: First blob: %s, Last blob: %s", blobPaths[0], blobPaths[len(blobPaths)-1])
					}
				} else {
					log.Printf("Export: Error writing index data: %v", err)
				}
			} else {
				log.Printf("Export: Error writing index header: %v", err)
			}
		} else {
			log.Printf("Export: Error marshaling index: %v", err)
		}
	} else {
		log.Printf("Export: No file offsets to write to index")
	}

	log.Printf("Export: Wrote %d directories and %d files", dirsWritten, filesWritten)
	return nil
}
