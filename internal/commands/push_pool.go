package commands

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/cli"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// pushRef is one ref destined for the registry. The coordinator owns it for
// the lifetime of a push run; callers populate it from the tarball pre-pass
// and hand it over.
//
// The manifest's mediatype is *not* stored — it's computed lazily at push
// time so the coordinator's coerceDocker flag can be honored, and so a
// chart-coercion error can fail the ref without disrupting other refs.
type pushRef struct {
	repoName       string
	tag            string
	manifestData   []byte
	manifestDigest string // sha256 of the bytes we'll actually push
	blobDigests    []string

	// Filled by coordinator; not for caller use.
	outstanding  atomic.Int32 // blobs left to land before the manifest goes up
	skipManifest atomic.Bool  // tag already at content-equivalent manifest, or conflict
	tagConflict  atomic.Bool  // tag points at different content, mount-incompatible
	conflictDgst string       // existing digest in registry, for the conflict report

	// pushFailed marks a *genuine* manifest-push failure (network/auth error,
	// or a refused coercion) as distinct from a benign tag-immutability
	// conflict. A conflict is the registry correctly rejecting an overwrite of
	// existing content; a failure means the ref did NOT land and the push must
	// exit non-zero. Kept separate from tagConflict so the run summary reports
	// each under the right heading with the right exit code.
	pushFailed atomic.Bool
	failReason string // human-readable cause for the failure report
}

// blobJob is one (repo, sha) pair the workers need to make true in the
// registry. For a sha shared by N refs, the coordinator enqueues N jobs.
type blobJob struct {
	repo   string
	digest string
	ref    *pushRef
}

// pushQueue is a plain LPT-ordered work queue: a slice of jobs plus an
// outstanding counter, guarded by a cond. A worker blocks only when the queue
// is momentarily empty but jobs are still outstanding (a peer may pushBack a
// deferred job or finish one). Per-digest dedup does NOT live here — it lives
// in dedupChecker, queried per job.
type pushQueue struct {
	mu          sync.Mutex
	cond        *sync.Cond
	items       []blobJob
	outstanding int // jobs not yet finished (in-queue + in-flight)
	closed      bool
}

func newPushQueue() *pushQueue {
	q := &pushQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// seed adds the initial job set and wakes any waiting workers.
func (q *pushQueue) seed(jobs []blobJob) {
	q.mu.Lock()
	q.items = append(q.items, jobs...)
	q.outstanding += len(jobs)
	q.cond.Broadcast()
	q.mu.Unlock()
}

// pop returns the next job, or (_, false) when all work is done. Blocks while
// the queue is momentarily empty but jobs are still outstanding (e.g. every
// remaining job was deferred and is being re-enqueued by its peer).
func (q *pushQueue) pop() (blobJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if q.outstanding == 0 || q.closed {
			q.cond.Broadcast()
			return blobJob{}, false
		}
		if len(q.items) > 0 {
			j := q.items[0]
			q.items = q.items[1:]
			return j, true
		}
		q.cond.Wait()
	}
}

// pushBack re-enqueues a deferred job (its digest is being established by a
// peer) without changing outstanding.
func (q *pushQueue) pushBack(j blobJob) {
	q.mu.Lock()
	q.items = append(q.items, j)
	q.cond.Broadcast()
	q.mu.Unlock()
}

// finishJob decrements outstanding for a truly-completed job. If the queue
// is now drained, all waiting pop()s wake and return.
func (q *pushQueue) finishJob() {
	q.mu.Lock()
	q.outstanding--
	if q.outstanding == 0 {
		q.cond.Broadcast()
	}
	q.mu.Unlock()
}

// shutdown unblocks every waiter, used on error to terminate the pool.
func (q *pushQueue) shutdown() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// dedupVerdict tells a worker how to satisfy a blob job.
type dedupVerdict int

const (
	vClaim dedupVerdict = iota // first to see this digest: HEAD then upload, then report
	vMount                     // digest known in srcRepo: try mount, else upload
	vSkip                      // digest already landed in THIS repo: nothing to do
	vDefer                     // digest is being established by a peer: requeue + retry
)

type dedupResult struct {
	verdict dedupVerdict
	srcRepo string // set for vMount
}

