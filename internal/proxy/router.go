package proxy

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/tls"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// startHTTPRouter starts the HTTP router on port 80
func (s *Server) startHTTPRouter(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	defer util.DeferClose(listener)

	log.Printf("HTTP router listening on %s", addr)

	for {
		clientConn, err := listener.Accept()
		if err != nil {
			log.Printf("HTTP router accept error: %v", err)
			continue
		}

		go s.handleHTTPConnection(clientConn)
	}
}

// handleHTTPConnection handles a single HTTP connection on port 80
func (s *Server) handleHTTPConnection(clientConn net.Conn) {
	defer util.DeferClose(clientConn)

	// Read HTTP request
	bufReader := bufio.NewReader(clientConn)
	req, err := http.ReadRequest(bufReader)
	if err != nil {
		log.Printf("HTTP router: failed to read request: %v", err)
		return
	}

	// Check for UI routes first (same as handleHTTP on port 3128)
	if s.uiServer != nil && (strings.HasPrefix(req.URL.Path, "/ui") || strings.HasPrefix(req.URL.Path, "/api")) {
		// Create a response writer for the connection
		resp := &httpResponseWriter{
			conn:   clientConn,
			header: make(http.Header),
			req:    req,
		}
		s.uiServer.ServeHTTP(resp, req)
		return
	}

	// Extract hostname from Host header
	hostname := req.Host
	if strings.Contains(hostname, ":") {
		hostname = strings.Split(hostname, ":")[0]
	}

	// Look up registry config for this hostname
	registryConfig, found := config.FindBestMatchingConfig(s.knownRegistries, hostname, req.URL.Path)
	if !found {
		log.Printf("HTTP router [%s]: %s %s -> unknown host, passing through", clientConn.RemoteAddr().String(), req.Method, req.Host)
		// Return 404 for unknown hosts
		resp := &http.Response{
			StatusCode: http.StatusNotFound,
			Status:     "404 Not Found",
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("Unknown host")),
		}
		resp.Header.Set("Content-Type", "text/plain")
		if err := resp.Write(clientConn); err != nil {
			log.Printf("Failed to write response: %v", err)
		}
		return
	}

	// Support local cache for plain HTTP requests
	if registryConfig.CacheType == "" || registryConfig.CacheType == "local" {
		log.Printf("HTTP router [%s]: %s %s -> Local cache (HTTP)", clientConn.RemoteAddr().String(), req.Method, req.Host)
		// Create a response writer for the connection
		resp := &httpResponseWriter{
			conn:   clientConn,
			header: make(http.Header),
			req:    req,
		}
		if s.registryProxy != nil {
			s.registryProxy.HandleLocalCacheHTTP(resp, req, hostname, registryConfig)
		}
		return
	}
	log.Printf("HTTP router [%s]: %s %s -> Unsupported cache_type: %s", clientConn.RemoteAddr().String(), req.Method, req.Host, registryConfig.CacheType)
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Status:     "400 Bad Request",
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(fmt.Sprintf("Unsupported cache_type: %s (only 'local' is supported)", registryConfig.CacheType))),
	}
	resp.Header.Set("Content-Type", "text/plain")
	if err := resp.Write(clientConn); err != nil {
		log.Printf("Failed to write response: %v", err)
	}
}

// startSNIRouter starts the SNI-based router on port 443
func (s *Server) startSNIRouter(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	defer util.DeferClose(listener)

	log.Printf("SNI router listening on %s", addr)

	for {
		clientConn, err := listener.Accept()
		if err != nil {
			log.Printf("SNI router accept error: %v", err)
			continue
		}

		go s.handleSNIConnection(clientConn)
	}
}

