package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Delphos-Labs/roxxy/internal/cache"
)

// readStoredRefcount is a test helper that returns the on-disk refcount for
// a blob (without the "sha256:" prefix), or -1 if no refcount file exists.
func readStoredRefcount(t *testing.T, cacheDir, digest string) int {
	t.Helper()
	hash := strings.TrimPrefix(digest, "sha256:")
	data, err := os.ReadFile(cache.GetRefcountPath(cacheDir, hash))
	if err != nil {
		if os.IsNotExist(err) {
			return -1
		}
		t.Fatalf("read refcount: %v", err)
	}
	n := 0
	if _, err := fmt.Sscanf(string(data), "%d", &n); err != nil {
		t.Fatalf("parse refcount %q: %v", string(data), err)
	}
	return n
}

func makeImageManifest(t *testing.T, configDigest string, layerDigests ...string) []byte {
	t.Helper()
	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
		"config":        map[string]any{"digest": configDigest, "size": 1},
		"layers": func() []map[string]any {
			out := make([]map[string]any, 0, len(layerDigests))
			for _, d := range layerDigests {
				out = append(out, map[string]any{"digest": d, "size": 1})
			}
			return out
		}(),
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// TestStoreManifestInStorage_RefcountDelta is the regression test for the
// "deleting an old tag eats the new tag's blobs" bug: each manifest file on
// disk must contribute exactly +1 to every blob digest it lists, and the
// delta must be applied atomically with the file write.
func TestStoreManifestInStorage_RefcountDelta(t *testing.T) {
	cacheDir := t.TempDir()
	p := &DockerRegistryProxy{
		cacheDir:    cacheDir,
		manifestDir: filepath.Join(cacheDir, "docker-manifests"),
		blobDir:     filepath.Join(cacheDir, "docker-blobs"),
		upstreamURL: "https://registry-1.docker.io",
	}

	cfg := "sha256:cfg0000000000000000000000000000000000000000000000000000000000000"
	layer := "sha256:layer00000000000000000000000000000000000000000000000000000000001"
	body := makeImageManifest(t, cfg, layer)

	// First write (digest-file) → +1 per blob.
	if err := p.storeManifestInStorage("library/nginx", "sha256:abcd", body, "application/vnd.docker.distribution.manifest.v2+json", p.upstreamURL); err != nil {
		t.Fatalf("store digest: %v", err)
	}
	if got := readStoredRefcount(t, cacheDir, cfg); got != 1 {
		t.Fatalf("after digest write: cfg refcount = %d, want 1", got)
	}
	if got := readStoredRefcount(t, cacheDir, layer); got != 1 {
		t.Fatalf("after digest write: layer refcount = %d, want 1", got)
	}

	// Second write of same body to tag-file → +1 per blob (audit invariant:
	// each on-disk manifest file contributes a reference).
	if err := p.storeManifestInStorage("library/nginx", "latest", body, "application/vnd.docker.distribution.manifest.v2+json", p.upstreamURL); err != nil {
		t.Fatalf("store tag: %v", err)
	}
	if got := readStoredRefcount(t, cacheDir, cfg); got != 2 {
		t.Fatalf("after tag write: cfg refcount = %d, want 2", got)
	}
	if got := readStoredRefcount(t, cacheDir, layer); got != 2 {
		t.Fatalf("after tag write: layer refcount = %d, want 2", got)
	}

	// Re-write tag-file with same body → no-op.
	if err := p.storeManifestInStorage("library/nginx", "latest", body, "application/vnd.docker.distribution.manifest.v2+json", p.upstreamURL); err != nil {
		t.Fatalf("re-store tag: %v", err)
	}
	if got := readStoredRefcount(t, cacheDir, cfg); got != 2 {
		t.Fatalf("after re-tag same body: cfg refcount = %d, want 2", got)
	}

	// Overwrite tag-file with NEW body sharing one blob and adding one new.
	newLayer := "sha256:layer00000000000000000000000000000000000000000000000000000000002"
	newBody := makeImageManifest(t, cfg, newLayer) // cfg shared, layer→newLayer
	if err := p.storeManifestInStorage("library/nginx", "latest", newBody, "application/vnd.docker.distribution.manifest.v2+json", p.upstreamURL); err != nil {
		t.Fatalf("overwrite tag: %v", err)
	}
	// cfg appeared in both old and new bodies of the rewritten file: net 0.
	// Total across the two files on disk is now: digest-file(cfg+layer) +
	// tag-file(cfg+newLayer) → cfg=2, layer=1, newLayer=1.
	if got := readStoredRefcount(t, cacheDir, cfg); got != 2 {
		t.Fatalf("after overwrite: cfg refcount = %d, want 2", got)
	}
	if got := readStoredRefcount(t, cacheDir, layer); got != 1 {
		t.Fatalf("after overwrite: layer (old-only) refcount = %d, want 1", got)
	}
	if got := readStoredRefcount(t, cacheDir, newLayer); got != 1 {
		t.Fatalf("after overwrite: newLayer refcount = %d, want 1", got)
	}
}

func TestNormalizeRegistryURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"docker.io https", "https://registry-1.docker.io", "docker.io"},
		{"docker.io http", "http://registry-1.docker.io", "docker.io"},
		{"docker.io no protocol", "registry-1.docker.io", "docker.io"},
		{"docker.io just domain", "docker.io", "docker.io"},
		{"ghcr.io", "https://ghcr.io", "ghcr.io"},
		{"quay.io", "https://quay.io", "quay.io"},
		{"with port", "https://registry.example.com:5000", "registry.example.com"},
		{"with path", "https://registry.example.com/v2/", "registry.example.com_v2"},
		{"with slashes", "https://registry.example.com/path/to/registry", "registry.example.com_path_to_registry"},
		{"with colons in domain", "https://registry:example:com", "registry"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeRegistryURL(tt.input)
			if got != tt.expected {
				t.Errorf("normalizeRegistryURL(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestDockerRegistryProxy_extractRepoNameFromPath(t *testing.T) {
	cacheDir := t.TempDir()
	proxy := &DockerRegistryProxy{
		manifestDir: filepath.Join(cacheDir, "docker-manifests"),
		blobDir:     filepath.Join(cacheDir, "docker-blobs"),
	}

	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{"manifest path", "/v2/library/nginx/manifests/latest", "library/nginx"},
		{"manifest path with tag", "/v2/myorg/myapp/manifests/v1.0.0", "myorg/myapp"},
		{"manifest path with digest", "/v2/myorg/myapp/manifests/sha256:abc123", "myorg/myapp"},
		{"blob path", "/v2/library/nginx/blobs/sha256:def456", "library/nginx"},
		{"tags list path", "/v2/myorg/myapp/tags/list", "myorg/myapp"},
		{"nested repo", "/v2/org/team/project/app/manifests/latest", "org/team/project/app"},
		{"too short", "/v2/library", ""},
		{"invalid path", "/v2", ""},
		{"root path", "/", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := proxy.extractRepoNameFromPath(tt.path)
			if got != tt.expected {
				t.Errorf("extractRepoNameFromPath(%q) = %q, want %q", tt.path, got, tt.expected)
			}
		})
	}
}

