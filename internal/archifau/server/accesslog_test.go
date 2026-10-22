package server

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a goroutine-safe buffer: the access line is emitted after the
// response is written, so the test may read while the server goroutine is
// still logging.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs swaps the default slog logger for a debug-level JSON handler
// writing to the returned buffer for the duration of the test. Tests using it
// must not run in parallel (slog.SetDefault is process-global).
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// records decodes every JSON line whose msg matches.
func records(buf *syncBuffer, msg string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// waitAccess polls for access records matching path until timeout: the line
// is written just after the response, so an immediate read can race it.
func waitAccess(t *testing.T, buf *syncBuffer, path string, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var got []map[string]any
		for _, rec := range records(buf, "access") {
			if rec["path"] == path {
				got = append(got, rec)
			}
		}
		if len(got) >= want {
			if len(got) > want {
				t.Fatalf("access lines for %s = %d, want %d", path, len(got), want)
			}
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d access line(s) for %s; have %d", want, path, len(got))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAccessLogAuthenticatedRequest(t *testing.T) {
	buf := captureLogs(t)
	e := newTestEnv(t)
	token := e.token(t, ciMain())

	resp := e.do(t, http.MethodGet, "/v2/", token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	rec := waitAccess(t, buf, "/v2/", 1)[0]
	for k, want := range map[string]any{
		"method": "GET", "status": float64(200), "proto": "HTTP/1.1",
		"event": "authenticated", "outcome": "allowed",
		"id_kind": "oidc", "id_subject": "delphos-labs/widget",
		"id_actor": "faye", "id_ref": "refs/heads/main",
	} {
		if rec[k] != want {
			t.Errorf("%s = %v, want %v", k, rec[k], want)
		}
	}
	for _, k := range []string{"remote_addr", "duration_ms", "bytes", "user_agent"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("missing field %s", k)
		}
	}
}

func TestAccessLogDenied(t *testing.T) {
	buf := captureLogs(t)
	e := newTestEnv(t)
	token := e.token(t, ciForeign())

	path := "/v2/delphos-labs/widget/app/manifests/v1"
	resp := e.do(t, http.MethodPut, path, token, []byte(`{}`), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	rec := waitAccess(t, buf, path, 1)[0]
	for k, want := range map[string]any{
		"status": float64(403), "level": "WARN",
		"event": "denied", "outcome": "denied", "reason": "not authorized",
		"object": "delphos-labs/widget/app", "id_subject": "delphos-labs/other",
	} {
		if rec[k] != want {
			t.Errorf("%s = %v, want %v", k, rec[k], want)
		}
	}
}

func TestAccessLogAnonymous(t *testing.T) {
	buf := captureLogs(t)
	e := newTestEnv(t)

	path := "/files/delphos-labs/widget/x"
	resp := e.do(t, http.MethodGet, path, "", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	rec := waitAccess(t, buf, path, 1)[0]
	if rec["event"] != "anonymous" || rec["outcome"] != "unauthenticated" {
		t.Errorf("event/outcome = %v/%v", rec["event"], rec["outcome"])
	}
	if _, ok := rec["id_subject"]; ok {
		t.Errorf("unexpected identity on anonymous request: %v", rec["id_subject"])
	}
}

func TestAccessLogUploadDownload(t *testing.T) {
	buf := captureLogs(t)
	e := newTestEnv(t)
	token := e.token(t, ciMain())
	content := []byte("audit-me")
	sum := hex.EncodeToString(sha256Bytes(content))
	path := "/files/delphos-labs/widget/dumps/audit.bin"

	if resp := e.do(t, http.MethodPut, path, token, content, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	up := waitAccess(t, buf, path, 1)[0]
	for k, want := range map[string]any{
		"event": "upload", "object": "delphos-labs/widget/dumps/audit.bin",
		"sha256": sum, "object_size": float64(len(content)),
		"id_subject": "delphos-labs/widget",
	} {
		if up[k] != want {
			t.Errorf("upload %s = %v, want %v", k, up[k], want)
		}
	}

	// Download: ServeFile goes through the ReaderFrom path of the wrapper
	// (httptest.NewServer is a real net/http server), so bytes must match.
	if resp := e.do(t, http.MethodGet, path, token, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("get: %d", resp.StatusCode)
	}
	dl := waitAccess(t, buf, path, 2)[1]
	for k, want := range map[string]any{
		"event": "download", "bytes": float64(len(content)),
		"sha256": sum, "status": float64(200),
	} {
		if dl[k] != want {
			t.Errorf("download %s = %v, want %v", k, dl[k], want)
		}
	}
}

func TestAccessLogPyPIUpload(t *testing.T) {
	buf := captureLogs(t)
	e := newTestEnv(t)
	token := e.token(t, ciMain())
	wheel := []byte("PK-wheel")
	sum := hex.EncodeToString(sha256Bytes(wheel))

	resp := twineUpload(t, e.srv.URL, token, "Audit.Widget", "1.0", "audit_widget-1.0-py3-none-any.whl", wheel)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d", resp.StatusCode)
	}

	rec := waitAccess(t, buf, "/pypi/legacy/", 1)[0]
	for k, want := range map[string]any{
		"event": "upload", "object": "audit-widget/audit_widget-1.0-py3-none-any.whl",
		"sha256": sum, "object_size": float64(len(wheel)),
	} {
		if rec[k] != want {
			t.Errorf("%s = %v, want %v", k, rec[k], want)
		}
	}
}

func TestAccessLogHealthzDebug(t *testing.T) {
	buf := captureLogs(t)
	e := newTestEnv(t)

	if resp := e.do(t, http.MethodGet, "/healthz", "", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
	rec := waitAccess(t, buf, "/healthz", 1)[0]
	if rec["level"] != "DEBUG" {
		t.Errorf("healthz access level = %v, want DEBUG", rec["level"])
	}
}
