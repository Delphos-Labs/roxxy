package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Delphos-Labs/roxxy/internal/archifau/s3cas"
	"github.com/Delphos-Labs/roxxy/internal/archifau/s3cas/s3castest"
	"github.com/aws/smithy-go"
)

// captureStore wraps a Fake but lets a test intercept PutCAS: it can fail the
// first attempt and run a hook (e.g. a concurrent overwrite) before the retry.
type captureStore struct {
	*s3castest.Fake
	putAttempts atomic.Int32
	failFirst   bool
	beforeRetry func()
}

func (c *captureStore) PutCAS(ctx context.Context, digest string, size int64, r io.Reader) error {
	n := c.putAttempts.Add(1)
	if c.failFirst && n == 1 {
		// Drain the reader so the contract (a streamed body) is honored, then
		// fail transiently and let the test mutate the content file before the
		// retry re-reads the fd.
		_, _ = io.Copy(io.Discard, r)
		if c.beforeRetry != nil {
			c.beforeRetry()
		}
		return errors.New("s3: transient 503")
	}
	return c.Fake.PutCAS(ctx, digest, size, r)
}

// TestReplicateThroughRejectsOverwrittenContentOnRetry pins M1: when a PutCAS
// retry re-reads the content fd after a concurrent same-length overwrite, the
// bytes no longer hash to the sidecar digest, so replication must refuse to
// store them under that digest rather than poisoning the immutable CAS.
func TestReplicateThroughRejectsOverwrittenContentOnRetry(t *testing.T) {
	dir := t.TempDir()
	contentPath := filepath.Join(dir, "blob")
	sidecarPath := filepath.Join(dir, "blob.json")

	good := []byte("the original bytes")
	goodDigest := hex.EncodeToString(sha256Bytes(good))
	evil := []byte("the EVIL byteszz") // padded below to match len(good)
	for len(evil) < len(good) {
		evil = append(evil, 'x')
	}
	evil = evil[:len(good)]
	if len(evil) != len(good) {
		t.Fatalf("setup: evil len %d != good len %d", len(evil), len(good))
	}

	if err := os.WriteFile(contentPath, good, 0644); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(&replicaMeta{SHA256: goodDigest, Size: int64(len(good))})
	if err := os.WriteFile(sidecarPath, meta, 0644); err != nil {
		t.Fatal(err)
	}

	store := &captureStore{
		Fake:      s3castest.NewFake(),
		failFirst: true,
		beforeRetry: func() {
			// Concurrent same-length overwrite during the backoff window.
			_ = os.WriteFile(contentPath, evil, 0644)
		},
	}

	key := s3cas.FilesMetaKey("delphos-labs/widget/poison.bin")
	replicateThrough(t.Context(), store, key, sidecarPath, contentPath, "files")

	// The evil bytes must NOT have landed under the original digest.
	if store.HasCAS(goodDigest) {
		rc, err := store.GetCAS(t.Context(), goodDigest)
		if err != nil {
			t.Fatal(err)
		}
		stored, _ := io.ReadAll(rc)
		_ = rc.Close()
		if got := hex.EncodeToString(sha256Bytes(stored)); got != goodDigest {
			t.Fatalf("CAS poisoned: digest %s holds bytes hashing to %s", goodDigest, got)
		}
	}
	// And the digest-mismatch is permanent, so the meta must not have been
	// pushed pointing at content that isn't durably stored.
	if store.HasMeta(key) {
		t.Error("meta pushed despite the CAS blob being rejected on digest mismatch")
	}
}

// permError is a smithy.APIError whose code marks it non-retryable.
type permError struct{ code string }

func (e permError) Error() string        { return e.code }
func (e permError) ErrorCode() string    { return e.code }
func (e permError) ErrorMessage() string { return e.code }
func (e permError) ErrorFault() smithy.ErrorFault {
	return smithy.FaultClient
}

// TestWithRetryStopsOnPermanentError pins S3: a permanent error (errPermanent
// or a non-retryable smithy.APIError) must be attempted exactly once, not
// retried through the whole budget.
func TestWithRetryStopsOnPermanentError(t *testing.T) {
	cases := map[string]error{
		"errPermanent":          errPermanent{errors.New("malformed digest")},
		"AccessDenied":          permError{code: "AccessDenied"},
		"InvalidRequest":        permError{code: "InvalidRequest"},
		"SignatureDoesNotMatch": permError{code: "SignatureDoesNotMatch"},
	}
	for name, retErr := range cases {
		t.Run(name, func(t *testing.T) {
			var calls int
			err := withRetry(t.Context(), func() error {
				calls++
				return retErr
			})
			if err == nil {
				t.Fatal("expected error to surface")
			}
			if calls != 1 {
				t.Errorf("op called %d times; a permanent error must not be retried", calls)
			}
		})
	}
}

// TestWithRetryRetriesTransientError is the non-vacuity counterpart: a generic
// (retryable) error must exhaust the full attempt budget.
func TestWithRetryRetriesTransientError(t *testing.T) {
	var calls int
	err := withRetry(t.Context(), func() error {
		calls++
		return errors.New("s3: throttled")
	})
	if err == nil {
		t.Fatal("expected error to surface")
	}
	if calls != replicateRetries+1 {
		t.Errorf("op called %d times; want %d (full retry budget)", calls, replicateRetries+1)
	}
}
