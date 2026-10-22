package cache

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPruneOrphanTemps(t *testing.T) {
	cacheDir := t.TempDir()

	// Seed the kinds of files we expect to find in a real cache:
	//   * temp files (should be pruned)
	//   * real cache files (must survive)
	//   * UI template (.tmpl, NOT .tmp — must survive)
	seed := func(rel string, body string) string {
		t.Helper()
		p := filepath.Join(cacheDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		return p
	}

	temps := []string{
		"docker-blobs/sha256/aa/bb/.tmp-1234567890",
		"docker-blobs/sha256/ab/cd/.tmp-deadbeef00",
		"tmp/stream-1234567890.tmp",
		"tmp/put-9999999999.tmp",
		"content/aa/bb/.metadata-1234567890.tmp",
		".tmp-rootlevel123",
	}
	tempPaths := make([]string, len(temps))
	totalBytes := int64(0)
	for i, rel := range temps {
		tempPaths[i] = seed(rel, "partial-download-bytes")
		totalBytes += int64(len("partial-download-bytes"))
	}

	keep := []string{
		"docker-blobs/sha256/aa/bb/sha256:realblob000",
		"docker-manifests/docker.io/library_nginx/latest",
		"static/templates/foo.tmpl", // .tmpl not .tmp — must NOT be deleted
		"refcount/aa/bb/realhash",
		"content/aa/bb/cached.bin",
	}
	keepPaths := make([]string, len(keep))
	for i, rel := range keep {
		keepPaths[i] = seed(rel, "keep-me")
	}

	rep, err := PruneOrphanTemps(cacheDir)
	if err != nil {
		t.Fatalf("PruneOrphanTemps: %v", err)
	}
	if rep.Filesystem != len(temps) {
		t.Fatalf("pruned %d files, want %d", rep.Filesystem, len(temps))
	}
	if rep.BytesFreed != totalBytes {
		t.Fatalf("freed %d bytes, want %d", rep.BytesFreed, totalBytes)
	}
	if rep.Errors != 0 {
		t.Fatalf("got %d errors, want 0", rep.Errors)
	}

	for _, p := range tempPaths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("temp file should be gone: %s", p)
		}
	}
	for _, p := range keepPaths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("non-temp file should survive: %s (%v)", p, err)
		}
	}
}

func TestPruneOrphanTemps_NonexistentDir(t *testing.T) {
	// Walking a missing dir should be a no-op, not an error.
	rep, err := PruneOrphanTemps(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("PruneOrphanTemps on missing dir: %v", err)
	}
	if rep.Filesystem != 0 || rep.BytesFreed != 0 {
		t.Fatalf("report should be zero, got %+v", rep)
	}
}

func TestPruneOrphanTemps_EmptyCacheDir(t *testing.T) {
	_, err := PruneOrphanTemps("")
	if err == nil {
		t.Fatal("PruneOrphanTemps with empty cacheDir should error")
	}
}

func TestIsTempName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Match: .tmp- prefix
		{".tmp-1234567890", true},
		{".tmp-abc", true},
		// Match: .tmp extension
		{"stream-1234567890.tmp", true},
		{"put-x.tmp", true},
		{".metadata-1234567890.tmp", true},
		// Must NOT match
		{"sha256:abcdef", false},
		{"foo.tmpl", false}, // template suffix, not temp
		{"latest", false},   // tag
		{"manifest.json", false},
		{"tmp", false},
		{".tmp", true}, // exact ".tmp" — extension match
		{"x.tmp.bak", false},
	}
	for _, c := range cases {
		if got := isTempName(c.name); got != c.want {
			t.Errorf("isTempName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
