package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// seedBlob writes a blob to docker-blobs/sha256/<aa>/<bb>/sha256:<hash> and
// returns its sha256:<hex> digest.
func seedBlob(t *testing.T, cacheDir string, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	hashHex := hex.EncodeToString(sum[:])
	digest := "sha256:" + hashHex
	dir := filepath.Join(cacheDir, "docker-blobs", "sha256", hashHex[0:2], hashHex[2:4])
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir blob: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest), body, 0644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	return digest
}

// seedManifestBoth writes a manifest body to both the tag-file and the
// digest-file under docker-manifests/<reg>/<safeRepo>/, returning the
// computed digest.
func seedManifestBoth(t *testing.T, cacheDir, registryID, safeRepo, tag string, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	hashHex := hex.EncodeToString(sum[:])
	digest := "sha256:" + hashHex
	dir := filepath.Join(cacheDir, "docker-manifests", registryID, safeRepo)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, tag), body, 0644); err != nil {
		t.Fatalf("write tag file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest), body, 0644); err != nil {
		t.Fatalf("write digest file: %v", err)
	}
	return digest
}

func TestEmitRef_ImageManifest_HardlinksBlobs(t *testing.T) {
	cacheDir := t.TempDir()
	registryID := "docker.io"
	safeRepo := "library_nginx"
	tag := "1.27"

	configBody := []byte(`{"architecture":"amd64"}`)
	layerBody := []byte("layer-content-deadbeef")
	configDigest := seedBlob(t, cacheDir, configBody)
	layerDigest := seedBlob(t, cacheDir, layerBody)

	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
		"config":        map[string]any{"mediaType": "application/vnd.docker.container.image.v1+json", "digest": configDigest, "size": len(configBody)},
		"layers": []map[string]any{{
			"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
			"digest":    layerDigest,
			"size":      len(layerBody),
		}},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestDigest := seedManifestBoth(t, cacheDir, registryID, safeRepo, tag, body)

	emitter := NewLayoutEmitter(cacheDir)
	if err := emitter.EmitRef(registryID, safeRepo, tag, body); err != nil {
		t.Fatalf("EmitRef: %v", err)
	}

	layoutDir := filepath.Join(LayoutsRoot(cacheDir), registryID, safeRepo, tag)

	// 1. oci-layout marker present.
	marker, err := os.ReadFile(filepath.Join(layoutDir, "oci-layout"))
	if err != nil {
		t.Fatalf("read oci-layout: %v", err)
	}
	if string(marker) != `{"imageLayoutVersion":"1.0.0"}` {
		t.Errorf("oci-layout body = %q, want imageLayoutVersion marker", string(marker))
	}

	// 2. index.json points to the manifest with declared mediaType.
	idxBytes, err := os.ReadFile(filepath.Join(layoutDir, "index.json"))
	if err != nil {
		t.Fatalf("read index.json: %v", err)
	}
	var idx struct {
		Manifests []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(idxBytes, &idx); err != nil {
		t.Fatalf("parse index.json: %v", err)
	}
	if len(idx.Manifests) != 1 {
		t.Fatalf("index.json manifests = %d, want 1", len(idx.Manifests))
	}
	if got := idx.Manifests[0].Digest; got != manifestDigest {
		t.Errorf("index manifest digest = %s, want %s", got, manifestDigest)
	}
	if got := idx.Manifests[0].MediaType; got != "application/vnd.docker.distribution.manifest.v2+json" {
		t.Errorf("index manifest mediaType = %s, want docker manifest v2", got)
	}

	// 3. blobs/sha256/<hash> present for manifest, config, layer.
	for _, dg := range []string{manifestDigest, configDigest, layerDigest} {
		hash := dg[len("sha256:"):]
		layoutBlob := filepath.Join(layoutDir, "blobs", "sha256", hash)
		info, err := os.Stat(layoutBlob)
		if err != nil {
			t.Fatalf("missing layout blob %s: %v", dg, err)
		}
		if info.Size() == 0 {
			t.Errorf("layout blob %s is empty", dg)
		}
	}

	// 4. Layout blobs are HARDLINKS to the cache originals (same inode).
	cases := []struct {
		layoutPath string
		cachePath  string
	}{
		{
			filepath.Join(layoutDir, "blobs", "sha256", manifestDigest[len("sha256:"):]),
			filepath.Join(cacheDir, "docker-manifests", registryID, safeRepo, manifestDigest),
		},
		{
			filepath.Join(layoutDir, "blobs", "sha256", configDigest[len("sha256:"):]),
			dockerBlobPath(cacheDir, configDigest),
		},
		{
			filepath.Join(layoutDir, "blobs", "sha256", layerDigest[len("sha256:"):]),
			dockerBlobPath(cacheDir, layerDigest),
		},
	}
	for _, c := range cases {
		if !sameInode(t, c.layoutPath, c.cachePath) {
			t.Errorf("layout blob %s is not a hardlink of %s", c.layoutPath, c.cachePath)
		}
	}

	rep := emitter.Report()
	if rep.Refs != 1 {
		t.Errorf("Refs = %d, want 1", rep.Refs)
	}
	if rep.BlobsLinked != 3 {
		t.Errorf("BlobsLinked = %d, want 3 (manifest + config + layer)", rep.BlobsLinked)
	}
	if rep.BlobsCopied != 0 || rep.BlobsMissed != 0 || rep.Errors != 0 {
		t.Errorf("expected zero copied/missed/errors, got %+v", rep)
	}
}

func TestEmitRef_Index_RecursesIntoSubManifests(t *testing.T) {
	cacheDir := t.TempDir()
	registryID := "docker.io"
	safeRepo := "library_busybox"

	// One platform sub-manifest with one config + one layer.
	configBody := []byte(`{"architecture":"amd64"}`)
	layerBody := []byte("layer-amd64")
	configDigest := seedBlob(t, cacheDir, configBody)
	layerDigest := seedBlob(t, cacheDir, layerBody)

	subManifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": len(configBody)},
		"layers": []map[string]any{{
			"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			"digest":    layerDigest,
			"size":      len(layerBody),
		}},
	}
	subBody, _ := json.Marshal(subManifest)
	subSum := sha256.Sum256(subBody)
	subDigest := "sha256:" + hex.EncodeToString(subSum[:])

	// Sub-manifest body is stored by digest under the same safeRepo.
	subPath := filepath.Join(cacheDir, "docker-manifests", registryID, safeRepo, subDigest)
	if err := os.MkdirAll(filepath.Dir(subPath), 0755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(subPath, subBody, 0644); err != nil {
		t.Fatalf("write sub: %v", err)
	}

	// Index pointing to the sub-manifest.
	index := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    subDigest,
			"size":      len(subBody),
			"platform":  map[string]string{"architecture": "amd64", "os": "linux"},
		}},
	}
	indexBody, _ := json.Marshal(index)
	indexDigest := seedManifestBoth(t, cacheDir, registryID, safeRepo, "latest", indexBody)

	emitter := NewLayoutEmitter(cacheDir)
	if err := emitter.EmitRef(registryID, safeRepo, "latest", indexBody); err != nil {
		t.Fatalf("EmitRef: %v", err)
	}

	layoutDir := filepath.Join(LayoutsRoot(cacheDir), registryID, safeRepo, "latest")
	for _, dg := range []string{indexDigest, subDigest, configDigest, layerDigest} {
		layoutBlob := filepath.Join(layoutDir, "blobs", "sha256", dg[len("sha256:"):])
		if _, err := os.Stat(layoutBlob); err != nil {
			t.Errorf("missing closure blob %s: %v", dg, err)
		}
	}

	// index.json mediaType should reflect the INDEX, not the sub.
	idxBytes, _ := os.ReadFile(filepath.Join(layoutDir, "index.json"))
	if !contains(idxBytes, "application/vnd.oci.image.index.v1+json") {
		t.Errorf("index.json should reference the index mediaType, body=%s", string(idxBytes))
	}

	rep := emitter.Report()
	if rep.Refs != 1 {
		t.Errorf("Refs = %d, want 1", rep.Refs)
	}
	// index body, sub body, config, layer = 4 hardlinks (or copies in
	// rare cross-vol cases — in t.TempDir() they should all be links).
	if rep.BlobsLinked+rep.BlobsCopied != 4 {
		t.Errorf("link+copy total = %d, want 4 (index+sub+config+layer)", rep.BlobsLinked+rep.BlobsCopied)
	}
}

