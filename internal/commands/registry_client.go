package commands

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// registryClient pushes blobs and manifests to an OCI/Docker v2 registry.
// It handles bearer-token challenges, threading any docker-config credentials
// through to the token endpoint (the path used by ECR, GHCR, Quay, etc.).
//
// One client serves many repositories under the same registry; bearer tokens
// are cached per-repo because OCI scopes them by `repository:<name>:<actions>`.
type registryClient struct {
	// clients is a POOL of HTTP clients, each with its own Transport and thus
	// its own TCP/h2 connection. Go's http2 transport multiplexes every request
	// onto a single ClientConn per host, which serializes N concurrent workers
	// behind one congestion window over a WAN. N independent connections give N
	// congestion windows — the real throughput win — while each stays h2.
	clients []*http.Client
	next    atomic.Uint64
	baseURL string // e.g. "https://123.dkr.ecr.us-east-1.amazonaws.com"
	creds   RegistryCreds
	bearers map[string]string // repoName -> bearer token
}

// do round-robins a request across the connection pool. Used for ONE-OFF
// requests — HEAD existence checks, cross-repo mounts, manifest GET/PUT, the
// bearer-token GET — where there's no congestion window to keep warm.
//
// A blob's multi-request upload sequence does NOT use this: it pins to a
// single pool client via clientFor (see pushBlob), so the connection's TCP
// congestion window stays sized across the serial PATCH loop instead of
// hitting a fresh, cold connection on every chunk.
func (rc *registryClient) do(req *http.Request) (*http.Response, error) {
	i := int(rc.next.Add(1)-1) % len(rc.clients)
	return rc.clients[i].Do(req)
}

// clientFor returns the pool client at index i (mod pool size). The push
// coordinator pins worker i -> clients[i] for the worker's whole blob upload
// (POST init + PATCH loop + commit PUT), keeping that one connection's window
// warm; different workers ride different connections for parallel throughput.
func (rc *registryClient) clientFor(i int) *http.Client {
	return rc.clients[i%len(rc.clients)]
}

func newRegistryClient(registry string, poolSize int) (*registryClient, error) {
	registryURL := registry
	if !strings.HasPrefix(registryURL, "http://") && !strings.HasPrefix(registryURL, "https://") {
		registryURL = "https://" + registryURL
	}
	baseURL := strings.TrimSuffix(registryURL, "/")

	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid registry URL: %w", err)
	}

	tlsConfig := localhostTLSConfig(parsedURL)

	// tunedDial is a plain dialer that lets the OS auto-tune the TCP socket
	// buffers. An earlier version forced SO_SNDBUF/SO_RCVBUF to 8 MiB on every
	// connection to cover the bandwidth-delay product to AWS — but with N
	// parallel workers that fixed per-socket over-provisioning exhausted the
	// kernel mbuf cluster pool (macOS ENOBUFS) and throttled throughput
	// dramatically, far worse than the small-default-window it was meant to
	// fix, and it forced a fallback to fewer workers. macOS/Linux TCP
	// auto-tuning grows the window dynamically with NO fixed reservation, so
	// the warm pinned per-worker connections saturate the link to ECR without
	// starving mbufs. Do NOT re-introduce SetWriteBuffer/SetReadBuffer here.
	tunedDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		return d.DialContext(ctx, network, addr)
	}

	if poolSize < 1 {
		poolSize = 1
	}
	// Build a POOL of N clients, each with its OWN Transport so each dials its
	// own TCP/h2 connection. Earlier this was one shared Transport with
	// ForceAttemptHTTP2 — but Go's h2 multiplexes every request onto ONE
	// ClientConn per host, so N concurrent workers serialized behind a single
	// congestion window (lethal over a WAN to ECR: ~1 blob in flight however
	// many --workers). N connections = N congestion windows = real parallel
	// throughput. Each conn is still h2 (cheap framing, warm between serial
	// PATCHes). MaxConnsPerHost is gone: it fought h2 multiplexing and is moot
	// now that each Transport owns exactly one connection.
	//
	// ForceAttemptHTTP2 is REQUIRED: net/http only auto-enables h2 when the
	// caller uses http.DefaultTransport; a custom *http.Transport disables it.
	clients := make([]*http.Client, poolSize)
	for i := range clients {
		transport := &http.Transport{
			Proxy:               config.GetUpstreamProxyFunc(),
			MaxIdleConns:        4,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
			DialContext:         tunedDial,
		}
		if tlsConfig != nil {
			transport.TLSClientConfig = tlsConfig
		}
		clients[i] = &http.Client{
			Timeout:       30 * time.Minute,
			Transport:     transport,
			CheckRedirect: refuseCrossOriginRedirect,
		}
	}

	creds, err := LookupRegistryCreds(parsedURL.Host)
	if err != nil {
		// Don't fail the whole push for a malformed config — log and continue anonymously.
		fmt.Fprintf(os.Stderr, "Warning: registry credential lookup for %s failed: %v\n", parsedURL.Host, err)
	}

	// Refuse to send credentials in cleartext to a non-loopback host over
	// plain http://. Fail fast here rather than leaking the token mid-push.
	// Loopback over http stays allowed (local dev), and the opt-in env var
	// covers the rare trusted-network plaintext registry.
	if creds.Username != "" && !credsAllowedOverScheme(parsedURL) {
		return nil, fmt.Errorf(
			"refusing to send registry credentials in cleartext to %s over http:// — "+
				"use https, or set ROAR_ALLOW_INSECURE_HTTP=1 to override on a trusted network",
			parsedURL.Host)
	}

	return &registryClient{
		clients: clients,
		baseURL: baseURL,
		creds:   creds,
		bearers: make(map[string]string),
	}, nil
}

