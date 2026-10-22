package proxy

import (
	cryptotls "crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"

	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// extractAndNormalizeIP extracts the client IP from the request and normalizes localhost IPs
// Normalizes both IPv4 (127.0.0.1) and IPv6 (::1) to "127.0.0.1" for consistency
func extractAndNormalizeIP(r *http.Request) string {
	var ip string

	// Check X-Forwarded-For header first (for proxies)
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded != "" {
		// X-Forwarded-For can contain multiple IPs, take the first one
		ips := strings.Split(forwarded, ",")
		if len(ips) > 0 {
			ip = strings.TrimSpace(ips[0])
			// Remove port if present
			if host, _, err := net.SplitHostPort(ip); err == nil {
				ip = host
			}
		}
	}

	// Fall back to RemoteAddr if no X-Forwarded-For
	if ip == "" {
		ip = r.RemoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}
	}

	// Normalize localhost IPs: both IPv4 (127.0.0.1) and IPv6 (::1) map to 127.0.0.1
	if ip == "::1" || ip == "[::1]" {
		return "127.0.0.1"
	}

	return ip
}

// handleConnect handles CONNECT requests (port 3128)
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	// Extract hostname from CONNECT request
	host := r.Host
	if !strings.Contains(host, ":") {
		host = util.ReplacePort(host, 443)
	}

	hostname := strings.Split(host, ":")[0]

	// Prevent proxy loops: never pass through CONNECT requests to localhost/127.0.0.1/::1
	// These should never be proxied through the proxy itself
	if util.IsLocalhost(hostname) {
		log.Printf("[%s] ERROR: CONNECT request to proxy itself (%s) - rejecting to prevent loop", r.RemoteAddr, hostname)
		http.Error(w, "Bad Request: Cannot proxy CONNECT requests to the proxy itself", http.StatusBadRequest)
		return
	}

	// Check if this is a package manager (APK, APT, NPM, PyPI) first
	if pmProxy, isPackageManager := s.packageProxies[hostname]; isPackageManager {
		log.Printf("[%s] CONNECT %s -> Package manager proxy", r.RemoteAddr, hostname)

		// Extract and normalize the original client IP from the CONNECT request
		originalClientIP := extractAndNormalizeIP(r)

		// Hijack the connection BEFORE sending response
		// This ensures we have full control over the connection
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
			return
		}

		clientConn, _, err := hijacker.Hijack()
		if err != nil {
			log.Printf("Error hijacking connection: %v", err)
			return
		}
		defer util.DeferClose(clientConn)

		// Send 200 Connection Established response directly to the connection
		// This ensures the client knows the tunnel is established before we try TLS
		_, err = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		if err != nil {
			log.Printf("Error writing CONNECT response: %v", err)
			return
		}

		// Generate certificate for this hostname
		cert, err := s.certManager.GenerateCertForHost(hostname)
		if err != nil {
			log.Printf("Error generating certificate for %s: %v", hostname, err)
			return
		}

		// Perform TLS handshake with client
		// For APK, prefer HTTP/1.1 to avoid HTTP/2 issues with upstream fetches
		// Other package managers can use HTTP/2
		nextProtos := []string{"h2", "http/1.1"}
		if hostname == "dl-cdn.alpinelinux.org" || strings.HasSuffix(hostname, ".alpinelinux.org") {
			// Prefer HTTP/1.1 for APK to avoid upstream fetch issues
			nextProtos = []string{"http/1.1", "h2"}
		}
		tlsConfig := &cryptotls.Config{
			Certificates: []cryptotls.Certificate{*cert},
			NextProtos:   nextProtos,
		}

		tlsClientConn := cryptotls.Server(clientConn, tlsConfig)
		if err := tlsClientConn.Handshake(); err != nil {
			// EOF errors are common when clients close connections early (e.g., connection pooling)
			// Only log if it's not EOF to avoid noise
			if err.Error() != "EOF" {
				log.Printf("Error performing TLS handshake for %s: %v", hostname, err)
			}
			return
		}
		defer util.DeferClose(tlsClientConn)

		log.Printf("[%s] TLS handshake complete for %s, negotiated protocol: %s",
			r.RemoteAddr, hostname, tlsClientConn.ConnectionState().NegotiatedProtocol)

		handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req.Header.Set("X-Forwarded-For", originalClientIP)
			log.Printf("[%s] [%s] %s %s", originalClientIP, hostname, req.Method, req.URL.Path)
			pmProxy.ServeHTTP(w, req)
		})

		if err := serveTLS(tlsClientConn, handler); err != nil {
			log.Printf("[%s] Error serving package manager %s: %v", r.RemoteAddr, hostname, err)
		}
		return
	}

	// Check if this is a Docker registry proxy
	if dockerProxy, isDocker := s.dockerProxies[hostname]; isDocker {
		log.Printf("[%s] CONNECT %s -> Docker registry proxy", r.RemoteAddr, hostname)
		s.handleDockerRegistry(w, r, hostname, dockerProxy)
		return
	}

	// Check if this is a known registry
	if configs, isKnown := s.knownRegistries[hostname]; isKnown && len(configs) > 0 {
		// For CONNECT, we use the first config (can't do path matching without seeing the request)
		registryConfig := configs[0]
		// Default to local cache if not specified
		if registryConfig.CacheType == "" || registryConfig.CacheType == "local" {
			log.Printf("[%s] CONNECT %s -> Local cache", r.RemoteAddr, hostname)
			if s.registryProxy != nil {
				s.registryProxy.HandleLocalCache(w, r, hostname, registryConfig, s.certManager)
			}
			return
		}
		log.Printf("[%s] CONNECT %s -> Unsupported cache_type: %s (only 'local' is supported)", r.RemoteAddr, hostname, registryConfig.CacheType)
		http.Error(w, fmt.Sprintf("Unsupported cache_type: %s (only 'local' is supported)", registryConfig.CacheType), http.StatusBadRequest)
		return
	}

	// Check for Docker blob storage (Cloudflare R2)
	// R2 URLs don't contain repository names, so we can't proxy them through Nexus
	// Pass through transparently instead (nginx mirror will cache if configured)
	if strings.HasPrefix(hostname, "docker-images") && strings.HasSuffix(hostname, ".r2.cloudflarestorage.com") {
		log.Printf("  -> Pass-through R2 blob storage (no repo name in URL)")
		s.handleUnknownRegistry(w, r, host)
		return
	}

	// Otherwise, transparent pass-through
	log.Printf("  -> Pass-through to %s", host)
	s.handleUnknownRegistry(w, r, host)
}