func TestDockerRegistryProxy_getManifestPath(t *testing.T) {
	cacheDir := t.TempDir()
	proxy := &DockerRegistryProxy{
		manifestDir: filepath.Join(cacheDir, "docker-manifests"),
		blobDir:     filepath.Join(cacheDir, "docker-blobs"),
	}

	tests := []struct {
		name        string
		repoName    string
		reference   string
		upstreamURL string
		expected    string
	}{
		{
			"docker.io with tag",
			"library/nginx",
			"latest",
			"https://registry-1.docker.io",
			filepath.Join(cacheDir, "docker-manifests", "docker.io", "library_nginx", "latest"),
		},
		{
			"ghcr.io with digest",
			"myorg/myapp",
			"sha256:abc123def456",
			"https://ghcr.io",
			filepath.Join(cacheDir, "docker-manifests", "ghcr.io", "myorg_myapp", "sha256:abc123def456"),
		},
		{
			"nested repo",
			"org/team/project/app",
			"v1.0.0",
			"https://registry.example.com",
			filepath.Join(cacheDir, "docker-manifests", "registry.example.com", "org_team_project_app", "v1.0.0"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := proxy.getManifestPath(tt.repoName, tt.reference, tt.upstreamURL)
			if got != tt.expected {
				t.Errorf("getManifestPath(%q, %q, %q) = %q, want %q",
					tt.repoName, tt.reference, tt.upstreamURL, got, tt.expected)
			}
		})
	}
}