// credsAllowedOverScheme reports whether it's safe to attach credentials to
// requests against u. https is always fine. Plain http is allowed only to a
// loopback host (local dev) or when the operator has explicitly opted in via
// ROAR_ALLOW_INSECURE_HTTP=1 for a trusted-network plaintext registry.
func credsAllowedOverScheme(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	// Any loopback IP (127.0.0.0/8, ::1) is local dev — not only 127.0.0.1, so a
	// service on 127.0.0.2 isn't wrongly nudged toward ROAR_ALLOW_INSECURE_HTTP.
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return os.Getenv("ROAR_ALLOW_INSECURE_HTTP") == "1"
}

// localhostTLSConfig returns a TLS config that trusts roxxy's CA when pushing
// to localhost over HTTPS, falling back to InsecureSkipVerify if the CA isn't
// installed yet. Non-localhost destinations get nil (system trust store).
func localhostTLSConfig(u *url.URL) *tls.Config {
	host := u.Hostname()
	isLocalhost := host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
	if !isLocalhost || u.Scheme != "https" {
		return nil
	}

	certPath := filepath.Join(config.GetDefaultCertsDir(), "ca-cert.pem")
	caCertPEM, err := os.ReadFile(certPath)
	if err == nil {
		if block, _ := pem.Decode(caCertPEM); block != nil {
			if caCert, err := x509.ParseCertificate(block.Bytes); err == nil {
				pool := x509.NewCertPool()
				pool.AddCert(caCert)
				return &tls.Config{RootCAs: pool}
			}
		}
	}

	fmt.Fprintf(os.Stderr, "Warning: CA certificate not found at %s, using InsecureSkipVerify for localhost\n", certPath)
	fmt.Fprintf(os.Stderr, "  Run 'roxxy certs generate' to create the CA certificate\n")
	return &tls.Config{InsecureSkipVerify: true} //nolint:gosec // localhost-only fallback
}

// authorize sets the Authorization header on req using the best available
// credential: cached bearer token for this repo, then Basic from
// RegistryCreds, then nothing.
func (rc *registryClient) authorize(req *http.Request, repoName string) {
	if t, ok := rc.bearers[repoName]; ok && t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
		return
	}
	if rc.creds.Username != "" {
		req.SetBasicAuth(rc.creds.Username, rc.creds.Password)
	}
}

// handleAuthChallenge inspects a 401 response, performs the token exchange if
// the registry asked for Bearer, caches the token under the repoName scope,
// and returns true if the caller should retry. The caller must drain/close
// resp.Body before the retry.
func (rc *registryClient) handleAuthChallenge(resp *http.Response, repoName, scopePath string) (bool, error) {
	if resp.StatusCode != http.StatusUnauthorized {
		return false, nil
	}
	wwwAuth := resp.Header.Get("WWW-Authenticate")
	if wwwAuth == "" {
		return false, nil
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(wwwAuth)), "bearer") {
		return false, nil // Basic-only realms are already covered by authorize()
	}
	token, err := rc.fetchBearerToken(wwwAuth, scopePath)
	if err != nil {
		return false, err
	}
	rc.bearers[repoName] = token
	return true, nil
}

