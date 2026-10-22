package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var cliBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "archifau-cli-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cliBinary = filepath.Join(dir, "archifau")
	if runtime.GOOS == "windows" {
		cliBinary += ".exe"
	}
	build := exec.Command("go", "build", "-o", cliBinary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build archifau: %v\n%s", err, output)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func cliEnvironment(config map[string]string) []string {
	var result []string
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(name, "ARCHIFAU_") || strings.HasPrefix(name, "GITHUB_") ||
			name == "ALLOWED_OWNERS" || name == "EXPECTED_AUD" ||
			name == "CLOCK_SKEW_SECS" || name == "TOKEN_TTL_SECS" {
			continue
		}
		result = append(result, value)
	}
	for name, value := range config {
		result = append(result, name+"="+value)
	}
	return result
}

func TestServerRequiresExplicitHostname(t *testing.T) {
	// Bug caught: silently selecting a company endpoint. Restoring a hostname fallback makes the missing-input diagnostic fail.
	certPath, keyPath, _ := testCertificate(t, "registry.example.test")
	for _, hostname := range []string{"", " \t"} {
		t.Run(fmt.Sprintf("hostname_%q", hostname), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, cliBinary)
			cmd.Env = cliEnvironment(map[string]string{
				"ARCHIFAU_HOSTNAME": hostname,
				"ARCHIFAU_LISTEN":   "127.0.0.1:0",
				"ARCHIFAU_DATA_DIR": t.TempDir(),
				"ARCHIFAU_TLS_CERT": certPath,
				"ARCHIFAU_TLS_KEY":  keyPath,
				"ALLOWED_OWNERS":    "example-owner",
			})

			output, err := cmd.CombinedOutput()

			if err == nil || ctx.Err() != nil {
				t.Fatalf("missing hostname must fail promptly: err=%v, context=%v", err, ctx.Err())
			}
			if !strings.Contains(string(output), "ARCHIFAU_HOSTNAME") {
				t.Fatalf("diagnostic must identify the required deployment input: %s", output)
			}
		})
	}
}

func TestServerRequiresAuthorizationOwners(t *testing.T) {
	// Bug caught: admitting startup without an authorization boundary. Removing the required-owner guard changes the diagnostic.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cliBinary)
	cmd.Env = cliEnvironment(map[string]string{
		"ARCHIFAU_HOSTNAME": "registry.example.test:9443",
		"ARCHIFAU_DATA_DIR": t.TempDir(),
	})

	output, err := cmd.CombinedOutput()

	if err == nil || ctx.Err() != nil || !strings.Contains(string(output), "ALLOWED_OWNERS") {
		t.Fatalf("startup must identify missing authorization owners: err=%v, output=%s", err, output)
	}
}

func testCA(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Endpoint Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func testCertificate(t *testing.T, hostname string) (string, string, string) {
	t.Helper()
	caPath, caKeyPath := testCA(t)
	prefix := filepath.Join(t.TempDir(), "server")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cliBinary, "gencert", "-ca-cert", caPath, "-ca-key", caKeyPath,
		"-hostname", hostname, "-out-prefix", prefix)
	cmd.Env = cliEnvironment(nil)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("creating server certificate: %v, %s", err, output)
	}
	return prefix + "-cert.pem", prefix + "-key.pem", caPath
}

func TestCertificateRequiresExplicitHostname(t *testing.T) {
	// Bug caught: issuing for an implicit domain. Removing the required-hostname check creates output without a chosen identity.
	certPath, keyPath := testCA(t)
	for _, hostname := range []string{"", " \t"} {
		t.Run(fmt.Sprintf("hostname_%q", hostname), func(t *testing.T) {
			prefix := filepath.Join(t.TempDir(), "server")
			args := []string{"gencert", "-ca-cert", certPath, "-ca-key", keyPath, "-out-prefix", prefix}
			if hostname != "" {
				args = append(args, "-hostname", hostname)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, cliBinary, args...)
			cmd.Env = cliEnvironment(nil)

			output, err := cmd.CombinedOutput()

			if err == nil || ctx.Err() != nil || !strings.Contains(string(output), "-hostname") {
				t.Fatalf("certificate generation must reject a missing identity: err=%v, output=%s", err, output)
			}
			for _, suffix := range []string{"-cert.pem", "-key.pem"} {
				if _, err := os.Stat(prefix + suffix); !os.IsNotExist(err) {
					t.Fatalf("missing identity must not create %s: %v", suffix, err)
				}
			}
		})
	}
}

func TestCertificateMatchesConfiguredHostname(t *testing.T) {
	// Bug caught: ignoring the chosen certificate identity. Replacing the flag value with a fixed hostname fails independent x509 validation.
	certPath, _, _ := testCertificate(t, "registry.example.test")
	data, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("certificate output is not PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := certificate.VerifyHostname("registry.example.test"); err != nil {
		t.Fatalf("certificate must authenticate the caller's chosen DNS name: %v", err)
	}
}

func TestConfiguredEndpointControlsChallengeAndTokenAudience(t *testing.T) {
	// Bug caught: retaining an unrelated registry identity. Hardcoding the realm or token service fails this CLI-level contract.
	certPath, keyPath, caPath := testCertificate(t, "registry.example.test")
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("test CA is invalid")
	}
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"sample-user"}`))
		case "/user/orgs":
			_, _ = w.Write([]byte(`[{"login":"example-owner"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(github.Close)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("s", 32)
	cmd := exec.CommandContext(t.Context(), cliBinary)
	cmd.Env = cliEnvironment(map[string]string{
		"ARCHIFAU_HOSTNAME":     "registry.example.test:9443",
		"ARCHIFAU_LISTEN":       address,
		"ARCHIFAU_DATA_DIR":     t.TempDir(),
		"ARCHIFAU_TLS_CERT":     certPath,
		"ARCHIFAU_TLS_KEY":      keyPath,
		"ARCHIFAU_TOKEN_SECRET": secret,
		"ALLOWED_OWNERS":        "example-owner",
		"GITHUB_API":            github.URL,
	})
	var logs bytes.Buffer
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
		}
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: roots, ServerName: "registry.example.test", MinVersion: tls.VersionTLS12,
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Timeout: time.Second, Transport: transport}
	base := "https://" + address
	deadline := time.Now().Add(5 * time.Second)
	var challenge string
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("server exited before readiness: %v, %s", waitErr, logs.String())
		default:
		}
		response, err := client.Get(base + "/v2/")
		if err == nil {
			challenge = response.Header.Get("WWW-Authenticate")
			_ = response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unauthenticated registry request must return 401, got %d", response.StatusCode)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(challenge, `realm="https://registry.example.test:9443/token"`) ||
		!strings.Contains(challenge, `service="registry.example.test:9443"`) {
		t.Fatalf("challenge must advertise the configured endpoint: %q", challenge)
	}
	request, err := http.NewRequest(http.MethodGet, base+"/token", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth("user", "synthetic-test-token")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("configured owner must receive a token, got %d", response.StatusCode)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if _, err := jwt.Parse(body.Token, func(_ *jwt.Token) (any, error) { return []byte(secret), nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience("registry.example.test:9443")); err != nil {
		t.Fatalf("issued token must validate for the configured registry service: %v", err)
	}
}
