package commands

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

// buildCoordinator opens a synthetic tarball, derives push refs, and wires a
// coordinator against the fake registry. progress is nil throughout: every
// reporter method is nil-safe, so this avoids spawning the render goroutine and
// keeps the test output clean while still exercising the real push paths.
func buildCoordinator(t *testing.T, fr *fakeRegistry, tarballPath string, workers int) (*pushCoordinator, []*pushRef) {
	t.Helper()
	reader := openSynthTarball(t, tarballPath)

	manifestPaths, err := reader.ListManifests()
	if err != nil {
		t.Fatalf("ListManifests: %v", err)
	}
	refs, sizes, skippedUnparseable, skippedErr, err := buildPushRefsFromTarball(reader, manifestPaths, "")
	if err != nil {
		t.Fatalf("buildPushRefsFromTarball: %v", err)
	}
	if len(skippedUnparseable) != 0 || skippedErr != 0 {
		t.Fatalf("unexpected skips: unparseable=%v skippedErr=%d", skippedUnparseable, skippedErr)
	}

	rc, err := newRegistryClient(fr.URL(), workers)
	if err != nil {
		t.Fatalf("newRegistryClient: %v", err)
	}
	coord := newPushCoordinator(rc, reader, workers, nil, sizes, false)
	return coord, refs
}

// distinctBlobDigests collects the unique config+layer digests across refs.
func distinctBlobDigests(refs []*pushRef) map[string]bool {
	set := make(map[string]bool)
	for _, r := range refs {
		for _, d := range r.blobDigests {
			set[d] = true
		}
	}
	return set
}

func TestPushCoordinator_Run(t *testing.T) {
	images, sharedDigest := twoImagesSharingABlob()
	tarballPath := writeSyntheticTarball(t, images, nil)
	fr := newFakeRegistry(t)

	coord, refs := buildCoordinator(t, fr, tarballPath, 4)
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}

	outcome, err := coord.Run(refs)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(outcome.failures) != 0 {
		t.Fatalf("expected no failures, got %d: %+v", len(outcome.failures), outcome.failures)
	}
	if len(outcome.conflicts) != 0 {
		t.Fatalf("expected no conflicts, got %d", len(outcome.conflicts))
	}

	// THE assertion: every distinct blob digest was uploaded exactly once across
	// all repos. The shared layer appears in both images, so it must be uploaded
	// once and mounted (or skipped) for the second — never re-uploaded.
	for digest := range distinctBlobDigests(refs) {
		if got := fr.totalUploads(digest); got != 1 {
			t.Errorf("blob %s uploaded %d times across all repos, want exactly 1", digest, got)
		}
	}
	if got := fr.totalUploads(sharedDigest); got != 1 {
		t.Errorf("shared blob %s uploaded %d times, want exactly 1 (should be deduped/mounted)", sharedDigest, got)
	}

	// Every ref's manifest landed.
	for _, r := range refs {
		if got := fr.manifestPushCount(r.repoName); got < 1 {
			t.Errorf("manifest for %s never pushed", r.repoName)
		}
	}
}

func TestPushCoordinator_Run_ErrorSurfaces(t *testing.T) {
	images, sharedDigest := twoImagesSharingABlob()
	tarballPath := writeSyntheticTarball(t, images, nil)
	fr := newFakeRegistry(t)
	// Force the shared blob's commit PUT to 500 on every attempt.
	fr.failUpload(sharedDigest)

	coord, refs := buildCoordinator(t, fr, tarballPath, 4)

	// If the worker pool fails to shut down on the upload error, Run never
	// returns. runWithDeadline bounds that so a deadlock fails THIS test in 30s
	// with a clear message instead of hanging the package to the global -timeout.
	outcome, err := runWithDeadline(t, coord, refs)

	// The failure must surface either as a returned error (the pool aborts on
	// the first upload error) or as a populated outcome — never silently
	// swallowed.
	if err == nil && len(outcome.failures) == 0 {
		t.Fatalf("upload 500 was not surfaced: err=nil and outcome.failures empty")
	}
	if err != nil && !strings.Contains(err.Error(), "upload") {
		t.Fatalf("error did not mention the upload: %v", err)
	}
}

