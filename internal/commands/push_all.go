package commands

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/cli"
	"github.com/Delphos-Labs/roxxy/internal/util"
	"golang.org/x/term"
)

// pushAllFromTarball replays every manifest in a tarball to the destination
// registry. Refs are derived from the manifest paths (registry/repo/tag) and
// pushed back to the destination under the same `<repo>:<tag>` — the source
// registry component is dropped, on the assumption you're consolidating
// multiple upstreams into one target.
//
// If platform is set and a ref resolves to a multi-arch index, only the
// matching child manifest is pushed (under the original tag).
//
// Progress is reported as distinct blobs established (uploaded once, then
// cross-repo-mounted to other refs) out of the total distinct-blob count, so
// the headline reflects real upload work rather than the dedup-inflated
// (repo×digest) slot count. Bytes and rate track actual wire transfer.
func pushAllFromTarball(_ Config, registry, tarballPath, platform string, workers int, coerceDocker bool) {
	reader, err := cli.OpenTarball(tarballPath)
	if err != nil {
		fmt.Printf("Error opening tarball: %v\n", err)
		os.Exit(1)
	}
	defer util.DeferClose(reader)

	manifestPaths, err := reader.ListManifests()
	if err != nil {
		fmt.Printf("Error listing manifests: %v\n", err)
		os.Exit(1)
	}

	refs, sizes, skippedUnparseable, skippedErr, err := buildPushRefsFromTarball(reader, manifestPaths, platform)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	if len(refs) == 0 {
		fmt.Println("No pushable manifests found in tarball")
		os.Exit(1)
	}

	var totalBytes int64
	var totalBlobs int
	uniqueBlobs := make(map[string]struct{})
	for _, r := range refs {
		for _, d := range r.blobDigests {
			totalBytes += sizes[d]
			totalBlobs++
			uniqueBlobs[d] = struct{}{}
		}
	}

	rc, err := newRegistryClient(registry, workers)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	platformSuffix := ""
	if platform != "" {
		platformSuffix = fmt.Sprintf(" [%s]", platform)
	}
	fmt.Printf("Pushing %d ref(s) from %s to %s%s\n", len(refs), tarballPath, registry, platformSuffix)
	fmt.Printf("Total: %d blob slot(s) across %d distinct blob(s), %s of data, %d worker(s)\n", totalBlobs, len(uniqueBlobs), formatBytes(totalBytes), workers)
	fmt.Println()

	progress := newProgressReporter(len(uniqueBlobs))
	progress.start()

	coord := newPushCoordinator(rc, reader, workers, progress, sizes, coerceDocker)
	outcome, err := coord.Run(refs)
	progress.stop()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	pushed := len(refs) - len(outcome.conflicts) - len(outcome.failures)
	fmt.Printf("\nSuccessfully pushed %d ref(s) to %s\n", pushed, registry)
	// Per-manifest read errors are usually benign — roxxy's cache
	// indexes bare-name aliases (argo-cd:8.1.3, busybox:1.28) for
	// bookkeeping while the real OCI manifest lives at the namespaced
	// path (argo/argo-cd:8.1.3, library/busybox:1.28). FindManifestByRepoTag
	// returning "manifest not found" for the bare-name entries is
	// expected; the namespaced ones push normally. Just surface a count
	// for transparency, no non-zero exit.
	if skippedErr > 0 {
		fmt.Printf("\n%d ref(s) skipped with read errors (typically bare-name aliases with no real manifest; see stderr above)\n", skippedErr)
	}
	if len(outcome.conflicts) > 0 {
		fmt.Printf("\n%d ref(s) skipped due to tag conflicts:\n", len(outcome.conflicts))
		for _, c := range outcome.conflicts {
			fmt.Printf("  %s:%s (registry has %s)\n", c.repoName, c.tag, c.conflictDgst)
		}
		fmt.Println("\nThe destination registry has tag immutability enabled and these")
		fmt.Println("tags already point at different content. To overwrite, delete the")
		fmt.Println("existing tags first or push under a different tag.")
	}

	// Genuine push failures (manifest push errored, coercion refused) mean the
	// destination is missing content the bundle claims to ship. Report them
	// loudly and exit non-zero — distinct from the benign tag-conflict case
	// above, which leaves the existing (correct) content intact.
	if len(outcome.failures) > 0 {
		fmt.Printf("\n%d ref(s) FAILED to push — destination registry is INCOMPLETE:\n", len(outcome.failures))
		for _, f := range outcome.failures {
			fmt.Printf("  %s:%s — %s\n", f.repoName, f.tag, f.failReason)
		}
		fmt.Println("These refs did not land. Investigate before deploying.")
	}

	// Unparseable manifest paths ARE a real problem — they used to hide helm OCI
	// chart artifacts (parseManifestPath only knows `docker-manifests/`, dropping
	// anything else silently). If any landed in the skip list, the destination is
	// missing content the tarball claims to ship — fail loud (exit 2, below).
	if len(skippedUnparseable) > 0 {
		fmt.Printf("\n%d unparseable manifest path(s) skipped — destination registry is INCOMPLETE:\n", len(skippedUnparseable))
		for i, p := range skippedUnparseable {
			if i >= 10 {
				fmt.Printf("  ...and %d more\n", len(skippedUnparseable)-10)
				break
			}
			fmt.Printf("  %s\n", p)
		}
		fmt.Println("These are typically helm OCI charts that --all doesn't recognize. Investigate before deploying.")
	}

	if code := pushAllExitCode(outcome, skippedUnparseable); code != 0 {
		os.Exit(code)
	}
}

