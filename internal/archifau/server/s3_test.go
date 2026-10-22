package server

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/archifau/auth"
	"github.com/Delphos-Labs/roxxy/internal/archifau/s3cas"
	"github.com/Delphos-Labs/roxxy/internal/archifau/s3cas/s3castest"
	"github.com/Delphos-Labs/roxxy/internal/registry"
)

// s3Env is a testEnv with the in-memory second level injected and handles on
// the storage handlers for replication synchronization.
type s3Env struct {
	*testEnv
	fake    *s3castest.Fake
	pypi    *PyPI
	files   *FileStore
	dataDir string
}

// newS3Env builds an archifau handler over the given fake store. Passing the
// same fake to several envs simulates multiple nodes sharing one bucket.
func newS3Env(t *testing.T, fake *s3castest.Fake) *s3Env {
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

	s := New(Config{DataDir: dataDir, Auth: a, Registry: dockerRegistry, S3: fake})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &s3Env{
		testEnv: &testEnv{srv: srv, minter: minter},
		fake:    fake, pypi: s.pypi, files: s.files, dataDir: dataDir,
	}
}

// sync waits for all in-flight write-through jobs.
func (e *s3Env) sync() {
	e.pypi.repl.wait()
	e.files.repl.wait()
}

func TestS3FilesWriteThrough(t *testing.T) {
	e := newS3Env(t, s3castest.NewFake())
	token := e.token(t, ciMain())
	content := []byte("durable bytes")
	digest := hex.EncodeToString(sha256Bytes(content))
	rel := "delphos-labs/widget/dumps/d.parquet"

	resp := e.do(t, http.MethodPut, "/files/"+rel, token, content,
		map[string]string{"Content-Type": "application/octet-stream"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	e.sync()

	if !e.fake.HasCAS(digest) {
		t.Error("CAS blob not replicated")
	}
	s3meta, err := e.fake.GetMeta(t.Context(), s3cas.FilesMetaKey(rel))
	if err != nil {
		t.Fatalf("meta not replicated: %v", err)
	}
	local, err := os.ReadFile(filepath.Join(e.dataDir, "files-meta", filepath.FromSlash(rel)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s3meta, local) {
		t.Errorf("S3 meta is not a byte-identical sidecar mirror:\n%s\nvs\n%s", s3meta, local)
	}
}

func TestS3FilesReadThroughFillsCache(t *testing.T) {
	fake := s3castest.NewFake()
	rel := "delphos-labs/widget/dumps/cold.bin"
	content := []byte("cold-node bytes")
	digest := hex.EncodeToString(sha256Bytes(content))
	meta, _ := json.MarshalIndent(&fileMeta{
		SHA256: digest, Size: int64(len(content)),
		ContentType: "application/x-test", UploadedAt: time.Now().UTC(),
		Uploader: "delphos-labs/widget", UploaderVia: "oidc", Ref: "refs/heads/main",
	}, "", "  ")
	fake.SeedCAS(digest, content)
	fake.SeedMeta(s3cas.FilesMetaKey(rel), meta, digest)

	e := newS3Env(t, fake) // fresh node: nothing local
	token := e.token(t, ciMain())

	resp := e.do(t, http.MethodGet, "/files/"+rel, token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: %d", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("body = %q, err %v", got, err)
	}
	for h, want := range map[string]string{
		"X-Content-SHA256": digest,
		"Content-Type":     "application/x-test",
	} {
		if v := resp.Header.Get(h); v != want {
			t.Errorf("%s = %q, want %q", h, v, want)
		}
	}

	// Cache filled: file + byte-identical sidecar on disk.
	if disk, err := os.ReadFile(filepath.Join(e.dataDir, "files", filepath.FromSlash(rel))); err != nil || !bytes.Equal(disk, content) {
		t.Errorf("local cache fill missing: %v", err)
	}
	if side, err := os.ReadFile(filepath.Join(e.dataDir, "files-meta", filepath.FromSlash(rel)+".json")); err != nil || !bytes.Equal(side, meta) {
		t.Errorf("local sidecar fill missing/mismatched: %v", err)
	}

	// Second read serves locally: no further CAS fetch.
	before := e.fake.GetCASCalls
	if resp := e.do(t, http.MethodGet, "/files/"+rel, token, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("second get: %d", resp.StatusCode)
	}
	if e.fake.GetCASCalls != before {
		t.Errorf("second read hit S3 (GetCAS %d -> %d)", before, e.fake.GetCASCalls)
	}
}

func TestS3FilesDeleteKeepsCASBlob(t *testing.T) {
	e := newS3Env(t, s3castest.NewFake())
	token := e.token(t, ciMain())
	content := []byte("delete me")
	digest := hex.EncodeToString(sha256Bytes(content))
	rel := "delphos-labs/widget/tmp.bin"

	if resp := e.do(t, http.MethodPut, "/files/"+rel, token, content, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	e.sync()
	if resp := e.do(t, http.MethodDelete, "/files/"+rel, token, nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	e.sync()

	if e.fake.HasMeta(s3cas.FilesMetaKey(rel)) {
		t.Error("S3 meta pointer survived delete")
	}
	if !e.fake.HasCAS(digest) {
		t.Error("CAS blob deleted — must be retained (immutable, possibly shared)")
	}
	// Name no longer resolves anywhere.
	if resp := e.do(t, http.MethodGet, "/files/"+rel, token, nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete: %d, want 404", resp.StatusCode)
	}
}

func TestS3PyPICrossNode(t *testing.T) {
	fake := s3castest.NewFake()
	wheel := []byte("PK-cross-node-wheel")
	const file = "widget-1.0-py3-none-any.whl"

	// Node A publishes.
	a := newS3Env(t, fake)
	resp := twineUpload(t, a.srv.URL, a.token(t, ciMain()), "widget", "1.0", file, wheel)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	a.sync()
	if !fake.HasMeta(s3cas.PyPIMetaKey("widget", file)) {
		t.Fatal("pypi meta not replicated")
	}

	// Node B (cold disk, same bucket): download by exact name works and
	// re-warms the local index.
	b := newS3Env(t, fake)
	token := b.token(t, ciMain())
	resp = b.do(t, http.MethodGet, "/pypi/packages/widget/"+file, token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cold download: %d", resp.StatusCode)
	}
	if got, _ := io.ReadAll(resp.Body); !bytes.Equal(got, wheel) {
		t.Error("cold download mismatch")
	}
	resp = b.do(t, http.MethodGet, "/pypi/simple/widget/", token, nil, nil)
	if body, _ := io.ReadAll(resp.Body); !strings.Contains(string(body), file) {
		t.Errorf("index not re-warmed after read-through: %s", body)
	}

	// Node C (cold disk): immutability holds across nodes via the S3 meta.
	c := newS3Env(t, fake)
	resp = twineUpload(t, c.srv.URL, c.token(t, ciMain()), "widget", "1.0", file, []byte("different bytes"))
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("cross-node conflicting re-upload: %d, want 409", resp.StatusCode)
	}
	resp = twineUpload(t, c.srv.URL, c.token(t, ciMain()), "widget", "1.0", file, wheel)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("cross-node identical re-upload: %d, want 200", resp.StatusCode)
	}
}

func TestS3DigestMismatchNotCommitted(t *testing.T) {
	// Equal-length but wrong content is the dangerous case: it satisfies the
	// Content-Length the meta advertised, so without the abort it would land
	// as a clean 200 with a digest header that doesn't match the bytes.
	fake := s3castest.NewFake()
	rel := "delphos-labs/widget/poisoned.bin"
	good := []byte("expected content")
	digest := hex.EncodeToString(sha256Bytes(good))
	corrupt := []byte("CORRUPTED conten") // same length as good, wrong bytes
	if len(corrupt) != len(good) {
		t.Fatalf("test setup: corrupt len %d != good len %d", len(corrupt), len(good))
	}
	meta, _ := json.Marshal(&fileMeta{SHA256: digest, Size: int64(len(good))})
	fake.SeedMeta(s3cas.FilesMetaKey(rel), meta, digest)
	fake.SeedCAS(digest, corrupt)

	e := newS3Env(t, fake)
	token := e.token(t, ciMain())

	resp, err := e.doRaw(t, http.MethodGet, "/files/"+rel, token, nil, nil)
	if err != nil {
		// Connection-level abort before any response is also a non-clean
		// transfer — acceptable.
		return
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	// The handler aborts the connection mid/after body, so a full clean read
	// of the advertised length must not happen.
	if readErr == nil && int64(len(body)) == int64(len(good)) {
		t.Error("client saw a clean transfer of equal-length corrupt content")
	}
	if _, statErr := os.Stat(filepath.Join(e.dataDir, "files", filepath.FromSlash(rel))); statErr == nil {
		t.Error("corrupt content committed to the local cache")
	}
}

func TestS3CASMissingIs404(t *testing.T) {
	fake := s3castest.NewFake()
	rel := "delphos-labs/widget/dangling.bin"
	digest := hex.EncodeToString(sha256Bytes([]byte("never stored")))
	meta, _ := json.Marshal(&fileMeta{SHA256: digest, Size: 12})
	fake.SeedMeta(s3cas.FilesMetaKey(rel), meta, digest)

	e := newS3Env(t, fake)
	resp := e.do(t, http.MethodGet, "/files/"+rel, e.token(t, ciMain()), nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("dangling meta: %d, want 404", resp.StatusCode)
	}
}

func TestS3ReadThroughUpstreamError(t *testing.T) {
	const filesRel = "delphos-labs/widget/x.bin"
	const pypiName, pypiFile = "widget", "widget-1.0.tar.gz"

	t.Run("files non-NotFound meta error is 502", func(t *testing.T) {
		fake := s3castest.NewFake()
		fake.GetMetaErr = errors.New("s3: throttled")
		e := newS3Env(t, fake)
		resp := e.do(t, http.MethodGet, "/files/"+filesRel, e.token(t, ciMain()), nil, nil)
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", resp.StatusCode)
		}
	})
	t.Run("pypi non-NotFound meta error is 502", func(t *testing.T) {
		fake := s3castest.NewFake()
		fake.GetMetaErr = errors.New("s3: throttled")
		e := newS3Env(t, fake)
		resp := e.do(t, http.MethodGet, "/pypi/packages/"+pypiName+"/"+pypiFile, e.token(t, ciMain()), nil, nil)
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", resp.StatusCode)
		}
	})
	t.Run("files malformed meta JSON is 502 and caches nothing", func(t *testing.T) {
		fake := s3castest.NewFake()
		fake.SeedMeta(s3cas.FilesMetaKey(filesRel), []byte("{not json"), "x")
		e := newS3Env(t, fake)
		resp := e.do(t, http.MethodGet, "/files/"+filesRel, e.token(t, ciMain()), nil, nil)
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", resp.StatusCode)
		}
		// 502 returns before streamThroughCAS, so nothing must land on disk.
		if _, err := os.Stat(filepath.Join(e.dataDir, "files", filepath.FromSlash(filesRel))); err == nil {
			t.Error("malformed-meta 502 path committed content to local cache")
		}
	})
	t.Run("pypi malformed meta JSON is 502 and caches nothing", func(t *testing.T) {
		fake := s3castest.NewFake()
		fake.SeedMeta(s3cas.PyPIMetaKey(pypiName, pypiFile), []byte("{not json"), "x")
		e := newS3Env(t, fake)
		resp := e.do(t, http.MethodGet, "/pypi/packages/"+pypiName+"/"+pypiFile, e.token(t, ciMain()), nil, nil)
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", resp.StatusCode)
		}
		if _, err := os.Stat(filepath.Join(e.dataDir, "pypi", pypiName, pypiFile)); err == nil {
			t.Error("malformed-meta 502 path committed content to local cache")
		}
	})
	t.Run("files GetCAS error after meta resolves is 502", func(t *testing.T) {
		fake := s3castest.NewFake()
		digest := hex.EncodeToString(sha256Bytes([]byte("present meta absent cas")))
		meta, _ := json.Marshal(&fileMeta{SHA256: digest, Size: 23})
		fake.SeedMeta(s3cas.FilesMetaKey(filesRel), meta, digest)
		fake.GetCASErr = errors.New("s3: throttled") // CAS fetch fails post-meta
		e := newS3Env(t, fake)
		resp := e.do(t, http.MethodGet, "/files/"+filesRel, e.token(t, ciMain()), nil, nil)
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", resp.StatusCode)
		}
	})
	t.Run("pypi GetCAS error after meta resolves is 502", func(t *testing.T) {
		fake := s3castest.NewFake()
		digest := hex.EncodeToString(sha256Bytes([]byte("present meta absent cas")))
		meta, _ := json.Marshal(&pypiFileMeta{SHA256: digest, Size: 23})
		fake.SeedMeta(s3cas.PyPIMetaKey(pypiName, pypiFile), meta, digest)
		fake.GetCASErr = errors.New("s3: throttled")
		e := newS3Env(t, fake)
		resp := e.do(t, http.MethodGet, "/pypi/packages/"+pypiName+"/"+pypiFile, e.token(t, ciMain()), nil, nil)
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", resp.StatusCode)
		}
	})
}