// handleSNIConnection handles a single SNI connection on port 443
func (s *Server) handleSNIConnection(clientConn net.Conn) {
	defer util.DeferClose(clientConn)

	// Read the first few bytes to detect if this is HTTP or HTTPS
	buf := make([]byte, 4096)
	n, err := clientConn.Read(buf)
	if err != nil {
		log.Printf("SNI router: failed to read data: %v", err)
		return
	}

	// Create a buffered connection to replay the bytes we already read
	fullConn := &bufferedConn{
		Conn: clientConn,
		buf:  buf[:n],
	}

	// Check if this looks like an HTTP request (not TLS)
	// HTTP requests start with a method like "GET ", "POST ", "HEAD ", etc.
	isHTTP := false
	switch {
	case n >= 4 && string(buf[0:4]) == "GET ":
		isHTTP = true
	case n >= 5 && string(buf[0:5]) == "POST ":
		isHTTP = true
	case n >= 5 && string(buf[0:5]) == "HEAD ":
		isHTTP = true
	case n >= 4 && string(buf[0:4]) == "PUT ":
		isHTTP = true
	case n >= 7 && string(buf[0:7]) == "DELETE ":
		isHTTP = true
	case n >= 6 && string(buf[0:6]) == "PATCH ":
		isHTTP = true
	}

	if isHTTP {
		log.Printf("SNI router: plain HTTP request detected on SNI port (Docker registries should use HTTPS/SNI routing)")
		// Handle as plain HTTP, no TLS - pass through
		backend := "docker.io:443"
		log.Printf("  -> Pass-through to: %s", backend)
		backendConn, err := net.DialTimeout("tcp", backend, 30*time.Second)
		if err != nil {
			log.Printf("SNI router: failed to connect to backend %s: %v", backend, err)
			return
		}
		defer util.DeferClose(backendConn)
		// Bidirectional copy
		go func() {
			if _, err := io.Copy(backendConn, fullConn); err != nil {
				log.Printf("Error copying to backend: %v", err)
			}
		}()
		if _, err := io.Copy(fullConn, backendConn); err != nil {
			log.Printf("Error copying from backend: %v", err)
		}
		return
	}

	// Extract SNI hostname from ClientHello (TLS)
	hostname := tls.ExtractSNI(buf[:n])
	if hostname == "" {
		log.Printf("SNI router: no SNI found in TLS ClientHello, defaulting to docker.io (port 5000)")
		hostname = "docker.io"
	}

	// Check if this is a package manager (APK, APT, NPM, PyPI) first
	if _, isPackageManager := s.packageProxies[hostname]; isPackageManager {
		log.Printf("SNI router: package manager detected: %s", hostname)
		// Package managers use HTTPS, so unwrap TLS and handle with package manager proxy
		s.handlePackageManagerSNI(fullConn, hostname, clientConn.RemoteAddr().String())
		return
	}

	// For localhost and host.docker.internal, route based on path
	if util.IsLocalhost(hostname) {
		// Set TCP_NODELAY to send data immediately (disable Nagle's algorithm)
		if tcpConn, ok := fullConn.Conn.(*net.TCPConn); ok {
			if err := tcpConn.SetNoDelay(true); err != nil {
				log.Printf("Failed to set TCP_NODELAY: %v", err)
			}
		}

		tlsConn, err := s.setupTLSConnection(fullConn, hostname)
		if err != nil {
			log.Printf("SNI router: TLS setup failed for %s: %v", hostname, err)
			return
		}
		defer util.DeferClose(tlsConn)

		// Create mux for routing
		mux := http.NewServeMux()
		if dockerProxy := s.findDockerProxy(hostname); dockerProxy != nil {
			mux.Handle("/v2/", dockerProxy)
		}
		if s.uiServer != nil {
			mux.Handle("/", s.uiServer)
		}

		if err := serveTLS(tlsConn, mux); err != nil {
			log.Printf("SNI router: Error serving localhost: %v", err)
		}
		return
	}

	// Check if this is a Docker registry proxy (for non-localhost hostnames)
	if dockerProxy, isDocker := s.dockerProxies[hostname]; isDocker {
		log.Printf("SNI router: Docker registry detected: %s", hostname)
		s.handleDockerRegistrySNI(fullConn, hostname, clientConn.RemoteAddr().String(), dockerProxy)
		return
	}

	// Check if this is a known registry - if so, use local cache (CONNECT only, not SNI)
	// SNI routing for known registries should go through CONNECT handler
	if configs, isKnown := s.knownRegistries[hostname]; isKnown && len(configs) > 0 {
		registryConfig := configs[0]
		if registryConfig.CacheType == "" || registryConfig.CacheType == "local" {
			log.Printf("SNI router: Known registry %s detected, but SNI routing for local cache not yet supported. Use CONNECT instead. Passing through.", hostname)
			// For now, pass through - local cache works with CONNECT, not SNI
			// Fall through to pass-through logic below
		} else {
			log.Printf("SNI router: Known registry %s has unsupported cache_type: %s. Passing through.", hostname, registryConfig.CacheType)
			// Fall through to pass-through logic below
		}
	}

	// For unknown hosts, pass through transparently
	var backend string
	if strings.HasPrefix(hostname, "docker-images") && strings.HasSuffix(hostname, ".r2.cloudflarestorage.com") {
		// R2 storage - pass through directly
		backend = hostname + ":443"
		log.Printf("  -> Pass-through to R2: %s", backend)
	} else {
		// Unknown host - pass through directly
		backend = hostname + ":443"
		log.Printf("  -> Pass-through to: %s", backend)
	}

	// Connect to backend
	backendConn, err := net.DialTimeout("tcp", backend, 30*time.Second)
	if err != nil {
		log.Printf("SNI router: failed to connect to backend %s: %v", backend, err)
		return
	}
	defer util.DeferClose(backendConn)

	// Enable TCP keepalive to prevent idle connection closure
	if tcpConn, ok := backendConn.(*net.TCPConn); ok {
		if err := tcpConn.SetKeepAlive(true); err != nil {
			log.Printf("Failed to set TCP keepalive: %v", err)
		}
		if err := tcpConn.SetKeepAlivePeriod(30 * time.Second); err != nil {
			log.Printf("Failed to set TCP keepalive period: %v", err)
		}
	}

	// First, forward the ClientHello we already read
	if _, err := backendConn.Write(buf[:n]); err != nil {
		log.Printf("SNI router: failed to forward ClientHello: %v", err)
		return
	}

	// Then do bidirectional copy
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if _, err := io.Copy(backendConn, clientConn); err != nil {
			log.Printf("Error copying to backend: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		if _, err := io.Copy(clientConn, backendConn); err != nil {
			log.Printf("Error copying from backend: %v", err)
		}
	}()

	wg.Wait()
}

