package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LayoutsRoot is the on-disk root under which per-ref OCI image-layout
// directories live: <cacheDir>/archive/oci-layouts/. SBOM tooling mounts
// this volume read-only and scans oci-dir:<root>/<registry>/<safeRepo>/<tag>.
func LayoutsRoot(cacheDir string) string {
	return filepath.Join(cacheDir, "archive", "oci-layouts")
}

// WipeLayouts removes the entire oci-layouts tree. Called at the start of
// every archive export so the layouts set is *exclusive* to the export
// underway: nothing stale from a prior run, nothing extra. This is the
// "by-construction exclusivity" the provenance attestation relies on.
func WipeLayouts(cacheDir string) error {
	root := LayoutsRoot(cacheDir)
	if err := os.RemoveAll(root); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("wipe oci-layouts %s: %w", root, err)
	}
	return nil
}

// LayoutEmitter materializes per-ref OCI image-layout directories from a
// roxxy cache. Built once per export with NewLayoutEmitter, then EmitRef is
// called per tagged ref the export includes. Not safe for concurrent use
// across goroutines — call from a single walker.
type LayoutEmitter struct {
	cacheDir string
	report   EmitReport
}

// EmitReport summarizes an emitter's work across all calls.
type EmitReport struct {
	Refs        int // # of tagged refs successfully emitted
	BlobsLinked int // # of blob/manifest files hardlinked
	BlobsCopied int // # of blob/manifest files written as copies (cross-vol etc.)
	BlobsMissed int // # of referenced blobs absent from the cache (skipped)
	Errors      int // # of materialize errors (logged, not fatal per blob)
}

// NewLayoutEmitter returns an emitter rooted at cacheDir. The caller is
// expected to have called WipeLayouts(cacheDir) already.
func NewLayoutEmitter(cacheDir string) *LayoutEmitter {
	return &LayoutEmitter{cacheDir: cacheDir}
}

// Report returns aggregate counters since construction.
func (e *LayoutEmitter) Report() EmitReport { return e.report }

// EmitRef builds one OCI image-layout directory:
//
//	<cacheDir>/archive/oci-layouts/<registryID>/<safeRepo>/<tag>/
//	├── oci-layout                — {"imageLayoutVersion":"1.0.0"}
//	├── index.json                — descriptor list pointing to <manifestBody>
//	└── blobs/sha256/<hash>       — manifest body, every sub-manifest (if index),
//	                                and every config + layer blob in the closure.
//
// Blob files are HARDLINKED from existing cache storage where the source
// lives on the same filesystem:
//
//   - manifest bodies hardlink from docker-manifests/<reg>/<safeRepo>/sha256:<hash>
//   - config/layer blobs hardlink from docker-blobs/sha256/<aa>/<bb>/sha256:<hash>
//
// Same-volume hardlinks are metadata-only — shared layers across many images
// cost a single set of inodes. Cross-volume / EXDEV failures fall through
// to a regular file write so the layout still completes.
//
// A blob referenced by the manifest closure but absent from the cache is
// logged and skipped; the resulting layout will have a hole in
// blobs/sha256/, which SBOM tooling will surface as an explicit missing
// component. We prefer that to silently emitting an incomplete layout that
// claims completeness.
func (e *LayoutEmitter) EmitRef(registryID, safeRepo, tag string, manifestBody []byte) error {
	layoutDir := filepath.Join(LayoutsRoot(e.cacheDir), registryID, safeRepo, tag)
	blobsDir := filepath.Join(layoutDir, "blobs", "sha256")
	if err := os.MkdirAll(blobsDir, 0755); err != nil {
		return fmt.Errorf("mkdir layout %s: %w", layoutDir, err)
	}

	if err := os.WriteFile(
		filepath.Join(layoutDir, "oci-layout"),
		[]byte(`{"imageLayoutVersion":"1.0.0"}`), 0644); err != nil {
		return fmt.Errorf("write oci-layout marker: %w", err)
	}

	mediaType, err := e.materialize(registryID, safeRepo, blobsDir, manifestBody, true)
	if err != nil {
		return fmt.Errorf("materialize closure: %w", err)
	}

	sum := sha256.Sum256(manifestBody)
	manifestDigest := "sha256:" + hex.EncodeToString(sum[:])
	index := map[string]any{
		"schemaVersion": 2,
		"manifests": []map[string]any{{
			"mediaType":   mediaType,
			"digest":      manifestDigest,
			"size":        len(manifestBody),
			"annotations": map[string]string{"org.opencontainers.image.ref.name": tag},
		}},
	}
	indexBytes, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal index.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(layoutDir, "index.json"), indexBytes, 0644); err != nil {
		return fmt.Errorf("write index.json: %w", err)
	}

	e.report.Refs++
	return nil
}