// dedupChecker is a lock-free, channel-based singleton. A single goroutine owns
// all dedup state; the resolve/report methods wrap a channel round-trip, so
// there are no mutexes. It replaces the per-digest inflight + source maps that
// used to live on pushQueue. No blocking I/O happens in the goroutine — workers
// do the HEAD/upload/mount and report results back.
type dedupChecker struct {
	queries chan dedupQuery
	reports chan dedupDone
	quit    chan struct{}
}

type dedupQuery struct {
	digest, repo string
	reply        chan dedupResult
}

type dedupDone struct {
	digest, repo string
	landed       bool // blob now exists at repo (uploaded ok or found already present)
}

func newDedupChecker() *dedupChecker {
	d := &dedupChecker{
		queries: make(chan dedupQuery),
		reports: make(chan dedupDone),
		quit:    make(chan struct{}),
	}
	go d.run()
	return d
}

func (d *dedupChecker) run() {
	inflight := map[string]bool{} // digest claimed by a worker, upload in progress
	source := map[string]string{} // digest -> a repo that holds it (mount source)
	for {
		select {
		case q := <-d.queries:
			src := source[q.digest]
			switch {
			case src != "" && src == q.repo:
				q.reply <- dedupResult{verdict: vSkip}
			case src != "":
				q.reply <- dedupResult{verdict: vMount, srcRepo: src}
			case inflight[q.digest]:
				q.reply <- dedupResult{verdict: vDefer}
			default:
				inflight[q.digest] = true
				q.reply <- dedupResult{verdict: vClaim}
			}
		case r := <-d.reports:
			delete(inflight, r.digest)
			if r.landed {
				if _, ok := source[r.digest]; !ok {
					source[r.digest] = r.repo
				}
			}
		case <-d.quit:
			return
		}
	}
}

// resolve asks what to do with (digest, repo). Round-trips through the actor.
func (d *dedupChecker) resolve(digest, repo string) dedupResult {
	reply := make(chan dedupResult, 1)
	d.queries <- dedupQuery{digest: digest, repo: repo, reply: reply}
	return <-reply
}

// report tells the actor a claimed upload finished. landed=true records repo as
// a mount source for the digest; either way it clears the inflight claim so the
// digest's deferred peers can resolve (to vMount, or to vClaim again on failure).
func (d *dedupChecker) report(digest, repo string, landed bool) {
	d.reports <- dedupDone{digest: digest, repo: repo, landed: landed}
}

func (d *dedupChecker) stop() { close(d.quit) }

// pushCoordinator dispatches work across N workers, with the queue handling
// per-digest contention via re-enqueue rather than blocking-and-waiting.
type pushCoordinator struct {
	rc       *registryClient
	reader   *cli.TarballReader
	workers  int
	progress *progressReporter
	sizes    map[string]int64 // digest -> byte size, for LPT-ordered enqueue
	dedup    *dedupChecker    // lock-free per-digest dedup actor (created in Run)

	// coerceDocker forces docker manifest v2 mediatype on push, except for
	// helm charts where coercion is rejected (would produce an unusable
	// artifact). Off by default; opt-in for callers that need to push to
	// docker-only registries that reject OCI image manifests.
	coerceDocker bool
}

func newPushCoordinator(rc *registryClient, reader *cli.TarballReader, workers int, progress *progressReporter, sizes map[string]int64, coerceDocker bool) *pushCoordinator {
	if workers < 1 {
		workers = 1
	}
	return &pushCoordinator{
		rc: rc, reader: reader, workers: workers,
		progress: progress, sizes: sizes,
		coerceDocker: coerceDocker,
	}
}

// pushOutcome partitions the refs the coordinator processed into the two
// categories callers must report differently:
//
//   - conflicts: the destination tag already points at different content and
//     the registry has immutability on. Benign — the caller reports it and
//     exits 0 in --all mode (the existing content is intact).
//   - failures: the ref genuinely did NOT land (manifest push errored, or a
//     coercion was refused). The caller MUST exit non-zero — the destination
//     is missing content the push claimed to deliver.
type pushOutcome struct {
	conflicts []*pushRef
	failures  []*pushRef
}

