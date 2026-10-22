package proxy

import (
	"bufio"
	cryptotls "crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/registry"
	"github.com/Delphos-Labs/roxxy/internal/tls"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// Server is the main proxy server that handles HTTP, HTTPS (SNI), and CONNECT requests
type Server struct {
	nexusURL        *url.URL
	transport       *http.Transport
	listenAddrs     []string
	certManager     *tls.CertificateManager
	registryProxy   *registry.Proxy
	packageProxies  map[string]http.Handler
	dockerProxies   map[string]http.Handler
	knownRegistries config.RegistryMap
	uiServer        http.Handler // Optional UI server
}

// Config holds configuration for the proxy server
type Config struct {
	ListenAddrs     []string
	NexusURL        *url.URL
	CertManager     *tls.CertificateManager
	RegistryProxy   *registry.Proxy
	PackageProxies  map[string]http.Handler
	DockerProxies   map[string]http.Handler
	KnownRegistries config.RegistryMap
	UIServer        http.Handler // Optional UI server
}

// NewServer creates a new proxy server
func NewServer(cfg Config) *Server {
	return &Server{
		nexusURL: cfg.NexusURL,
		transport: &http.Transport{
			MaxResponseHeaderBytes: 10 << 20,         // 10 MB (handles long URLs with tokens/signatures)
			ResponseHeaderTimeout:  30 * time.Second, // Timeout for reading response headers
			IdleConnTimeout:        90 * time.Second, // Keep idle connections alive
			MaxIdleConns:           100,              // Max idle connections across all hosts
			MaxIdleConnsPerHost:    10,               // Max idle connections per host
			TLSHandshakeTimeout:    10 * time.Second, // Timeout for TLS handshake
			ExpectContinueTimeout:  1 * time.Second,  // Timeout for Expect: 100-continue
		},
		listenAddrs:     cfg.ListenAddrs,
		certManager:     cfg.CertManager,
		registryProxy:   cfg.RegistryProxy,
		packageProxies:  cfg.PackageProxies,
		dockerProxies:   cfg.DockerProxies,
		knownRegistries: cfg.KnownRegistries,
		uiServer:        cfg.UIServer,
	}
}

// Start starts the proxy server on all configured ports
func (s *Server) Start() error {
	// Start HTTP/HTTPS proxy on all configured listen addresses
	// Port 3128 needs to support: HTTP proxy, HTTPS proxy (CONNECT), and direct HTTPS
	for _, addr := range s.listenAddrs {
		go func(listenAddr string) {
			if err := s.startProxyServer(listenAddr); err != nil {
				log.Printf("Proxy server error on %s: %v", listenAddr, err)
			}
		}(addr)
	}

	// Only start ports 80 and 443 when running in Docker (detected by /app directory)
	// On host, these ports require root and aren't needed for HTTP_PROXY usage
	if _, err := os.Stat("/app"); err == nil {
		// Running in Docker - start HTTP router on port 80
		go func() {
			if err := s.startHTTPRouter(":80"); err != nil {
				log.Printf("HTTP router error: %v", err)
			}
		}()

		// Start SNI router on port 443
		go func() {
			if err := s.startSNIRouter(":443"); err != nil {
				log.Printf("SNI router error: %v", err)
			}
		}()
	} else {
		log.Printf("Running on host - skipping ports 80 and 443 (use HTTP_PROXY on configured listen addresses)")
	}

	// Block forever (servers run in goroutines)
	select {}
}

// findDockerProxy finds the appropriate Docker proxy for a hostname
// It checks the hostname first, then falls back to common registries, then returns the first available proxy
func (s *Server) findDockerProxy(hostname string) http.Handler {
	// Try hostname first
	if proxy, exists := s.dockerProxies[hostname]; exists {
		return proxy
	}
	// Try common fallbacks
	if proxy, exists := s.dockerProxies["docker.io"]; exists {
		return proxy
	}
	if proxy, exists := s.dockerProxies["registry-1.docker.io"]; exists {
		return proxy
	}
	// Return first available proxy as last resort
	for _, proxy := range s.dockerProxies {
		return proxy
	}
	return nil
}

// setupTLSConnection generates a certificate for the hostname and performs TLS handshake
// Returns the TLS connection or an error. Caller is responsible for closing the connection.
func (s *Server) setupTLSConnection(conn net.Conn, hostname string) (*cryptotls.Conn, error) {
	// Generate certificate for this hostname
	cert, err := s.certManager.GenerateCertForHost(hostname)
	if err != nil {
		return nil, fmt.Errorf("failed to generate cert: %w", err)
	}

	// Create TLS config with HTTP/2 support
	tlsConfig := &cryptotls.Config{
		Certificates: []cryptotls.Certificate{*cert},
		NextProtos:   []string{"h2", "http/1.1"}, // Support HTTP/2 and HTTP/1.1
		MinVersion:   cryptotls.VersionTLS12,
	}

	// Perform TLS handshake
	tlsConn := cryptotls.Server(conn, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		return nil, fmt.Errorf("TLS handshake failed: %w", err)
	}

	// Log negotiated protocol
	negotiatedProto := tlsConn.ConnectionState().NegotiatedProtocol
	if negotiatedProto == "" {
		negotiatedProto = "http/1.1"
	}
	log.Printf("TLS handshake complete for %s, negotiated protocol: %s", hostname, negotiatedProto)

	return tlsConn, nil
}

// startProxyServer starts a proxy server that handles both HTTP and HTTPS on the same port
// It detects the protocol by peeking at the first bytes of the connection
func (s *Server) startProxyServer(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	defer util.DeferClose(listener)

	log.Printf("Starting proxy server on %s (supports HTTP, HTTPS proxy, and direct HTTPS)", addr)

	for {
		clientConn, err := listener.Accept()
		if err != nil {
			log.Printf("Proxy server accept error on %s: %v", addr, err)
			continue
		}

		go s.handleProxyConnection(clientConn, addr)
	}
}

// handleProxyConnection handles a connection on port 3128, detecting if it's HTTP or HTTPS
func (s *Server) handleProxyConnection(clientConn net.Conn, listenAddr string) {
	// For most protocols, we close the connection when this function returns.
	// For plain HTTP, http.Serve takes ownership and manages the connection
	// lifecycle (including keep-alive), so we must not close it here.
	shouldClose := true
	defer func() {
		if shouldClose {
			util.DeferClose(clientConn)
		}
	}()

	// Read the first few bytes to detect protocol
	buf := make([]byte, 4096)
	n, err := clientConn.Read(buf)
	if err != nil {
		if err != io.EOF {
			log.Printf("Proxy server: failed to read data: %v", err)
		}
		return
	}

	// Create a buffered connection to replay the bytes we already read
	fullConn := &bufferedConn{
		Conn: clientConn,
		buf:  buf[:n],
	}

	// Check if this looks like TLS (starts with 0x16 for TLS handshake)
	isTLS := n >= 1 && buf[0] == 0x16

	if isTLS {
		// Direct HTTPS connection - handle like SNI router does for localhost
		s.handleDirectHTTPS(fullConn, buf[:n])
		return
	}

	// Check if this looks like a CONNECT request (must be checked before other HTTP methods)
	isCONNECT := n >= 7 && string(buf[0:7]) == "CONNECT"

	if isCONNECT {
		// For CONNECT requests, we need to parse just enough to get the Host
		// but NOT consume the buffer, so handleConnect can read the full request
		// Parse the request line: "CONNECT host:port HTTP/1.1"
		requestText := string(buf[:n])
		lines := strings.Split(requestText, "\r\n")
		if len(lines) == 0 {
			log.Printf("Proxy server: invalid CONNECT request")
			return
		}

		// Parse "CONNECT host:port HTTP/1.1"
		parts := strings.Fields(lines[0])
		if len(parts) < 2 {
			log.Printf("Proxy server: invalid CONNECT request line: %s", lines[0])
			return
		}

		host := parts[1]
		if !strings.Contains(host, ":") {
			host = util.ReplacePort(host, 443)
		}
		hostname := strings.Split(host, ":")[0]

		// Prevent proxy loops
		if util.IsLocalhost(hostname) {
			log.Printf("ERROR: CONNECT request to proxy itself (%s) - rejecting to prevent loop", hostname)
			if _, err := fullConn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n")); err != nil {
				log.Printf("Error writing error response: %v", err)
			}
			return
		}

		// Create a minimal request object with just the Host set
		// handleConnect will hijack and read the full request from the buffered connection
		req := &http.Request{
			Method: http.MethodConnect,
			Host:   host,
			Header: make(http.Header),
		}
		req.RemoteAddr = clientConn.RemoteAddr().String()

		// Create a hijackable response writer that uses the buffered connection
		// The bufferedConn will replay the bytes when handleConnect reads from it
		hijackableWriter := &hijackableResponseWriter{
			conn: fullConn, // Use the buffered connection so bytes are replayed
			req:  req,
		}

		// Route CONNECT request to appropriate handler
		// handleConnect will hijack and read the full request from the buffered connection
		s.handleConnect(hijackableWriter, req)
		return
	}

	// Check if this looks like an HTTP request (non-CONNECT)
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
		// Plain HTTP (non-CONNECT) - use http.Serve for proper keep-alive support.
		// This is critical for OCI registry push flows where the client sends multiple
		// requests (HEAD, POST, PATCH, PUT) on the same connection.
		// http.Serve takes ownership of the connection and manages its lifecycle.
		shouldClose = false
		if err := http.Serve(
			&singleConnListener{conn: fullConn},
			http.HandlerFunc(s.handleHTTP),
		); err != nil {
			// Ignore expected errors from connection close
			if err != io.EOF {
				log.Printf("Proxy server: HTTP serve error: %v", err)
			}
		}
		return
	}

	// Unknown protocol - log and close
	log.Printf("Proxy server: unknown protocol detected (first bytes: %v)", buf[:min(n, 20)])
}