// handleDockerRegistry handles CONNECT requests for Docker registries
func (s *Server) handleDockerRegistry(w http.ResponseWriter, r *http.Request, hostname string, dockerProxy http.Handler) {
	// Extract and normalize the original client IP from the CONNECT request
	originalClientIP := extractAndNormalizeIP(r)

	// Hijack the connection BEFORE writing response
	// This ensures we have full control over the connection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		log.Printf("Error hijacking connection: %v", err)
		return
	}
	defer util.DeferClose(clientConn)

	// Send 200 Connection Established response directly to the connection
	// Format: "HTTP/1.1 200 Connection Established\r\n\r\n"
	response := "HTTP/1.1 200 Connection Established\r\n\r\n"
	if _, err := clientConn.Write([]byte(response)); err != nil {
		log.Printf("Error writing CONNECT response: %v", err)
		return
	}

	// Generate certificate for this hostname
	cert, err := s.certManager.GenerateCertForHost(hostname)
	if err != nil {
		log.Printf("Error generating certificate for %s: %v", hostname, err)
		return
	}

	// Perform TLS handshake with client
	// Support both HTTP/2 and HTTP/1.1
	tlsConfig := &cryptotls.Config{
		Certificates: []cryptotls.Certificate{*cert},
		NextProtos:   []string{"h2", "http/1.1"}, // Support HTTP/2 and HTTP/1.1
	}

	tlsClientConn := cryptotls.Server(clientConn, tlsConfig)
	if err := tlsClientConn.Handshake(); err != nil {
		if err.Error() != "EOF" {
			// Check if this is a certificate validation error
			if strings.Contains(err.Error(), "bad certificate") || strings.Contains(err.Error(), "certificate") {
				log.Printf("Error performing TLS handshake for %s: %v", hostname, err)
				log.Printf("  Docker client rejected our certificate. To enable MITM caching for %s, Docker must trust the proxy's CA certificate.", hostname)
				log.Printf("  Install the CA certificate from: http://localhost:3128/api/v1/certs/ca")
				log.Printf("  Or configure Docker to use InsecureSkipVerify (not recommended for production)")
			} else {
				log.Printf("Error performing TLS handshake for %s: %v", hostname, err)
			}
		}
		return
	}
	defer util.DeferClose(tlsClientConn)

	log.Printf("[%s] TLS handshake complete for %s, negotiated protocol: %s",
		r.RemoteAddr, hostname, tlsClientConn.ConnectionState().NegotiatedProtocol)

	wrappedHandler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Header.Set("X-Forwarded-For", originalClientIP)
		dockerProxy.ServeHTTP(w, req)
	})

	if err := serveTLS(tlsClientConn, wrappedHandler); err != nil {
		log.Printf("[%s] Error serving Docker registry %s: %v", r.RemoteAddr, hostname, err)
	}
}