func TestDockerRegistryProxy_getBlobPath(t *testing.T) {
	cacheDir := t.TempDir()
	proxy := &DockerRegistryProxy{
		manifestDir: filepath.Join(cacheDir, "docker-manifests"),
		blobDir:     filepath.Join(cacheDir, "docker-blobs"),
	}

	tests := []struct {
		name     string
		digest   string
		expected string
	}{
		{
			"sha256 digest",
			"sha256:abc123def4567890abcdef1234567890abcdef1234567890abcdef1234567890",
			filepath.Join(cacheDir, "docker-blobs", "sha256", "ab", "c1", "sha256:abc123def4567890abcdef1234567890abcdef1234567890abcdef1234567890"),
		},
		{
			"sha512 digest",
			"sha512:def456",
			filepath.Join(cacheDir, "docker-blobs", "sha512:def456"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := proxy.getBlobPath(tt.digest)
			if got != tt.expected {
				t.Errorf("getBlobPath(%q) = %q, want %q", tt.digest, got, tt.expected)
			}
		})
	}
}

func TestDockerRegistryProxy_isLocalOnlyPath(t *testing.T) {
	cacheDir := t.TempDir()

	tests := []struct {
		name           string
		localOnlyPaths []string
		repoName       string
		expected       bool
	}{
		{
			"no local only paths",
			nil,
			"myorg/myapp",
			false,
		},
		{
			"empty local only paths",
			[]string{},
			"myorg/myapp",
			false,
		},
		{
			"matches prefix",
			[]string{"myorg/"},
			"myorg/myapp",
			true,
		},
		{
			"matches prefix without trailing slash",
			[]string{"myorg"},
			"myorg/myapp",
			true,
		},
		{
			"does not match",
			[]string{"otherorg/"},
			"myorg/myapp",
			false,
		},
		{
			"matches nested prefix",
			[]string{"myorg/team/"},
			"myorg/team/project/app",
			true,
		},
		{
			"multiple prefixes, matches first",
			[]string{"myorg/", "otherorg/"},
			"myorg/myapp",
			true,
		},
		{
			"multiple prefixes, matches second",
			[]string{"myorg/", "otherorg/"},
			"otherorg/myapp",
			true,
		},
		{
			"multiple prefixes, matches none",
			[]string{"myorg/", "otherorg/"},
			"thirdorg/myapp",
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy := &DockerRegistryProxy{
				manifestDir:    filepath.Join(cacheDir, "docker-manifests"),
				blobDir:        filepath.Join(cacheDir, "docker-blobs"),
				localOnlyPaths: tt.localOnlyPaths,
			}
			got := proxy.isLocalOnlyPath(tt.repoName)
			if got != tt.expected {
				t.Errorf("isLocalOnlyPath(%q) with paths %v = %v, want %v",
					tt.repoName, tt.localOnlyPaths, got, tt.expected)
			}
		})
	}
}

// Helper to create a test proxy with temp directories
func newTestProxy(t *testing.T) *DockerRegistryProxy {
	t.Helper()
	cacheDir := t.TempDir()

	// NewDockerRegistryProxy creates all necessary directories including tmp/
	return NewDockerRegistryProxy(
		"https://registry-1.docker.io",
		nil,   // statsTracker
		nil,   // hostnames
		false, // airgapProxy
		true,  // airgapped (local storage only)
		false, // proxyPushes
		nil,   // localOnlyPaths
		cacheDir,
		nil,   // sbomManager
		false, // debug
	)
}

