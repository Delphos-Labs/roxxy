package server

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/archifau/auth"
	"github.com/Delphos-Labs/roxxy/internal/archifau/s3cas"
	"github.com/Delphos-Labs/roxxy/internal/registry"
)

// newEnvWithConfig builds a handler with a caller-supplied Config (DataDir,
// Auth, and Registry are filled in). Returns the env plus the *Server so tests
// can drive Drain directly.
func newEnvWithConfig(t *testing.T, cfg Config) (*testEnv, *Server) {
	t.Helper()
	dataDir := t.TempDir()
	minter := &auth.Minter{Secret: []byte("test-secret"), Service: "archifau.test"}
	a := &auth.Authenticator{
		OIDC:    &auth.OIDCVerifier{ExpectedAud: "archifau.test"},
		Users:   &auth.UserVerifier{},
		Minter:  minter,
		Realm:   "https://archifau.test/token",
		Service: "archifau.test",
	}
	dockerRegistry := registry.NewDockerRegistryProxy(
		"https://archifau.test", nil, []string{"archifau.test"},
		false, true, false, nil, dataDir, nil, false)

	cfg.DataDir = dataDir
	cfg.Auth = a
	cfg.Registry = dockerRegistry
	s := New(cfg)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &testEnv{srv: srv, minter: minter}, s
}

func TestUploadLimitReturns413(t *testing.T) {
	const cap = 16

	t.Run("files PUT over the cap is 413", func(t *testing.T) {
		e, _ := newEnvWithConfig(t, Config{MaxUploadBytes: cap})
		token := e.token(t, ciMain())
		body := bytes.Repeat([]byte("x"), cap+1)
		resp := e.do(t, http.MethodPut, "/files/delphos-labs/widget/big.bin", token, body, nil)
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", resp.StatusCode)
		}
	})

	t.Run("files PUT at the cap succeeds", func(t *testing.T) {
		e, _ := newEnvWithConfig(t, Config{MaxUploadBytes: cap})
		token := e.token(t, ciMain())
		body := bytes.Repeat([]byte("x"), cap)
		resp := e.do(t, http.MethodPut, "/files/delphos-labs/widget/ok.bin", token, body, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("status = %d, want 201", resp.StatusCode)
		}
	})

	t.Run("pypi upload over the cap is 413", func(t *testing.T) {
		e, _ := newEnvWithConfig(t, Config{MaxUploadBytes: cap})
		token := e.token(t, ciMain())
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("name", "widget")
		fw, _ := mw.CreateFormFile("content", "widget-1.0.tar.gz")
		_, _ = fw.Write(bytes.Repeat([]byte("y"), cap*4)) // overflow the multipart body
		_ = mw.Close()

		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/pypi/legacy/", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.SetBasicAuth("__token__", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", resp.StatusCode)
		}
	})
}

// blockingStore lets a test hold PutMeta open on a channel so Drain has
// genuinely-unfinished in-flight work to wait on.
type blockingStore struct {
	s3cas.Store
	release chan struct{}
	entered chan struct{}
}

func (b *blockingStore) PutCAS(context.Context, string, int64, io.Reader) error { return nil }
func (b *blockingStore) GetMeta(context.Context, string) ([]byte, error) {
	return nil, s3cas.ErrNotFound
}
func (b *blockingStore) PutMeta(ctx context.Context, _ string, _ []byte, _ string) error {
	close(b.entered)
	// Block until the test releases us OR the job's context is cancelled —
	// mirroring a real aws-sdk call that aborts when its context is done.
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}
func (b *blockingStore) DeleteMeta(context.Context, string) error { return nil }

// TestDrainAwaitsInFlightReplication pins the Drain behavior: an enqueued-but-
// unfinished replication job must be awaited, not dropped. A regression making
// DrainContext a no-op returns before the job is released and fails here.
func TestDrainAwaitsInFlightReplication(t *testing.T) {
	store := &blockingStore{release: make(chan struct{}), entered: make(chan struct{})}
	e, s := newEnvWithConfig(t, Config{S3: store})
	token := e.token(t, ciMain())

	if resp := e.do(t, http.MethodPut, "/files/delphos-labs/widget/d.bin", token, []byte("body"), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	<-store.entered // the replication job is now blocked inside PutMeta

	drained := make(chan struct{})
	go func() {
		_ = s.DrainContext(context.Background())
		close(drained)
	}()

	select {
	case <-drained:
		t.Fatal("DrainContext returned while a replication job was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(store.release) // let the job finish
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("DrainContext did not return after the in-flight job finished")
	}
}

// TestDrainContextBoundedByDeadline pins the shutdown-deadline fix: a wedged S3
// must not hang Drain past its budget — DrainContext cancels in-flight jobs and
// returns ctx.Err() rather than blocking on the per-job 5m timeout.
func TestDrainContextBoundedByDeadline(t *testing.T) {
	store := &blockingStore{release: make(chan struct{}), entered: make(chan struct{})}
	e, s := newEnvWithConfig(t, Config{S3: store})
	t.Cleanup(func() { close(store.release) })
	token := e.token(t, ciMain())

	if resp := e.do(t, http.MethodPut, "/files/delphos-labs/widget/wedged.bin", token, []byte("body"), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	<-store.entered

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.DrainContext(ctx)
	if err == nil {
		t.Error("DrainContext returned nil; want a deadline error for the wedged job")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("DrainContext blocked %v; should have returned near the 100ms budget", elapsed)
	}
}