func TestS3ReplicationConvergesOnLastSequentialWrite(t *testing.T) {
	e := newS3Env(t, s3castest.NewFake())
	token := e.token(t, ciMain())
	rel := "delphos-labs/widget/converge.bin"
	first := []byte("first body")
	last := []byte("second, different body")
	lastDigest := hex.EncodeToString(sha256Bytes(last))

	if resp := e.do(t, http.MethodPut, "/files/"+rel, token, first, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first put: %d", resp.StatusCode)
	}
	if resp := e.do(t, http.MethodPut, "/files/"+rel, token, last, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("second put: %d", resp.StatusCode)
	}
	e.sync()

	s3meta, err := e.fake.GetMeta(t.Context(), s3cas.FilesMetaKey(rel))
	if err != nil {
		t.Fatalf("meta not replicated: %v", err)
	}
	var m fileMeta
	if err := json.Unmarshal(s3meta, &m); err != nil {
		t.Fatal(err)
	}
	if m.SHA256 != lastDigest {
		t.Errorf("S3 meta digest = %s, want last write %s", m.SHA256, lastDigest)
	}
	if !e.fake.HasCAS(lastDigest) {
		t.Error("last content not in CAS")
	}
}

// TestS3ReplicationConvergesUnderConcurrentSameKeyWrites exercises the actual
// hardening the PR claims: many overlapping PUTs to one name, racing for the
// per-key stripe. The barrier maximizes overlap. The invariant that must hold
// is integrity, not which write wins: after the dust settles the S3 meta must
// point at a CAS blob that is present and whose bytes genuinely hash to that
// digest — i.e. the stripe serialization + per-attempt re-verify (M1) never let
// a racing same-key overwrite poison the immutable CAS under a stale digest.
func TestS3ReplicationConvergesUnderConcurrentSameKeyWrites(t *testing.T) {
	e := newS3Env(t, s3castest.NewFake())
	token := e.token(t, ciMain())
	rel := "delphos-labs/widget/race.bin"

	const writers = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := []byte(fmt.Sprintf("body-variant-%02d-padding", i))
			<-start // release all goroutines at once for maximal overlap
			resp, err := e.doRaw(t, http.MethodPut, "/files/"+rel, token, body, nil)
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	close(start)
	wg.Wait()
	// A final settled PUT (no longer racing) gives the last replicate job a
	// stable sidecar+content pair to converge on, so the meta is durable.
	final := []byte("the-final-settled-body")
	finalDigest := hex.EncodeToString(sha256Bytes(final))
	if resp := e.do(t, http.MethodPut, "/files/"+rel, token, final, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("final put: %d", resp.StatusCode)
	}
	e.sync()

	s3meta, err := e.fake.GetMeta(t.Context(), s3cas.FilesMetaKey(rel))
	if err != nil {
		t.Fatalf("meta not replicated after concurrent writes: %v", err)
	}
	var m fileMeta
	if err := json.Unmarshal(s3meta, &m); err != nil {
		t.Fatal(err)
	}
	if m.SHA256 != finalDigest {
		t.Errorf("S3 meta digest = %s, want the final settled write %s", m.SHA256, finalDigest)
	}
	// The CAS blob the meta points at must be present and genuinely hash to it:
	// this is what the M1 fix (re-verify the fd before each PutCAS) protects —
	// a racing overwrite must never land its bytes under another digest.
	rc, err := e.fake.GetCAS(t.Context(), m.SHA256)
	if err != nil {
		t.Fatalf("CAS blob for replicated digest missing: %v", err)
	}
	casBytes, _ := io.ReadAll(rc)
	_ = rc.Close()
	if got := hex.EncodeToString(sha256Bytes(casBytes)); got != m.SHA256 {
		t.Errorf("CAS blob under digest %s actually hashes to %s (CAS poisoned)", m.SHA256, got)
	}
}