// materialize writes body to blobs/sha256/<hash> (hardlink-first) and, if
// it's a manifest, recurses on referenced sub-manifests / config / layers.
// Returns the body's declared mediaType for the caller's index.json. Blob
// files that are already present in blobsDir are treated as a cache hit and
// not re-linked (idempotent within one EmitRef call).
func (e *LayoutEmitter) materialize(registryID, safeRepo, blobsDir string, body []byte, isManifest bool) (string, error) {
	sum := sha256.Sum256(body)
	hashHex := hex.EncodeToString(sum[:])
	digest := "sha256:" + hashHex
	targetPath := filepath.Join(blobsDir, hashHex)

	if _, err := os.Stat(targetPath); err == nil {
		// Already present in this layout — common when an index references
		// the same sub-manifest twice, or two refs share a base layer that
		// the caller chose to put in the same layoutDir (we don't, but
		// idempotence here is cheap and useful for future callers).
		return extractMediaType(body), nil
	}

	// Try hardlinking from cache storage. Manifest bodies and blob bodies
	// live in different trees, so try each.
	sources := make([]string, 0, 2)
	if isManifest {
		sources = append(sources, filepath.Join(e.cacheDir, "docker-manifests", registryID, safeRepo, digest))
	}
	sources = append(sources, dockerBlobPath(e.cacheDir, digest))

	linked := false
	for _, src := range sources {
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Link(src, targetPath); err == nil {
			linked = true
			e.report.BlobsLinked++
			break
		}
		// Hardlink failed (EXDEV cross-volume, permissions, etc.) — fall
		// through to write from the in-memory body.
	}
	if !linked {
		if err := os.WriteFile(targetPath, body, 0644); err != nil {
			return "", fmt.Errorf("write blob %s: %w", digest[:19], err)
		}
		e.report.BlobsCopied++
	}

	if !isManifest {
		return "", nil
	}

	mediaType := extractMediaType(body)
	if subs := extractSubManifests(body); len(subs) > 0 {
		// Index — recurse on each per-platform sub-manifest. Its body lives
		// at docker-manifests/<reg>/<safeRepo>/<sub-digest>.
		for _, sub := range subs {
			subPath := filepath.Join(e.cacheDir, "docker-manifests", registryID, safeRepo, sub)
			subBody, rerr := os.ReadFile(subPath)
			if rerr != nil {
				e.report.BlobsMissed++
				continue
			}
			if _, err := e.materialize(registryID, safeRepo, blobsDir, subBody, true); err != nil {
				e.report.Errors++
				continue
			}
		}
	} else {
		// Image manifest — config + layers are plain blobs in docker-blobs/.
		for _, dg := range extractConfigAndLayerDigests(body) {
			blobPath := dockerBlobPath(e.cacheDir, dg)
			info, serr := os.Stat(blobPath)
			if serr != nil {
				e.report.BlobsMissed++
				continue
			}
			// For hardlink-only materialization of large layer blobs we
			// don't want to read the body into memory. Use a thin helper
			// that hardlinks-or-copies by path directly.
			if err := e.materializeBlobFromPath(blobsDir, blobPath, dg, info.Size()); err != nil {
				e.report.Errors++
				continue
			}
		}
	}
	return mediaType, nil
}

// materializeBlobFromPath places a blob whose body lives on disk at srcPath
// into blobsDir/sha256/<hash> without reading the body. Hardlink first; on
// EXDEV (cross-volume) fall through to an os.ReadFile / os.WriteFile copy.
// Used for large layer blobs where reading-then-writing would be wasteful.
func (e *LayoutEmitter) materializeBlobFromPath(blobsDir, srcPath, digest string, _ int64) error {
	hash, ok := strings.CutPrefix(digest, "sha256:")
	if !ok {
		return fmt.Errorf("blob digest missing sha256: prefix: %s", digest)
	}
	targetPath := filepath.Join(blobsDir, hash)
	if _, err := os.Stat(targetPath); err == nil {
		return nil
	}
	if err := os.Link(srcPath, targetPath); err == nil {
		e.report.BlobsLinked++
		return nil
	}
	// Fall back to a stream copy via ReadFile/WriteFile. For multi-GB
	// layers this is wasteful; in practice it only happens when oci-layouts
	// lives on a different volume than docker-blobs, which we don't ship
	// as a supported layout.
	data, rerr := os.ReadFile(srcPath)
	if rerr != nil {
		return fmt.Errorf("read blob %s: %w", srcPath, rerr)
	}
	if err := os.WriteFile(targetPath, data, 0644); err != nil {
		return fmt.Errorf("copy blob %s: %w", targetPath, err)
	}
	e.report.BlobsCopied++
	return nil
}

func dockerBlobPath(cacheDir, digest string) string {
	if hash, ok := strings.CutPrefix(digest, "sha256:"); ok && len(hash) >= 4 {
		return filepath.Join(cacheDir, "docker-blobs", "sha256", hash[0:2], hash[2:4], digest)
	}
	return filepath.Join(cacheDir, "docker-blobs", digest)
}

// extractMediaType returns body.mediaType, defaulting to OCI image manifest
// when absent. Bodies missing a top-level mediaType still produce a valid
// (if slightly less informative) layout descriptor.
func extractMediaType(body []byte) string {
	var m struct {
		MediaType string `json:"mediaType"`
	}
	if err := json.Unmarshal(body, &m); err == nil && m.MediaType != "" {
		return m.MediaType
	}
	return "application/vnd.oci.image.manifest.v1+json"
}

// extractSubManifests returns the per-platform sub-manifest digests
// referenced by an OCI image index or Docker manifest list. Empty slice
// for ordinary image manifests.
func extractSubManifests(body []byte) []string {
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

// extractConfigAndLayerDigests returns the config + layer digests from an
// image manifest. Empty for indexes.
func extractConfigAndLayerDigests(body []byte) []string {
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
	out := make([]string, 0, 1+len(m.Layers))
	if m.Config.Digest != "" {
		out = append(out, m.Config.Digest)
	}
	for _, l := range m.Layers {
		if l.Digest != "" {
			out = append(out, l.Digest)
		}
	}
	return out
}