// Run is the main entry point. Phase 1 parallelizes the manifest pre-check;
// phase 2 builds the blob job list (skipping refs that don't need any work);
// phase 3 drains the job queue. Returns the partitioned outcome (tag conflicts
// vs genuine push failures); both are non-fatal to the pool but only conflicts
// are non-fatal to the overall exit code.
func (c *pushCoordinator) Run(refs []*pushRef) (pushOutcome, error) {
	if err := c.precheckManifests(refs); err != nil {
		return pushOutcome{}, err
	}

	jobs := c.buildBlobJobs(refs)

	c.dedup = newDedupChecker()
	defer c.dedup.stop()

	if err := c.runWorkers(jobs); err != nil {
		return pushOutcome{}, err
	}

	var out pushOutcome
	for _, r := range refs {
		switch {
		case r.pushFailed.Load():
			out.failures = append(out.failures, r)
		case r.tagConflict.Load():
			out.conflicts = append(out.conflicts, r)
		}
	}
	return out, nil
}

// precheckManifests parallelizes the per-ref HEAD-manifest + content-
// equivalence comparison so the long-tail of small refs doesn't serialize
// on auth round-trips.
func (c *pushCoordinator) precheckManifests(refs []*pushRef) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobCh := make(chan *pushRef)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	for range c.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobCh {
				if ctx.Err() != nil {
					return
				}
				if err := c.precheckOne(r); err != nil {
					select {
					case errCh <- err:
						cancel()
					default:
					}
					return
				}
			}
		}()
	}
dispatch:
	for _, r := range refs {
		select {
		case <-ctx.Done():
			break dispatch
		case jobCh <- r:
		}
	}
	close(jobCh)
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func (c *pushCoordinator) precheckOne(r *pushRef) error {
	existing, err := c.rc.manifestDigest(r.repoName, r.tag)
	if err != nil {
		return fmt.Errorf("HEAD manifest %s:%s: %w", r.repoName, r.tag, err)
	}
	if existing == "" {
		return nil
	}
	if existing == r.manifestDigest {
		c.progress.Logf("  %s:%s already at %s, skipping\n", r.repoName, r.tag, r.manifestDigest[:19])
		r.skipManifest.Store(true)
		return nil
	}
	equiv, cmpErr := manifestsEquivalent(c.rc, r.repoName, r.tag, r.manifestData)
	if cmpErr == nil && equiv {
		c.progress.Logf("  %s:%s content-equivalent (bytes differ, image identical), skipping\n", r.repoName, r.tag)
		r.skipManifest.Store(true)
		return nil
	}
	c.progress.Logf("  %s:%s conflict: registry has %s, computed %s\n",
		r.repoName, r.tag, existing[:19], r.manifestDigest[:19])
	r.tagConflict.Store(true)
	r.conflictDgst = existing
	r.skipManifest.Store(true)
	return nil
}

// buildBlobJobs converts each ref's blob list into per-(repo, digest) jobs.
// Refs whose manifests already match in the registry are skipped wholesale,
// and their blob counts are credited to the progress bar so the percentage
// reflects "this ref needs no further work."
func (c *pushCoordinator) buildBlobJobs(refs []*pushRef) []blobJob {
	var jobs []blobJob
	for _, r := range refs {
		if r.skipManifest.Load() {
			for _, d := range r.blobDigests {
				c.progress.skip()
				c.progress.blobEstablished(d)
			}
			continue
		}
		r.outstanding.Store(int32(len(r.blobDigests)))
		for _, d := range r.blobDigests {
			jobs = append(jobs, blobJob{repo: r.repoName, digest: d, ref: r})
		}
	}
	return jobs
}

func (c *pushCoordinator) runWorkers(jobs []blobJob) error {
	if len(jobs) == 0 {
		return nil
	}
	// LPT scheduling: sort by descending size so the biggest blobs start at
	// time zero. Workers stay saturated longer (small blobs fill gaps as
	// they free up), and the makespan is bounded by the longest single
	// blob's transfer time rather than waiting on a late-scheduled big one.
	sort.SliceStable(jobs, func(i, j int) bool {
		return c.sizes[jobs[i].digest] > c.sizes[jobs[j].digest]
	})

	totalJobs := len(jobs)
	var doneCount atomic.Int32

	q := newPushQueue()
	q.seed(jobs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	for w := 0; w < c.workers; w++ {
		workerID := w // pin this worker to pool connection clientFor(workerID)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				j, ok := q.pop()
				if !ok {
					return
				}
				deferred, err := c.processJob(ctx, j, totalJobs, &doneCount, workerID)
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					q.shutdown()
					cancel()
					return
				}
				if deferred {
					// Digest is being established by a peer (vDefer). Requeue and
					// pick up other work; the brief sleep caps CPU when the only
					// work left is duplicates of a single in-flight upload.
					q.pushBack(j)
					time.Sleep(2 * time.Millisecond)
					continue
				}
				q.finishJob()
			}
		}()
	}
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

