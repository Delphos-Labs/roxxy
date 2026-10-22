package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// seedManifest writes a manifest JSON to the cache and bumps refcounts for its
// blobs, simulating what the registry proxy does when storing a pulled image.
func seedManifest(t *testing.T, cacheDir, registry, repo, reference string, configDigest string, layerDigests []string) string {
	t.Helper()
	manifest := map[string]any{
		"schemaVersion": 2,
		"config":        map[string]any{"digest": configDigest, "size": 1},
		"layers": func() []map[string]any {
			out := make([]map[string]any, 0, len(layerDigests))
			for _, d := range layerDigests {
				out = append(out, map[string]any{"digest": d, "size": 1})
			}
			return out
		}(),
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	manifestPath := filepath.Join(cacheDir, "docker-manifests", registry, repo, reference)
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(manifestPath, body, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	for _, d := range append([]string{configDigest}, layerDigests...) {
		hash := d[len("sha256:"):]
		if err := IncrementRefcount(cacheDir, hash); err != nil {
			t.Fatalf("increment: %v", err)
		}
		blobPath := util.DockerBlobPath(cacheDir, d)
		if err := os.MkdirAll(filepath.Dir(blobPath), 0755); err != nil {
			t.Fatalf("blob mkdir: %v", err)
		}
		if err := os.WriteFile(blobPath, []byte("x"), 0644); err != nil {
			t.Fatalf("blob write: %v", err)
		}
	}
	return manifestPath
}

func TestRemoveManifestParksBlobsAtRefcountZero(t *testing.T) {
	cacheDir := t.TempDir()
	cfg := "sha256:cfg0000000000000000000000000000000000000000000000000000000000000"
	l1 := "sha256:layer00000000000000000000000000000000000000000000000000000000001"
	mp := seedManifest(t, cacheDir, "docker.io", "library_busybox", "latest", cfg, []string{l1})

	if _, err := RemoveManifest(cacheDir, mp); err != nil {
		t.Fatalf("RemoveManifest: %v", err)
	}
	// New contract: blob files are NOT deleted on refcount=0 — orphan
	// reclamation is deferred to docker prune-blobs / audit. The refcount
	// file stays at 0 so a quick retag re-uses the bytes already on disk.
	for _, d := range []string{cfg, l1} {
		if _, err := os.Stat(util.DockerBlobPath(cacheDir, d)); err != nil {
			t.Fatalf("blob %s should be parked on disk pending flush, got: %v", d, err)
		}
		hash := d[len("sha256:"):]
		if got := readStoredRefcount(cacheDir, hash); got != 0 {
			t.Fatalf("blob %s refcount = %d, want 0", d, got)
		}
	}
	if _, err := os.Stat(mp); !os.IsNotExist(err) {
		t.Fatalf("manifest file should be gone")
	}
}

func TestRemoveManifestSharedBlobStaysAtPositiveRefcount(t *testing.T) {
	cacheDir := t.TempDir()
	cfg1 := "sha256:cfg1000000000000000000000000000000000000000000000000000000000000"
	cfg2 := "sha256:cfg2000000000000000000000000000000000000000000000000000000000000"
	shared := "sha256:shared000000000000000000000000000000000000000000000000000000000"

	m1 := seedManifest(t, cacheDir, "docker.io", "library_a", "latest", cfg1, []string{shared})
	_ = seedManifest(t, cacheDir, "docker.io", "library_b", "latest", cfg2, []string{shared})

	if _, err := RemoveManifest(cacheDir, m1); err != nil {
		t.Fatalf("RemoveManifest: %v", err)
	}
	// Shared blob stays at refcount=1 (still referenced by manifest B); its
	// file is on disk. cfg1 dropped to 0 (no other manifests reference it)
	// and its file is also on disk pending flush.
	sharedHash := shared[len("sha256:"):]
	if got := readStoredRefcount(cacheDir, sharedHash); got != 1 {
		t.Fatalf("shared refcount = %d, want 1", got)
	}
	if _, err := os.Stat(util.DockerBlobPath(cacheDir, shared)); err != nil {
		t.Fatalf("shared blob must survive: %v", err)
	}
	cfg1Hash := cfg1[len("sha256:"):]
	if got := readStoredRefcount(cacheDir, cfg1Hash); got != 0 {
		t.Fatalf("cfg1 refcount = %d, want 0", got)
	}
	if _, err := os.Stat(util.DockerBlobPath(cacheDir, cfg1)); err != nil {
		t.Fatalf("cfg1 blob should be parked on disk pending flush, got: %v", err)
	}
}

func TestAuditRefcountsHealsDriftAndOrphans(t *testing.T) {
	cacheDir := t.TempDir()
	cfg := "sha256:cfg0000000000000000000000000000000000000000000000000000000000000"
	l1 := "sha256:layer00000000000000000000000000000000000000000000000000000000001"
	orphanDigest := "sha256:orphan0000000000000000000000000000000000000000000000000000000000"
	mp := seedManifest(t, cacheDir, "docker.io", "library_x", "latest", cfg, []string{l1})

	// Simulate the rmi bug: manifest gone but refcounts + blobs stranded.
	if err := os.Remove(mp); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}

	// Also simulate a separate intact manifest that should survive.
	keepCfg := "sha256:keep00000000000000000000000000000000000000000000000000000000000"
	seedManifest(t, cacheDir, "docker.io", "library_keep", "v1", keepCfg, nil)

	// And simulate drift: write a bogus refcount of 99 for keepCfg.
	if err := writeRefcount(cacheDir, keepCfg[len("sha256:"):], 99); err != nil {
		t.Fatalf("writeRefcount: %v", err)
	}

	// And an entirely dangling refcount file with no blob on disk.
	if err := writeRefcount(cacheDir, orphanDigest[len("sha256:"):], 1); err != nil {
		t.Fatalf("writeRefcount: %v", err)
	}

	rep, err := AuditRefcounts(cacheDir)
	if err != nil {
		t.Fatalf("AuditRefcounts: %v", err)
	}

	// The two stranded blobs from the removed manifest should be gone.
	for _, d := range []string{cfg, l1} {
		if _, err := os.Stat(util.DockerBlobPath(cacheDir, d)); !os.IsNotExist(err) {
			t.Fatalf("orphan blob %s should have been deleted", d)
		}
	}
	if rep.BlobsRemoved != 2 {
		t.Fatalf("want 2 blobs removed, got %d", rep.BlobsRemoved)
	}
	// keepCfg refcount should be healed back to 1.
	got := readStoredRefcount(cacheDir, keepCfg[len("sha256:"):])
	if got != 1 {
		t.Fatalf("drift not healed: keepCfg refcount = %d, want 1", got)
	}
	if rep.RefcountsHealed < 1 {
		t.Fatalf("want refcounts_healed >= 1, got %d", rep.RefcountsHealed)
	}
	// Dangling refcount file should be gone.
	if _, err := os.Stat(GetRefcountPath(cacheDir, orphanDigest[len("sha256:"):])); !os.IsNotExist(err) {
		t.Fatalf("dangling refcount should be removed")
	}
	if rep.StaleRefcountsRemoved < 1 {
		t.Fatalf("want stale_refcounts_removed >= 1, got %d", rep.StaleRefcountsRemoved)
	}
}