func TestBuildPushRefsFromTarball(t *testing.T) {
	images, _ := twoImagesSharingABlob()
	// Inject one manifest path that parseManifestPath cannot parse (not under
	// docker-manifests/) — it must land in skippedUnparseable, not in refs.
	extra := map[string]string{
		"helm-charts/library/mychart/1.2.3": `{"schemaVersion":2}`,
	}
	tarballPath := writeSyntheticTarball(t, images, extra)
	reader := openSynthTarball(t, tarballPath)

	manifestPaths, err := reader.ListManifests()
	if err != nil {
		t.Fatalf("ListManifests: %v", err)
	}

	refs, _, skippedUnparseable, skippedErr, err := buildPushRefsFromTarball(reader, manifestPaths, "")
	if err != nil {
		t.Fatalf("buildPushRefsFromTarball: %v", err)
	}

	if len(refs) != 2 {
		t.Errorf("expected 2 parseable refs, got %d", len(refs))
	}
	// The helm-charts/ path isn't under docker-manifests/, so ListManifests
	// won't surface it (it only lists docker-manifests/). The partition still
	// must not misclassify the two real refs as errors.
	if skippedErr != 0 {
		t.Errorf("expected 0 read-error skips, got %d", skippedErr)
	}
	_ = skippedUnparseable // see TestBuildPushRefsFromTarball_Unparseable below

	// Each ref carries its computed manifest digest matching sha256(bytes).
	for _, r := range refs {
		sum := sha256.Sum256(r.manifestData)
		want := fmt.Sprintf("sha256:%x", sum)
		if r.manifestDigest != want {
			t.Errorf("%s:%s manifestDigest=%s want %s", r.repoName, r.tag, r.manifestDigest, want)
		}
		if len(r.blobDigests) != 3 {
			t.Errorf("%s:%s has %d blob digests, want 3 (config + 2 layers)", r.repoName, r.tag, len(r.blobDigests))
		}
	}
}

func TestBuildPushRefsFromTarball_Unparseable(t *testing.T) {
	images, _ := twoImagesSharingABlob()
	tarballPath := writeSyntheticTarball(t, images, nil)
	reader := openSynthTarball(t, tarballPath)

	realPaths, err := reader.ListManifests()
	if err != nil {
		t.Fatalf("ListManifests: %v", err)
	}
	// Feed buildPushRefsFromTarball a manually-extended path list including a
	// path whose prefix isn't docker-manifests/ — that path must be partitioned
	// into skippedUnparseable, the valid ones into refs.
	paths := append([]string{"oci-charts/library/mychart/1.2.3"}, realPaths...)

	refs, _, skippedUnparseable, skippedErr, err := buildPushRefsFromTarball(reader, paths, "")
	if err != nil {
		t.Fatalf("buildPushRefsFromTarball: %v", err)
	}
	if len(refs) != 2 {
		t.Errorf("expected 2 refs, got %d", len(refs))
	}
	if len(skippedUnparseable) != 1 || skippedUnparseable[0] != "oci-charts/library/mychart/1.2.3" {
		t.Errorf("expected the non-docker-manifests path in skippedUnparseable, got %v", skippedUnparseable)
	}
	if skippedErr != 0 {
		t.Errorf("expected 0 read-error skips, got %d", skippedErr)
	}
}