var (
	realmRegex   = regexp.MustCompile(`realm="([^"]+)"`)
	serviceRegex = regexp.MustCompile(`service="([^"]+)"`)
	scopeRegex   = regexp.MustCompile(`scope="([^"]+)"`)
)

// fetchBearerToken exchanges credentials for a bearer token at the realm URL
// advertised by the registry. ECR-style endpoints require Basic auth on this
// call; public realms (Docker Hub) accept anonymous and return a pull token.
func (rc *registryClient) fetchBearerToken(wwwAuth, scopePath string) (string, error) {
	m := realmRegex.FindStringSubmatch(wwwAuth)
	if len(m) < 2 {
		return "", fmt.Errorf("no realm in WWW-Authenticate: %q", wwwAuth)
	}
	realm := m[1]

	service := ""
	if m := serviceRegex.FindStringSubmatch(wwwAuth); len(m) > 1 {
		service = m[1]
	}
	scope := ""
	if m := scopeRegex.FindStringSubmatch(wwwAuth); len(m) > 1 {
		scope = m[1]
	} else {
		// Scope inferred from the request path: /v2/<repo>/blobs/... -> repository:<repo>:push,pull
		parts := strings.Split(strings.TrimPrefix(scopePath, "/v2/"), "/")
		if len(parts) >= 2 {
			repo := strings.Join(parts[:len(parts)-2], "/")
			if repo == "" {
				repo = parts[0]
			}
			scope = fmt.Sprintf("repository:%s:push,pull", repo)
		}
	}

	tokenURL := realm
	q := url.Values{}
	if service != "" {
		q.Set("service", service)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	if enc := q.Encode(); enc != "" {
		sep := "?"
		if strings.Contains(tokenURL, "?") {
			sep = "&"
		}
		tokenURL += sep + enc
	}

	req, err := http.NewRequest("GET", tokenURL, nil)
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	if rc.creds.Username != "" {
		req.SetBasicAuth(rc.creds.Username, rc.creds.Password)
	}

	resp, err := rc.do(req)
	if err != nil {
		return "", fmt.Errorf("fetch token: %w", err)
	}
	defer util.DeferClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if tr.Token != "" {
		return tr.Token, nil
	}
	if tr.AccessToken != "" {
		return tr.AccessToken, nil
	}
	return "", fmt.Errorf("token response had no token field")
}

// blobExists returns true if the registry already has the blob, allowing the
// caller to skip uploading.
func (rc *registryClient) blobExists(repoName, digest string) (bool, error) {
	blobURL := fmt.Sprintf("%s/v2/%s/blobs/%s", rc.baseURL, repoName, digest)
	scopePath := fmt.Sprintf("/v2/%s/blobs/%s", repoName, digest)
	for range 2 {
		req, err := http.NewRequest("HEAD", blobURL, nil)
		if err != nil {
			return false, err
		}
		rc.authorize(req, repoName)
		resp, err := rc.do(req)
		if err != nil {
			return false, err
		}
		retry, err := rc.handleAuthChallenge(resp, repoName, scopePath)
		util.Logerr(resp.Body.Close())
		if err != nil {
			return false, err
		}
		if retry {
			continue
		}
		return resp.StatusCode == http.StatusOK, nil
	}
	return false, nil
}

// chunkedBlobThreshold is the size at or above which pushBlob switches from
// monolithic (POST + single PUT) to chunked (POST + PATCH loop + PUT) upload.
//
// The monolithic path is materially faster for everything that fits: one PUT
// streams the whole body over one TCP connection, the kernel autotunes the
// congestion window once, and the rest is steady-state. Chunked PATCHes pay
// a TCP slow-start-after-idle penalty between chunks (each PATCH waits on a
// registry response, the connection idles ~hundreds of ms, cwnd resets,
// chunk N+1 ramps back up — that's the sawtooth we observed).
//
// 3 GiB sits comfortably under ECR's monolithic ceiling (S3 single-PUT caps
// at 5 GiB) while still triggering the chunked path for the genuinely huge
// model-bundle layers where ECR's S3 finalize delay would otherwise blow
// past our http.Client.Timeout on monolithic PUT.
const chunkedBlobThreshold = 3 * 1024 * 1024 * 1024 // 3 GiB

// chunkedBlobChunkSize is the PATCH body size during chunked upload.
//
// 512 MiB. Chunked upload is strictly sequential per OCI spec — each PATCH
// must wait for the previous response to learn the next Location — so the
// throughput floor per chunk is (chunk bytes) / (PATCH RTT + server time)
// AND the TCP cwnd resets after each PATCH because the connection idles
// while the registry processes. Bigger chunks both cut per-chunk overhead
// AND give cwnd more time to ramp inside each chunk before slow-start
// kicks back in. 512 MiB is well under any production registry's PATCH
// ceiling (S3 multipart parts allow up to 5 GiB).
//
// Override with ROAR_CHUNK_SIZE_MIB if a target registry rejects 512 MiB
// PATCHes (rare; some self-hosted registries have body-size caps).
//
// MEMORY: a chunked upload holds ~2 chunks live (the one being PATCHed, held by
// bytes.NewReader, plus the next the reader goroutine prefills via the
// buffered(1) channel), so peak resident is ~ workers × 2 × effectiveChunkSize —
// ~1 GiB per worker, ~8 GiB at 8 workers, at the 512 MiB default. runWorkers
// front-loads the largest blobs (LPT) and chunked only triggers at >=
// chunkedBlobThreshold, so several concurrent chunked uploads is the steady
// state for a bundle with big layers. On a memory-constrained CI runner, lower
// ROAR_CHUNK_SIZE_MIB or --workers.
const chunkedBlobChunkSize = 512 * 1024 * 1024 // 512 MiB

// effectiveChunkSize honors ROAR_CHUNK_SIZE_MIB (positive integer, MiB). Any
// parse failure or non-positive value falls back to the default — invalid
// overrides shouldn't tank the push.
func effectiveChunkSize() int {
	if v := os.Getenv("ROAR_CHUNK_SIZE_MIB"); v != "" {
		var mib int
		if _, err := fmt.Sscanf(v, "%d", &mib); err == nil && mib > 0 {
			return mib * 1024 * 1024
		}
	}
	return chunkedBlobChunkSize
}

// pushBlob streams `size` bytes from data into the registry as `digest`.
//
// Two paths:
//   - Monolithic (size < chunkedBlobThreshold): POST init then single PUT
//     with ?digest= and full Content-Length. One round-trip after init.
//     Cheap for the common case (most layers are < 100 MiB).
//   - Chunked (size >= chunkedBlobThreshold): POST init, loop PATCH
//     effectiveChunkSize chunks (chunkedBlobChunkSize = 512 MiB default,
//     configurable via ROAR_CHUNK_SIZE_MIB) with Content-Range, then PUT
//     ?digest= with empty body. Each request is bounded in time, so multi-GB
//     blobs survive http.Client deadlines and registries that finalize async.
//
// Both paths set Content-Length so registries that reject Transfer-Encoding:
// chunked on the wire (ECR, GHCR) accept the request. "Chunked" here refers
// to OCI spec chunked blob upload (multiple PATCH calls), NOT HTTP/1.1
// chunked transfer-encoding.
func (rc *registryClient) pushBlob(cl *http.Client, repoName, digest string, size int64, data io.Reader) error {
	location, err := rc.initBlobUpload(cl, repoName)
	if err != nil {
		return err
	}

	// Escape hatch for registries that don't support OCI chunked blob upload
	// (PATCH /v2/.../blobs/uploads/<session>). Modern registries all do (ECR,
	// Harbor, JFrog, GHCR, Quay, distribution/distribution); but some
	// minimal / proprietary registries are PUT-only. If the operator hits
	// that and sees the chunked path fail with 405/501/413, they can re-run
	// with ROAR_FORCE_MONOLITHIC=1 to force the single-PUT path for all
	// blob sizes. We do not retry-and-fallback transparently because the
	// upload reader is streamed once from the tarball + sha256-verified in
	// place, so we'd have to re-open the blob from the push-pool layer;
	// kept that as a follow-up. The env-var hatch is cheap and explicit.
	if size < chunkedBlobThreshold || os.Getenv("ROAR_FORCE_MONOLITHIC") == "1" {
		return rc.finalizeBlobMonolithic(cl, repoName, location, digest, size, data)
	}
	return rc.pushBlobChunked(cl, repoName, location, digest, size, data)
}

// initBlobUpload performs the POST /v2/<name>/blobs/uploads/ that starts an
// upload session and returns the registry-supplied Location for the next step.
// Loops once on a bearer-token auth challenge.
func (rc *registryClient) initBlobUpload(cl *http.Client, repoName string) (string, error) {
	uploadURL := fmt.Sprintf("%s/v2/%s/blobs/uploads/", rc.baseURL, repoName)
	scopePath := fmt.Sprintf("/v2/%s/blobs/uploads/", repoName)

	for range 2 {
		req, err := http.NewRequest("POST", uploadURL, nil)
		if err != nil {
			return "", fmt.Errorf("build upload request: %w", err)
		}
		rc.authorize(req, repoName)
		resp, err := cl.Do(req)
		if err != nil {
			return "", fmt.Errorf("initiate upload: %w", err)
		}
		retry, err := rc.handleAuthChallenge(resp, repoName, scopePath)
		if err != nil {
			util.Logerr(resp.Body.Close())
			return "", err
		}
		if retry {
			util.Logerr(resp.Body.Close())
			continue
		}
		if resp.StatusCode != http.StatusAccepted {
			body, _ := io.ReadAll(resp.Body)
			util.Logerr(resp.Body.Close())
			return "", fmt.Errorf("upload init: status %d: %s", resp.StatusCode, string(body))
		}
		location := resp.Header.Get("Location")
		util.Logerr(resp.Body.Close())
		if location == "" {
			return "", fmt.Errorf("upload init: no Location header returned")
		}
		return location, nil
	}
	return "", fmt.Errorf("upload init: auth challenge retry exhausted")
}

// finalizeBlobMonolithic uploads the whole blob in a single PUT with the
// digest as a query parameter. The fast path for small blobs.
func (rc *registryClient) finalizeBlobMonolithic(cl *http.Client, repoName, location, digest string, size int64, data io.Reader) error {
	finalizeURL, err := buildFinalizeURL(rc.baseURL, location, digest)
	if err != nil {
		return fmt.Errorf("resolve finalize URL: %w", err)
	}
	req, err := http.NewRequest("PUT", finalizeURL, data)
	if err != nil {
		return fmt.Errorf("build finalize request: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	rc.authorize(req, repoName)

	resp, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("finalize upload: %w", err)
	}
	defer util.DeferClose(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("finalize: status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// pushBlobChunked uploads the blob via the OCI chunked-upload protocol:
// repeated PATCH calls with Content-Range, then an empty-body PUT to commit
// the digest. The registry returns a new Location header on each PATCH; the
// final PUT goes to whichever Location was returned by the last PATCH.
//
// Per spec, Content-Range uses inclusive byte offsets: "bytes <start>-<end>".
// No total-size suffix, no slash. Each PATCH is its own bounded HTTP request,
// which is the whole point — neither the upload body nor the registry's
// finalize step can stall a single request long enough to hit Client.Timeout.
//
// PATCHes themselves can't be parallelized (OCI sequences chunks per upload
// session and ECR's Location embeds S3 multipart-part state), but the reader
// runs in a background goroutine so chunk N+1 is already in memory when
// PATCH N returns. That hides disk + sha256 latency behind the PATCH RTT.
func (rc *registryClient) pushBlobChunked(cl *http.Client, repoName, location, digest string, size int64, data io.Reader) error {
	type readChunk struct {
		buf    []byte
		offset int64
		err    error
	}
	chunkSize := effectiveChunkSize()

	// Buffered by 1 so the reader can stay one chunk ahead of the PATCH loop:
	// reader fills [N+1] while we're transmitting [N]. Increasing this further
	// just consumes RAM (chunk-sized buffers each) without any throughput win
	// because PATCHes remain strictly sequential on the wire.
	chunks := make(chan readChunk, 1)
	// done lets the reader goroutine unblock if the consumer loop returns early
	// (PATCH error, bad status, build error). Without it, the reader blocks
	// forever on a send into the buffered(1) channel that nobody drains —
	// leaking the goroutine AND its chunkSize buffer (512 MiB by default) for
	// the rest of the process lifetime. close(done) on every return path below
	// releases it; the select makes each send abandonable.
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer close(chunks)
		var off int64
		for off < size {
			buf := make([]byte, chunkSize)
			n, err := io.ReadFull(data, buf)
			if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
				select {
				case chunks <- readChunk{err: fmt.Errorf("read blob at offset %d: %w", off, err)}:
				case <-done:
				}
				return
			}
			if n == 0 {
				select {
				case chunks <- readChunk{err: fmt.Errorf("blob truncated: expected %d bytes, got %d", size, off)}:
				case <-done:
				}
				return
			}
			select {
			case chunks <- readChunk{buf: buf[:n], offset: off}:
			case <-done:
				return
			}
			off += int64(n)
		}
	}()

	var offset int64
	for c := range chunks {
		if c.err != nil {
			return c.err
		}
		n := len(c.buf)
		end := c.offset + int64(n) - 1

		patchURL, err := resolveLocation(rc.baseURL, location)
		if err != nil {
			return fmt.Errorf("resolve PATCH URL at offset %d: %w", c.offset, err)
		}
		// bytes.NewReader (not strings.NewReader(string(...))) avoids an
		// extra full-chunk byte copy per PATCH — at 100 MiB that's 100 MiB
		// of unnecessary allocation and copy per chunk.
		req, err := http.NewRequest("PATCH", patchURL, bytes.NewReader(c.buf))
		if err != nil {
			return fmt.Errorf("build chunk PATCH at offset %d: %w", c.offset, err)
		}
		req.ContentLength = int64(n)
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Range", fmt.Sprintf("%d-%d", c.offset, end))
		rc.authorize(req, repoName)

		resp, err := cl.Do(req)
		if err != nil {
			return fmt.Errorf("PATCH chunk at offset %d: %w", c.offset, err)
		}
		// OCI spec is 202 Accepted, but registries vary in practice:
		//   - distribution/distribution, GHCR, Quay: 202.
		//   - ECR: 201 Created (treats each PATCH like a finalize ack).
		//   - A few others have been observed to return 204.
		// All three mean "chunk stored, continue with the next Location" — so
		// accept the 2xx range. Anything outside it is a real failure.
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			util.Logerr(resp.Body.Close())
			return fmt.Errorf("PATCH chunk at offset %d: status %d: %s", c.offset, resp.StatusCode, string(body))
		}
		// Registry MAY return a new Location; use it if present, otherwise reuse.
		if next := resp.Header.Get("Location"); next != "" {
			location = next
		}
		util.Logerr(resp.Body.Close())
		offset = c.offset + int64(n)
	}
	if offset != size {
		return fmt.Errorf("blob truncated: expected %d bytes, sent %d", size, offset)
	}

	// Empty-body PUT to commit. Content-Length 0 so the transport doesn't try
	// to chunked-transfer-encode (ECR rejects that). After this returns 201,
	// the blob is durably stored at the named digest.
	finalizeURL, err := buildFinalizeURL(rc.baseURL, location, digest)
	if err != nil {
		return fmt.Errorf("resolve chunked finalize URL: %w", err)
	}
	req, err := http.NewRequest("PUT", finalizeURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("build chunked finalize request: %w", err)
	}
	req.ContentLength = 0
	req.Header.Set("Content-Type", "application/octet-stream")
	rc.authorize(req, repoName)

	resp, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("chunked finalize: %w", err)
	}
	defer util.DeferClose(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("chunked finalize: status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// resolveLocation turns a registry-supplied Location header into an absolute
// URL to send the next authenticated request to, ENFORCING that it stays on
// the same origin as the registry we authenticated against.
//
// Security: roar re-attaches the bearer/Basic credential (rc.authorize) on the
// follow-up PATCH/PUT. A path-only Location (the normal ECR/distribution case)
// resolves against rc.baseURL and is always safe. An ABSOLUTE Location is only
// honored when it shares the registry's scheme AND host; otherwise a malicious
// or MITM registry could redirect the credentialed request to an attacker host
// and harvest the ECR/GHCR token. A cross-origin absolute Location is rejected
// rather than silently followed (or followed without creds, which would just
// fail confusingly downstream).
func resolveLocation(baseURL, location string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse registry base URL: %w", err)
	}
	loc, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("parse Location header %q: %w", location, err)
	}
	// Resolve via the stdlib for EVERY input — never string-concatenate. A
	// path-only Location resolves against the base; resolving it properly is what
	// defeats userinfo/host-confusion vectors ("@evil.com/...", ".evil.com/...",
	// ":@evil.com/...") that a string-prefix branch would wave straight through.
	resolved := base.ResolveReference(loc)
	if !sameOrigin(resolved, base) {
		return "", fmt.Errorf("registry redirected upload to a different origin (%s://%s vs %s://%s); refusing to send credentials cross-origin",
			resolved.Scheme, resolved.Host, base.Scheme, base.Host)
	}
	return resolved.String(), nil
}

// refuseCrossOriginRedirect is the CheckRedirect policy for the credentialed
// pool clients. Go's default policy auto-follows up to 10 redirects and re-sends
// the Authorization header on same-host (including an https->http downgrade) and
// subdomain targets — so a malicious/MITM/compromised registry could answer a
// credentialed request with a 3xx and harvest the bearer/Basic credential. We
// follow only same-origin redirects; a cross-origin (or scheme-downgrade) hop is
// refused. roar handles upload Locations explicitly (resolveLocation) and pushes
// both bare and library keys, so it never relies on cross-origin auto-follow.
func refuseCrossOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if len(via) > 0 && !sameOrigin(req.URL, via[0].URL) {
		return fmt.Errorf("refusing to follow cross-origin redirect to %s://%s (credential-leak guard)",
			req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// sameOrigin reports whether a and b share scheme + host, normalizing host case
// and default ports so "ecr.io:443" == "ecr.io" and "Reg.Example.com" ==
// "reg.example.com" (avoids over-strict same-origin rejection). An https->http
// downgrade is a *different* origin (rejected) — that's the cleartext-leak case.
func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && normalizedHostPort(a) == normalizedHostPort(b)
}

func normalizedHostPort(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" ||
		(u.Scheme == "https" && port == "443") ||
		(u.Scheme == "http" && port == "80") {
		return host
	}
	return host + ":" + port
}

// buildFinalizeURL appends the digest query param to the upload Location URL,
// resolving + same-origin-validating it via resolveLocation first (see the
// security note there).
func buildFinalizeURL(baseURL, location, digest string) (string, error) {
	full, err := resolveLocation(baseURL, location)
	if err != nil {
		return "", err
	}
	sep := "?"
	if strings.Contains(full, "?") {
		sep = "&"
	}
	return full + sep + "digest=" + url.QueryEscape(digest), nil
}

// manifestDigest looks up the digest currently stored under `reference` in the
// registry. Returns ("", nil) if the manifest doesn't exist. Used to short-
// circuit re-pushes when a tag-immutable registry (ECR with IMMUTABLE policy,
// GHCR with retention) would reject an identical re-upload.
func (rc *registryClient) manifestDigest(repoName, reference string) (string, error) {
	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", rc.baseURL, repoName, reference)
	scopePath := fmt.Sprintf("/v2/%s/manifests/%s", repoName, reference)

	for range 2 {
		req, err := http.NewRequest("HEAD", manifestURL, nil)
		if err != nil {
			return "", err
		}
		// Accept any manifest media type — we only care about the digest header.
		req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, "+
			"application/vnd.oci.image.index.v1+json, "+
			"application/vnd.docker.distribution.manifest.v2+json, "+
			"application/vnd.docker.distribution.manifest.list.v2+json, "+
			"*/*")
		rc.authorize(req, repoName)
		resp, err := rc.do(req)
		if err != nil {
			return "", err
		}
		retry, err := rc.handleAuthChallenge(resp, repoName, scopePath)
		util.Logerr(resp.Body.Close())
		if err != nil {
			return "", err
		}
		if retry {
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			return "", nil
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("manifest HEAD: status %d", resp.StatusCode)
		}
		return resp.Header.Get("Docker-Content-Digest"), nil
	}
	return "", nil
}

// tryMount attempts an OCI cross-repository blob mount: a hint to the
// registry that the blob already lives in srcRepo, and we'd like dstRepo
// to point at the same storage rather than re-uploading the bytes.
//
// Return shape:
//
//	(true, nil)  — registry mounted (201 Created); blob is now in dstRepo.
//	(false, nil) — registry declined to mount (202 Accepted with a Location
//	               header for a regular upload); caller should fall back to
//	               a normal upload.
//	(false, err) — actual error; caller decides whether to bail or fall back.
//
// ECR/GCR/Distribution v2 all support mount with this exact shape; registries
// that don't support mount typically respond 202 (treat the request as a
// regular upload init) so the (false, nil) branch handles them transparently.
func (rc *registryClient) tryMount(dstRepo, srcRepo, digest string) (bool, error) {
	u := fmt.Sprintf("%s/v2/%s/blobs/uploads/?mount=%s&from=%s",
		rc.baseURL, dstRepo, url.QueryEscape(digest), url.QueryEscape(srcRepo))
	scopePath := fmt.Sprintf("/v2/%s/blobs/uploads/", dstRepo)

	for range 2 {
		req, err := http.NewRequest("POST", u, nil)
		if err != nil {
			return false, err
		}
		rc.authorize(req, dstRepo)
		resp, err := rc.do(req)
		if err != nil {
			return false, err
		}
		retry, err := rc.handleAuthChallenge(resp, dstRepo, scopePath)
		if err != nil {
			util.Logerr(resp.Body.Close())
			return false, err
		}
		if retry {
			util.Logerr(resp.Body.Close())
			continue
		}
		status := resp.StatusCode
		body, _ := io.ReadAll(resp.Body)
		util.Logerr(resp.Body.Close())
		switch status {
		case http.StatusCreated:
			return true, nil
		case http.StatusAccepted:
			// Mount declined; the response includes a Location header for a
			// regular upload, but we just bail and let the caller do its own
			// fresh upload init. Wasted POST is one round-trip.
			return false, nil
		default:
			return false, fmt.Errorf("mount: status %d: %s", status, string(body))
		}
	}
	return false, nil
}

// fetchManifest GETs the raw manifest bytes for repoName:reference. Used by
// the conflict path to compare logical content of an existing tag against
// ours when the byte-level digests differ.
func (rc *registryClient) fetchManifest(repoName, reference string) ([]byte, error) {
	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", rc.baseURL, repoName, reference)
	scopePath := fmt.Sprintf("/v2/%s/manifests/%s", repoName, reference)

	for range 2 {
		req, err := http.NewRequest("GET", manifestURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, "+
			"application/vnd.oci.image.index.v1+json, "+
			"application/vnd.docker.distribution.manifest.v2+json, "+
			"application/vnd.docker.distribution.manifest.list.v2+json, "+
			"*/*")
		rc.authorize(req, repoName)
		resp, err := rc.do(req)
		if err != nil {
			return nil, err
		}
		retry, err := rc.handleAuthChallenge(resp, repoName, scopePath)
		if err != nil {
			util.Logerr(resp.Body.Close())
			return nil, err
		}
		if retry {
			util.Logerr(resp.Body.Close())
			continue
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			util.Logerr(resp.Body.Close())
			return nil, fmt.Errorf("manifest GET: status %d: %s", resp.StatusCode, string(body))
		}
		body, err := io.ReadAll(resp.Body)
		util.Logerr(resp.Body.Close())
		return body, err
	}
	return nil, fmt.Errorf("manifest GET: exhausted auth retries")
}

// pushManifest uploads a manifest by tag (or digest reference). The manifest
// JSON is small (KBs) so we send it as a byte slice rather than streaming.
func (rc *registryClient) pushManifest(repoName, reference, mediaType string, manifestData []byte) error {
	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", rc.baseURL, repoName, reference)
	scopePath := fmt.Sprintf("/v2/%s/manifests/%s", repoName, reference)

	for range 2 {
		req, err := http.NewRequest("PUT", manifestURL, strings.NewReader(string(manifestData)))
		if err != nil {
			return fmt.Errorf("build manifest request: %w", err)
		}
		req.ContentLength = int64(len(manifestData))
		req.Header.Set("Content-Type", mediaType)
		rc.authorize(req, repoName)

		resp, err := rc.do(req)
		if err != nil {
			return fmt.Errorf("push manifest: %w", err)
		}
		retry, err := rc.handleAuthChallenge(resp, repoName, scopePath)
		if err != nil {
			util.Logerr(resp.Body.Close())
			return err
		}
		if retry {
			util.Logerr(resp.Body.Close())
			continue
		}
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			util.Logerr(resp.Body.Close())
			return fmt.Errorf("manifest push: status %d: %s", resp.StatusCode, string(body))
		}
		util.Logerr(resp.Body.Close())
		return nil
	}
	return fmt.Errorf("manifest push: exhausted auth retries")
}