// Helper to compute SHA256 digest of data
func computeDigest(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// Helper to extract upload UUID from Location header
func extractUploadUUID(location string) string {
	parts := strings.Split(location, "/")
	return parts[len(parts)-1]
}

func TestCatalog_EmptyStorage(t *testing.T) {
	proxy := newTestProxy(t)

	req := httptest.NewRequest("GET", "/v2/_catalog", nil)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
	// Should return empty repositories list
	body := rr.Body.String()
	if !strings.Contains(body, `"repositories"`) {
		t.Errorf("expected repositories key in response, got %s", body)
	}
}

func TestCatalog_WithRepositories(t *testing.T) {
	proxy := newTestProxy(t)

	// Create fake repository directories in the manifest store
	registryID := normalizeRegistryURL(proxy.upstreamURL)
	registryDir := filepath.Join(proxy.manifestDir, registryID)

	for _, repo := range []string{"library_nginx", "myorg_myapp"} {
		if err := os.MkdirAll(filepath.Join(registryDir, repo), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest("GET", "/v2/_catalog", nil)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}

	body := rr.Body.String()
	if !strings.Contains(body, "library/nginx") {
		t.Errorf("expected library/nginx in response, got %s", body)
	}
	if !strings.Contains(body, "myorg/myapp") {
		t.Errorf("expected myorg/myapp in response, got %s", body)
	}
}

func TestCatalog_CachedCatalogFile_Airgapped(t *testing.T) {
	// In airgapped mode, cached _catalog.json takes priority
	proxy := newTestProxy(t) // newTestProxy sets airgapped=true

	registryID := normalizeRegistryURL(proxy.upstreamURL)
	catalogDir := filepath.Join(proxy.manifestDir, registryID)
	if err := os.MkdirAll(catalogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	catalogJSON := `{"repositories":["cached/repo1","cached/repo2"]}`
	if err := os.WriteFile(filepath.Join(catalogDir, "_catalog.json"), []byte(catalogJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/v2/_catalog", nil)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}

	body := rr.Body.String()
	if body != catalogJSON {
		t.Errorf("expected cached catalog %q, got %q", catalogJSON, body)
	}
}

func TestCatalog_CachedFileIgnored_NonAirgapped(t *testing.T) {
	// In non-airgapped mode, cached _catalog.json is stale — always scan
	cacheDir := t.TempDir()
	proxy := NewDockerRegistryProxy(
		"https://registry-1.docker.io",
		nil, nil, false, false, false, nil, cacheDir, nil, false,
	)

	registryID := normalizeRegistryURL(proxy.upstreamURL)
	registryDir := filepath.Join(proxy.manifestDir, registryID)
	if err := os.MkdirAll(registryDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a stale cached catalog that lists repos NOT on disk
	staleJSON := `{"repositories":["stale/gone"]}`
	if err := os.WriteFile(filepath.Join(registryDir, "_catalog.json"), []byte(staleJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a repo directory that IS on disk
	if err := os.MkdirAll(filepath.Join(registryDir, "library_nginx"), 0o755); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/v2/_catalog", nil)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}

	body := rr.Body.String()
	// Should reflect the filesystem scan, not the stale cached file
	if strings.Contains(body, "stale/gone") {
		t.Errorf("non-airgapped catalog should not use stale cached file, got %s", body)
	}
	if !strings.Contains(body, "library/nginx") {
		t.Errorf("expected library/nginx from filesystem scan, got %s", body)
	}
}

func TestCatalog_NonAirgapped(t *testing.T) {
	// Verify _catalog works in non-airgapped mode (the behavior we added)
	cacheDir := t.TempDir()
	proxy := NewDockerRegistryProxy(
		"https://registry-1.docker.io",
		nil,   // statsTracker
		nil,   // hostnames
		false, // airgapProxy
		false, // airgapped = false
		false, // proxyPushes
		nil,   // localOnlyPaths
		cacheDir,
		nil,   // sbomManager
		false, // debug
	)

	// Create a repo directory
	registryID := normalizeRegistryURL(proxy.upstreamURL)
	repoDir := filepath.Join(proxy.manifestDir, registryID, "library_alpine")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/v2/_catalog", nil)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}

	body := rr.Body.String()
	if !strings.Contains(body, "library/alpine") {
		t.Errorf("expected library/alpine in non-airgapped catalog response, got %s", body)
	}
}

func TestBlobUpload_Chunked(t *testing.T) {
	// Test: POST → PATCH → PUT (traditional chunked upload)
	proxy := newTestProxy(t)

	blobData := []byte("hello world - this is test blob data for chunked upload")
	digest := computeDigest(blobData)

	// Step 1: POST to initiate upload
	req := httptest.NewRequest("POST", "/v2/test/repo/blobs/uploads/", nil)
	rr := httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST initiate: expected status %d, got %d: %s", http.StatusAccepted, rr.Code, rr.Body.String())
	}

	location := rr.Header().Get("Location")
	if location == "" {
		t.Fatal("POST initiate: missing Location header")
	}
	uploadUUID := extractUploadUUID(location)

	// Step 2: PATCH to upload chunk data
	req = httptest.NewRequest("PATCH", location, bytes.NewReader(blobData))
	req.Header.Set("Content-Type", "application/octet-stream")
	rr = httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("PATCH chunk: expected status %d, got %d: %s", http.StatusNoContent, rr.Code, rr.Body.String())
	}

	// Step 3: PUT to finalize (no body data)
	req = httptest.NewRequest("PUT", location+"?digest="+digest, nil)
	rr = httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("PUT finalize: expected status %d, got %d: %s", http.StatusCreated, rr.Code, rr.Body.String())
	}

	// Verify the blob was stored correctly
	blobPath := proxy.getBlobPath(digest)
	storedData, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("Failed to read stored blob: %v", err)
	}
	if !bytes.Equal(storedData, blobData) {
		t.Errorf("Stored blob data mismatch: got %d bytes, expected %d bytes", len(storedData), len(blobData))
	}

	t.Logf("Chunked upload successful: UUID=%s, digest=%s", uploadUUID[:8], digest[:19])
}

func TestBlobUpload_Monolithic(t *testing.T) {
	// Test: POST → PUT with body (monolithic upload - the bug case)
	proxy := newTestProxy(t)

	blobData := []byte("hello world - this is test blob data for monolithic upload")
	digest := computeDigest(blobData)

	// Step 1: POST to initiate upload
	req := httptest.NewRequest("POST", "/v2/test/repo/blobs/uploads/", nil)
	rr := httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST initiate: expected status %d, got %d: %s", http.StatusAccepted, rr.Code, rr.Body.String())
	}

	location := rr.Header().Get("Location")
	if location == "" {
		t.Fatal("POST initiate: missing Location header")
	}
	uploadUUID := extractUploadUUID(location)

	// Step 2: PUT with all data in body (skip PATCH entirely)
	req = httptest.NewRequest("PUT", location+"?digest="+digest, bytes.NewReader(blobData))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(blobData))
	rr = httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("PUT finalize: expected status %d, got %d: %s", http.StatusCreated, rr.Code, rr.Body.String())
	}

	// Verify the blob was stored correctly
	blobPath := proxy.getBlobPath(digest)
	storedData, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("Failed to read stored blob: %v", err)
	}
	if !bytes.Equal(storedData, blobData) {
		t.Errorf("Stored blob data mismatch: got %d bytes, expected %d bytes", len(storedData), len(blobData))
	}

	t.Logf("Monolithic upload successful: UUID=%s, digest=%s", uploadUUID[:8], digest[:19])
}