// processJob satisfies one blob job by asking the dedup actor what to do, then
// doing the I/O. Returns (deferred=true) when the digest is being established
// by a peer (vDefer): the worker requeues and picks up other work; by the time
// the job comes back, resolve() returns vMount.
func (c *pushCoordinator) processJob(ctx context.Context, j blobJob, totalJobs int, doneCount *atomic.Int32, workerID int) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	switch res := c.dedup.resolve(j.digest, j.repo); res.verdict {
	case vSkip:
		// Already landed in this repo earlier this run.
		c.progress.skip()
		idx := doneCount.Add(1)
		c.progress.Logf("  [%d/%d] %s in %s: already present\n", idx, totalJobs, j.digest[:12], j.repo)
		c.afterBlob(j)
		return false, nil

	case vDefer:
		// A peer is establishing this digest; come back for the cheap mount path.
		return true, nil

	case vMount:
		// Known elsewhere: try a cross-repo mount (no bytes on the wire). On a
		// registry without mount support tryMount declines (202, ok=false) and
		// we upload to this repo instead. Jobs are unique per (repo,digest), so
		// no claim is needed for the fallback upload.
		if ok, mErr := c.rc.tryMount(j.repo, res.srcRepo, j.digest); mErr == nil && ok {
			c.progress.skip()
			idx := doneCount.Add(1)
			c.progress.Logf("  [%d/%d] %s mounted %s -> %s\n", idx, totalJobs, j.digest[:12], res.srcRepo, j.repo)
			c.afterBlob(j)
			return false, nil
		} else if mErr != nil {
			fmt.Fprintf(os.Stderr, "  Warning: mount %s from %s -> %s failed (%v); uploading instead\n", j.digest, res.srcRepo, j.repo, mErr)
		}
		size, err := c.realUpload(j, workerID)
		if err != nil {
			return false, fmt.Errorf("upload %s -> %s: %w", j.digest, j.repo, err)
		}
		c.progress.add(j.digest, size)
		idx := doneCount.Add(1)
		c.progress.Logf("  [%d/%d] %s -> %s (%s) uploaded\n", idx, totalJobs, j.digest[:12], j.repo, formatBytes(size))
		c.afterBlob(j)
		return false, nil

	default: // vClaim — first to see this digest; we own the upload.
		// HEAD first so a resume skips an already-present blob. Report either
		// way: success records this repo as a mount source for deferred peers;
		// failure clears the claim so a peer can re-claim.
		if exists, err := c.rc.blobExists(j.repo, j.digest); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: HEAD %s in %s failed: %v (will attempt upload)\n", j.digest, j.repo, err)
		} else if exists {
			c.dedup.report(j.digest, j.repo, true)
			c.progress.skip()
			idx := doneCount.Add(1)
			c.progress.Logf("  [%d/%d] %s in %s: already present\n", idx, totalJobs, j.digest[:12], j.repo)
			c.afterBlob(j)
			return false, nil
		}
		size, err := c.realUpload(j, workerID)
		c.dedup.report(j.digest, j.repo, err == nil)
		if err != nil {
			return false, fmt.Errorf("upload %s -> %s: %w", j.digest, j.repo, err)
		}
		c.progress.add(j.digest, size)
		idx := doneCount.Add(1)
		c.progress.Logf("  [%d/%d] %s -> %s (%s) uploaded\n", idx, totalJobs, j.digest[:12], j.repo, formatBytes(size))
		c.afterBlob(j)
		return false, nil
	}
}

