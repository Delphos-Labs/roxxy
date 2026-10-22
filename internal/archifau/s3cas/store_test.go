package s3cas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestCASKey(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	k, err := casKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	if want := "cas/sha256/ab/ab/" + digest; k != want {
		t.Errorf("casKey = %q, want %q", k, want)
	}

	for _, bad := range []string{"", "short", strings.ToUpper(digest), strings.Repeat("ab", 32) + "ff"} {
		if _, err := casKey(bad); err == nil {
			t.Errorf("casKey(%q) accepted malformed digest", bad)
		}
	}
}

func TestMetaKeys(t *testing.T) {
	if got := PyPIMetaKey("widget-tools", "widget_tools-1.0.whl"); got != "pypi-meta/widget-tools/widget_tools-1.0.whl.json" {
		t.Errorf("PyPIMetaKey = %q", got)
	}
	if got := FilesMetaKey("delphos-labs/widget/dumps/x.parquet"); got != "files-meta/delphos-labs/widget/dumps/x.parquet.json" {
		t.Errorf("FilesMetaKey = %q", got)
	}
}

func TestKeyPrefixJoin(t *testing.T) {
	for prefix, want := range map[string]string{
		"":     "cas/sha256/aa/bb/x",
		"team": "team/cas/sha256/aa/bb/x",
	} {
		c := &Client{prefix: prefix}
		if got := c.key("cas/sha256/aa/bb/x"); got != want {
			t.Errorf("prefix %q: key = %q, want %q", prefix, got, want)
		}
	}
}

func TestNewDisabledWhenBucketUnset(t *testing.T) {
	t.Setenv("ARCHIFAU_S3_BUCKET", "")
	c, err := New(context.Background())
	if err != nil || c != nil {
		t.Errorf("New = (%v, %v), want (nil, nil)", c, err)
	}
}

// --- real-SDK wiring against a minimal in-memory S3 stub --------------------

type stubObject struct {
	body []byte
	meta map[string]string // lower-cased x-amz-meta-* keys
}

// s3Stub speaks just enough path-style S3 for the SDK round-trip:
// PUT/GET/HEAD/DELETE of /<bucket>/<key>.
type s3Stub struct {
	mu      sync.Mutex
	objects map[string]stubObject
}

func (s *s3Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		meta := map[string]string{}
		for h, v := range r.Header {
			lh := strings.ToLower(h)
			if rest, ok := strings.CutPrefix(lh, "x-amz-meta-"); ok {
				meta[rest] = v[0]
			}
		}
		s.objects[key] = stubObject{body: body, meta: meta}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		o, ok := s.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Key>%s</Key></Error>`, key)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(o.body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(o.body)
	case http.MethodHead:
		o, ok := s.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		for k, v := range o.meta {
			w.Header().Set("x-amz-meta-"+k, v)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(o.body)))
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func newStubClient(t *testing.T) (*Client, *s3Stub) {
	t.Helper()
	stub := &s3Stub{objects: map[string]stubObject{}}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)

	t.Setenv("ARCHIFAU_S3_BUCKET", "test-bucket")
	t.Setenv("ARCHIFAU_S3_PREFIX", "pfx")
	t.Setenv("ARCHIFAU_S3_ENDPOINT", srv.URL)
	t.Setenv("ARCHIFAU_S3_FORCE_PATH_STYLE", "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	c, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("New returned nil client with bucket set")
	}
	return c, stub
}

func TestClientRoundTrip(t *testing.T) {
	c, stub := newStubClient(t)
	ctx := context.Background()
	content := []byte("cas bytes")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	// CAS put + get + dedup.
	if err := c.PutCAS(ctx, digest, int64(len(content)), bytes.NewReader(content)); err != nil {
		t.Fatalf("PutCAS: %v", err)
	}
	wantKey := "test-bucket/pfx/cas/sha256/" + digest[:2] + "/" + digest[2:4] + "/" + digest
	if _, ok := stub.objects[wantKey]; !ok {
		t.Fatalf("CAS object not at %q; have %v", wantKey, keys(stub))
	}
	rc, err := c.GetCAS(ctx, digest)
	if err != nil {
		t.Fatalf("GetCAS: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, content) {
		t.Errorf("GetCAS roundtrip = %q", got)
	}
	if _, err := c.GetCAS(ctx, strings.Repeat("0", 64)); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetCAS miss = %v, want ErrNotFound", err)
	}

	// Meta put + dedupe-skip + get + delete.
	metaKey := PyPIMetaKey("widget", "widget-1.0.whl")
	if err := c.PutMeta(ctx, metaKey, []byte(`{"sha256":"`+digest+`"}`), digest); err != nil {
		t.Fatalf("PutMeta: %v", err)
	}
	if err := c.PutMeta(ctx, metaKey, []byte(`{"sha256":"`+digest+`"}`), digest); err != nil {
		t.Fatalf("PutMeta repeat: %v", err)
	}
	data, err := c.GetMeta(ctx, metaKey)
	if err != nil || !strings.Contains(string(data), digest) {
		t.Fatalf("GetMeta = %q, %v", data, err)
	}
	if err := c.DeleteMeta(ctx, metaKey); err != nil {
		t.Fatalf("DeleteMeta: %v", err)
	}
	if _, err := c.GetMeta(ctx, metaKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetMeta after delete = %v, want ErrNotFound", err)
	}
}

func keys(s *s3Stub) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		out = append(out, k)
	}
	return out
}