func TestBlobUpload_ChunkedWithFinalChunk(t *testing.T) {
	// Test: POST → PATCH → PUT with final chunk in body
	proxy := newTestProxy(t)

	chunk1 := []byte("first chunk of data - ")
	chunk2 := []byte("second and final chunk")
	fullData := append(chunk1, chunk2...)
	digest := computeDigest(fullData)

	// Step 1: POST to initiate upload
	req := httptest.NewRequest("POST", "/v2/test/repo/blobs/uploads/", nil)
	rr := httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST initiate: expected status %d, got %d: %s", http.StatusAccepted, rr.Code, rr.Body.String())
	}

	location := rr.Header().Get("Location")
	uploadUUID := extractUploadUUID(location)

	// Step 2: PATCH first chunk
	req = httptest.NewRequest("PATCH", location, bytes.NewReader(chunk1))
	req.Header.Set("Content-Type", "application/octet-stream")
	rr = httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("PATCH chunk: expected status %d, got %d: %s", http.StatusNoContent, rr.Code, rr.Body.String())
	}

	// Step 3: PUT with final chunk in body
	req = httptest.NewRequest("PUT", location+"?digest="+digest, bytes.NewReader(chunk2))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(chunk2))
	rr = httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("PUT finalize: expected status %d, got %d: %s", http.StatusCreated, rr.Code, rr.Body.String())
	}

	// Verify the blob was stored correctly with both chunks
	blobPath := proxy.getBlobPath(digest)
	storedData, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("Failed to read stored blob: %v", err)
	}
	if !bytes.Equal(storedData, fullData) {
		t.Errorf("Stored blob data mismatch: got %q, expected %q", string(storedData), string(fullData))
	}

	t.Logf("Chunked upload with final chunk successful: UUID=%s, digest=%s", uploadUUID[:8], digest[:19])
}

