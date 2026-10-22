package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/config"
)

// newTestCacheProxy creates a Proxy with a temp cache dir for testing.
func newTestCacheProxy(t *testing.T) *Proxy {
	t.Helper()
	cacheDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cacheDir, "tmp"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return &Proxy{
		cacheDir:        cacheDir,
		knownRegistries: config.RegistryMap{},
	}
}

func TestPutAndGetLocalFile(t *testing.T) {
	p := newTestCacheProxy(t)
	body := []byte("hello world — this is a test file")

	// PUT the file
	putReq := httptest.NewRequest("PUT", "http://localhost/seeds/test.txt", bytes.NewReader(body))
	putReq.Header.Set("Content-Type", "text/plain")
	putW := httptest.NewRecorder()

	regConfig := config.RegistryConfig{CacheType: "local"}
	p.handleLocalCacheRequest(putReq, "localhost", regConfig, "http", putW)

	if putW.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d: %s", putW.Code, putW.Body.String())
	}

	// Verify response contains sha256
	var putResp map[string]interface{}
	if err := json.Unmarshal(putW.Body.Bytes(), &putResp); err != nil {
		t.Fatalf("PUT response not JSON: %v", err)
	}
	if _, ok := putResp["sha256"]; !ok {
		t.Fatal("PUT response missing sha256 field")
	}
	if int(putResp["size"].(float64)) != len(body) {
		t.Fatalf("PUT response size=%v, want %d", putResp["size"], len(body))
	}

	// GET the file back
	getReq := httptest.NewRequest("GET", "http://localhost/seeds/test.txt", nil)
	getW := httptest.NewRecorder()

	p.handleLocalCacheRequest(getReq, "localhost", regConfig, "http", getW)

	if getW.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", getW.Code, getW.Body.String())
	}
	if !bytes.Equal(getW.Body.Bytes(), body) {
		t.Fatalf("GET body mismatch: got %q, want %q", getW.Body.String(), string(body))
	}
	if ct := getW.Header().Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("GET Content-Type=%q, want text/plain", ct)
	}
}

func TestPutLocalFileContentAddressed(t *testing.T) {
	p := newTestCacheProxy(t)
	body := []byte("content-addressed storage test")

	// Compute expected hash
	h := sha256.Sum256(body)
	expectedHash := hex.EncodeToString(h[:])

	// PUT the file
	putReq := httptest.NewRequest("PUT", "http://localhost/seeds/ca-test.bin", bytes.NewReader(body))
	putW := httptest.NewRecorder()

	regConfig := config.RegistryConfig{CacheType: "local"}
	p.handleLocalCacheRequest(putReq, "localhost", regConfig, "http", putW)

	if putW.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d", putW.Code)
	}

	var putResp map[string]interface{}
	if err := json.Unmarshal(putW.Body.Bytes(), &putResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if putResp["sha256"] != expectedHash {
		t.Fatalf("hash mismatch: got %v, want %s", putResp["sha256"], expectedHash)
	}

	// Verify file exists in content-addressed storage
	contentPath := cache.GetContentPath(p.cacheDir, expectedHash)
	stored, err := os.ReadFile(contentPath)
	if err != nil {
		t.Fatalf("content file not found at %s: %v", contentPath, err)
	}
	if !bytes.Equal(stored, body) {
		t.Fatal("stored content doesn't match uploaded body")
	}
}

func TestPutDuplicateContentDeduplicates(t *testing.T) {
	p := newTestCacheProxy(t)
	body := []byte("duplicate content")

	regConfig := config.RegistryConfig{CacheType: "local"}

	// PUT same content under two different paths
	for _, path := range []string{"/seeds/file1.bin", "/seeds/file2.bin"} {
		req := httptest.NewRequest("PUT", "http://localhost"+path, bytes.NewReader(body))
		w := httptest.NewRecorder()
		p.handleLocalCacheRequest(req, "localhost", regConfig, "http", w)
		if w.Code != http.StatusCreated {
			t.Fatalf("PUT %s: expected 201, got %d", path, w.Code)
		}
	}

	// Both paths should return the same content
	for _, path := range []string{"/seeds/file1.bin", "/seeds/file2.bin"} {
		req := httptest.NewRequest("GET", "http://localhost"+path, nil)
		w := httptest.NewRecorder()
		p.handleLocalCacheRequest(req, "localhost", regConfig, "http", w)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d", path, w.Code)
		}
		if !bytes.Equal(w.Body.Bytes(), body) {
			t.Fatalf("GET %s: body mismatch", path)
		}
	}

	// Only one content file should exist (deduplication)
	h := sha256.Sum256(body)
	contentPath := cache.GetContentPath(p.cacheDir, hex.EncodeToString(h[:]))
	if _, err := os.Stat(contentPath); err != nil {
		t.Fatal("content file missing")
	}
}

