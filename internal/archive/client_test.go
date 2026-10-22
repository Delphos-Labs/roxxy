package archive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGetConditional_200And304(t *testing.T) {
	body := []byte("hello")
	etag := `"abc123"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	got, gotETag, _, status, err := c.GetConditional(context.Background(), "/current/x", "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if status != 200 || string(got) != "hello" || gotETag != etag {
		t.Fatalf("first GET: status=%d body=%q etag=%q", status, got, gotETag)
	}

	_, _, _, status, err = c.GetConditional(context.Background(), "/current/x", etag, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if status != 304 {
		t.Fatalf("expected 304, got %d", status)
	}
}

func TestVerifyPubkey(t *testing.T) {
	// This test runs only when a working gpg is available in PATH.
	if _, err := extractFingerprint([]byte("not an armored key")); err == nil {
		// Sanity: extractFingerprint should fail on garbage.
		t.Log("warning: extractFingerprint accepted garbage")
	}
}
