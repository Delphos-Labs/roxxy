package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestPlainHTTPKeepAlive verifies that multiple HTTP requests can be sent
// on the same TCP connection (HTTP keep-alive) to the proxy server.
// This is critical for OCI registry push flows where helm sends:
// HEAD (exists check), POST (initiate upload), PATCH (upload chunk), PUT (finalize)
// all on the same connection.
func TestPlainHTTPKeepAlive(t *testing.T) {
	// Create a simple handler that counts requests
	requestCount := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("X-Request-Count", fmt.Sprintf("%d", requestCount))
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "request %d: %s %s", requestCount, r.Method, r.URL.Path)
	})

	// Start a TCP listener
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer func() { _ = listener.Close() }()

	addr := listener.Addr().String()

	// Accept connections and handle them the same way handleProxyConnection does
	// for plain HTTP - using http.Serve with singleConnListener
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				// Note: do NOT defer c.Close() here - http.Serve manages the connection
				// This is the fix pattern: use http.Serve instead of single-request handling
				if err := http.Serve(&singleConnListener{conn: c}, handler); err != nil {
					// http.Serve returns ErrServerClosed or similar on clean close
					return
				}
			}(conn)
		}
	}()

	// Open a single TCP connection
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Send multiple HTTP requests on the same connection (keep-alive)
	methods := []struct {
		method string
		path   string
	}{
		{"HEAD", "/v2/delphos-charts/application/manifests/sha256:abc123"},
		{"POST", "/v2/delphos-charts/application/blobs/uploads/"},
		{"PATCH", "/v2/delphos-charts/application/blobs/uploads/uuid-1234"},
		{"PUT", "/v2/delphos-charts/application/blobs/uploads/uuid-1234?digest=sha256:abc123"},
	}

	for i, m := range methods {
		// Write raw HTTP request
		reqStr := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: localhost\r\nConnection: keep-alive\r\n\r\n", m.method, m.path)
		if _, err := conn.Write([]byte(reqStr)); err != nil {
			t.Fatalf("Request %d (%s %s): failed to write: %v", i+1, m.method, m.path, err)
		}

		// Read response - set a deadline so we don't hang forever
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("Request %d (%s %s): failed to read response: %v", i+1, m.method, m.path, err)
		}

		resp := string(buf[:n])
		if !strings.Contains(resp, "200 OK") {
			t.Errorf("Request %d (%s %s): expected 200 OK, got: %s", i+1, m.method, m.path, resp)
		}

		expectedCount := fmt.Sprintf("X-Request-Count: %d", i+1)
		if !strings.Contains(resp, expectedCount) {
			t.Errorf("Request %d (%s %s): expected %s in response, got: %s", i+1, m.method, m.path, expectedCount, resp)
		}
	}

	if requestCount != len(methods) {
		t.Errorf("Expected %d requests handled, got %d", len(methods), requestCount)
	}
}

// TestPlainHTTPSingleRequestCloses is a regression test to demonstrate that
// without http.Serve (using manual ReadRequest + handle), only one request
// is served per connection.
func TestPlainHTTPSingleRequestCloses(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "ok")
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer func() { _ = listener.Close() }()

	addr := listener.Addr().String()

	// Simulate the BROKEN behavior: read one request, handle it, close
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				// Read only ONE request (broken behavior)
				req, err := http.ReadRequest(
					bufio.NewReader(c),
				)
				if err != nil {
					return
				}
				rw := &singleResponseWriter{conn: c}
				handler.ServeHTTP(rw, req)
				// Connection closes here - second request will fail
			}(conn)
		}
	}()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// First request should work
	req1 := "GET /first HTTP/1.1\r\nHost: localhost\r\nConnection: keep-alive\r\n\r\n"
	if _, err := conn.Write([]byte(req1)); err != nil {
		t.Fatalf("Failed to write first request: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	_, err = conn.Read(buf)
	if err != nil {
		t.Fatalf("First request failed: %v", err)
	}

	// Second request should fail (connection closed by server)
	req2 := "GET /second HTTP/1.1\r\nHost: localhost\r\nConnection: keep-alive\r\n\r\n"
	// Write might succeed (buffered) even if conn is closing — ignore result.
	_, _ = conn.Write([]byte(req2))

	_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	_, err = conn.Read(buf)
	if err == nil {
		t.Log("Second request unexpectedly succeeded (may get response from buffer)")
	} else {
		// Expected: EOF or connection reset
		if err != io.EOF && !strings.Contains(err.Error(), "reset") && !strings.Contains(err.Error(), "timeout") {
			t.Logf("Second request failed with unexpected error: %v", err)
		}
	}
}

// TestBufferedConnKeepAlive verifies that bufferedConn (used for protocol detection)
// works correctly with http.Serve for multiple requests.
// The first request's bytes are partially consumed during detection,
// then replayed via bufferedConn.
func TestBufferedConnKeepAlive(t *testing.T) {
	requestPaths := []string{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPaths = append(requestPaths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "ok")
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer func() { _ = listener.Close() }()

	addr := listener.Addr().String()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				// Note: do NOT defer c.Close() - http.Serve manages the connection

				// Simulate protocol detection: read first bytes
				buf := make([]byte, 4096)
				n, err := c.Read(buf)
				if err != nil {
					return
				}

				// Wrap in bufferedConn to replay peeked bytes
				bc := &bufferedConn{
					Conn: c,
					buf:  buf[:n],
				}

				// Use http.Serve - this should handle keep-alive correctly
				// even though first bytes were already consumed
				if err := http.Serve(&singleConnListener{conn: bc}, handler); err != nil {
					return
				}
			}(conn)
		}
	}()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Send 3 requests on the same connection
	for i, path := range []string{"/first", "/second", "/third"} {
		reqStr := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: localhost\r\nConnection: keep-alive\r\n\r\n", path)
		if _, err := conn.Write([]byte(reqStr)); err != nil {
			t.Fatalf("Request %d: failed to write: %v", i+1, err)
		}

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("Request %d (%s): failed to read response: %v", i+1, path, err)
		}

		if !strings.Contains(string(buf[:n]), "200 OK") {
			t.Errorf("Request %d (%s): expected 200 OK, got: %s", i+1, path, string(buf[:n]))
		}
	}

	if len(requestPaths) != 3 {
		t.Errorf("Expected 3 requests, got %d: %v", len(requestPaths), requestPaths)
	}
	for i, expected := range []string{"/first", "/second", "/third"} {
		if i < len(requestPaths) && requestPaths[i] != expected {
			t.Errorf("Request %d: expected path %s, got %s", i+1, expected, requestPaths[i])
		}
	}
}

// singleResponseWriter is a minimal ResponseWriter that writes raw HTTP to a connection
// Used only for the broken-behavior regression test
type singleResponseWriter struct {
	conn        net.Conn
	headersSent bool
	header      http.Header
}

func (w *singleResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *singleResponseWriter) Write(data []byte) (int, error) {
	if !w.headersSent {
		w.WriteHeader(http.StatusOK)
	}
	return w.conn.Write(data)
}

func (w *singleResponseWriter) WriteHeader(statusCode int) {
	if w.headersSent {
		return
	}
	w.headersSent = true
	_, _ = fmt.Fprintf(w.conn, "HTTP/1.1 %d %s\r\n", statusCode, http.StatusText(statusCode))
	for k, v := range w.header {
		for _, val := range v {
			_, _ = fmt.Fprintf(w.conn, "%s: %s\r\n", k, val)
		}
	}
	_, _ = fmt.Fprintf(w.conn, "\r\n")
}
