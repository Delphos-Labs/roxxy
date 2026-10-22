package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/aws/smithy-go"

	"github.com/Delphos-Labs/roxxy/internal/archifau/s3cas"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// replicateTimeout bounds one write-through replication attempt.
const replicateTimeout = 5 * time.Minute

// replicateWorkers caps the number of replication jobs doing S3 work (and thus
// open files / S3 connections) concurrently. The semaphore is acquired inside
// each job's goroutine, so an upload burst may park extra goroutines on it
// (cheap); this bounds concurrent execution and open files, not goroutine count.
const replicateWorkers = 8

// replicateRetries is the number of additional attempts for a retryable S3
// failure before a job gives up (so up to replicateRetries+1 total tries).
// Permanent failures (see isPermanent) break out early.
const replicateRetries = 3

// casPointer is what a name-index meta object resolves to.
type casPointer struct {
	digest      string
	size        int64
	contentType string
}

// streamThroughCAS serves a local read miss from the S3 second level: the CAS
// bytes stream to the client while a temp copy is hashed, and only a verified
// copy is committed to localDest (plus its sidecar via commitMeta). Returns
// false when the CAS object is absent so the caller can 404; any other
// outcome has already written a response.
//
// Content-Length is set up front from the meta, so the response is committed
// before the bytes are verified. If the copy errors or the streamed content
// does not match the expected size/digest, the handler panics with
// http.ErrAbortHandler: Go's HTTP server recovers that silently but forcibly
// breaks the connection, so the client sees a failed transfer rather than a
// clean 200 of unverified (possibly corrupt) bytes. Range requests are served
// whole (200): only locally-cached content gets range support via
// http.ServeFile.
func streamThroughCAS(
	w http.ResponseWriter, r *http.Request,
	store s3cas.Store, ptr casPointer,
	localDest, tmpDir string,
	commitMeta func() error,
) bool {
	rc, err := store.GetCAS(r.Context(), ptr.digest)
	if errors.Is(err, s3cas.ErrNotFound) {
		slog.Warn("s3: meta points at missing CAS object", "digest", ptr.digest, "dest", localDest)
		return false
	}
	if err != nil {
		slog.Error("s3: fetching CAS object", "digest", ptr.digest, "err", err)
		http.Error(w, "upstream storage error", http.StatusBadGateway)
		return true
	}
	defer util.DeferClose(rc)

	ct := ptr.contentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(ptr.size, 10))
	w.Header().Set("X-Content-SHA256", ptr.digest)

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return true
	}

	if err := os.MkdirAll(tmpDir, 0755); err != nil { // cold node: dir may not exist yet
		slog.Error("s3: creating fill temp dir", "dir", tmpDir, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return true
	}
	tmp, err := os.CreateTemp(tmpDir, ".s3fill-*")
	if err != nil {
		slog.Error("s3: creating fill temp", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return true
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	hasher := sha256.New()
	w.WriteHeader(http.StatusOK)
	n, copyErr := io.Copy(io.MultiWriter(hasher, tmp, w), rc)
	if closeErr := tmp.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		slog.Error("s3: streaming CAS object", "digest", ptr.digest, "err", copyErr)
		// Header (incl. Content-Length) is already on the wire; abort the
		// connection so the client never mistakes a truncated body for success.
		panic(http.ErrAbortHandler)
	}
	if sum := hex.EncodeToString(hasher.Sum(nil)); n != ptr.size || sum != ptr.digest {
		slog.Error("s3: CAS content mismatch", "digest", ptr.digest, "got_sha256", sum,
			"size", ptr.size, "got_size", n)
		// Equal-length-but-corrupt content would otherwise satisfy
		// Content-Length and land as a clean 200; abort so it can't.
		panic(http.ErrAbortHandler)
	}

	if err := os.MkdirAll(filepath.Dir(localDest), 0755); err != nil {
		slog.Error("s3: creating cache dir", "dest", localDest, "err", err)
		return true
	}
	if err := os.Rename(tmpName, localDest); err != nil {
		slog.Error("s3: committing cache fill", "dest", localDest, "err", err)
		return true
	}
	if err := commitMeta(); err != nil {
		// The blob is committed but its sidecar is not. We deliberately do NOT
		// remove the blob: a concurrent cold read of the same name may have just
		// renamed its own bytes onto localDest (last writer wins, inode reused),
		// so os.Remove(localDest) could delete that request's freshly-committed
		// blob and leave it with a sidecar pointing at nothing. The orphan blob
		// is harmless on the read path — serving requires the sidecar, and
		// replicateThrough re-reads sidecar state at send time — so leaving it
		// is strictly safer than an inode-imprecise removal.
		slog.Error("s3: writing cache-fill sidecar", "dest", localDest, "err", err)
		return true
	}
	slog.Info("s3: cache fill", "dest", localDest, "sha256", ptr.digest, "size", ptr.size)
	return true
}

// replicaMeta is the subset of a sidecar that replication needs: the content
// digest and size used to push the CAS blob and tag the meta object.
type replicaMeta struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// replicateThrough converges the S3 second level on the current local state of
// one name: absent sidecar => the S3 meta is deleted (CAS blobs are immutable
// and possibly shared, never deleted); present => the CAS blob and meta are
// pushed. It re-reads local state at call time so concurrent writes to one key
// converge on the last local commit. Retryable S3 failures are retried with
// exponential backoff + jitter (permanent failures break out early); a final
// failure is logged at error level tagged with the object and digest so
// divergence is alertable.
func replicateThrough(
	ctx context.Context,
	store s3cas.Store,
	metaKey, sidecarPath, contentPath, logPrefix string,
) {
	raw, err := os.ReadFile(sidecarPath)
	if os.IsNotExist(err) {
		if err := withRetry(ctx, func() error { return store.DeleteMeta(ctx, metaKey) }); err != nil {
			slog.Error(logPrefix+": s3 delete meta gave up", "key", metaKey, "err", err)
		}
		return
	}
	if err != nil {
		slog.Error(logPrefix+": s3 replicate sidecar read", "key", metaKey, "err", err)
		return
	}
	var m replicaMeta
	if err := json.Unmarshal(raw, &m); err != nil || m.SHA256 == "" {
		slog.Error(logPrefix+": s3 replicate sidecar malformed", "key", metaKey, "err", err)
		return
	}
	file, err := os.Open(contentPath)
	if err != nil {
		slog.Error(logPrefix+": s3 replicate content open", "key", metaKey, "err", err)
		return
	}
	defer util.DeferClose(file)

	if err := withRetry(ctx, func() error {
		// Re-verify the fd's current bytes hash to m.SHA256 before every attempt.
		// The CAS is immutable and HeadObject-gated, so a digest that doesn't yet
		// exist in S3 admits whatever we stream under it permanently. Between the
		// initial open and a retry, a concurrent same-length overwrite on the
		// files surface (which renames over an existing path with no immutability
		// gate) can replace these bytes; streaming them under the original digest
		// would poison the store. The per-key stripe lock serializes writers, but
		// the content file is still re-read each attempt, so hash it each time.
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			return seekErr
		}
		if err := verifyDigest(file, m.SHA256); err != nil {
			return err
		}
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			return seekErr
		}
		return store.PutCAS(ctx, m.SHA256, m.Size, file)
	}); err != nil {
		slog.Error(logPrefix+": s3 replicate PutCAS gave up", "key", metaKey, "sha256", m.SHA256, "err", err)
		return
	}
	if err := withRetry(ctx, func() error { return store.PutMeta(ctx, metaKey, raw, m.SHA256) }); err != nil {
		slog.Error(logPrefix+": s3 replicate PutMeta gave up", "key", metaKey, "sha256", m.SHA256, "err", err)
	}
}