// handleUnknownRegistry handles CONNECT requests for unknown registries (pass-through)
func (s *Server) handleUnknownRegistry(w http.ResponseWriter, r *http.Request, targetHost string) {
	// Standard CONNECT tunnel - just pass through

	// Hijack the connection BEFORE connecting to target
	// This ensures we have full control over the connection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		log.Printf("Error hijacking connection: %v", err)
		return
	}
	defer util.DeferClose(clientConn)

	// Connect to the target server
	targetConn, err := net.DialTimeout("tcp", targetHost, 30*time.Second)
	if err != nil {
		log.Printf("Error connecting to %s: %v", targetHost, err)
		// Send error response to client
		if _, err := clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n")); err != nil {
			log.Printf("Failed to send error response to client: %v", err)
		}
		return
	}
	defer util.DeferClose(targetConn)

	// Send 200 Connection Established response directly to the connection
	// Format: "HTTP/1.1 200 Connection Established\r\n\r\n"
	response := "HTTP/1.1 200 Connection Established\r\n\r\n"
	if _, err := clientConn.Write([]byte(response)); err != nil {
		log.Printf("Error writing CONNECT response: %v", err)
		return
	}

	// Bidirectional copy - wait for BOTH to complete
	var wg sync.WaitGroup
	wg.Add(2)
	startTime := time.Now()
	lastActivity := time.Now()
	var lastActivityMu sync.Mutex

	// Client -> Target
	go func() {
		defer wg.Done()
		n, _ := copyWithActivity(targetConn, clientConn, &lastActivity, &lastActivityMu)
		log.Printf("  Client->Target: %d bytes", n)
	}()

	// Target -> Client
	go func() {
		defer wg.Done()
		n, _ := copyWithActivity(clientConn, targetConn, &lastActivity, &lastActivityMu)
		log.Printf("  Target->Client: %d bytes", n)
	}()

	// Wait for BOTH directions to complete
	wg.Wait()
	duration := time.Since(startTime)

	lastActivityMu.Lock()
	idleTime := time.Since(lastActivity)
	lastActivityMu.Unlock()

	log.Printf("  CONNECT to %s completed in %v (idle for %v before close)", targetHost, duration, idleTime)
}

