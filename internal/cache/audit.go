package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// AuditReport summarizes the result of a refcount audit pass.
type AuditReport struct {
	ManifestsScanned      int
	BlobsScanned          int
	BlobsRemoved          int
	BytesFreed            int64
	RefcountsHealed       int
	StaleRefcountsRemoved int
	// MissingBlobWarnings: real drift — primary manifests reference a blob
	// that isn't on disk. Worth investigating.
	MissingBlobWarnings []string
	// SpeculativePlatformBlobsMissing: blobs missing only because they're
	// referenced exclusively by index sub-manifests for platforms the
	// proxy never actually pulled. Expected behavior; reported as a
	// count so operators can see the scale without scrolling 1000s of
	// per-digest lines.
	SpeculativePlatformBlobsMissing int
	// ManifestOnlyFetchBlobsMissing: blobs missing only because the
	// manifest(s) referencing them have 100% of blobs absent — i.e. the
	// manifest was fetched but no layers ever were (typical of `docker
	// manifest inspect`, HEAD requests, SBOM probes, catalog enumerators).
	// Expected behavior; suppressed from per-digest warnings.
	ManifestOnlyFetchBlobsMissing int
	// RepairTargets: per real-drift missing blob, one referrer's repo +
	// upstream URL, suitable for a re-fetch. Populated alongside the
	// WARNINGs.
	RepairTargets []RepairTarget
	// LeakAttributionByRepo: repo path → count of blobs with refcount
	// drift attributable to that repo's manifests. Drift comes from
	// manifest deletes that didn't decrement blob refcounts (e.g. a
	// code path that unlinks the manifest file but skips
	// DecrementRefcount). For each drifted blob, we attribute to every
	// CURRENT-still-on-disk referrer's repo — the actual leaker is the
	// since-deleted manifest, but current referrers share the same repo
	// as the leaker if the bug is in a repo-scoped push/rmi path.
	//
	// Orphan blobs (want=0) with no current referrer can't be
	// attributed at audit-time (their manifests are gone), so they
	// accumulate under the special key "(unattributed)" — high counts
	// there indicate per-repo tracking is incomplete and the leak fix
	// needs an rmi-time journal rather than after-the-fact inference.
	LeakAttributionByRepo map[string]int
}

// RepairTarget identifies one missing blob and the upstream + repo it
// can be re-fetched from.
type RepairTarget struct {
	Digest      string // "sha256:..."
	RepoName    string // e.g. "library/redis"
	UpstreamURL string // e.g. "https://registry-1.docker.io"
}

