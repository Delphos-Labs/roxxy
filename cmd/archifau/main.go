// archifau is the stripped-down origin-server build of roxxy: a docker
// registry, a PyPI registry, and a bare file store behind GitHub-derived
// authentication. No proxying, no TLS interception — one fixed certificate,
// one listener.
//
// Only the packages this binary needs are imported, so the proxy/UI/SBOM
// machinery in the rest of the repository never builds into it.
//
// Configuration comes from the environment (config, not code):
//
//	ARCHIFAU_LISTEN       listen address                 (default ":8443")
//	ARCHIFAU_HOSTNAME     registry host, with port when not 443 (required)
//	ARCHIFAU_DATA_DIR     storage root                   (default "./archifau-data")
//	ARCHIFAU_TLS_CERT     server certificate PEM         (required unless ARCHIFAU_INSECURE_HTTP=1)
//	ARCHIFAU_TLS_KEY      server key PEM                 (required unless ARCHIFAU_INSECURE_HTTP=1)
//	ARCHIFAU_TOKEN_SECRET token-signing secret           (default: random per process;
//	                      MUST be set to one shared value across all live replicas,
//	                      else each rejects the others' minted tokens. A random
//	                      per-process secret only suffices for a single instance —
//	                      and a restart then forces clients to re-exchange at /token.)
//	EXPECTED_AUD          OIDC audience to require       (default: ARCHIFAU_HOSTNAME)
//	ALLOWED_OWNERS        comma-separated GitHub orgs/users allowed (required)
//	CLOCK_SKEW_SECS       OIDC exp leeway; also narrows go-oidc's fixed 5m nbf
//	                      leeway, so a larger value is stricter about not-yet-valid
//	                      tokens. Must be < 300 (startup fails otherwise). (default 60)
//	TOKEN_TTL_SECS        minted token lifetime          (default 300)
//	ARCHIFAU_LOG_LEVEL    "debug" enables debug logs     (default info; all output is JSON on stderr)
//	ARCHIFAU_S3_BUCKET    S3 bucket for the durable second level (unset => disabled)
//	ARCHIFAU_S3_PREFIX    key prefix within that bucket  (default "")
//	                      credentials/region come from the standard AWS_* chain
//	ARCHIFAU_MAX_UPLOAD_BYTES per-upload body cap, bytes (default 0 = unlimited; >0 => 413 on oversize)
//	ARCHIFAU_READ_TIMEOUT_SECS  whole-request read timeout (default 0 = off; large blobs need it off)
//	ARCHIFAU_WRITE_TIMEOUT_SECS response write timeout      (default 0 = off; large blobs need it off)
//	GITHUB_OIDC_ISSUER    OIDC issuer override           (tests only)
//	GITHUB_API            GitHub API base override       (tests only)
//	ARCHIFAU_S3_ENDPOINT  custom S3 endpoint             (tests only)
//
// Subcommand:
//
//	archifau gencert -ca-cert CA.pem -ca-key CA-key.pem -hostname H [-out-prefix P]
//	    signs a temporary server certificate for H with the given CA.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/archifau/auth"
	"github.com/Delphos-Labs/roxxy/internal/archifau/s3cas"
	"github.com/Delphos-Labs/roxxy/internal/archifau/server"
	"github.com/Delphos-Labs/roxxy/internal/registry"
	roxxytls "github.com/Delphos-Labs/roxxy/internal/tls"
)

func main() {
	setupLogging()
	if len(os.Args) > 1 && os.Args[1] == "gencert" {
		if err := gencert(os.Args[2:]); err != nil {
			log.Fatalf("gencert: %v", err)
		}
		return
	}
	if err := serve(); err != nil {
		log.Fatal(err)
	}
}

// setupLogging makes the whole stderr stream JSON (12-factor): slog with a
// JSON handler is the default logger, and the stdlib log package — used by
// the shared packages this binary reuses (internal/registry et al.) — is
// bridged into the same handler, tagged source=stdlog.
func setupLogging() {
	level := slog.LevelInfo
	if env("ARCHIFAU_LOG_LEVEL", "") == "debug" {
		level = slog.LevelDebug
	}
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))

	log.SetFlags(0) // slog adds its own timestamp
	log.SetOutput(slog.NewLogLogger(
		handler.WithAttrs([]slog.Attr{slog.String("source", "stdlog")}),
		slog.LevelInfo,
	).Writer())
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envSeconds(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0, fmt.Errorf("%s: invalid seconds value %q", key, v)
	}
	return time.Duration(secs) * time.Second, nil
}