// handleHTTP handles plain HTTP requests (port 3128, non-CONNECT)
func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// Handle favicon.ico requests - return 404 to avoid proxy loop
	if r.URL.Path == "/favicon.ico" {
		http.NotFound(w, r)
		return
	}

	// Check for UI routes first
	if s.uiServer != nil && (strings.HasPrefix(r.URL.Path, "/ui") || strings.HasPrefix(r.URL.Path, "/api")) {
		s.uiServer.ServeHTTP(w, r)
		return
	}

	// For plain HTTP requests (Docker registries, APK, and APT repos)
	hostname := r.Host
	if strings.Contains(hostname, ":") {
		hostname = strings.Split(hostname, ":")[0]
	}

	// For localhost requests with /v2/ paths, route to Docker registry proxy (direct access, not proxy loop)
	if util.IsLocalhost(hostname) && (strings.HasPrefix(r.URL.Path, "/v2/") || r.URL.Path == "/v2") {
		dockerProxy := s.findDockerProxy(hostname)
		if dockerProxy != nil {
			log.Printf("[%s] Direct HTTP request to %s -> Docker registry proxy", r.RemoteAddr, r.URL.Path)
			dockerProxy.ServeHTTP(w, r)
			return
		}
	}

	// For localhost requests with non-API/non-Docker paths, route to the local
	// file cache (supports GET/PUT/DELETE for plain files like database dumps).
	if util.IsLocalhost(hostname) {
		if s.registryProxy != nil {
			localConfig := config.RegistryConfig{CacheType: "local"}
			log.Printf("[%s] %s %s -> Local file cache", r.RemoteAddr, r.Method, r.URL.Path)
			s.registryProxy.HandleLocalCacheHTTP(w, r, "localhost", localConfig)
			return
		}
		// No registry proxy available — reject to prevent loop
		log.Printf("[%s] ERROR: Request to proxy itself (%s) with path %q - no local cache available",
			r.RemoteAddr, hostname, r.URL.Path)
		http.Error(w, "Bad Request: Cannot proxy requests to the proxy itself", http.StatusBadRequest)
		return
	}

	// Check for package managers FIRST (they take precedence over legacy registry config)
	if pmProxy, isPackageManager := s.packageProxies[hostname]; isPackageManager {
		pmProxy.ServeHTTP(w, r)
		return
	}

	// Check for Docker registries
	if dockerProxy, isDocker := s.dockerProxies[hostname]; isDocker {
		dockerProxy.ServeHTTP(w, r)
		return
	}

	// Check if this is a known registry (local cache only)
	if configs, isKnown := s.knownRegistries[hostname]; isKnown && len(configs) > 0 {
		registryConfig, found := config.FindBestMatchingConfig(s.knownRegistries, hostname, r.URL.Path)
		if found {
			// Default to local cache if not specified
			if registryConfig.CacheType == "" || registryConfig.CacheType == "local" {
				log.Printf("[%s] %s %s -> Local cache (HTTP)", r.RemoteAddr, r.Method, r.Host)
				if s.registryProxy != nil {
					s.registryProxy.HandleLocalCacheHTTP(w, r, hostname, registryConfig)
				}
				return
			}
			log.Printf("[%s] %s %s -> Unsupported cache_type: %s (only 'local' is supported)", r.RemoteAddr, r.Method, r.Host, registryConfig.CacheType)
			http.Error(w, fmt.Sprintf("Unsupported cache_type: %s (only 'local' is supported)", registryConfig.CacheType), http.StatusBadRequest)
			return
		}
	}
	log.Printf("[%s] %s %s -> Pass-through", r.RemoteAddr, r.Method, r.Host)
	s.proxyHTTPDirect(w, r)
}

// handlePackageManagerSNI handles package manager requests via SNI
func (s *Server) handlePackageManagerSNI(clientConn net.Conn, hostname string, remoteAddr string) {
	defer util.DeferClose(clientConn)

	// Get the package manager proxy
	pmProxy, exists := s.packageProxies[hostname]
	if !exists {
		log.Printf("Package manager proxy not found for: %s", hostname)
		return
	}

	// Generate certificate for this hostname
	cert, err := s.certManager.GenerateCertForHost(hostname)
	if err != nil {
		log.Printf("SNI router: failed to generate cert for %s: %v", hostname, err)
		return
	}

	// Perform TLS handshake with client
	// Support both HTTP/1.1 and HTTP/2 (PyPI and npm may use HTTP/2)
	tlsConfig := &cryptotls.Config{
		Certificates: []cryptotls.Certificate{*cert},
		NextProtos:   []string{"h2", "http/1.1"}, // Support HTTP/2 and HTTP/1.1
	}

	tlsConn := cryptotls.Server(clientConn, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("SNI router: TLS handshake failed for %s: %v", hostname, err)
		return
	}
	defer util.DeferClose(tlsConn)

	log.Printf("SNI router: Package manager TLS handshake complete for %s, negotiated protocol: %s",
		hostname, tlsConn.ConnectionState().NegotiatedProtocol)

	if err := serveTLS(tlsConn, pmProxy); err != nil {
		log.Printf("SNI router: Error serving package manager %s: %v", hostname, err)
	}
}

// handleDockerRegistrySNI handles Docker registry requests via SNI
func (s *Server) handleDockerRegistrySNI(clientConn net.Conn, hostname string, remoteAddr string, dockerProxy http.Handler) {
	defer util.DeferClose(clientConn)

	// Generate certificate for this hostname
	cert, err := s.certManager.GenerateCertForHost(hostname)
	if err != nil {
		log.Printf("SNI router: failed to generate cert for %s: %v", hostname, err)
		return
	}

	// Perform TLS handshake with client
	// Support both HTTP/2 and HTTP/1.1
	tlsConfig := &cryptotls.Config{
		Certificates: []cryptotls.Certificate{*cert},
		NextProtos:   []string{"h2", "http/1.1"},
	}

	tlsConn := cryptotls.Server(clientConn, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		// Log all handshake errors for debugging (including EOF)
		log.Printf("SNI router: TLS handshake failed for %s: %v (type: %T)", hostname, err, err)
		return
	}
	defer util.DeferClose(tlsConn)

	log.Printf("SNI router: TLS handshake complete for %s, negotiated protocol: %s",
		hostname, tlsConn.ConnectionState().NegotiatedProtocol)

	if err := serveTLS(tlsConn, dockerProxy); err != nil {
		log.Printf("SNI router: Error serving Docker registry %s: %v", hostname, err)
	}
}