func TestBlobUpload_DigestMismatch(t *testing.T) {
	// Test that digest verification catches mismatches
	proxy := newTestProxy(t)

	blobData := []byte("actual data")
	wrongDigest := computeDigest([]byte("different data"))

	// POST to initiate
	req := httptest.NewRequest("POST", "/v2/test/repo/blobs/uploads/", nil)
	rr := httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	location := rr.Header().Get("Location")

	// PUT with wrong digest
	req = httptest.NewRequest("PUT", location+"?digest="+wrongDigest, bytes.NewReader(blobData))
	req.ContentLength = int64(len(blobData))
	rr = httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("Expected status %d for digest mismatch, got %d", http.StatusInternalServerError, rr.Code)
	}

	if !strings.Contains(rr.Body.String(), "digest mismatch") {
		t.Errorf("Expected 'digest mismatch' error, got: %s", rr.Body.String())
	}
}

func TestBlobUpload_EmptyUpload(t *testing.T) {
	// Test that empty upload (POST → PUT with no data) fails appropriately
	proxy := newTestProxy(t)

	digest := computeDigest([]byte{})

	// POST to initiate
	req := httptest.NewRequest("POST", "/v2/test/repo/blobs/uploads/", nil)
	rr := httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	location := rr.Header().Get("Location")

	// PUT with no body data
	req = httptest.NewRequest("PUT", location+"?digest="+digest, nil)
	req.ContentLength = 0
	rr = httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	// Should fail because temp file is empty and no body data
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("Expected status %d for empty upload, got %d: %s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
}

func TestBlobUpload_MultipleChunks(t *testing.T) {
	// Test multiple PATCH requests followed by PUT
	proxy := newTestProxy(t)

	chunks := [][]byte{
		[]byte("chunk 1 - "),
		[]byte("chunk 2 - "),
		[]byte("chunk 3 - "),
		[]byte("final chunk"),
	}

	var fullData []byte
	for _, chunk := range chunks {
		fullData = append(fullData, chunk...)
	}
	digest := computeDigest(fullData)

	// POST to initiate
	req := httptest.NewRequest("POST", "/v2/test/repo/blobs/uploads/", nil)
	rr := httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	location := rr.Header().Get("Location")

	// PATCH each chunk
	for i, chunk := range chunks {
		req = httptest.NewRequest("PATCH", location, bytes.NewReader(chunk))
		rr = httptest.NewRecorder()
		proxy.handleBlobUpload(rr, req)

		if rr.Code != http.StatusNoContent {
			t.Fatalf("PATCH chunk %d: expected status %d, got %d", i+1, http.StatusNoContent, rr.Code)
		}
	}

	// PUT to finalize
	req = httptest.NewRequest("PUT", location+"?digest="+digest, nil)
	rr = httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("PUT finalize: expected status %d, got %d: %s", http.StatusCreated, rr.Code, rr.Body.String())
	}

	// Verify
	blobPath := proxy.getBlobPath(digest)
	storedData, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("Failed to read stored blob: %v", err)
	}
	if !bytes.Equal(storedData, fullData) {
		t.Errorf("Stored blob data mismatch")
	}
}

func TestBlobUpload_LargeBlob(t *testing.T) {
	// Test with a larger blob to ensure streaming works
	proxy := newTestProxy(t)

	// Create 1MB of test data
	blobData := make([]byte, 1024*1024)
	for i := range blobData {
		blobData[i] = byte(i % 256)
	}
	digest := computeDigest(blobData)

	// POST to initiate
	req := httptest.NewRequest("POST", "/v2/test/repo/blobs/uploads/", nil)
	rr := httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	location := rr.Header().Get("Location")

	// Monolithic PUT
	req = httptest.NewRequest("PUT", location+"?digest="+digest, bytes.NewReader(blobData))
	req.ContentLength = int64(len(blobData))
	rr = httptest.NewRecorder()
	proxy.handleBlobUpload(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("PUT finalize: expected status %d, got %d: %s", http.StatusCreated, rr.Code, rr.Body.String())
	}

	// Verify
	blobPath := proxy.getBlobPath(digest)
	storedData, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("Failed to read stored blob: %v", err)
	}
	if !bytes.Equal(storedData, blobData) {
		t.Errorf("Large blob data mismatch")
	}

	t.Logf("Large blob upload successful: %d bytes", len(blobData))
}