// realUpload streams the blob from the tarball directly to the registry.
// Each call opens its own tarball file handle, so it's safe to invoke from
// any worker. The whole POST+PATCH+PUT sequence is pinned to this worker's
// pool connection (clientFor(workerID)) so it rides one warm connection
// rather than scattering chunks across cold ones. Wrapped with
// progress.uploadStarted/uploadFinished so the status line shows how many
// workers are mid-transfer.
func (c *pushCoordinator) realUpload(j blobJob, workerID int) (int64, error) {
	body, size, err := c.reader.OpenBlob(j.digest)
	if err != nil {
		return 0, fmt.Errorf("open blob: %w", err)
	}
	defer util.DeferClose(body)

	c.progress.uploadStarted()
	defer c.progress.uploadFinished()

	tick := &tickingReader{r: body, p: c.progress}
	vr := &verifyingReader{r: tick, h: sha256.New(), expected: j.digest}
	if err := c.rc.pushBlob(c.rc.clientFor(workerID), j.repo, j.digest, size, vr); err != nil {
		return 0, err
	}
	if err := vr.verify(); err != nil {
		return 0, err
	}
	return size, nil
}

// afterBlob decrements the ref's outstanding counter and, if this was the
// last blob, dispatches the manifest push. Called from any worker.
func (c *pushCoordinator) afterBlob(j blobJob) {
	// Runs once per completed job (any terminal path), before the per-ref
	// outstanding decrement gates manifest finalization. The reporter dedups
	// by digest, so this is the single establishment hook for the job path.
	c.progress.blobEstablished(j.digest)
	if j.ref.outstanding.Add(-1) != 0 {
		return
	}
	if j.ref.skipManifest.Load() {
		return
	}
	mediaType, err := detectManifestMediaType(j.ref.manifestData, c.coerceDocker)
	if err != nil {
		// A refused coercion is a genuine failure: the ref did not land.
		fmt.Fprintf(os.Stderr, "Error: %s:%s: %v\n", j.ref.repoName, j.ref.tag, err)
		j.ref.pushFailed.Store(true)
		j.ref.failReason = err.Error()
		return
	}
	if err := c.rc.pushManifest(j.ref.repoName, j.ref.tag, mediaType, j.ref.manifestData); err != nil {
		// A mid-run manifest push error is a genuine failure (network/auth/5xx),
		// NOT a benign tag-immutability conflict — the ref did not land. Track it
		// separately so the run summary reports it under an error heading and
		// exits non-zero. Real conflicts are detected up front in precheckOne.
		fmt.Fprintf(os.Stderr, "Error pushing manifest %s:%s: %v\n", j.ref.repoName, j.ref.tag, err)
		j.ref.pushFailed.Store(true)
		j.ref.failReason = err.Error()
		return
	}
	c.progress.Logf("  %s:%s manifest pushed (%s)\n", j.ref.repoName, j.ref.tag, j.ref.manifestDigest[:19])

	// Mirror to bare Docker Hub name. Roxxy itself 308-redirects bare names
	// to library/<name> so the canonical key on a roxxy destination is the
	// library/ one — but clients that hit a non-roxxy registry, or an older
	// roxxy without the redirect, need both keys present to resolve a `redis`
	// reference. We treat any "already exists" style response from the second
	// PUT as expected: the registry might canonicalize bare→library itself
	// (409 on duplicate), reject bare names outright (400 NAME_INVALID), or
	// simply accept it (200/201). Only a network/auth error is worth surfacing.
	if bare, ok := strings.CutPrefix(j.ref.repoName, "library/"); ok && bare != "" && !strings.Contains(bare, "/") {
		if err := c.rc.pushManifest(bare, j.ref.tag, mediaType, j.ref.manifestData); err != nil && !isExpectedAliasPushError(err) {
			fmt.Fprintf(os.Stderr, "Note: bare-alias push %s:%s failed (non-fatal): %v\n", bare, j.ref.tag, err)
		}
	}
}

// isExpectedAliasPushError returns true for HTTP statuses that mean "the
// destination already has this content under the canonical name" or "rejects
// bare names" — both are non-errors for the dual-push fanout. The status code
// is embedded in the error string by pushManifest as "status NNN:".
func isExpectedAliasPushError(err error) bool {
	if err == nil {
		return true
	}
	msg := err.Error()
	// 400: NAME_INVALID from registries that require namespaced names.
	// 404: route not registered for bare names on some proxies.
	// 409: duplicate / tag-immutability conflict against existing content.
	return strings.Contains(msg, "status 400:") ||
		strings.Contains(msg, "status 404:") ||
		strings.Contains(msg, "status 409:")
}
