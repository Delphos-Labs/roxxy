package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Delphos-Labs/roxxy/internal/archifau/auth"
	"github.com/Delphos-Labs/roxxy/internal/registry"
)

// testEnv wires a complete archifau handler with a stub Minter-only
// authenticator: tests mint identities directly instead of standing up a
// fake GitHub. (The credential verifiers have their own tests in the auth
// package.)
type testEnv struct {
	srv    *httptest.Server
	minter *auth.Minter
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dataDir := t.TempDir()

	minter := &auth.Minter{Secret: []byte("test-secret"), Service: "archifau.test"}
	a := &auth.Authenticator{
		OIDC:    &auth.OIDCVerifier{ExpectedAud: "archifau.test"}, // never reached in these tests
		Users:   &auth.UserVerifier{},
		Minter:  minter,
		Realm:   "https://archifau.test/token",
		Service: "archifau.test",
	}

	dockerRegistry := registry.NewDockerRegistryProxy(
		"https://archifau.test", nil, []string{"archifau.test"},
		false, true, false, nil, dataDir, nil, false)

	s := New(Config{DataDir: dataDir, Auth: a, Registry: dockerRegistry})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &testEnv{srv: srv, minter: minter}
}

func (e *testEnv) token(t *testing.T, id *auth.Identity) string {
	t.Helper()
	token, _, _, err := e.minter.Mint(id)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func ciMain() *auth.Identity {
	return &auth.Identity{Kind: "oidc", Subject: "delphos-labs/widget",
		Repository: "delphos-labs/widget", Ref: "refs/heads/main",
		Actor: "faye", Namespaces: []string{"delphos-labs/widget"}}
}

func ciBranch() *auth.Identity {
	id := ciMain()
	id.Ref = "refs/heads/feature"
	return id
}

func ciForeign() *auth.Identity {
	return &auth.Identity{Kind: "oidc", Subject: "delphos-labs/other",
		Repository: "delphos-labs/other", Ref: "refs/heads/main",
		Namespaces: []string{"delphos-labs/other"}}
}

func human() *auth.Identity {
	return &auth.Identity{Kind: "user", Subject: "faye", Actor: "faye",
		Namespaces: []string{"delphos-labs"}}
}

func (e *testEnv) do(t *testing.T, method, path, token string, body []byte, hdr map[string]string) *http.Response {
	t.Helper()
	resp, err := e.doRaw(t, method, path, token, body, hdr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// doRaw is do without fataling on a transport error — for paths that abort the
// connection (e.g. the integrity-abort response), where the error is expected.
func (e *testEnv) doRaw(t *testing.T, method, path, token string, body []byte, hdr map[string]string) (*http.Response, error) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

// --- docker registry ---------------------------------------------------------

func TestDockerChallengeWhenUnauthenticated(t *testing.T) {
	e := newTestEnv(t)
	resp := e.do(t, http.MethodGet, "/v2/", "", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	ch := resp.Header.Get("WWW-Authenticate")
	if !strings.Contains(ch, `Bearer realm="https://archifau.test/token"`) ||
		!strings.Contains(ch, `service="archifau.test"`) {
		t.Errorf("challenge = %q", ch)
	}
}

func TestDockerPushPullOwnNamespace(t *testing.T) {
	e := newTestEnv(t)
	token := e.token(t, ciMain())

	// Ping.
	if resp := e.do(t, http.MethodGet, "/v2/", token, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("/v2/ ping: %d", resp.StatusCode)
	}

	// Push a blob (monolithic PUT by digest) into our own namespace.
	blob := []byte("layer-bytes")
	digest := "sha256:" + hex.EncodeToString(sha256Bytes(blob))
	resp := e.do(t, http.MethodPut,
		"/v2/delphos-labs/widget/app/blobs/"+digest, token, blob, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("blob put: %d", resp.StatusCode)
	}

	// Push a manifest referencing nothing in particular (storage is by path).
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`)
	resp = e.do(t, http.MethodPut,
		"/v2/delphos-labs/widget/app/manifests/v1", token, manifest,
		map[string]string{"Content-Type": "application/vnd.docker.distribution.manifest.v2+json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("manifest put: %d", resp.StatusCode)
	}

	// Pull the manifest back.
	resp = e.do(t, http.MethodGet, "/v2/delphos-labs/widget/app/manifests/v1", token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest get: %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, manifest) {
		t.Errorf("manifest roundtrip mismatch: %s", got)
	}

	// Pull the blob back.
	resp = e.do(t, http.MethodGet, "/v2/delphos-labs/widget/app/blobs/"+digest, token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("blob get: %d", resp.StatusCode)
	}
	got, _ = io.ReadAll(resp.Body)
	if !bytes.Equal(got, blob) {
		t.Error("blob roundtrip mismatch")
	}
}

func TestDockerAuthorizationDenials(t *testing.T) {
	e := newTestEnv(t)
	manifest := []byte(`{"schemaVersion":2}`)

	t.Run("foreign repo cannot push into widget namespace", func(t *testing.T) {
		token := e.token(t, ciForeign())
		resp := e.do(t, http.MethodPut, "/v2/delphos-labs/widget/app/manifests/v1", token, manifest, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})
	t.Run("foreign repo cannot pull widget namespace", func(t *testing.T) {
		token := e.token(t, ciForeign())
		resp := e.do(t, http.MethodGet, "/v2/delphos-labs/widget/app/manifests/v1", token, nil, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})
	t.Run("non-main ref cannot push own namespace", func(t *testing.T) {
		token := e.token(t, ciBranch())
		resp := e.do(t, http.MethodPut, "/v2/delphos-labs/widget/app/manifests/v1", token, manifest, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})
	t.Run("non-main ref can pull own namespace", func(t *testing.T) {
		token := e.token(t, ciBranch())
		resp := e.do(t, http.MethodGet, "/v2/delphos-labs/widget/app/manifests/does-not-exist", token, nil, nil)
		// Authorization passes; content simply isn't there.
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("status = %d, pull should be authorized", resp.StatusCode)
		}
	})
	t.Run("human can push anywhere in org", func(t *testing.T) {
		token := e.token(t, human())
		resp := e.do(t, http.MethodPut, "/v2/delphos-labs/anything/manifests/v1", token, manifest, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("status = %d, want 201", resp.StatusCode)
		}
	})
	t.Run("upload session path is authorized as its repo", func(t *testing.T) {
		token := e.token(t, ciForeign())
		resp := e.do(t, http.MethodPost, "/v2/delphos-labs/widget/app/blobs/uploads/", token, nil, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})
}

// TestDockerCatalogAndMountAuthorization covers the per-repo authorization hook
// the archifau server injects into the reused registry: /v2/_catalog is filtered
// to the caller's readable namespaces, and a cross-repo blob mount is only
// honored when the caller can read the source (from=) repo.
func TestDockerCatalogAndMountAuthorization(t *testing.T) {
	e := newTestEnv(t)

	// ciMain publishes a blob + a manifest that references it, both in its own
	// namespace. The blob then exists in the global store AND is a member of
	// delphos-labs/widget/app (the mount source below), which the registry's
	// blob-membership check requires before honoring a cross-repo mount.
	ciToken := e.token(t, ciMain())
	blob := []byte("layer-bytes-for-authz")
	digest := "sha256:" + hex.EncodeToString(sha256Bytes(blob))
	if resp := e.do(t, http.MethodPut, "/v2/delphos-labs/widget/app/blobs/"+digest, ciToken, blob, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("blob push: %d", resp.StatusCode)
	}
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[{"digest":"` + digest + `"}]}`)
	if resp := e.do(t, http.MethodPut, "/v2/delphos-labs/widget/app/manifests/v1", ciToken, manifest,
		map[string]string{"Content-Type": "application/vnd.docker.distribution.manifest.v2+json"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("manifest push: %d", resp.StatusCode)
	}

	// ciForeign publishes its own repo so the catalog selectivity below has a
	// second tenant present (keep-mine/drop-theirs is then observable, and an
	// inverted or substring filter would leak it).
	foreignToken := e.token(t, ciForeign())
	fblob := []byte("other-tenant-layer")
	fdigest := "sha256:" + hex.EncodeToString(sha256Bytes(fblob))
	if resp := e.do(t, http.MethodPut, "/v2/delphos-labs/other/app/blobs/"+fdigest, foreignToken, fblob, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("foreign blob push: %d", resp.StatusCode)
	}
	fmanifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[{"digest":"` + fdigest + `"}]}`)
	if resp := e.do(t, http.MethodPut, "/v2/delphos-labs/other/app/manifests/v1", foreignToken, fmanifest,
		map[string]string{"Content-Type": "application/vnd.docker.distribution.manifest.v2+json"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("foreign manifest push: %d", resp.StatusCode)
	}

	catalog := func(id *auth.Identity) []string {
		resp := e.do(t, http.MethodGet, "/v2/_catalog", e.token(t, id), nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("catalog: %d", resp.StatusCode)
		}
		var body struct {
			Repositories []string `json:"repositories"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.Repositories
	}

	const repo = "delphos-labs/widget/app"
	const foreignRepo = "delphos-labs/other/app"
	t.Run("owner sees own repo and not the other tenant's", func(t *testing.T) {
		c := catalog(ciMain())
		if !slices.Contains(c, repo) {
			t.Errorf("ciMain should see %q, got %v", repo, c)
		}
		if slices.Contains(c, foreignRepo) {
			t.Errorf("ciMain must not see %q, got %v", foreignRepo, c)
		}
	})
	t.Run("org human sees both org repos", func(t *testing.T) {
		c := catalog(human())
		if !slices.Contains(c, repo) || !slices.Contains(c, foreignRepo) {
			t.Errorf("human (delphos-labs) should see both repos, got %v", c)
		}
	})
	t.Run("foreign repo sees its own and not widget", func(t *testing.T) {
		c := catalog(ciForeign())
		if !slices.Contains(c, foreignRepo) {
			t.Errorf("ciForeign should see its own %q, got %v", foreignRepo, c)
		}
		if slices.Contains(c, repo) {
			t.Errorf("ciForeign must not see %q in the catalog, got %v", repo, c)
		}
	})

	t.Run("mount from unauthorized source is not honored", func(t *testing.T) {
		// ciForeign can write its own repo (target) but cannot read the source,
		// so the mount must fall through to a normal upload session, not a 201
		// mount. Assert the exact 202 fallthrough with an uploads/ session
		// Location — proving no cross-tenant blob reference was created.
		resp := e.do(t, http.MethodPost,
			"/v2/delphos-labs/other/app/blobs/uploads/?mount="+digest+"&from=delphos-labs/widget/app",
			e.token(t, ciForeign()), nil, nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("unauthorized mount must fall through to a 202 upload session, got %d", resp.StatusCode)
		}
		loc := resp.Header.Get("Location")
		if !strings.Contains(loc, "/blobs/uploads/") || strings.HasSuffix(loc, "/blobs/"+digest) {
			t.Errorf("denied mount must point at an upload session, not the blob; Location=%q", loc)
		}
	})
	t.Run("mount from authorized source is honored", func(t *testing.T) {
		// human can read delphos-labs/* and the blob is a member of the source
		// manifest, so the mount is honored with the mount-target Location.
		resp := e.do(t, http.MethodPost,
			"/v2/delphos-labs/widget/copy/blobs/uploads/?mount="+digest+"&from=delphos-labs/widget/app",
			e.token(t, human()), nil, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("mount from a readable source should be honored (got %d)", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/v2/delphos-labs/widget/copy/blobs/"+digest {
			t.Errorf("mount Location = %q", loc)
		}
		if dig := resp.Header.Get("Docker-Content-Digest"); dig != digest {
			t.Errorf("mount digest = %q", dig)
		}
	})
}

// TestNewPanicsWithoutRepoAuthorizer proves the tenant-isolation hook is
// mandatory: if cfg.Registry doesn't implement repoAuthorizer, New must refuse
// to start rather than silently serve an unfiltered catalog / unconfined mounts.
func TestNewPanicsWithoutRepoAuthorizer(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New must panic when Registry does not implement repoAuthorizer")
		}
	}()
	bare := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	_ = New(Config{DataDir: t.TempDir(), Auth: &auth.Authenticator{}, Registry: bare})
}

func TestDockerScopeParsing(t *testing.T) {
	cases := []struct {
		method, path string
		repo, action string
	}{
		{"GET", "/v2/", "", "pull"},
		{"GET", "/v2/_catalog", "", "pull"},
		{"GET", "/v2/a/b/manifests/latest", "a/b", "pull"},
		{"HEAD", "/v2/a/b/c/blobs/sha256:x", "a/b/c", "pull"},
		{"PUT", "/v2/a/b/manifests/latest", "a/b", "push,pull"},
		{"POST", "/v2/a/b/blobs/uploads/", "a/b", "push,pull"},
		{"PATCH", "/v2/a/b/blobs/uploads/uuid-1", "a/b", "push,pull"},
		{"GET", "/v2/a/b/tags/list", "a/b", "pull"},
		{"GET", "/v2/A/B/manifests/x", "a/b", "pull"},
		{"GET", "/v2/weird-shape", "weird-shape", "pull"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		repo, action := dockerScope(r)
		if repo != tc.repo || action != tc.action {
			t.Errorf("%s %s: got (%q, %q), want (%q, %q)", tc.method, tc.path, repo, action, tc.repo, tc.action)
		}
	}
}

// --- bare files ----------------------------------------------------------------

func TestFilesPutGetDelete(t *testing.T) {
	e := newTestEnv(t)
	token := e.token(t, ciMain())
	content := []byte("parquet-ish bytes")
	sum := hex.EncodeToString(sha256Bytes(content))

	// PUT inside own namespace.
	resp := e.do(t, http.MethodPut, "/files/delphos-labs/widget/dumps/data.parquet", token, content,
		map[string]string{"Content-Type": "application/octet-stream"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Content-SHA256"); got != sum {
		t.Errorf("sha header = %s, want %s", got, sum)
	}

	// GET it back.
	resp = e.do(t, http.MethodGet, "/files/delphos-labs/widget/dumps/data.parquet", token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, content) {
		t.Error("content roundtrip mismatch")
	}

	// Directory listing.
	resp = e.do(t, http.MethodGet, "/files/delphos-labs/widget/dumps", token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	var listing struct {
		Entries []string `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 1 || listing.Entries[0] != "data.parquet" {
		t.Errorf("entries = %v", listing.Entries)
	}

	// DELETE.
	resp = e.do(t, http.MethodDelete, "/files/delphos-labs/widget/dumps/data.parquet", token, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp = e.do(t, http.MethodGet, "/files/delphos-labs/widget/dumps/data.parquet", token, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete: %d, want 404", resp.StatusCode)
	}
}

func TestFilesDenials(t *testing.T) {
	e := newTestEnv(t)

	t.Run("unauthenticated gets basic challenge", func(t *testing.T) {
		resp := e.do(t, http.MethodGet, "/files/delphos-labs/widget/x", "", nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic") {
			t.Errorf("challenge = %q", resp.Header.Get("WWW-Authenticate"))
		}
	})
	t.Run("foreign namespace write denied", func(t *testing.T) {
		token := e.token(t, ciForeign())
		resp := e.do(t, http.MethodPut, "/files/delphos-labs/widget/x", token, []byte("nope"), nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})
	t.Run("non-main ref write denied", func(t *testing.T) {
		token := e.token(t, ciBranch())
		resp := e.do(t, http.MethodPut, "/files/delphos-labs/widget/x", token, []byte("nope"), nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})
	t.Run("traversal rejected", func(t *testing.T) {
		token := e.token(t, human())
		// Build the request manually so the path isn't cleaned client-side.
		req, _ := http.NewRequest(http.MethodPut, e.srv.URL+"/files/delphos-labs/"+"%2e%2e/escape", bytes.NewReader([]byte("x")))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusMovedPermanently {
			t.Errorf("status = %d, want 400 (or redirect-cleaned)", resp.StatusCode)
		}
	})
}

// --- pypi ------------------------------------------------------------------------

func twineUpload(t *testing.T, url, token, name, version, filename string, content []byte) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField(":action", "file_upload")
	_ = mw.WriteField("protocol_version", "1")
	_ = mw.WriteField("name", name)
	_ = mw.WriteField("version", version)
	sum := hex.EncodeToString(sha256Bytes(content))
	_ = mw.WriteField("sha256_digest", sum)
	fw, err := mw.CreateFormFile("content", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	_ = mw.Close()

	req, err := http.NewRequest(http.MethodPost, url+"/pypi/legacy/", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetBasicAuth("__token__", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestPyPIUploadIndexDownload(t *testing.T) {
	e := newTestEnv(t)
	token := e.token(t, ciMain())
	wheel := []byte("PK-fake-wheel-bytes")
	sum := hex.EncodeToString(sha256Bytes(wheel))

	// Upload. Mixed-case, dotted project name exercises PEP 503 normalization.
	resp := twineUpload(t, e.srv.URL, token, "Delphos.Widget_Tools", "1.0.0",
		"delphos_widget_tools-1.0.0-py3-none-any.whl", wheel)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload: %d: %s", resp.StatusCode, body)
	}

	// Root index lists the normalized name.
	resp = e.do(t, http.MethodGet, "/pypi/simple/", token, nil, nil)
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `href="/pypi/simple/delphos-widget-tools/"`) {
		t.Errorf("root index missing project: %s", body)
	}

	// Project page links the file with its hash; un-normalized lookups work.
	resp = e.do(t, http.MethodGet, "/pypi/simple/Delphos.Widget_Tools/", token, nil, nil)
	body, _ = io.ReadAll(resp.Body)
	want := fmt.Sprintf(`/pypi/packages/delphos-widget-tools/delphos_widget_tools-1.0.0-py3-none-any.whl#sha256=%s`, sum)
	if !strings.Contains(string(body), want) {
		t.Errorf("project index missing %q: %s", want, body)
	}

	// Download.
	resp = e.do(t, http.MethodGet, "/pypi/packages/delphos-widget-tools/delphos_widget_tools-1.0.0-py3-none-any.whl", token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download: %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, wheel) {
		t.Error("wheel roundtrip mismatch")
	}

	// Idempotent identical re-upload is fine…
	resp = twineUpload(t, e.srv.URL, token, "Delphos.Widget_Tools", "1.0.0",
		"delphos_widget_tools-1.0.0-py3-none-any.whl", wheel)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("identical re-upload: %d, want 200", resp.StatusCode)
	}
	// …but changing published bytes under the same filename is not.
	resp = twineUpload(t, e.srv.URL, token, "Delphos.Widget_Tools", "1.0.0",
		"delphos_widget_tools-1.0.0-py3-none-any.whl", []byte("different bytes"))
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("conflicting re-upload: %d, want 409", resp.StatusCode)
	}
}

func TestPyPIDenials(t *testing.T) {
	e := newTestEnv(t)
	wheel := []byte("wheel")

	t.Run("unauthenticated read challenged", func(t *testing.T) {
		resp := e.do(t, http.MethodGet, "/pypi/simple/", "", nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d", resp.StatusCode)
		}
	})
	t.Run("non-main ref cannot upload", func(t *testing.T) {
		token := e.token(t, ciBranch())
		resp := twineUpload(t, e.srv.URL, token, "widget", "0.1", "widget-0.1.tar.gz", wheel)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})
	t.Run("non-main ref can still read", func(t *testing.T) {
		token := e.token(t, ciBranch())
		resp := e.do(t, http.MethodGet, "/pypi/simple/", token, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})
	t.Run("digest mismatch rejected", func(t *testing.T) {
		token := e.token(t, ciMain())
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("name", "widget")
		_ = mw.WriteField("sha256_digest", strings.Repeat("0", 64))
		fw, _ := mw.CreateFormFile("content", "widget-0.2.tar.gz")
		_, _ = fw.Write(wheel)
		_ = mw.Close()
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/pypi/legacy/", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestCleanRelPath(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantOK  bool
		comment string
	}{
		{"a/../b", "", false, "traversal collapses to escape sibling"},
		{"../x", "", false, "leading traversal"},
		{"a/./b", "", false, "current-dir segment is not canonical"},
		{"", "", false, "empty"},
		{"/", "", false, "root only"},
		{"org/repo/dir/file.bin", "org/repo/dir/file.bin", true, "valid nested"},
	}
	for _, tc := range cases {
		t.Run(tc.comment, func(t *testing.T) {
			got, ok := cleanRelPath(tc.in)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("cleanRelPath(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestPyPIRejectsBadNameAndFilename(t *testing.T) {
	e := newTestEnv(t)
	token := e.token(t, ciMain())
	wheel := []byte("wheel")

	t.Run("filename failing the regex is 400", func(t *testing.T) {
		resp := twineUpload(t, e.srv.URL, token, "widget", "1.0", "bad name!.whl", wheel)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
	t.Run("name failing the PEP 503 charset is 400", func(t *testing.T) {
		// Slash, backslash, space, leading/trailing/double separators, and the
		// bare "." / ".." must all be rejected post-normalization so an uploader
		// can't pollute the S3 key namespace or on-disk dirs.
		for _, bad := range []string{"ns/evil", `ns\evil`, "bad name", ".", "..", "-lead", "trail-", ""} {
			resp := twineUpload(t, e.srv.URL, token, bad, "1.0", "widget-1.0.tar.gz", wheel)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("name %q: status = %d, want 400", bad, resp.StatusCode)
			}
		}
	})
	t.Run("filename of bare dot-segments is 400", func(t *testing.T) {
		for _, bad := range []string{".", ".."} {
			resp := twineUpload(t, e.srv.URL, token, "widget", "1.0", bad, wheel)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("filename %q: status = %d, want 400", bad, resp.StatusCode)
			}
		}
	})
	t.Run("serveFile rejects a bad filename", func(t *testing.T) {
		resp := e.do(t, http.MethodGet, "/pypi/packages/widget/bad%20name.whl", token, nil, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

// --- misc ---------------------------------------------------------------------------

func TestHealthzUnauthenticated(t *testing.T) {
	e := newTestEnv(t)
	resp := e.do(t, http.MethodGet, "/healthz", "", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz: %d", resp.StatusCode)
	}
}

func sha256Bytes(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