// proxyHTTPDirect proxies HTTP requests directly (pass-through)
func (s *Server) proxyHTTPDirect(w http.ResponseWriter, r *http.Request) {
	// Create a simple pass-through proxy
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// Ensure the request URL is properly formed
			// If URL doesn't have a scheme, determine it from the request
			if req.URL.Scheme == "" {
				// Check if request is HTTPS (TLS connection)
				if req.TLS != nil {
					req.URL.Scheme = "https"
				} else {
					req.URL.Scheme = "http"
				}
			}

			// If URL doesn't have a host, use the Host header
			if req.URL.Host == "" {
				req.URL.Host = req.Host
			}

			// Ensure Host header matches URL host
			req.Host = req.URL.Host

			// Remove proxy-related headers
			req.Header.Del("X-Forwarded-For")
			req.Header.Del("X-Forwarded-Proto")
		},
		Transport: s.transport,
	}

	proxy.ServeHTTP(w, r)
}

// serveTLS serves HTTP/1.1 and HTTP/2 on a pre-negotiated TLS connection using
// Go's standard http.Server with http2.ConfigureServer. This replaces manual
// protocol branching (checking NegotiatedProtocol and using http2.Server.ServeConn
// directly) with the standard library's integrated connection management, which
// properly handles HTTP/2 flow control, connection lifecycle, and graceful shutdown.
//
// The increased upload buffer sizes (4MB vs 1MB default) reduce flow-control
// pressure when clients like crane send parallel streaming uploads over HTTP/2.
func serveTLS(tlsConn *cryptotls.Conn, handler http.Handler) error {
	listener := &singleConnListener{
		conn: tlsConn,
		done: make(chan struct{}),
	}
	httpServer := &http.Server{
		Handler:           handler,
		IdleTimeout:       120 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				listener.signalDone()
			}
		},
	}
	h2s := &http2.Server{
		MaxUploadBufferPerConnection: 4 << 20, // 4MB (default 1MB)
		MaxUploadBufferPerStream:     4 << 20, // 4MB (default 1MB)
		IdleTimeout:                  120 * time.Second,
	}
	if err := http2.ConfigureServer(httpServer, h2s); err != nil {
		return fmt.Errorf("failed to configure HTTP/2: %w", err)
	}
	return httpServer.Serve(listener)
}

// singleConnListener wraps a single connection to work with http.Server.Serve.
// The first Accept returns the connection; the second blocks until signalDone
// is called (via ConnState callback when the connection closes), preventing
// Serve from returning while the connection handler goroutine is still active.
type singleConnListener struct {
	conn     net.Conn
	once     sync.Once
	done     chan struct{}
	doneOnce sync.Once
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var conn net.Conn
	l.once.Do(func() {
		conn = l.conn
	})
	if conn != nil {
		return conn, nil
	}
	// Block until the connection is done being served
	<-l.done
	return nil, io.EOF
}

func (l *singleConnListener) signalDone() {
	l.doneOnce.Do(func() { close(l.done) })
}

func (l *singleConnListener) Close() error {
	l.signalDone()
	return nil
}

func (l *singleConnListener) Addr() net.Addr {
	return l.conn.LocalAddr()
}

// copyWithActivity copies data and tracks last activity time
func copyWithActivity(dst io.Writer, src io.Reader, lastActivity *time.Time, mu *sync.Mutex) (int64, error) {
	buf := make([]byte, 32*1024)
	var written int64
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			if nw > 0 {
				written += int64(nw)
				mu.Lock()
				*lastActivity = time.Now()
				mu.Unlock()
			}
			if ew != nil {
				return written, ew
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if er != nil {
			if er != io.EOF {
				return written, er
			}
			break
		}
	}
	return written, nil
}