// AuditRefcounts walks the docker-manifests tree to build a ground-truth map of
// blob_digest → reference_count, then reconciles that against the on-disk
// refcount files and blob files. Orphan blobs are deleted, drifted refcount
// files are rewritten, and dangling refcount files (no blob) are removed.
//
// Manifests are treated as source of truth; refcount files are treated as cache.
// This heals drift introduced by code paths that skip DecrementRefcount when
// removing a manifest.
func AuditRefcounts(cacheDir string) (AuditReport, error) {
	var rep AuditReport

	manifestDir := filepath.Join(cacheDir, "docker-manifests")
	blobsRoot := filepath.Join(cacheDir, "docker-blobs")
	refcountRoot := filepath.Join(cacheDir, "refcount")

	// digest (without "sha256:" prefix) → actual reference count from manifests
	actual := make(map[string]int)

	// Pre-pass: enumerate blobs on disk so manifest classification knows
	// which of a manifest's references are actually present.
	blobsOnDisk := make(map[string]bool)
	_ = filepath.WalkDir(blobsRoot, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if strings.HasPrefix(base, "sha256:") {
			blobsOnDisk[strings.TrimPrefix(base, "sha256:")] = true
		}
		return nil
	})

	// Pass 1: scan manifests for sub-manifest digests listed in indexes.
	// Used to classify a manifest as "platform variant of an index" — its
	// blobs may legitimately be absent if that platform was never pulled.
	indexSubs := make(map[string]bool)
	_ = filepath.WalkDir(manifestDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".meta") || strings.HasSuffix(name, ".metadata") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, sub := range extractSubManifestDigests(data) {
			indexSubs[sub] = true
		}
		return nil
	})

	// Pass 2: classify each manifest and accumulate blob refs.
	//
	// Categories:
	//   speculative-sub: manifest is a sub-manifest of an index (platform
	//                    variant not pulled)
	//   manifest-only:   manifest has ≥1 blob ref and *every* one of those
	//                    blobs is absent (metadata-only fetch — inspect,
	//                    HEAD, SBOM probe, catalog enumerator)
	//   real:            partial blob set present, or no blob refs (image
	//                    manifest claiming a blob that exists; OR a real
	//                    drift case if one of its blobs is now missing)
	//
	// For each missing blob hash we track which categories of manifest
	// referred to it. A blob warns only if at least one "real" referrer
	// claimed it.
	hasRealReferrer := make(map[string]bool)
	suppressedSpec := make(map[string]bool) // blob hash referenced only by speculative-sub manifests
	suppressedMOF := make(map[string]bool)  // blob hash referenced only by manifest-only fetches
	// blob hash → manifest file path of one "real" referrer (for repair)
	realReferrerPath := make(map[string]string)
	// blob hash → set of repos that currently reference it. Populated
	// from manifest path prefixes (everything below docker-manifests/
	// down to the tag's parent directory). Used to attribute refcount
	// drift to a likely-buggy repo path when audit finds got != want.
	referrerReposByHash := make(map[string]map[string]bool)
	err := filepath.WalkDir(manifestDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".meta") || strings.HasSuffix(name, ".metadata") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		rep.ManifestsScanned++

		isSub := strings.HasPrefix(name, "sha256:") && indexSubs[name]

		// Compute blob-presence ratio for manifest-only classification.
		blobDigests := extractManifestBlobDigests(data)
		present := 0
		for _, dg := range blobDigests {
			if blobsOnDisk[strings.TrimPrefix(dg, "sha256:")] {
				present++
			}
		}
		isManifestOnly := len(blobDigests) > 0 && present == 0 && !isSub

		category := "real"
		switch {
		case isSub:
			category = "speculative"
		case isManifestOnly:
			category = "manifest-only"
		}

		// Extract repo path (manifest dir relative to docker-manifests/,
		// minus the trailing tag/digest filename). Used for leak
		// attribution below. Examples:
		//   docker-manifests/docker.io/library/redis/v7.2-alpine
		//     → repo "docker.io/library/redis"
		//   docker-manifests/quay.io/argoproj/argocd/v3.0.11
		//     → repo "quay.io/argoproj/argocd"
		repo := ""
		if rel, relErr := filepath.Rel(manifestDir, path); relErr == nil {
			if dir := filepath.Dir(rel); dir != "." {
				repo = filepath.ToSlash(dir)
			}
		}

		for _, digest := range blobDigests {
			hash := strings.TrimPrefix(digest, "sha256:")
			actual[hash]++
			if repo != "" {
				if referrerReposByHash[hash] == nil {
					referrerReposByHash[hash] = make(map[string]bool)
				}
				referrerReposByHash[hash][repo] = true
			}
			switch category {
			case "real":
				hasRealReferrer[hash] = true
				if _, ok := realReferrerPath[hash]; !ok {
					realReferrerPath[hash] = path
				}
				// A real referrer overrides any prior suppression flags.
				delete(suppressedSpec, hash)
				delete(suppressedMOF, hash)
			case "speculative":
				if !hasRealReferrer[hash] {
					suppressedSpec[hash] = true
				}
			case "manifest-only":
				if !hasRealReferrer[hash] {
					suppressedMOF[hash] = true
				}
			}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return rep, fmt.Errorf("walk manifests: %w", err)
	}

	// Walk blobs: reconcile each blob against actual[hash].
	seenHashes := make(map[string]bool)
	err = filepath.WalkDir(blobsRoot, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if !strings.HasPrefix(base, "sha256:") {
			return nil
		}
		rep.BlobsScanned++
		hash := strings.TrimPrefix(base, "sha256:")
		seenHashes[hash] = true

		want := actual[hash]
		got := readStoredRefcount(cacheDir, hash)

		// Attribute leak/drift to current referrer repos. A blob with
		// got > want had at least one past delete that didn't decrement
		// its refcount; the deleted manifest is gone, but if the bug is
		// in a repo-scoped push/rmi path then current referrers in the
		// same repo are the smoking gun. Orphan blobs (want == 0) have
		// no current referrer to attribute to, bucket those separately.
		leaked := want == 0 || got != want
		if leaked {
			if rep.LeakAttributionByRepo == nil {
				rep.LeakAttributionByRepo = make(map[string]int)
			}
			if repos := referrerReposByHash[hash]; len(repos) > 0 {
				for repo := range repos {
					rep.LeakAttributionByRepo[repo]++
				}
			} else if want == 0 {
				rep.LeakAttributionByRepo["(unattributed)"]++
			}
		}

		switch {
		case want == 0:
			// Orphan blob — delete blob file and its refcount file.
			info, statErr := os.Stat(path)
			if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
				return nil
			}
			rep.BlobsRemoved++
			if statErr == nil {
				rep.BytesFreed += info.Size()
			}
			util.Logerr(os.Remove(GetRefcountPath(cacheDir, hash)))
		case got != want:
			if err := writeRefcount(cacheDir, hash, want); err == nil {
				rep.RefcountsHealed++
			}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return rep, fmt.Errorf("walk blobs: %w", err)
	}

	// Classify missing blobs:
	//   - hasRealReferrer  → genuine drift, emit warning
	//   - suppressedSpec   → only an index sub-manifest for a platform we
	//                        never pulled refers to this; expected gap
	//   - suppressedMOF    → only a manifest-only fetch (inspect / HEAD /
	//                        SBOM probe) refers to this; expected gap
	for hash, count := range actual {
		if count == 0 || seenHashes[hash] {
			continue
		}
		switch {
		case hasRealReferrer[hash]:
			rep.MissingBlobWarnings = append(rep.MissingBlobWarnings,
				fmt.Sprintf("sha256:%s referenced by %d manifest(s) but blob file missing", hash, count))
			if mfPath := realReferrerPath[hash]; mfPath != "" {
				if t, ok := deriveRepairTarget(manifestDir, mfPath, "sha256:"+hash); ok {
					rep.RepairTargets = append(rep.RepairTargets, t)
				}
			}
		case suppressedMOF[hash]:
			rep.ManifestOnlyFetchBlobsMissing++
		case suppressedSpec[hash]:
			rep.SpeculativePlatformBlobsMissing++
		default:
			rep.MissingBlobWarnings = append(rep.MissingBlobWarnings,
				fmt.Sprintf("sha256:%s referenced by %d manifest(s) but blob file missing", hash, count))
		}
	}

	// Walk refcount files: any refcount file whose blob doesn't exist is stale.
	err = filepath.WalkDir(refcountRoot, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		hash := filepath.Base(path)
		if seenHashes[hash] {
			return nil
		}
		util.Logerr(os.Remove(path))
		rep.StaleRefcountsRemoved++
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return rep, fmt.Errorf("walk refcounts: %w", err)
	}

	return rep, nil
}