// TestPushBlobChunked_Boundaries drives the real OCI chunked-upload path
// directly. pushBlobChunked is gated in pushBlob behind a 3 GiB threshold that
// a test can't reach with a real blob, so we call the unexported method
// directly (white-box) with ROAR_CHUNK_SIZE_MIB=1, the smallest override that
// forces multiple chunks for a multi-MiB body.
func TestPushBlobChunked_Boundaries(t *testing.T) {
	t.Setenv("ROAR_CHUNK_SIZE_MIB", "1") // 1 MiB chunks

	fr := newChunkRecordingRegistry(t)
	rc, err := newRegistryClient(fr.URL(), 1)
	if err != nil {
		t.Fatalf("newRegistryClient: %v", err)
	}

	// 3.5 MiB forces 4 chunks (1+1+1+0.5).
	const size = 3*1024*1024 + 512*1024
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	sum := sha256.Sum256(payload)
	digest := fmt.Sprintf("sha256:%x", sum)

	location, err := rc.initBlobUpload(rc.clientFor(0), "library/chunked")
	if err != nil {
		t.Fatalf("initBlobUpload: %v", err)
	}
	if err := rc.pushBlobChunked(rc.clientFor(0), "library/chunked", location, digest, size, bytes.NewReader(payload)); err != nil {
		t.Fatalf("pushBlobChunked: %v", err)
	}

	// Ranges must be contiguous, non-overlapping, and cover exactly [0,size).
	fr.assertContiguousCoverage(t, size)
	// Reassembled body equals the input.
	if got := fr.assembled(); !bytes.Equal(got, payload) {
		t.Errorf("reassembled body mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	// Final commit PUT carried ?digest=.
	if fr.finalizeDigest != digest {
		t.Errorf("finalize digest = %q, want %q", fr.finalizeDigest, digest)
	}
}

func TestPushBlobChunked_TruncatedReaderTripsGuard(t *testing.T) {
	t.Setenv("ROAR_CHUNK_SIZE_MIB", "1")

	fr := newChunkRecordingRegistry(t)
	rc, err := newRegistryClient(fr.URL(), 1)
	if err != nil {
		t.Fatalf("newRegistryClient: %v", err)
	}

	// Reader delivers fewer bytes than the declared size: the truncation guard
	// must fire rather than committing a short blob.
	const declaredSize = 4 * 1024 * 1024
	short := bytes.NewReader(make([]byte, declaredSize-1024))
	location, err := rc.initBlobUpload(rc.clientFor(0), "library/short")
	if err != nil {
		t.Fatalf("initBlobUpload: %v", err)
	}
	// Baseline before the chunked upload spawns its prefetch reader goroutine.
	baseGoroutines := runtime.NumGoroutine()
	err = rc.pushBlobChunked(rc.clientFor(0), "library/short", location, "sha256:deadbeef", declaredSize, short)
	if err == nil {
		t.Fatal("expected truncation error, got nil")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("expected a truncation error, got: %v", err)
	}
	// The early-return guard must signal the reader goroutine (close(done)), not
	// leak it parked on a chunk-sized buffer. -race does NOT detect a parked
	// goroutine, so poll for the count to settle back to baseline — this is the
	// regression guard for dropping close(done) / the `case <-done:` arm.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseGoroutines && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseGoroutines {
		t.Errorf("goroutine leak after truncation guard: %d goroutines (baseline %d)", n, baseGoroutines)
	}
}

// chunkRecordingRegistry is a focused fake for the chunked-upload path: it
// records every PATCH's Content-Range and body so the test can verify range
// math and reassembly, and captures the finalize PUT's ?digest=.
type chunkRecordingRegistry struct {
	*fakeRegistry
	// patches preserves arrival order: each is the parsed [start,end] and bytes.
	patchStarts    []int64
	patchEnds      []int64
	patchBodies    [][]byte
	finalizeDigest string
}

func newChunkRecordingRegistry(t *testing.T) *chunkRecordingRegistry {
	t.Helper()
	cr := &chunkRecordingRegistry{}
	base := newFakeRegistry(t)
	cr.fakeRegistry = base
	// Swap the handler to one that records chunk ranges. We override the shared
	// handler so the recording logic stays local to this fake.
	base.server.Config.Handler = http.HandlerFunc(cr.handle)
	return cr
}

// handle wraps the upload path with Content-Range recording, delegating
// everything else to the embedded fakeRegistry.
func (cr *chunkRecordingRegistry) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/blobs/uploads/") {
		body, _ := io.ReadAll(r.Body)
		rng := r.Header.Get("Content-Range")
		var start, end int64
		// A malformed Content-Range leaves start/end at 0; assertContiguousCoverage
		// then catches the resulting gap, so a parse failure can't pass silently.
		_, _ = fmt.Sscanf(rng, "%d-%d", &start, &end)
		cr.mu.Lock()
		cr.patchStarts = append(cr.patchStarts, start)
		cr.patchEnds = append(cr.patchEnds, end)
		cr.patchBodies = append(cr.patchBodies, body)
		cr.mu.Unlock()
		w.Header().Set("Location", r.URL.Path)
		w.Header().Set("Range", fmt.Sprintf("0-%d", end))
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/uploads/") {
		cr.mu.Lock()
		cr.finalizeDigest = r.URL.Query().Get("digest")
		cr.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		return
	}
	cr.fakeRegistry.handle(w, r)
}