// handleDirectHTTPS handles a direct HTTPS connection to port 3128 (e.g., curl https://localhost:3128/v2/charts)
func (s *Server) handleDirectHTTPS(clientConn net.Conn, buf []byte) {
	// Extract SNI hostname from ClientHello (TLS)
	hostname := tls.ExtractSNI(buf)
	if hostname == "" {
		// No SNI - default to localhost for port 3128
		hostname = "localhost"
	}

	// For localhost/127.0.0.1/host.docker.internal, handle like the SNI router does
	if util.IsLocalhost(hostname) {
		// Perform TLS handshake for localhost
		tlsConn, err := s.setupTLSConnection(clientConn, hostname)
		if err != nil {
			log.Printf("Proxy server: TLS setup failed for %s: %v", hostname, err)
			return
		}
		defer util.DeferClose(tlsConn)

		// Create a router handler that routes based on path
		router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/v2/") || r.URL.Path == "/v2" {
				dockerProxy := s.findDockerProxy(hostname)
				if dockerProxy != nil {
					log.Printf("Proxy server: HTTPS request to %s -> Docker registry proxy", r.URL.Path)
					dockerProxy.ServeHTTP(w, r)
					return
				}
			}

			if s.uiServer != nil {
				log.Printf("Proxy server: HTTPS request to %s -> UI server", r.URL.Path)
				s.uiServer.ServeHTTP(w, r)
				return
			}

			log.Printf("Proxy server: No handler for HTTPS request to %s", r.URL.Path)
			http.Error(w, "Not Found", http.StatusNotFound)
		})

		if err := serveTLS(tlsConn, router); err != nil {
			log.Printf("Proxy server: Error serving localhost HTTPS: %v", err)
		}
	}

	// For non-localhost HTTPS, this shouldn't happen on port 3128
	// But if it does, we could handle it similar to SNI router
	log.Printf("Proxy server: HTTPS connection to non-localhost %s on port 3128 - not supported", hostname)
}

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// hijackableResponseWriter wraps a connection to implement http.ResponseWriter and http.Hijacker
// This allows CONNECT requests to be handled when we already have the raw connection
type hijackableResponseWriter struct {
	conn net.Conn
	req  *http.Request
}

