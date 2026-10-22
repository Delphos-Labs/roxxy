package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These tests cover the per-repo authorization hook (SetAuthorizer) the
// archifau origin injects into the reused registry. They live in the registry
// package — not archifau/server — so they exercise the dangerous branches
// directly (nil identity, cached-catalog bypass, blob-membership on mount) and
// fail loudly if a refactor flips a fail-closed guard to fail-open. The
// registry must not import archifau/auth (it's shared with the roxxy proxy), so
// these model the identity as a slice of readable namespace prefixes carried in
// the request context, mirroring archifau's CanRead semantics.

type authzTestKey struct{}

// withReadable returns a context carrying the caller's readable namespaces.
// Absent => no identity, which the authorizer below must treat as fail-closed.
func withReadable(ctx context.Context, namespaces ...string) context.Context {
	return context.WithValue(ctx, authzTestKey{}, namespaces)
}

// failClosedAuthorizer mirrors archifau/server's wiring: deny when no identity
// is present, otherwise allow repos under one of the caller's namespaces.
func failClosedAuthorizer(ctx context.Context, repo string) bool {
	ns, ok := ctx.Value(authzTestKey{}).([]string)
	if !ok || ns == nil {
		return false // fail closed if no identity is present
	}
	for _, n := range ns {
		if repo == n || strings.HasPrefix(repo, n+"/") {
			return true
		}
	}
	return false
}