// envOptSeconds parses an optional, default-off duration: unset or "0" means
// disabled (zero duration). A negative or non-numeric value is an error.
func envOptSeconds(key string) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return 0, nil
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0, fmt.Errorf("%s: invalid seconds value %q", key, v)
	}
	return time.Duration(secs) * time.Second, nil
}

// envOptBytes parses an optional, default-off byte count: unset or "0" means
// unlimited. A negative or non-numeric value is an error.
func envOptBytes(key string) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: invalid byte count %q", key, v)
	}
	return n, nil
}

func serve() error {
	hostname := strings.TrimSpace(env("ARCHIFAU_HOSTNAME", ""))
	if hostname == "" {
		return fmt.Errorf("ARCHIFAU_HOSTNAME must be set (registry host, with port when not 443)")
	}
	listen := env("ARCHIFAU_LISTEN", ":8443")
	dataDir := env("ARCHIFAU_DATA_DIR", "./archifau-data")

	owners := strings.Split(strings.ToLower(env("ALLOWED_OWNERS", "")), ",")
	for i := range owners {
		owners[i] = strings.TrimSpace(owners[i])
	}
	if len(owners) == 1 && owners[0] == "" {
		return fmt.Errorf("ALLOWED_OWNERS must be set (comma-separated GitHub orgs/users)")
	}

	skew, err := envSeconds("CLOCK_SKEW_SECS", 60*time.Second)
	if err != nil {
		return err
	}
	// The skew shifts the OIDC verify-clock back, which also subtracts from
	// go-oidc's fixed 5-minute nbf leeway; at/above 5m, legitimate not-yet-valid
	// tokens would be rejected. A few seconds covers real clock drift.
	if skew >= 5*time.Minute {
		return fmt.Errorf("CLOCK_SKEW_SECS must be under 300 (it reduces the OIDC nbf leeway)")
	}
	ttl, err := envSeconds("TOKEN_TTL_SECS", 5*time.Minute)
	if err != nil {
		return err
	}

	secret := []byte(os.Getenv("ARCHIFAU_TOKEN_SECRET"))
	if len(secret) == 0 {
		// Random per-process: minted tokens die with the process, which is
		// fine — they only live minutes and clients re-exchange. But with 2+
		// replicas each gets a DIFFERENT secret, so a client's token minted by
		// one replica fails HMAC verification on another → intermittent 401s
		// that flap with LB routing. Warn so that misconfig is debuggable
		// (mirrors the insecure-HTTP fallback warning below).
		slog.Warn("archifau: ARCHIFAU_TOKEN_SECRET unset — generating a random per-process secret; " +
			"multi-replica deployments MUST set one shared value or clients will see intermittent 401s")
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return fmt.Errorf("generating token secret: %w", err)
		}
	}

	insecureHTTP := os.Getenv("ARCHIFAU_INSECURE_HTTP") == "1"
	scheme := "https"
	if insecureHTTP {
		scheme = "http"
	}

	authn := &auth.Authenticator{
		OIDC: &auth.OIDCVerifier{
			Issuer:        os.Getenv("GITHUB_OIDC_ISSUER"), // empty -> GitHub
			ExpectedAud:   env("EXPECTED_AUD", hostname),
			AllowedOwners: owners,
			ClockSkew:     skew,
		},
		Users: &auth.UserVerifier{
			APIBase:       os.Getenv("GITHUB_API"), // empty -> api.github.com
			AllowedOwners: owners,
		},
		Minter: &auth.Minter{
			Secret:  secret,
			Service: hostname,
			TTL:     ttl,
		},
		Realm:   scheme + "://" + hostname + "/token",
		Service: hostname,
	}

	// Roxxy's docker registry in origin mode: airgapped (never contacts an
	// upstream) with pushes stored locally. The upstream URL is only used to
	// name the on-disk layout.
	dockerRegistry := registry.NewDockerRegistryProxy(
		"https://"+hostname, // upstreamURL: names the storage dir, never dialed
		nil,                 // statsTracker
		[]string{hostname},
		false, // airgapProxy
		true,  // airgapped: local content only
		false, // proxyPushes: store pushes locally
		nil,   // localOnlyPaths
		dataDir,
		nil,   // sbomManager
		false, // debug
	)

	// Optional S3 second level for the PyPI and file surfaces: durable CAS
	// behind the local disk. The bucket itself is terraform-managed.
	s3store, err := s3cas.New(context.Background())
	if err != nil {
		return fmt.Errorf("configuring S3 second level: %w", err)
	}
	var s3iface s3cas.Store
	if s3store != nil {
		s3iface = s3store
		slog.Info("archifau: S3 second level enabled",
			"bucket", s3store.Bucket(), "prefix", s3store.Prefix())
	} else {
		slog.Info("archifau: S3 second level disabled (ARCHIFAU_S3_BUCKET unset)")
	}

	maxUpload, err := envOptBytes("ARCHIFAU_MAX_UPLOAD_BYTES")
	if err != nil {
		return err
	}
	readTimeout, err := envOptSeconds("ARCHIFAU_READ_TIMEOUT_SECS")
	if err != nil {
		return err
	}
	writeTimeout, err := envOptSeconds("ARCHIFAU_WRITE_TIMEOUT_SECS")
	if err != nil {
		return err
	}

	archifau := server.New(server.Config{
		DataDir:        dataDir,
		Auth:           authn,
		Registry:       dockerRegistry,
		S3:             s3iface,
		MaxUploadBytes: maxUpload,
	})

	srv := &http.Server{
		Addr:              listen,
		Handler:           archifau.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// IdleTimeout is the always-on safe default; read/write timeouts stay
		// off by default because large blob transfers need them unbounded.
		IdleTimeout:  120 * time.Second,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
	}

	certFile := os.Getenv("ARCHIFAU_TLS_CERT")
	keyFile := os.Getenv("ARCHIFAU_TLS_KEY")
	if !insecureHTTP {
		if certFile == "" || keyFile == "" {
			return fmt.Errorf("ARCHIFAU_TLS_CERT and ARCHIFAU_TLS_KEY must be set (or ARCHIFAU_INSECURE_HTTP=1 for tests)")
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	return runServer(srv, archifau, insecureHTTP, certFile, keyFile, hostname, listen, dataDir, owners)
}

// shutdownTimeout bounds how long graceful shutdown waits for in-flight
// requests before forcing the listener closed.
const shutdownTimeout = 30 * time.Second

// runServer serves until SIGINT/SIGTERM, then gracefully shuts the listener
// down (letting in-flight requests finish) and drains background replication
// so a deploy never silently abandons durability work.
func runServer(
	srv *http.Server,
	archifau *server.Server,
	insecureHTTP bool,
	certFile, keyFile, hostname, listen, dataDir string,
	owners []string,
) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		if insecureHTTP {
			slog.Warn("archifau: serving INSECURE HTTP — test mode only", "listen", listen, "hostname", hostname)
			serveErr <- srv.ListenAndServe()
			return
		}
		slog.Info("archifau: serving HTTPS",
			"listen", listen, "hostname", hostname, "data_dir", dataDir, "owners", strings.Join(owners, ","))
		serveErr <- srv.ListenAndServeTLS(certFile, keyFile)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		slog.Info("archifau: shutdown signal received, draining")
	}

	// One budget covers both phases (listener drain + replication drain) so a
	// wedged S3 can't push total shutdown past the deadline; align with the
	// orchestrator's terminationGracePeriodSeconds.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("archifau: graceful shutdown timed out", "err", err)
	}
	if err := archifau.DrainContext(shutdownCtx); err != nil {
		// Budget exhausted before replication finished: in-flight jobs were
		// cancelled. The local commit is durable; S3 will reconverge on the next
		// write to each name. Log loudly so the abandoned work is alertable.
		slog.Error("archifau: replication drain cut short by shutdown deadline", "err", err)
	}
	slog.Info("archifau: drained, exiting")
	return nil
}