func (w *hijackableResponseWriter) Header() http.Header {
	return make(http.Header)
}

func (w *hijackableResponseWriter) Write(data []byte) (int, error) {
	return w.conn.Write(data)
}

func (w *hijackableResponseWriter) WriteHeader(statusCode int) {
	statusText := http.StatusText(statusCode)
	if statusText == "" {
		statusText = "Unknown"
	}
	if _, err := fmt.Fprintf(w.conn, "HTTP/1.1 %d %s\r\n\r\n", statusCode, statusText); err != nil {
		log.Printf("Error writing response header: %v", err)
	}
}

func (w *hijackableResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	// If the connection is a bufferedConn, we need to consume the CONNECT request
	// and then use the underlying connection for the actual data transfer
	var underlyingConn net.Conn
	if bc, ok := w.conn.(*bufferedConn); ok {
		// Read the entire CONNECT request (including headers) from the buffered connection
		// This consumes the buffer and positions us to read the actual data (TLS handshake)
		reader := bufio.NewReader(bc)
		_, err := http.ReadRequest(reader)
		if err != nil {
			log.Printf("Error reading CONNECT request from buffered connection: %v", err)
			return nil, nil, err
		}
		// Now the buffer is consumed, use the underlying connection
		underlyingConn = bc.Conn
	} else {
		underlyingConn = w.conn
	}

	// Create a ReadWriter for compatibility
	reader := bufio.NewReader(underlyingConn)
	writer := bufio.NewWriter(underlyingConn)
	rw := bufio.NewReadWriter(reader, writer)
	return underlyingConn, rw, nil
}
