package registry

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeImageRef(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"nginx", "nginx"},
		{"nginx:latest", "nginx:latest"},
		{"library/nginx", "nginx"},
		{"library/nginx:latest", "nginx:latest"},
		{"docker:27-dind", "docker:27-dind"},
		{"ghcr.io/astral-sh/uv:0.9.6", "ghcr.io/astral-sh/uv:0.9.6"},
		{"delphos/node:22-slim", "delphos/node:22-slim"},
		// Digest refs pass through unchanged
		{"nginx@sha256:abcd1234", "nginx@sha256:abcd1234"},
	}

	for _, tt := range tests {
		got := normalizeImageRef(tt.input)
		if got != tt.want {
			t.Errorf("normalizeImageRef(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestOverrideStore_CRUD(t *testing.T) {
	dir := t.TempDir()

	store := NewOverrideStore(dir)

	// Initially empty
	overrides := store.List()
	if len(overrides) != 0 {
		t.Fatalf("expected empty store, got %d overrides", len(overrides))
	}

	// Set an override
	if err := store.Set("docker:27-dind", "delphos/docker-dind:27"); err != nil {
		t.Fatal(err)
	}

	// Lookup
	target, ok := store.Lookup("docker:27-dind")
	if !ok || target != "delphos/docker-dind:27" {
		t.Fatalf("Lookup(docker:27-dind) = %q, %v; want delphos/docker-dind:27, true", target, ok)
	}

	// Lookup with library/ prefix normalization
	target, ok = store.Lookup("library/docker:27-dind")
	if !ok || target != "delphos/docker-dind:27" {
		t.Fatalf("Lookup(library/docker:27-dind) = %q, %v; want delphos/docker-dind:27, true", target, ok)
	}

	// Lookup miss
	_, ok = store.Lookup("nonexistent:latest")
	if ok {
		t.Fatal("expected Lookup miss for nonexistent:latest")
	}

	// Update
	if err := store.Set("docker:27-dind", "delphos/docker-dind:28"); err != nil {
		t.Fatal(err)
	}
	target, _ = store.Lookup("docker:27-dind")
	if target != "delphos/docker-dind:28" {
		t.Fatalf("after update, target = %q, want delphos/docker-dind:28", target)
	}

	// List
	overrides = store.List()
	if len(overrides) != 1 {
		t.Fatalf("expected 1 override, got %d", len(overrides))
	}

	// Delete
	deleted, err := store.Delete("docker:27-dind")
	if err != nil || !deleted {
		t.Fatalf("Delete failed: deleted=%v, err=%v", deleted, err)
	}

	// Delete nonexistent
	deleted, err = store.Delete("docker:27-dind")
	if err != nil || deleted {
		t.Fatalf("Delete of nonexistent should return false: deleted=%v, err=%v", deleted, err)
	}
}

func TestOverrideStore_Persistence(t *testing.T) {
	dir := t.TempDir()

	// Create store and add overrides
	store := NewOverrideStore(dir)
	if err := store.Set("docker:27-dind", "delphos/docker-dind:27"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("node:22-slim", "delphos/node:22-slim"); err != nil {
		t.Fatal(err)
	}

	// Verify file exists
	filePath := filepath.Join(dir, "docker-overrides.json")
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("overrides file should exist: %v", err)
	}

	// Create new store from same directory — should load persisted overrides
	store2 := NewOverrideStore(dir)
	overrides := store2.List()
	if len(overrides) != 2 {
		t.Fatalf("expected 2 overrides after reload, got %d", len(overrides))
	}

	target, ok := store2.Lookup("docker:27-dind")
	if !ok || target != "delphos/docker-dind:27" {
		t.Fatalf("after reload, Lookup(docker:27-dind) = %q, %v", target, ok)
	}
}

func TestApplyOverride(t *testing.T) {
	dir := t.TempDir()
	store := NewOverrideStore(dir)
	if err := store.Set("docker:27-dind", "delphos/docker-dind:27"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("node:22-slim", "delphos/node:22-slim"); err != nil {
		t.Fatal(err)
	}

	// Docker Hub proxy (registryName = "docker.io")
	proxy := &DockerRegistryProxy{
		overrideStore: store,
		registryName:  "docker.io",
	}

	tests := []struct {
		name        string
		path        string
		wantPath    string
		wantApplied bool
	}{
		{
			name:        "manifest override",
			path:        "/v2/library/docker/manifests/27-dind",
			wantPath:    "/v2/delphos/docker-dind/manifests/27",
			wantApplied: true,
		},
		{
			name:        "no override",
			path:        "/v2/library/nginx/manifests/latest",
			wantPath:    "/v2/library/nginx/manifests/latest",
			wantApplied: false,
		},
		{
			name:        "node override",
			path:        "/v2/library/node/manifests/22-slim",
			wantPath:    "/v2/delphos/node/manifests/22-slim",
			wantApplied: true,
		},
		{
			name:        "non-manifest path unchanged",
			path:        "/v2/",
			wantPath:    "/v2/",
			wantApplied: false,
		},
		{
			name:        "digest manifest not overridden",
			path:        "/v2/library/docker/manifests/sha256:abc123",
			wantPath:    "/v2/library/docker/manifests/sha256:abc123",
			wantApplied: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPath, gotApplied := proxy.applyOverride(tt.path)
			if gotPath != tt.wantPath || gotApplied != tt.wantApplied {
				t.Errorf("applyOverride(%q) = (%q, %v), want (%q, %v)",
					tt.path, gotPath, gotApplied, tt.wantPath, tt.wantApplied)
			}
		})
	}
}

func TestApplyOverride_CrossRegistry(t *testing.T) {
	dir := t.TempDir()
	store := NewOverrideStore(dir)
	// Override stored with registry prefix (as the CLI would set it)
	if err := store.Set("ghcr.io/astral-sh/uv:0.9.6", "delphos/uv:0.9.6"); err != nil {
		t.Fatal(err)
	}
	// Docker Hub override (no prefix)
	if err := store.Set("node:22-slim", "delphos/node:22-slim"); err != nil {
		t.Fatal(err)
	}

	// ghcr.io proxy should match ghcr.io-prefixed overrides
	ghcrProxy := &DockerRegistryProxy{
		overrideStore: store,
		registryName:  "ghcr.io",
	}

	// Docker Hub proxy should NOT match ghcr.io overrides
	dockerProxy := &DockerRegistryProxy{
		overrideStore: store,
		registryName:  "docker.io",
	}

	tests := []struct {
		name        string
		proxy       *DockerRegistryProxy
		path        string
		wantPath    string
		wantApplied bool
	}{
		{
			name:        "ghcr.io proxy matches ghcr.io override",
			proxy:       ghcrProxy,
			path:        "/v2/astral-sh/uv/manifests/0.9.6",
			wantPath:    "/v2/delphos/uv/manifests/0.9.6",
			wantApplied: true,
		},
		{
			name:        "docker.io proxy does not match ghcr.io override",
			proxy:       dockerProxy,
			path:        "/v2/astral-sh/uv/manifests/0.9.6",
			wantPath:    "/v2/astral-sh/uv/manifests/0.9.6",
			wantApplied: false,
		},
		{
			name:        "docker.io proxy matches docker hub override",
			proxy:       dockerProxy,
			path:        "/v2/library/node/manifests/22-slim",
			wantPath:    "/v2/delphos/node/manifests/22-slim",
			wantApplied: true,
		},
		{
			name:        "ghcr.io proxy does not match docker hub override",
			proxy:       ghcrProxy,
			path:        "/v2/library/node/manifests/22-slim",
			wantPath:    "/v2/library/node/manifests/22-slim",
			wantApplied: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPath, gotApplied := tt.proxy.applyOverride(tt.path)
			if gotPath != tt.wantPath || gotApplied != tt.wantApplied {
				t.Errorf("applyOverride(%q) = (%q, %v), want (%q, %v)",
					tt.path, gotPath, gotApplied, tt.wantPath, tt.wantApplied)
			}
		})
	}
}