func TestWipeLayouts_RemovesStaleAndIsIdempotent(t *testing.T) {
	cacheDir := t.TempDir()
	stale := filepath.Join(LayoutsRoot(cacheDir), "docker.io", "library_oldimg", "v1")
	if err := os.MkdirAll(stale, 0755); err != nil {
		t.Fatalf("mkdir stale: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stale, "marker"), []byte("stale"), 0644); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	if err := WipeLayouts(cacheDir); err != nil {
		t.Fatalf("WipeLayouts: %v", err)
	}
	if _, err := os.Stat(LayoutsRoot(cacheDir)); !os.IsNotExist(err) {
		t.Errorf("LayoutsRoot should be gone after wipe, err=%v", err)
	}

	// Idempotent — second call on a missing root is a no-op.
	if err := WipeLayouts(cacheDir); err != nil {
		t.Errorf("WipeLayouts idempotent: %v", err)
	}
}

func TestEmitRef_MissingBlobIsLoggedNotFatal(t *testing.T) {
	cacheDir := t.TempDir()
	registryID := "docker.io"
	safeRepo := "library_broken"
	tag := "latest"

	// Manifest references a blob that doesn't exist on disk.
	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
		"config":        map[string]any{"digest": "sha256:0000000000000000000000000000000000000000000000000000000000000001", "size": 1},
		"layers": []map[string]any{{
			"digest": "sha256:0000000000000000000000000000000000000000000000000000000000000002",
			"size":   1,
		}},
	}
	body, _ := json.Marshal(manifest)
	seedManifestBoth(t, cacheDir, registryID, safeRepo, tag, body)

	emitter := NewLayoutEmitter(cacheDir)
	if err := emitter.EmitRef(registryID, safeRepo, tag, body); err != nil {
		t.Fatalf("EmitRef should not return fatal error for missing referenced blobs, got: %v", err)
	}
	rep := emitter.Report()
	if rep.BlobsMissed != 2 {
		t.Errorf("BlobsMissed = %d, want 2 (config + layer)", rep.BlobsMissed)
	}
	// Manifest body itself is materialized (we have it in-hand).
	if rep.BlobsLinked+rep.BlobsCopied < 1 {
		t.Errorf("manifest body should still materialize, got linked=%d copied=%d", rep.BlobsLinked, rep.BlobsCopied)
	}
}

// sameInode reports whether two paths refer to the same on-disk inode
// (i.e., one is a hardlink to the other). Used to verify hardlink
// emission. Tests run in t.TempDir() which is always same-volume, so a
// failure here indicates a real bug rather than EXDEV.
func sameInode(t *testing.T, a, b string) bool {
	t.Helper()
	ai, err := os.Stat(a)
	if err != nil {
		t.Errorf("stat %s: %v", a, err)
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		t.Errorf("stat %s: %v", b, err)
		return false
	}
	return os.SameFile(ai, bi)
}

func contains(haystack []byte, needle string) bool {
	return len(haystack) >= len(needle) && (string(haystack) == needle || indexOf(string(haystack), needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