// pushAllExitCode maps a `push --all` result to a process exit code: 2 if any
// ref FAILED to push or any manifest path was unparseable (the destination
// registry is INCOMPLETE — do not deploy), else 0. Benign tag conflicts alone
// are NOT fatal (existing content is intact), so a conflicts-only run exits 0.
// Extracted from pushAllFromTarball so the contract is unit-testable without
// intercepting os.Exit.
func pushAllExitCode(outcome pushOutcome, skippedUnparseable []string) int {
	if len(outcome.failures) > 0 || len(skippedUnparseable) > 0 {
		return 2
	}
	return 0
}

// buildPushRefsFromTarball materializes a pushRef per manifest path and
// returns it alongside the digest→size map for progress denominators. Skips
// unparseable paths and digest-only refs with a stderr note.
//
// Caller-visible failure modes the caller MUST surface (don't swallow):
//   - skippedUnparseable: manifest paths whose prefix isn't `docker-manifests/`.
//     These are typically helm OCI chart artifacts in the tarball that
//     parseManifestPath doesn't understand — and silently dropping them
//     means `roar push --all` returns success while leaving the
//     destination missing every helm chart in the bundle. Surface a count
//     in the run summary AND exit non-zero so operators don't deploy
//     against half-populated registries.
func buildPushRefsFromTarball(reader *cli.TarballReader, manifestPaths []string, platform string) (refs []*pushRef, sizes map[string]int64, skippedUnparseable []string, skippedErr int, err error) {
	for _, p := range manifestPaths {
		repo, tag, ok := parseManifestPath(p)
		if !ok {
			fmt.Fprintf(os.Stderr, "  Skipping unparseable manifest path: %s\n", p)
			skippedUnparseable = append(skippedUnparseable, p)
			continue
		}
		if strings.HasPrefix(tag, "sha256:") {
			continue
		}
		manifestData, _, blobDigests, mErr := reader.FindManifestByRepoTag(repo, tag, platform)
		if mErr != nil {
			fmt.Fprintf(os.Stderr, "  Skipping %s:%s: %v\n", repo, tag, mErr)
			skippedErr++
			continue
		}
		refs = append(refs, &pushRef{
			repoName:       repo,
			tag:            tag,
			manifestData:   manifestData,
			manifestDigest: digestOfBytes(manifestData),
			blobDigests:    blobDigests,
		})
	}
	sizes, err = reader.BlobSizes()
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("read blob sizes: %w", err)
	}
	return refs, sizes, skippedUnparseable, skippedErr, nil
}

// parseManifestPath inverts the `docker-manifests/<registryID>/<safeRepo>/<tag>`
// layout. The original (cached-from) registry is discarded; we only need
// the repo and tag for the destination push. If safeRepo collided with a
// real underscore in the source repo this loses information, but that's
// already a pre-existing tarball roundtrip limitation.
func parseManifestPath(path string) (repo, tag string, ok bool) {
	rest := strings.TrimPrefix(path, "docker-manifests/")
	if rest == path {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", "", false
	}
	repo = UnsafeRepoName(parts[1])
	tag = parts[2]
	return repo, tag, true
}

// progressReporter renders distinct-blob progress to stderr on a timer. The
// renderer runs on its own ticker so we don't redraw mid-blob and don't pin
// rendering rate to network burstiness. Two modes:
//
//   - tty: keeps a single "sticky" status line at the bottom that overwrites
//     itself in place. Stdout log lines flow through Logf, which clears the
//     sticky line, prints the log, and repaints the status below — so the
//     visual is exactly "scrolling logs above, fixed progress bar below."
//
//   - non-tty (pipe, file, CI log): emits the status as ordinary newline-
//     terminated lines on a slower cadence, so the captured log stays readable
//     and grep-friendly. Logf becomes a plain stdout write.
type progressReporter struct {
	uniqueTotal int // distinct blobs to establish; each digest uploads once, mounts elsewhere
	inTTY       bool

	mu             sync.Mutex
	done           int64           // real bytes transferred over the wire; mounts/skips/present add nothing
	pushN          int             // real uploads (PUT bodies), including mount-fallback uploads
	skipN          int             // deduped: cross-repo mounts, same-repo skips, already-present blobs
	established    int             // count of distinct digests landed (uploaded, mounted, or present)
	establishedSet map[string]bool // dedups establishment across job + skip-manifest paths
	inFlight       int             // real uploads currently in progress (HEAD/mount excluded)
	start0         time.Time

	stopCh chan struct{}
	wg     sync.WaitGroup
}