func (cr *chunkRecordingRegistry) assembled() []byte {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	var buf bytes.Buffer
	for _, b := range cr.patchBodies {
		buf.Write(b)
	}
	return buf.Bytes()
}

func (cr *chunkRecordingRegistry) assertContiguousCoverage(t *testing.T, size int64) {
	t.Helper()
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if len(cr.patchStarts) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(cr.patchStarts))
	}
	var next int64
	for i := range cr.patchStarts {
		if cr.patchStarts[i] != next {
			t.Errorf("chunk %d starts at %d, want %d (gap/overlap)", i, cr.patchStarts[i], next)
		}
		if cr.patchEnds[i] < cr.patchStarts[i] {
			t.Errorf("chunk %d end %d before start %d", i, cr.patchEnds[i], cr.patchStarts[i])
		}
		next = cr.patchEnds[i] + 1
	}
	if next != size {
		t.Errorf("chunks cover [0,%d), want [0,%d)", next, size)
	}
}

// runWithDeadline runs the coordinator with a bounded timeout so a deadlocked
// pool fails the calling test in 30s with a clear message instead of hanging the
// whole package to the global -timeout.
func runWithDeadline(t *testing.T, coord *pushCoordinator, refs []*pushRef) (pushOutcome, error) {
	t.Helper()
	type result struct {
		outcome pushOutcome
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		o, e := coord.Run(refs)
		ch <- result{o, e}
	}()
	select {
	case r := <-ch:
		return r.outcome, r.err
	case <-time.After(30 * time.Second):
		t.Fatal("coord.Run did not return within 30s — deadlocked")
		return pushOutcome{}, nil
	}
}

// TestPushCoordinator_Run_ManifestFailurePartition: a manifest-push 500 must land
// in outcome.failures (exit 2), NOT outcome.conflicts (exit 0) — the partition
// commit 8d5d5a5 added. The fake's manifest handler returns 201 by default, so
// without failManifest this path was untestable; a refactor mis-routing a
// manifest failure into conflicts would otherwise ship an incomplete registry.
func TestPushCoordinator_Run_ManifestFailurePartition(t *testing.T) {
	images, _ := twoImagesSharingABlob()
	images = images[:1] // single image: library/alpha:1.0
	tarballPath := writeSyntheticTarball(t, images, nil)
	fr := newFakeRegistry(t)
	fr.failManifest("library/alpha") // blobs upload fine; the manifest PUT 500s

	coord, refs := buildCoordinator(t, fr, tarballPath, 2)
	outcome, _ := runWithDeadline(t, coord, refs)

	if len(outcome.conflicts) != 0 {
		t.Errorf("manifest 500 mis-routed into conflicts (would exit 0): %+v", outcome.conflicts)
	}
	if len(outcome.failures) != 1 {
		t.Fatalf("manifest 500 not recorded in outcome.failures: got %d", len(outcome.failures))
	}
	if outcome.failures[0].repoName != "library/alpha" || outcome.failures[0].failReason == "" {
		t.Errorf("failure entry incomplete: repo=%q reason=%q",
			outcome.failures[0].repoName, outcome.failures[0].failReason)
	}
	if code := pushAllExitCode(outcome, nil); code != 2 {
		t.Errorf("pushAllExitCode on a genuine failure = %d, want 2", code)
	}
}
