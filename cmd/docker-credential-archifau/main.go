// docker-credential-archifau is a docker credential helper for archifau.
//
// Instead of a one-shot `docker login` (whose stored OIDC token dies in
// minutes), docker invokes this helper on every registry operation, and the
// helper fetches a fresh credential each time — the ECR-style ergonomics
// from the original design:
//
//   - in GitHub Actions, it requests the job's OIDC ID token from
//     ACTIONS_ID_TOKEN_REQUEST_URL/_TOKEN (audience ARCHIFAU_AUDIENCE,
//     default ARCHIFAU_REGISTRY);
//   - elsewhere, it falls back to `gh auth token` for human use.
//
// Install: place on PATH and add to ~/.docker/config.json:
//
//	{"credHelpers": {"registry.example.com": "archifau"}}
//
// Replace the example key with the configured registry host, including its port.
//
// Only the `get` verb does real work; store/erase are accepted no-ops so
// docker login/logout don't error.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// BuildTime is set at build time using -ldflags.
var BuildTime = "unknown"

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: docker-credential-archifau <get|store|erase|list|version>")
		os.Exit(1)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "docker-credential-archifau: %v\n", err)
		os.Exit(1)
	}
}

func run(verb string) error {
	switch verb {
	case "get":
		server, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		return get(strings.TrimSpace(server))
	case "store", "erase":
		// Credentials are never stored; nothing to do.
		_, _ = io.Copy(io.Discard, os.Stdin)
		return nil
	case "list":
		registry := os.Getenv("ARCHIFAU_REGISTRY")
		out := map[string]string{}
		if registry != "" {
			out["https://"+registry] = "github-oidc"
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	case "version":
		fmt.Println(BuildTime)
		return nil
	default:
		return fmt.Errorf("unknown verb %q", verb)
	}
}

func get(server string) error {
	secret, user, err := credential(server)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{
		"ServerURL": server,
		"Username":  user,
		"Secret":    secret,
	})
}

// credential returns the freshest GitHub credential available: the job's
// OIDC token inside Actions, the gh CLI token otherwise.
func credential(server string) (secret, user string, err error) {
	reqURL := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")
	reqToken := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if reqURL != "" && reqToken != "" {
		token, err := fetchOIDCToken(reqURL, reqToken, audience(server))
		if err != nil {
			return "", "", err
		}
		return token, "github-oidc", nil
	}

	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", "", fmt.Errorf("not in GitHub Actions and `gh auth token` failed: %w", err)
	}
	return strings.TrimSpace(string(out)), "github-user", nil
}

// audience picks the OIDC audience: explicit env, else the registry host
// (matching archifau's EXPECTED_AUD default).
func audience(server string) string {
	if aud := os.Getenv("ARCHIFAU_AUDIENCE"); aud != "" {
		return aud
	}
	host := strings.TrimPrefix(strings.TrimPrefix(server, "https://"), "http://")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	if host != "" {
		return host
	}
	return os.Getenv("ARCHIFAU_REGISTRY")
}

func fetchOIDCToken(reqURL, reqToken, aud string) (string, error) {
	u := reqURL
	if aud != "" {
		u += "&audience=" + url.QueryEscape(aud)
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "bearer "+reqToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer util.DeferClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("OIDC token request: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Value == "" {
		return "", fmt.Errorf("OIDC token response had no value")
	}
	return out.Value, nil
}