// errPermanent wraps an error that retrying cannot fix, so withRetry should
// give up immediately rather than burning its budget.
type errPermanent struct{ err error }

func (e errPermanent) Error() string { return e.err.Error() }
func (e errPermanent) Unwrap() error { return e.err }

// verifyDigest hashes r in full and returns a permanent error if the result
// doesn't match want. A mismatch is deterministic (the bytes are wrong, not S3
// flaking), so it must not be retried.
func verifyDigest(r io.Reader, want string) error {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return err // I/O error reading the local fd: transient, may retry
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return errPermanent{fmt.Errorf("content digest %s does not match expected %s", got, want)}
	}
	return nil
}

// withRetry runs op, retrying failures with exponential backoff and jitter up
// to replicateRetries extra attempts. It stops early when the context is done
// or when op returns a permanently-failing error (errPermanent, or an S3
// smithy.APIError the SDK won't recover from such as AccessDenied /
// InvalidRequest), since retrying those only wastes the budget and amplifies
// load on an already-failing dependency.
func withRetry(ctx context.Context, op func() error) error {
	var err error
	backoff := 100 * time.Millisecond
	for attempt := 0; attempt <= replicateRetries; attempt++ {
		if err = op(); err == nil {
			return nil
		}
		if attempt == replicateRetries || isPermanent(err) {
			break
		}
		jitter := time.Duration(rand.Int64N(int64(backoff)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff + jitter):
		}
		backoff *= 2
	}
	return err
}