func TestPaginateStringSlice(t *testing.T) {
	items := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}

	tests := []struct {
		name     string
		n        string
		last     string
		expected []string
	}{
		{"no pagination", "", "", items},
		{"first 2", "2", "", []string{"alpha", "bravo"}},
		{"after bravo", "2", "bravo", []string{"charlie", "delta"}},
		{"after delta", "10", "delta", []string{"echo", "foxtrot"}},
		{"after last item", "2", "foxtrot", []string{}},
		{"last only", "", "charlie", []string{"delta", "echo", "foxtrot"}},
		{"n larger than remaining", "100", "", items},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := url.Values{}
			if tt.n != "" {
				q.Set("n", tt.n)
			}
			if tt.last != "" {
				q.Set("last", tt.last)
			}
			// Make a copy since paginateStringSlice returns a slice of the input
			input := make([]string, len(items))
			copy(input, items)
			got := paginateStringSlice(input, q)
			if len(got) == 0 && len(tt.expected) == 0 {
				return
			}
			if len(got) != len(tt.expected) {
				t.Fatalf("got %v, want %v", got, tt.expected)
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Fatalf("got %v, want %v", got, tt.expected)
				}
			}
		})
	}
}

func TestCatalog_Pagination(t *testing.T) {
	cacheDir := t.TempDir()
	proxy := NewDockerRegistryProxy("https://registry-1.docker.io", nil, nil, false, false, false, nil, cacheDir, nil, false)

	// Create 3 repositories
	registryID := normalizeRegistryURL(proxy.upstreamURL)
	for _, repo := range []string{"alpha", "bravo", "charlie"} {
		repoDir := filepath.Join(proxy.manifestDir, registryID, repo)
		if err := os.MkdirAll(repoDir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Request first 2
	req := httptest.NewRequest("GET", "/v2/_catalog?n=2", nil)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Repositories) != 2 {
		t.Fatalf("expected 2 repos, got %d: %v", len(resp.Repositories), resp.Repositories)
	}

	// Check Link header
	link := rr.Header().Get("Link")
	if link == "" {
		t.Fatal("expected Link header for pagination")
	}
	if !strings.Contains(link, "rel=\"next\"") {
		t.Fatalf("Link header missing rel=next: %s", link)
	}

	// Request next page using last
	req = httptest.NewRequest("GET", "/v2/_catalog?n=2&last=bravo", nil)
	rr = httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Repositories) != 1 || resp.Repositories[0] != "charlie" {
		t.Fatalf("expected [charlie], got %v", resp.Repositories)
	}
	// No Link header on last page
	if rr.Header().Get("Link") != "" {
		t.Fatal("expected no Link header on last page")
	}
}

func TestReferrers_Empty(t *testing.T) {
	cacheDir := t.TempDir()
	proxy := NewDockerRegistryProxy("https://ghcr.io", nil, nil, false, false, false, nil, cacheDir, nil, false)

	req := httptest.NewRequest("GET", "/v2/myrepo/referrers/sha256:abc123", nil)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/vnd.oci.image.index.v1+json" {
		t.Fatalf("unexpected content type: %s", ct)
	}

	var idx struct {
		SchemaVersion int               `json:"schemaVersion"`
		MediaType     string            `json:"mediaType"`
		Manifests     []json.RawMessage `json:"manifests"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&idx); err != nil {
		t.Fatal(err)
	}
	if idx.SchemaVersion != 2 {
		t.Fatalf("expected schemaVersion 2, got %d", idx.SchemaVersion)
	}
	if len(idx.Manifests) != 0 {
		t.Fatalf("expected 0 manifests, got %d", len(idx.Manifests))
	}
}

func TestReferrers_WithSubject(t *testing.T) {
	cacheDir := t.TempDir()
	proxy := NewDockerRegistryProxy("https://ghcr.io", nil, nil, false, false, false, nil, cacheDir, nil, false)

	registryID := normalizeRegistryURL(proxy.upstreamURL)
	repoDir := filepath.Join(proxy.manifestDir, registryID, "myrepo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}

	targetDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Create a manifest that references our target via subject
	sbomManifest := `{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"artifactType": "application/spdx+json",
		"subject": {"digest": "` + targetDigest + `"}
	}`
	sbomDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := os.WriteFile(filepath.Join(repoDir, sbomDigest), []byte(sbomManifest), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a manifest that does NOT reference our target
	otherManifest := `{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"subject": {"digest": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}
	}`
	otherDigest := "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	if err := os.WriteFile(filepath.Join(repoDir, otherDigest), []byte(otherManifest), 0644); err != nil {
		t.Fatal(err)
	}

	// Query referrers for targetDigest
	req := httptest.NewRequest("GET", "/v2/myrepo/referrers/"+targetDigest, nil)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var idx struct {
		Manifests []struct {
			Digest       string `json:"digest"`
			ArtifactType string `json:"artifactType"`
		} `json:"manifests"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Manifests) != 1 {
		t.Fatalf("expected 1 referrer, got %d", len(idx.Manifests))
	}
	if idx.Manifests[0].Digest != sbomDigest {
		t.Fatalf("expected digest %s, got %s", sbomDigest, idx.Manifests[0].Digest)
	}
	if idx.Manifests[0].ArtifactType != "application/spdx+json" {
		t.Fatalf("expected artifactType application/spdx+json, got %s", idx.Manifests[0].ArtifactType)
	}
}