// deriveRepairTarget reconstructs (repo, upstreamURL) from a manifest's
// on-disk path + .meta sidecar. The path shape is
// "<manifestDir>/<registryID>/<safeRepo>/<reference>" where safeRepo
// has '/' replaced by '_' for filesystem safety. The .meta sidecar
// stores the upstream URL the manifest came from.
func deriveRepairTarget(manifestDir, manifestPath, digest string) (RepairTarget, bool) {
	rel, err := filepath.Rel(manifestDir, manifestPath)
	if err != nil {
		return RepairTarget{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 3 {
		return RepairTarget{}, false
	}
	// parts: <registryID>/<safeRepo>/<reference>
	safeRepo := parts[1]
	repo := util.UnsafeRepoName(safeRepo)

	upstream := ""
	if metaBytes, err := os.ReadFile(manifestPath + ".meta"); err == nil {
		var meta struct {
			UpstreamURL string `json:"upstream_url"`
		}
		if json.Unmarshal(metaBytes, &meta) == nil {
			upstream = meta.UpstreamURL
		}
	}
	if upstream == "" {
		// Fallback: synthesize from registryID. Works for hosts like
		// "ghcr.io", "quay.io", "docker.io" → "https://<host>".
		upstream = "https://" + parts[0]
	}
	return RepairTarget{Digest: digest, RepoName: repo, UpstreamURL: upstream}, true
}

func readStoredRefcount(cacheDir, hash string) int {
	data, err := os.ReadFile(GetRefcountPath(cacheDir, hash))
	if err != nil {
		return 0
	}
	count := 0
	util.LogerrT(fmt.Sscanf(string(data), "%d", &count))
	return count
}

func writeRefcount(cacheDir, hash string, count int) error {
	p := GetRefcountPath(cacheDir, hash)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(fmt.Sprintf("%d", count)), 0644)
}

// ManifestAuditReport summarizes a manifest-refcount audit pass.
type ManifestAuditReport struct {
	ManifestsScanned         int
	TagRefs                  int
	IndexSubRefs             int
	ManifestsRemoved         int
	ManifestBytesFreed       int64
	ManifestRefcountsWritten int
	StaleManifestRefcounts   int
	// OrphanSidecarsRemoved counts .meta and .metadata files whose base
	// manifest file is gone. Pre-2026-05 deletes didn't always clean up
	// .metadata; audit sweeps them so cache size matches actual content.
	OrphanSidecarsRemoved   int
	OrphanSidecarBytesFreed int64
}

// AuditManifests rebuilds manifest refcounts from the docker-manifests tree.
//
// Refcount sources, in order:
//
//   - Each by-tag file (filename does NOT start with "sha256:") contributes
//     +1 to its content's digest. (A tag is a reference to a manifest.)
//   - Each by-digest index manifest contributes +1 to each digest in its
//     `manifests` array. (An index references its platform manifests.)
//
// On-disk manifest refcount files are wiped first and then re-written from
// the freshly computed counts — they're treated as cache, not source of
// truth. Manifests with computed refcount == 0 are cascade-removed (the
// by-digest file is unlinked; blob refcounts get recomputed by the
// subsequent AuditRefcounts pass).
//
// Iterates to a fixed point: removing an orphan index manifest decrements
// its sub-manifests, which may themselves drop to 0 and need removal.
func AuditManifests(cacheDir string) (ManifestAuditReport, error) {
	var rep ManifestAuditReport
	manifestDir := filepath.Join(cacheDir, "docker-manifests")
	manifestRefcountRoot := filepath.Join(cacheDir, "refcount", "manifest")

	// digestHash → ref count
	refs := make(map[string]int)
	// digestHash → path of by-digest file (so we can read body / delete it)
	byDigestPath := make(map[string]string)
	// digestHash → cached body (avoid re-read during cascade)
	bodyCache := make(map[string][]byte)

	err := filepath.WalkDir(manifestDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".meta") || strings.HasSuffix(name, ".metadata") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		rep.ManifestsScanned++

		if strings.HasPrefix(name, "sha256:") {
			// By-digest file. Doesn't contribute a ref to itself; if it's
			// an index, contributes +1 per sub-manifest.
			hash := strings.TrimPrefix(name, "sha256:")
			byDigestPath[hash] = path
			bodyCache[hash] = body
			for _, sub := range extractSubManifestDigests(body) {
				subHash := strings.TrimPrefix(sub, "sha256:")
				refs[subHash]++
				rep.IndexSubRefs++
			}
		} else {
			// By-tag file. +1 to its content's digest.
			hash := contentHashOf(body)
			refs[hash]++
			rep.TagRefs++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return rep, fmt.Errorf("walk manifests: %w", err)
	}

	// Cascade-remove orphans (refcount == 0) to a fixed point. Removing
	// an orphan index may drop its sub-manifest refs to 0.
	for {
		progress := false
		for hash, path := range byDigestPath {
			if refs[hash] != 0 {
				continue
			}
			// Orphan: cascade-dec sub-manifest refs, unlink the file.
			body := bodyCache[hash]
			for _, sub := range extractSubManifestDigests(body) {
				subHash := strings.TrimPrefix(sub, "sha256:")
				refs[subHash]--
			}
			info, _ := os.Stat(path)
			if rmErr := os.Remove(path); rmErr == nil && info != nil {
				rep.ManifestsRemoved++
				rep.ManifestBytesFreed += info.Size()
			}
			util.Logerr(os.Remove(path + ".meta"))
			util.Logerr(os.Remove(path + ".metadata"))
			// Drop from tracking so we don't reprocess.
			delete(byDigestPath, hash)
			delete(bodyCache, hash)
			delete(refs, hash)
			progress = true
		}
		if !progress {
			break
		}
	}

	// Wipe existing manifest refcount files; they're stale by definition
	// (we just recomputed from scratch).
	err = filepath.WalkDir(manifestRefcountRoot, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		util.Logerr(os.Remove(path))
		rep.StaleManifestRefcounts++
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return rep, fmt.Errorf("clear manifest refcounts: %w", err)
	}

	// Write fresh refcount files for survivors (refs > 0). refs[h] == 0
	// at this point means either an unreachable that we removed, or a
	// hash that only appeared as a sub-manifest reference but whose
	// own by-digest file doesn't exist (already missing — no file to
	// refcount).
	for hash, count := range refs {
		if count <= 0 {
			continue
		}
		if _, ok := byDigestPath[hash]; !ok {
			// No by-digest file on disk for this hash; skip refcount.
			continue
		}
		if err := writeManifestRefcount(cacheDir, hash, count); err == nil {
			rep.ManifestRefcountsWritten++
		}
	}

	// Sweep orphaned .meta / .metadata sidecars whose base manifest file
	// is gone. These accumulated from pre-2026-05 cascade deletes that
	// missed the .metadata sibling; idempotent on a clean cache.
	_ = filepath.WalkDir(manifestDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".meta") && !strings.HasSuffix(path, ".metadata") {
			return nil
		}
		base := strings.TrimSuffix(strings.TrimSuffix(path, ".metadata"), ".meta")
		if _, err := os.Stat(base); err == nil {
			return nil
		}
		info, _ := os.Stat(path)
		if rmErr := os.Remove(path); rmErr == nil {
			rep.OrphanSidecarsRemoved++
			if info != nil {
				rep.OrphanSidecarBytesFreed += info.Size()
			}
		}
		return nil
	})

	return rep, nil
}