// httpResponseWriter implements http.ResponseWriter for writing to a TLS connection
type httpResponseWriter struct {
	conn        net.Conn
	header      http.Header
	req         *http.Request
	status      int
	closed      bool // Connection is closed (for keep-alive detection)
	headersSent bool // Headers have been written (prevents writing twice)
}

func (rw *httpResponseWriter) Header() http.Header {
	return rw.header
}

func (rw *httpResponseWriter) Write(data []byte) (int, error) {
	if rw.status == 0 {
		rw.status = http.StatusOK
	}
	if !rw.headersSent {
		// Write status line and headers
		rw.WriteHeader(rw.status)
	}
	return rw.conn.Write(data)
}

// Flush implements http.Flusher
func (rw *httpResponseWriter) Flush() {
	// Since we write directly to the connection, there's nothing to flush
	// But we need to implement this interface so callers can detect we support flushing
}

func (rw *httpResponseWriter) WriteHeader(statusCode int) {
	if rw.headersSent {
		return
	}
	rw.status = statusCode
	statusText := http.StatusText(statusCode)
	if statusText == "" {
		statusText = "Unknown"
	}

	// Ensure Content-Length is set for responses with no body BEFORE writing headers
	if rw.header.Get("Content-Length") == "" {
		// For status codes that typically have no body, set Content-Length: 0
		if statusCode == http.StatusNoContent || statusCode == http.StatusAccepted ||
			statusCode == http.StatusCreated {
			rw.header.Set("Content-Length", "0")
		}
	}

	// Add Connection header if not already set
	if rw.header.Get("Connection") == "" {
		// Check if client requested keep-alive
		if rw.req != nil && strings.EqualFold(rw.req.Header.Get("Connection"), "keep-alive") {
			rw.header.Set("Connection", "keep-alive")
		} else if rw.req != nil && rw.req.Close {
			rw.header.Set("Connection", "close")
		}
		// If no Connection header in request, don't add one (let client/server negotiate)
	}

	// Write status line directly to ensure immediate write
	statusLine := fmt.Sprintf("HTTP/1.1 %d %s\r\n", statusCode, statusText)
	if _, err := rw.conn.Write([]byte(statusLine)); err != nil {
		log.Printf("httpResponseWriter: Error writing status line: %v", err)
		return
	}

	// Write headers directly to ensure immediate write
	for k, v := range rw.header {
		for _, val := range v {
			headerLine := fmt.Sprintf("%s: %s\r\n", k, val)
			if _, err := rw.conn.Write([]byte(headerLine)); err != nil {
				log.Printf("httpResponseWriter: Error writing header %s: %v", k, err)
				return
			}
		}
	}

	// End headers
	if _, err := rw.conn.Write([]byte("\r\n")); err != nil {
		log.Printf("httpResponseWriter: Error writing header terminator: %v", err)
		return
	}

	// Mark headers as sent (prevents writing headers twice)
	rw.headersSent = true

	// Only mark connection as closed if Connection: close is set, or if we're not using keep-alive
	// The closed flag is used by the keep-alive loop to determine if the connection should be closed
	connection := rw.header.Get("Connection")
	if strings.EqualFold(connection, "close") || (rw.req != nil && rw.req.Close) {
		rw.closed = true
	}
	// Note: For keep-alive connections, we don't set closed=true, allowing
	// the keep-alive loop to continue reading more requests
}

// bufferedConn wraps a connection and prepends buffered data to reads
type bufferedConn struct {
	net.Conn
	buf    []byte
	offset int
}

func (bc *bufferedConn) Read(p []byte) (int, error) {
	if bc.offset < len(bc.buf) {
		n := copy(p, bc.buf[bc.offset:])
		bc.offset += n
		return n, nil
	}
	return bc.Conn.Read(p)
}