func TestGetMissingLocalFileReturns404(t *testing.T) {
	p := newTestCacheProxy(t)

	req := httptest.NewRequest("GET", "http://localhost/seeds/nonexistent.bin", nil)
	w := httptest.NewRecorder()

	regConfig := config.RegistryConfig{CacheType: "local"}
	p.handleLocalCacheRequest(req, "localhost", regConfig, "http", w)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteLocalFile(t *testing.T) {
	p := newTestCacheProxy(t)
	body := []byte("to be deleted")

	regConfig := config.RegistryConfig{CacheType: "local"}

	// PUT a file
	putReq := httptest.NewRequest("PUT", "http://localhost/seeds/delete-me.bin", bytes.NewReader(body))
	putW := httptest.NewRecorder()
	p.handleLocalCacheRequest(putReq, "localhost", regConfig, "http", putW)
	if putW.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d", putW.Code)
	}

	// DELETE it
	delReq := httptest.NewRequest("DELETE", "http://localhost/seeds/delete-me.bin", nil)
	delW := httptest.NewRecorder()
	p.handleLocalCacheRequest(delReq, "localhost", regConfig, "http", delW)
	if delW.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", delW.Code)
	}

	// GET should now 404
	getReq := httptest.NewRequest("GET", "http://localhost/seeds/delete-me.bin", nil)
	getW := httptest.NewRecorder()
	p.handleLocalCacheRequest(getReq, "localhost", regConfig, "http", getW)
	if getW.Code != http.StatusNotFound {
		t.Fatalf("GET after DELETE: expected 404, got %d", getW.Code)
	}
}

func TestPutLargeFile(t *testing.T) {
	p := newTestCacheProxy(t)

	// 5MB file
	body := make([]byte, 5*1024*1024)
	for i := range body {
		body[i] = byte(i % 256)
	}

	putReq := httptest.NewRequest("PUT", "http://localhost/seeds/large.dump", bytes.NewReader(body))
	putReq.Header.Set("Content-Type", "application/octet-stream")
	putW := httptest.NewRecorder()

	regConfig := config.RegistryConfig{CacheType: "local"}
	p.handleLocalCacheRequest(putReq, "localhost", regConfig, "http", putW)

	if putW.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d: %s", putW.Code, putW.Body.String())
	}

	// GET it back and verify
	getReq := httptest.NewRequest("GET", "http://localhost/seeds/large.dump", nil)
	getW := httptest.NewRecorder()
	p.handleLocalCacheRequest(getReq, "localhost", regConfig, "http", getW)

	if getW.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d", getW.Code)
	}
	if getW.Body.Len() != len(body) {
		t.Fatalf("GET size=%d, want %d", getW.Body.Len(), len(body))
	}
	if !bytes.Equal(getW.Body.Bytes(), body) {
		t.Fatal("GET body mismatch for large file")
	}
}

func TestPutNoBodyReturnsBadRequest(t *testing.T) {
	p := newTestCacheProxy(t)

	req := httptest.NewRequest("PUT", "http://localhost/seeds/empty", nil)
	req.Body = nil
	w := httptest.NewRecorder()

	regConfig := config.RegistryConfig{CacheType: "local"}
	p.handleLocalCacheRequest(req, "localhost", regConfig, "http", w)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPutDefaultContentType(t *testing.T) {
	p := newTestCacheProxy(t)
	body := []byte("no content-type header")

	putReq := httptest.NewRequest("PUT", "http://localhost/seeds/no-ct.bin", bytes.NewReader(body))
	// Don't set Content-Type
	putW := httptest.NewRecorder()

	regConfig := config.RegistryConfig{CacheType: "local"}
	p.handleLocalCacheRequest(putReq, "localhost", regConfig, "http", putW)

	if putW.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d", putW.Code)
	}

	// GET should return application/octet-stream
	getReq := httptest.NewRequest("GET", "http://localhost/seeds/no-ct.bin", nil)
	getW := httptest.NewRecorder()
	p.handleLocalCacheRequest(getReq, "localhost", regConfig, "http", getW)

	if ct := getW.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type=%q, want application/octet-stream", ct)
	}
}
