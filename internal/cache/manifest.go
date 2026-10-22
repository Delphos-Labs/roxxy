package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// RemoveResult summarizes blob cleanup performed by RemoveManifest.
type RemoveResult struct {
	BlobsRemoved int
	BytesFreed   int64
	RefErrors    int
}

// RemoveManifest deletes a Docker manifest file and its sidecar metadata
// files, and decrements refcounts for every blob the manifest references.
//
// Blob files are NOT deleted here even when their refcount reaches zero —
// orphan reclamation is deferred to `roxxy docker prune-blobs` /
// `docker audit`. This keeps "delete tag, re-push new version sharing
// some blobs" zero-copy across the refcount=0 window (the blob stays
// on disk for the next Increment to find).
//
// This is the single entry point every manifest-removal code path should
// use — skipping it leaks refcounts and strands blobs that prune can
// never reclaim.
//
// For OCI/Docker manifest lists (indexes), there are no config/layer
// blobs to decrement; the per-platform manifests referenced by the index
// carry their own refcounts and are cleaned up when they themselves are
// removed.
func RemoveManifest(cacheDir, manifestPath string) (RemoveResult, error) {
	var res RemoveResult

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return res, fmt.Errorf("read manifest: %w", err)
	}

	digests := extractManifestBlobDigests(data)

	if err := os.Remove(manifestPath); err != nil && !os.IsNotExist(err) {
		return res, fmt.Errorf("remove manifest: %w", err)
	}
	removeIfExists(manifestPath + ".meta")
	removeIfExists(manifestPath + ".metadata")

	for _, digest := range digests {
		if digest == "" {
			continue
		}
		contentHash := strings.TrimPrefix(digest, "sha256:")
		if _, derr := DecrementRefcount(cacheDir, contentHash); derr != nil {
			res.RefErrors++
		}
	}

	return res, nil
}

func removeIfExists(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		util.Logerr(err)
	}
}

// extractManifestBlobDigests parses a manifest body and returns every blob
// digest (config + layers) the manifest references. Manifest-list / OCI-index
// bodies have no config/layers and yield an empty slice.
func extractManifestBlobDigests(body []byte) []string {
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