func catalogRepos(t *testing.T, proxy *DockerRegistryProxy, ctx context.Context) []string {
	t.Helper()
	req := httptest.NewRequest("GET", "/v2/_catalog", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("catalog status = %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	return body.Repositories
}

func seedRepoDirs(t *testing.T, proxy *DockerRegistryProxy, repos ...string) {
	t.Helper()
	registryID := normalizeRegistryURL(proxy.upstreamURL)
	for _, repo := range repos {
		dir := filepath.Join(proxy.manifestDir, registryID, repo) // repo already safe-named
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCatalogAuthz_FailsClosedWithoutIdentity proves the nil-identity branch:
// a /v2/_catalog request whose context carries no identity must enumerate
// nothing. If the authorizer's nil-check were flipped to fail-open this test
// would see the seeded repos and fail.
func TestCatalogAuthz_FailsClosedWithoutIdentity(t *testing.T) {
	proxy := newTestProxy(t)
	proxy.SetAuthorizer(failClosedAuthorizer)
	seedRepoDirs(t, proxy, "mine_app", "secret_other")

	// No withReadable on the context => no identity.
	if repos := catalogRepos(t, proxy, context.Background()); len(repos) != 0 {
		t.Errorf("catalog without identity must be empty, got %v", repos)
	}
}

// TestCatalogAuthz_FiltersToReadableNamespaces proves the keep-mine/drop-theirs
// selectivity with two tenants present, so an inverted or substring predicate
// is detectable (not masked by a single-repo fixture).
func TestCatalogAuthz_FiltersToReadableNamespaces(t *testing.T) {
	proxy := newTestProxy(t)
	proxy.SetAuthorizer(failClosedAuthorizer)
	seedRepoDirs(t, proxy, "mine_app", "secret_other")

	mine := catalogRepos(t, proxy, withReadable(context.Background(), "mine"))
	if !slices.Contains(mine, "mine/app") {
		t.Errorf("caller of mine/* should see mine/app, got %v", mine)
	}
	if slices.Contains(mine, "secret/other") {
		t.Errorf("caller of mine/* must not see secret/other, got %v", mine)
	}

	theirs := catalogRepos(t, proxy, withReadable(context.Background(), "secret"))
	if !slices.Contains(theirs, "secret/other") || slices.Contains(theirs, "mine/app") {
		t.Errorf("caller of secret/* should see only secret/other, got %v", theirs)
	}
}

// TestCatalogAuthz_CachedCatalogBypassedWhenFiltering proves the conditional at
// the cache read (d.airgapped && d.authorizeRepo == nil) is the *only* thing
// guarding the unfiltered on-disk _catalog.json. With an authorizer set, a
// seeded cache listing a foreign repo must be ignored in favor of the filtered
// scan. If the guard were changed to `if d.airgapped {` this test would serve
// secret/other from the cache and fail.
func TestCatalogAuthz_CachedCatalogBypassedWhenFiltering(t *testing.T) {
	proxy := newTestProxy(t) // airgapped=true
	proxy.SetAuthorizer(failClosedAuthorizer)

	registryID := normalizeRegistryURL(proxy.upstreamURL)
	dir := filepath.Join(proxy.manifestDir, registryID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-seed an unfiltered cached catalog listing a foreign repo.
	cached := `{"repositories":["mine/app","secret/other"]}`
	if err := os.WriteFile(filepath.Join(dir, "_catalog.json"), []byte(cached), 0o644); err != nil {
		t.Fatal(err)
	}
	// Both repos also exist on disk so the filtered scan can find mine/app.
	seedRepoDirs(t, proxy, "mine_app", "secret_other")

	repos := catalogRepos(t, proxy, withReadable(context.Background(), "mine"))
	if !slices.Contains(repos, "mine/app") {
		t.Errorf("filtered scan should surface mine/app, got %v", repos)
	}
	if slices.Contains(repos, "secret/other") {
		t.Errorf("cached unfiltered catalog must be bypassed; secret/other leaked: %v", repos)
	}
}

// mountResponse issues a cross-repo mount POST and returns the recorder.
func mountResponse(t *testing.T, proxy *DockerRegistryProxy, ctx context.Context, target, mount, from string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/v2/" + target + "/blobs/uploads/?mount=" + mount + "&from=" + from
	req := httptest.NewRequest("POST", url, nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)
	return rr
}

// seedManifestWithBlob stores a blob and a manifest referencing it in repoName,
// returning the blob digest. The blob lands in the global CAS; the manifest in
// repoName's manifest dir — so the blob is a "member" of repoName.
func seedManifestWithBlob(t *testing.T, proxy *DockerRegistryProxy, ctx context.Context, repoName string, blob []byte) string {
	t.Helper()
	digest := computeDigest(blob)

	// Monolithic blob push into the global store.
	post := httptest.NewRequest("POST", "/v2/"+repoName+"/blobs/uploads/", nil).WithContext(ctx)
	postRR := httptest.NewRecorder()
	proxy.handleBlobUpload(postRR, post)
	if postRR.Code != http.StatusAccepted {
		t.Fatalf("blob upload init: %d", postRR.Code)
	}
	loc := postRR.Header().Get("Location")
	put := httptest.NewRequest("PUT", loc+"?digest="+digest, strings.NewReader(string(blob))).WithContext(ctx)
	put.Header.Set("Content-Type", "application/octet-stream")
	putRR := httptest.NewRecorder()
	proxy.handleBlobUpload(putRR, put)
	if putRR.Code != http.StatusCreated {
		t.Fatalf("blob put: %d", putRR.Code)
	}

	// Store a manifest referencing the blob as a layer in repoName.
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[{"digest":"` + digest + `"}]}`
	if err := proxy.storeManifestInStorage(repoName, "v1", []byte(manifest),
		"application/vnd.docker.distribution.manifest.v2+json", proxy.upstreamURL); err != nil {
		t.Fatalf("store manifest: %v", err)
	}
	return digest
}

// TestMountAuthz_ForeignBlobViaOwnReadableRepoDenied is the core regression for
// the global-CAS mount hole: the attacker names their OWN readable repo as
// from= but a digest that belongs to a *different* tenant. CanRead(from) passes,
// so the only thing stopping the cross-mount is the blob-membership check. The
// response must NOT be 201, and the attacker's target repo must not acquire a
// reference to the foreign blob.
func TestMountAuthz_ForeignBlobViaOwnReadableRepoDenied(t *testing.T) {
	proxy := newTestProxy(t)
	proxy.SetAuthorizer(failClosedAuthorizer)

	// Victim tenant pushes a secret blob+manifest into its own namespace; the
	// blob now exists in the global CAS.
	victimCtx := withReadable(context.Background(), "victim")
	secret := seedManifestWithBlob(t, proxy, victimCtx, "victim/app", []byte("victim-secret-layer"))

	// Attacker can read only its own namespace and seeds an unrelated manifest
	// there (so attacker/app exists as a repo) — but NOT the secret digest.
	attackerCtx := withReadable(context.Background(), "attacker")
	seedManifestWithBlob(t, proxy, attackerCtx, "attacker/app", []byte("attacker-own-layer"))

	rr := mountResponse(t, proxy, attackerCtx, "attacker/copy", secret, "attacker/app")
	if rr.Code == http.StatusCreated {
		t.Fatalf("mounting a foreign blob via own readable from= must not be honored (got 201)")
	}
	if loc := rr.Header().Get("Location"); strings.Contains(loc, "/blobs/"+secret) {
		t.Errorf("denied mount must not link the foreign blob, Location=%q", loc)
	}
}

// TestMountAuthz_MemberBlobHonored is the positive case: from= legitimately
// contains the blob, so the mount is honored with the mount-target Location.
func TestMountAuthz_MemberBlobHonored(t *testing.T) {
	proxy := newTestProxy(t)
	proxy.SetAuthorizer(failClosedAuthorizer)

	ctx := withReadable(context.Background(), "mine")
	digest := seedManifestWithBlob(t, proxy, ctx, "mine/app", []byte("mine-layer"))

	rr := mountResponse(t, proxy, ctx, "mine/copy", digest, "mine/app")
	if rr.Code != http.StatusCreated {
		t.Fatalf("mount of a member blob from a readable repo should be honored, got %d", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/v2/mine/copy/blobs/"+digest {
		t.Errorf("mount Location = %q", loc)
	}
	if dig := rr.Header().Get("Docker-Content-Digest"); dig != digest {
		t.Errorf("mount digest header = %q", dig)
	}
}

// TestMountAuthz_TraversalFromRejected proves from= is path-validated before
// the read check: a traversal source that prefix-matches a readable namespace
// must be rejected.
func TestMountAuthz_TraversalFromRejected(t *testing.T) {
	proxy := newTestProxy(t)
	proxy.SetAuthorizer(failClosedAuthorizer)

	ctx := withReadable(context.Background(), "mine")
	digest := seedManifestWithBlob(t, proxy, ctx, "mine/app", []byte("mine-layer"))

	// from= prefix-matches "mine/" but escapes it via ../..; must not mount.
	rr := mountResponse(t, proxy, ctx, "mine/copy", digest, "mine/app/../../secret/other")
	if rr.Code == http.StatusCreated {
		t.Errorf("traversal from= must be rejected (got 201)")
	}
}

// TestTruncDigest guards the mount log slice: a sub-19-char mount= value (e.g.
// "sha256:ab") must not panic the handler goroutine via a raw digest[:19].
func TestTruncDigest(t *testing.T) {
	if got := truncDigest("sha256:ab"); got != "sha256:ab" {
		t.Errorf("truncDigest(short) = %q, want the input unchanged", got)
	}
	long := "sha256:abcdef0123456789"
	if got := truncDigest(long); got != long[:19] {
		t.Errorf("truncDigest(long) = %q, want %q", got, long[:19])
	}
}

// TestProxyConstruction_AuthorizerNilByDefault pins the load-bearing contract:
// a freshly constructed proxy (the roxxy proxy's path — it never calls
// SetAuthorizer) leaves authorizeRepo nil, i.e. allow-all, preserving the
// proxy's prior behavior. If a refactor ever defaulted this to a deny-all
// callback it would silently break the shared proxy; this catches it.
func TestProxyConstruction_AuthorizerNilByDefault(t *testing.T) {
	proxy := newTestProxy(t)
	if proxy.authorizeRepo != nil {
		t.Error("newly constructed proxy must leave authorizeRepo nil (roxxy allow-all default)")
	}
}

func TestIsValidRepoName(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"delphos-labs/widget/app", true},
		{"a", true},
		{"", false},
		{"/a", false},
		{"a/", false},
		{"a//b", false},
		{"a/./b", false},
		{"a/../b", false},
		{"delphos-labs/other/../../widget/app", false},
		{"Delphos-Labs/Widget", false},
	}
	for _, tc := range cases {
		if got := isValidRepoName(tc.in); got != tc.want {
			t.Errorf("isValidRepoName(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