func newProgressReporter(uniqueTotal int) *progressReporter {
	return &progressReporter{
		uniqueTotal:    uniqueTotal,
		establishedSet: make(map[string]bool),
		inTTY:          term.IsTerminal(int(os.Stderr.Fd())),
		stopCh:         make(chan struct{}),
	}
}

func (p *progressReporter) start() {
	p.start0 = time.Now()
	tickEvery := 500 * time.Millisecond
	if !p.inTTY {
		// In non-tty mode every paint becomes a permanent line in the log,
		// so slow down to keep captured logs readable.
		tickEvery = 5 * time.Second
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTicker(tickEvery)
		defer t.Stop()
		for {
			select {
			case <-p.stopCh:
				return
			case <-t.C:
				p.mu.Lock()
				p.paintLocked()
				p.mu.Unlock()
			}
		}
	}()
}

func (p *progressReporter) stop() {
	close(p.stopCh)
	p.wg.Wait()
	p.mu.Lock()
	p.paintLocked()
	if p.inTTY {
		// Leave the final sticky line in place and move past it.
		fmt.Fprintln(os.Stderr)
	}
	p.mu.Unlock()
}

// add marks a blob as fully pushed. The bytes themselves were already
// credited to `done` byte-by-byte via addBytes during the upload stream;
// this just bumps the per-blob counter so the "N pushed" display ticks.
func (p *progressReporter) add(_ string, _ int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.pushN++
	p.mu.Unlock()
}

// addBytes credits bytes-in-flight to the progress total. Called from a
// reader wrapper around the streamed PUT body so the rate display stays
// live during multi-minute large-blob uploads.
func (p *progressReporter) addBytes(n int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.done += n
	p.mu.Unlock()
}

// skip records a deduplicated blob: a cross-repo mount, a same-repo skip, or a
// blob already present in the registry. No bytes crossed the wire, so `done` is
// untouched — only the deduped counter ticks.
func (p *progressReporter) skip() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.skipN++
	p.mu.Unlock()
}

// blobEstablished records that a distinct digest has landed in the registry —
// uploaded fresh, mounted, or confirmed already-present. Called once per
// completed job (and per skip-manifest blob); the set dedups so each distinct
// digest counts once even when it appears in many refs. uniqueTotal minus
// established is the real blobs-remaining count.
func (p *progressReporter) blobEstablished(digest string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if !p.establishedSet[digest] {
		p.establishedSet[digest] = true
		p.established++
	}
	p.mu.Unlock()
}

// uploadStarted/uploadFinished bracket a real upload so the status line can
// show how many workers are actively transferring bytes right now. HEAD and
// mount calls are excluded because they finish too fast to be a useful
// snapshot signal — counting them just makes the number flicker.
func (p *progressReporter) uploadStarted() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.inFlight++
	p.mu.Unlock()
}

func (p *progressReporter) uploadFinished() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
}

// Logf writes a log line to stdout, taking care to clear and repaint the
// sticky progress line on tty so the line never gets stomped or duplicated.
// Safe on a nil receiver — falls back to plain fmt.Printf for callers that
// don't have a reporter (single-ref push).
func (p *progressReporter) Logf(format string, args ...any) {
	if p == nil {
		fmt.Printf(format, args...)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inTTY {
		// Clear the current stderr line (where the sticky progress lives)
		// before writing to stdout, so it doesn't get appended to.
		fmt.Fprint(os.Stderr, "\r\033[2K")
	}
	fmt.Printf(format, args...)
	if p.inTTY {
		p.paintLocked()
	}
}

// paintLocked writes the status line. Caller must hold p.mu. The headline is
// distinct-blob progress (established/total and blobs-remaining) — the real
// upload work — rather than the dedup-inflated (repo×digest) slot count.
// Bytes/rate report actual wire transfer; mounts and skips move no bytes.
func (p *progressReporter) paintLocked() {
	elapsed := time.Since(p.start0)
	rate := float64(0)
	if elapsed.Seconds() > 0.1 {
		rate = float64(p.done) / elapsed.Seconds()
	}
	remaining := p.uniqueTotal - p.established
	if remaining < 0 {
		remaining = 0
	}

	line := fmt.Sprintf("  [%d/%d blobs · %d left · %d uploading] %d uploaded, %d deduped · %s @ %s/s",
		p.established, p.uniqueTotal, remaining, p.inFlight, p.pushN, p.skipN, formatBytes(p.done), formatBytes(int64(rate)))
	if p.inTTY {
		fmt.Fprintf(os.Stderr, "\r\033[2K%s", line)
	} else {
		fmt.Fprintf(os.Stderr, "%s\n", line)
	}
}