// isPermanent reports whether err is deterministic enough that retrying it is
// pointless: our own errPermanent, or an S3 API error whose code signals a
// non-transient client/auth fault rather than throttling or a 5xx.
func isPermanent(err error) bool {
	var perm errPermanent
	if errors.As(err, &perm) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "AccessDenied", "InvalidRequest", "InvalidArgument",
			"InvalidBucketName", "MalformedXML", "SignatureDoesNotMatch",
			"NoSuchBucket":
			return true
		}
	}
	return false
}

// s3replicator runs write-through jobs asynchronously, serialized per key so
// S3 converges on the latest local state. Jobs re-read local state at send
// time (see replicateThrough), so even when two goroutines for the same key
// race for the stripe, both push the current content and order stops
// mattering — content overwritten locally before its replication ran simply
// never becomes durable. A bounded worker pool caps concurrency.
type s3replicator struct {
	stripes  [64]sync.Mutex
	workers  chan struct{}  // bounded semaphore; lazily initialized
	initOnce sync.Once      // guards lazy initialization
	wg       sync.WaitGroup // tests / shutdown wait for in-flight jobs

	// baseCtx parents every job's context; cancelBase cancels it so a bounded
	// drain can interrupt in-flight S3 calls instead of waiting out each job's
	// full replicateTimeout. Lazily initialized alongside the semaphore.
	baseCtx    context.Context
	cancelBase context.CancelFunc
}

func (r *s3replicator) sem() chan struct{} {
	r.initOnce.Do(func() {
		r.workers = make(chan struct{}, replicateWorkers)
		r.baseCtx, r.cancelBase = context.WithCancel(context.Background())
	})
	return r.workers
}

func (r *s3replicator) enqueue(key string, job func(ctx context.Context)) {
	sem := r.sem()
	stripe := &r.stripes[fnvHash(key)%uint32(len(r.stripes))]
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		sem <- struct{}{} // block once replicateWorkers jobs are in flight
		defer func() { <-sem }()
		stripe.Lock()
		defer stripe.Unlock()
		ctx, cancel := context.WithTimeout(r.baseCtx, replicateTimeout)
		defer cancel()
		job(ctx)
	}()
}

// wait blocks until all enqueued jobs finish (test synchronization).
func (r *s3replicator) wait() { r.wg.Wait() }

// waitContext blocks until all in-flight jobs finish or ctx expires. On
// expiry it cancels the jobs' base context so their in-flight S3 calls abort
// promptly, then waits for them to unwind. Returns ctx.Err() if it had to
// cut the drain short, nil if everything finished within the budget.
func (r *s3replicator) waitContext(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		if r.cancelBase != nil {
			r.cancelBase() // interrupt in-flight S3 calls
		}
		<-done // jobs observe the cancellation and unwind
		return ctx.Err()
	}
}

func fnvHash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}