func TestS3WriteMetaFailureIs500(t *testing.T) {
	e := newS3Env(t, s3castest.NewFake())
	token := e.token(t, ciMain())
	rel := "delphos-labs/widget/nometa.bin"

	// Make the meta directory unwritable so writeMeta fails after the content
	// rename: the upload is not durably complete and must 500.
	metaDir := filepath.Join(e.dataDir, "files-meta")
	if err := os.Chmod(metaDir, 0500); err != nil {
		t.Skipf("cannot make meta dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(metaDir, 0755) })
	// Running as root defeats the permission gate; skip rather than flake.
	if probe := filepath.Join(metaDir, ".probe"); os.WriteFile(probe, nil, 0644) == nil {
		_ = os.Remove(probe)
		t.Skip("meta dir still writable (likely root); cannot induce writeMeta failure")
	}

	// Pre-seed an S3 meta for this key so the test pins the real durability
	// property: a failed writeMeta must NOT enqueue replication, and so must
	// not DELETE the existing S3 meta. Asserting HasMeta stays false would be
	// vacuous — a stray replicate hits replicateThrough's os.IsNotExist branch
	// and DeleteMeta's, a no-op on a key the fresh fake never held.
	prevMeta := []byte(`{"sha256":"` + strings.Repeat("a", 64) + `","size":1}`)
	metaKey := s3cas.FilesMetaKey(rel)
	e.fake.SeedMeta(metaKey, prevMeta, strings.Repeat("a", 64))

	resp := e.do(t, http.MethodPut, "/files/"+rel, token, []byte("body"), nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	e.sync()
	// A failed writeMeta must not have enqueued replication: no DeleteMeta...
	if e.fake.DeleteMetaCalls != 0 {
		t.Errorf("DeleteMeta called %d times; a failed writeMeta must not enqueue replication", e.fake.DeleteMetaCalls)
	}
	// ...and the pre-existing S3 meta must survive unchanged.
	got, err := e.fake.GetMeta(t.Context(), metaKey)
	if err != nil {
		t.Fatalf("pre-seeded S3 meta was deleted by a stray replication: %v", err)
	}
	if !bytes.Equal(got, prevMeta) {
		t.Errorf("S3 meta changed: got %s, want %s", got, prevMeta)
	}
}

func TestS3DedupAcrossSurfaces(t *testing.T) {
	e := newS3Env(t, s3castest.NewFake())
	token := e.token(t, ciMain())
	content := []byte("identical bytes via two surfaces")

	if resp := e.do(t, http.MethodPut, "/files/delphos-labs/widget/blob.bin", token, content, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("files put: %d", resp.StatusCode)
	}
	if resp := twineUpload(t, e.srv.URL, token, "widget", "1.0", "widget-1.0.tar.gz", content); resp.StatusCode != http.StatusOK {
		t.Fatalf("pypi upload: %d", resp.StatusCode)
	}
	e.sync()

	if n := e.fake.CASCount(); n != 1 {
		t.Errorf("CAS objects = %d, want 1 (dedup)", n)
	}
	if !e.fake.HasMeta(s3cas.FilesMetaKey("delphos-labs/widget/blob.bin")) ||
		!e.fake.HasMeta(s3cas.PyPIMetaKey("widget", "widget-1.0.tar.gz")) {
		t.Error("missing one of the two meta pointers")
	}
}