func TestReferrers_ArtifactTypeFilter(t *testing.T) {
	cacheDir := t.TempDir()
	proxy := NewDockerRegistryProxy("https://ghcr.io", nil, nil, false, false, false, nil, cacheDir, nil, false)

	registryID := normalizeRegistryURL(proxy.upstreamURL)
	repoDir := filepath.Join(proxy.manifestDir, registryID, "myrepo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}

	targetDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// SBOM referrer
	sbomManifest := `{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"artifactType": "application/spdx+json",
		"subject": {"digest": "` + targetDigest + `"}
	}`
	if err := os.WriteFile(filepath.Join(repoDir, "sha256:1111111111111111111111111111111111111111111111111111111111111111"), []byte(sbomManifest), 0644); err != nil {
		t.Fatal(err)
	}

	// Signature referrer
	sigManifest := `{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"artifactType": "application/vnd.cncf.notary.signature",
		"subject": {"digest": "` + targetDigest + `"}
	}`
	if err := os.WriteFile(filepath.Join(repoDir, "sha256:2222222222222222222222222222222222222222222222222222222222222222"), []byte(sigManifest), 0644); err != nil {
		t.Fatal(err)
	}

	// Filter by SBOM only
	req := httptest.NewRequest("GET", "/v2/myrepo/referrers/"+targetDigest+"?artifactType="+url.QueryEscape("application/spdx+json"), nil)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	var idx struct {
		Manifests []struct {
			ArtifactType string `json:"artifactType"`
		} `json:"manifests"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Manifests) != 1 {
		t.Fatalf("expected 1 filtered referrer, got %d", len(idx.Manifests))
	}
	if idx.Manifests[0].ArtifactType != "application/spdx+json" {
		t.Fatalf("expected SBOM, got %s", idx.Manifests[0].ArtifactType)
	}

	// Check OCI-Filters-Applied header
	if rr.Header().Get("OCI-Filters-Applied") != "artifactType" {
		t.Fatal("expected OCI-Filters-Applied header")
	}
}

func TestUploadStatus_GET(t *testing.T) {
	cacheDir := t.TempDir()
	proxy := NewDockerRegistryProxy("https://ghcr.io", nil, nil, false, false, false, nil, cacheDir, nil, false)

	// Create a temp file simulating an in-progress upload
	tmpFile, err := os.CreateTemp(filepath.Join(cacheDir, "tmp"), "upload-test-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	// Write some data to simulate partial upload
	if _, err := tmpFile.Write([]byte("partial upload data here")); err != nil {
		t.Fatal(err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatal(err)
	}

	uuid := "deadbeef12345678"
	proxy.uploadMutex.Lock()
	proxy.uploadMap[uuid] = tmpFile.Name()
	proxy.uploadMutex.Unlock()

	req := httptest.NewRequest("GET", "/v2/myrepo/blobs/uploads/"+uuid, nil)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
	if rr.Header().Get("Docker-Upload-UUID") != uuid {
		t.Fatalf("expected UUID header %s, got %s", uuid, rr.Header().Get("Docker-Upload-UUID"))
	}
	if !strings.HasPrefix(rr.Header().Get("Range"), "0-") {
		t.Fatalf("expected Range header, got %s", rr.Header().Get("Range"))
	}
	if !strings.Contains(rr.Header().Get("Location"), uuid) {
		t.Fatalf("expected Location header with UUID, got %s", rr.Header().Get("Location"))
	}
}

func TestUploadStatus_GET_NotFound(t *testing.T) {
	cacheDir := t.TempDir()
	proxy := NewDockerRegistryProxy("https://ghcr.io", nil, nil, false, false, false, nil, cacheDir, nil, false)

	req := httptest.NewRequest("GET", "/v2/myrepo/blobs/uploads/nonexistent", nil)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}