func writeManifestRefcount(cacheDir, hash string, count int) error {
	p := GetManifestRefcountPath(cacheDir, hash)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(fmt.Sprintf("%d", count)), 0644)
}

// contentHashOf returns sha256(body) as a hex string (no "sha256:" prefix).
func contentHashOf(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// extractSubManifestDigests returns sub-manifest digests from an index
// manifest body. Empty for image manifests or unparsable bodies.
func extractSubManifestDigests(body []byte) []string {
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

// MarshalAuditReport returns a JSON-ish summary string suitable for logs.
func (r AuditReport) String() string {
	b, _ := json.Marshal(struct {
		ManifestsScanned      int            `json:"manifests_scanned"`
		BlobsScanned          int            `json:"blobs_scanned"`
		BlobsRemoved          int            `json:"blobs_removed"`
		BytesFreed            int64          `json:"bytes_freed"`
		RefcountsHealed       int            `json:"refcounts_healed"`
		StaleRefcountsRemoved int            `json:"stale_refcounts_removed"`
		MissingBlobWarnings   []string       `json:"missing_blob_warnings,omitempty"`
		LeakAttributionByRepo map[string]int `json:"leak_attribution_by_repo,omitempty"`
	}{r.ManifestsScanned, r.BlobsScanned, r.BlobsRemoved, r.BytesFreed,
		r.RefcountsHealed, r.StaleRefcountsRemoved, r.MissingBlobWarnings,
		r.LeakAttributionByRepo})
	return string(b)
}