// gencert signs a server certificate for the selected hostname with the given CA.
func gencert(args []string) error {
	fs := flag.NewFlagSet("gencert", flag.ExitOnError)
	caCert := fs.String("ca-cert", "", "CA certificate PEM (required)")
	caKey := fs.String("ca-key", "", "CA private key PEM (required)")
	hostname := fs.String("hostname", "", "certificate hostname (required)")
	outPrefix := fs.String("out-prefix", "archifau", "output file prefix (<prefix>-cert.pem, <prefix>-key.pem)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *caCert == "" || *caKey == "" {
		return fmt.Errorf("-ca-cert and -ca-key are required")
	}
	*hostname = strings.TrimSpace(*hostname)
	if *hostname == "" {
		return fmt.Errorf("-hostname is required")
	}

	cm := roxxytls.NewCertificateManager(*caCert, *caKey)
	certPEM, keyPEM, err := cm.GenerateCertPEMForHost(*hostname)
	if err != nil {
		return err
	}

	certOut := *outPrefix + "-cert.pem"
	keyOut := *outPrefix + "-key.pem"
	if err := os.WriteFile(certOut, certPEM, 0644); err != nil {
		return err
	}
	if err := os.WriteFile(keyOut, keyPEM, 0600); err != nil {
		return err
	}
	fmt.Printf("wrote %s and %s for %s\n", certOut, keyOut, *hostname)
	return nil
}
