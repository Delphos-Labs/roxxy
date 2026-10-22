package archive

import (
	"testing"
	"time"
)

func TestMirrorWriteThenRead(t *testing.T) {
	m, err := OpenMirror(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Write("pypi/pypi.org/simple/foo/index.html", []byte("v1"), `"abc"`, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := m.Read("pypi/pypi.org/simple/foo/index.html")
	if err != nil || string(got) != "v1" {
		t.Fatalf("got %q err %v", got, err)
	}
	if m.LookupEntry("pypi/pypi.org/simple/foo/index.html").ETag != `"abc"` {
		t.Fatal("ETag not recorded")
	}
}

func TestMirrorPersistsAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	m, err := OpenMirror(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Write("x", []byte("v1"), `"abc"`, time.Now()); err != nil {
		t.Fatal(err)
	}

	m2, err := OpenMirror(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m2.LookupEntry("x").ETag != `"abc"` {
		t.Fatal("state didn't survive reopen")
	}
}

func TestMirrorRejectsParentEscape(t *testing.T) {
	m, err := OpenMirror(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Write("../escape", []byte("x"), "", time.Time{}); err == nil {
		t.Fatal("expected error")
	}
}
