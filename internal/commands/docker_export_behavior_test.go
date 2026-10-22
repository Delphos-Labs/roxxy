package commands

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExportOCIRoundTrip exports a synthetic image to an OCI tarball, then
// re-opens it with archive/tar and verifies the OCI layout invariants:
// oci-layout present, index.json's manifest descriptor digest == sha256(manifest
// bytes), and every referenced blob (config + each layer) is present at
// blobs/sha256/<hex>. The happy path of ExportDockerImageToOCITarball does not
// call os.Exit, so this drives the real exporter end to end.
func TestExportOCIRoundTrip(t *testing.T) {
	img := syntheticImage{
		registryID: "docker.io",
		repo:       "library/alpha",
		tag:        "1.0",
		config:     makeBlob("config-for-alpha"),
		layers: []syntheticBlob{
			makeBlob("layer-one-bytes"),
			makeBlob("layer-two-bytes"),
		},
	}
	srcTarball := writeSyntheticTarball(t, []syntheticImage{img}, nil)
	img.buildManifest() // recompute so the test knows the exact manifest bytes
	wantManifestDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(img.manifest))

	outPath := filepath.Join(t.TempDir(), "alpha-oci.tar")
	ExportDockerImageToOCITarball(Config{}, "library/alpha:1.0", outPath, srcTarball, "")

	entries := readTarEntries(t, outPath)

	if _, ok := entries["oci-layout"]; !ok {
		t.Error("oci-layout missing from exported tarball")
	}

	indexRaw, ok := entries["index.json"]
	if !ok {
		t.Fatal("index.json missing from exported tarball")
	}
	var index struct {
		SchemaVersion int `json:"schemaVersion"`
		Manifests     []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int    `json:"size"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(indexRaw, &index); err != nil {
		t.Fatalf("index.json does not parse: %v", err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("expected 1 manifest descriptor, got %d", len(index.Manifests))
	}
	if index.Manifests[0].Digest != wantManifestDigest {
		t.Errorf("index manifest digest = %s, want sha256(manifest bytes) = %s",
			index.Manifests[0].Digest, wantManifestDigest)
	}

	// The manifest itself is stored as a blob; its bytes must hash to the
	// descriptor digest.
	manifestBlobName := "blobs/sha256/" + strings.TrimPrefix(wantManifestDigest, "sha256:")
	manifestBlob, ok := entries[manifestBlobName]
	if !ok {
		t.Fatalf("manifest blob %s missing", manifestBlobName)
	}
	if got := fmt.Sprintf("sha256:%x", sha256.Sum256(manifestBlob)); got != wantManifestDigest {
		t.Errorf("stored manifest blob hashes to %s, want %s", got, wantManifestDigest)
	}

	// Config + each layer must be present as blobs/sha256/<hex>.
	for _, b := range append([]syntheticBlob{img.config}, img.layers...) {
		name := "blobs/sha256/" + strings.TrimPrefix(b.digest, "sha256:")
		got, ok := entries[name]
		if !ok {
			t.Errorf("blob %s missing from exported tarball", name)
			continue
		}
		if got := fmt.Sprintf("sha256:%x", sha256.Sum256(got)); got != b.digest {
			t.Errorf("blob %s content hashes to %s", name, got)
		}
	}

	// Layer order is preserved: the manifest's layers[] must list them in the
	// same order the source manifest declared.
	var parsed struct {
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifestBlob, &parsed); err != nil {
		t.Fatalf("stored manifest does not parse: %v", err)
	}
	if len(parsed.Layers) != len(img.layers) {
		t.Fatalf("manifest has %d layers, want %d", len(parsed.Layers), len(img.layers))
	}
	for i, l := range img.layers {
		if parsed.Layers[i].Digest != l.digest {
			t.Errorf("layer %d digest = %s, want %s (order not preserved)", i, parsed.Layers[i].Digest, l.digest)
		}
	}
}

// readTarEntries reads an uncompressed tar into a name->bytes map. Sufficient
// for the small OCI tarballs these tests produce.
func readTarEntries(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open exported tarball: %v", err)
	}
	defer func() { _ = f.Close() }()

	entries := make(map[string][]byte)
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar entry: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar body for %s: %v", hdr.Name, err)
		}
		entries[hdr.Name] = data
	}
	return entries
}

// TestExportSinkPartialCleanup covers exportSink's two integrity guards (commit
// 911aa3f): removePartial deletes a half-written output, and finishErr surfaces a
// Close/Sync failure instead of letting a truncated tarball pass as "success".
func TestExportSinkPartialCleanup(t *testing.T) {
	t.Run("removePartial deletes a file-backed partial", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "out.tar")
		s := newExportSink(path, false)
		s.removePartial()
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("removePartial left %s behind (stat err=%v)", path, err)
		}
	})

	t.Run("stdout sink removePartial is a no-op", func(t *testing.T) {
		s := &exportSink{toStdout: true, file: os.Stdout} // path == "" — must not touch anything
		s.removePartial()
	})

	t.Run("finishErr surfaces a Close/Sync failure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "out.tar")
		s := newExportSink(path, false)
		// Close the underlying file out from under the sink: the trailer flush /
		// Sync / Close now fail — a truncated-tarball condition that must be
		// reported as an error, never swallowed into a success print.
		_ = s.file.Close()
		if err := s.finishErr(); err == nil {
			t.Error("finishErr returned nil on a closed file; truncated tarball would pass as success")
		}
	})
}
