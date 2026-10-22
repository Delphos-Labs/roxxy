package commands

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

func TestDetectManifestMediaType(t *testing.T) {
	tests := []struct {
		name         string
		manifest     string
		coerceDocker bool
		want         string
		wantErr      bool
	}{
		{
			name:     "explicit docker mediaType is preserved",
			manifest: `{"mediaType":"` + dockerManifestV2MediaType + `","config":{"mediaType":"` + dockerContainerConfigMediaType + `"}}`,
			want:     dockerManifestV2MediaType,
		},
		{
			name:     "explicit oci mediaType is preserved",
			manifest: `{"mediaType":"` + ociImageManifestMediaType + `","config":{"mediaType":"` + ociImageConfigMediaType + `"}}`,
			want:     ociImageManifestMediaType,
		},
		{
			name:     "helm config without top-level mediaType maps to oci manifest",
			manifest: `{"config":{"mediaType":"` + helmConfigMediaType + `"}}`,
			want:     ociImageManifestMediaType,
		},
		{
			name:     "oci config without top-level mediaType maps to oci manifest",
			manifest: `{"config":{"mediaType":"` + ociImageConfigMediaType + `"}}`,
			want:     ociImageManifestMediaType,
		},
		{
			name:     "docker config without top-level mediaType maps to docker manifest",
			manifest: `{"config":{"mediaType":"` + dockerContainerConfigMediaType + `"}}`,
			want:     dockerManifestV2MediaType,
		},
		{
			name:     "empty config falls back to docker manifest",
			manifest: `{"config":{}}`,
			want:     dockerManifestV2MediaType,
		},
		{
			name:     "unknown config type assumes oci manifest",
			manifest: `{"config":{"mediaType":"application/vnd.example.unknown.v1+json"}}`,
			want:     ociImageManifestMediaType,
		},
		{
			name:         "coerce docker on a docker image yields docker manifest",
			manifest:     `{"config":{"mediaType":"` + dockerContainerConfigMediaType + `"}}`,
			coerceDocker: true,
			want:         dockerManifestV2MediaType,
		},
		{
			name:         "coerce docker on a helm chart is refused",
			manifest:     `{"config":{"mediaType":"` + helmConfigMediaType + `"}}`,
			coerceDocker: true,
			wantErr:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := detectManifestMediaType([]byte(tt.manifest), tt.coerceDocker)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got mediaType %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseImageSpec(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		wantRepo string
		wantTag  string
		wantErr  bool
	}{
		{
			name:    "no colon is an error",
			spec:    "redis",
			wantErr: true,
		},
		{
			name:     "bare repo gets library prefix",
			spec:     "redis:7",
			wantRepo: "library/redis",
			wantTag:  "7",
		},
		{
			name:     "namespaced repo is left untouched",
			spec:     "argo/argo-cd:8.1.3",
			wantRepo: "argo/argo-cd",
			wantTag:  "8.1.3",
		},
		{
			name:     "splits on the first colon only",
			spec:     "host:5000/repo:tag",
			wantRepo: "library/host",
			wantTag:  "5000/repo:tag",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, tag, err := parseImageSpec(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got repo=%q tag=%q", repo, tag)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if repo != tt.wantRepo || tag != tt.wantTag {
				t.Errorf("got repo=%q tag=%q, want repo=%q tag=%q", repo, tag, tt.wantRepo, tt.wantTag)
			}
		})
	}
}

// TestVerifyingReader covers the push-path integrity primitive: Read hashes the
// streamed bytes and verify() must accept a matching digest and reject a mismatch
// — this is what stops corrupt/modified content landing under a promised digest.
func TestVerifyingReader(t *testing.T) {
	payload := []byte("the-blob-bytes")
	sum := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	t.Run("matching digest verifies", func(t *testing.T) {
		vr := &verifyingReader{r: bytes.NewReader(payload), h: sha256.New(), expected: digest}
		if _, err := io.Copy(io.Discard, vr); err != nil {
			t.Fatal(err)
		}
		if err := vr.verify(); err != nil {
			t.Errorf("verify() on matching content = %v, want nil", err)
		}
	})
	t.Run("mismatched digest is rejected", func(t *testing.T) {
		vr := &verifyingReader{r: bytes.NewReader(payload), h: sha256.New(), expected: "sha256:" + strings.Repeat("00", 32)}
		if _, err := io.Copy(io.Discard, vr); err != nil {
			t.Fatal(err)
		}
		if err := vr.verify(); err == nil {
			t.Error("verify() on mismatched content = nil, want a digest-mismatch error")
		}
	})
}
