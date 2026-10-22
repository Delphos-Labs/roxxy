package commands

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// dockerConfig is the relevant subset of ~/.docker/config.json.
type dockerConfig struct {
	Auths       map[string]dockerAuthEntry `json:"auths"`
	CredHelpers map[string]string          `json:"credHelpers"`
	CredsStore  string                     `json:"credsStore"`
}

type dockerAuthEntry struct {
	Auth     string `json:"auth"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// RegistryCreds is a username/password pair for a registry.
// An empty Username means no credentials were found; the caller should send
// requests anonymously and let the registry's WWW-Authenticate flow drive
// any subsequent token exchange.
type RegistryCreds struct {
	Username string
	Password string
}

// LookupRegistryCreds resolves credentials for `host` (e.g.
// "123456789012.dkr.ecr.us-east-1.amazonaws.com") using the Docker config
// resolution order:
//  1. credHelpers[host] — exec docker-credential-<helper>
//  2. credsStore — exec docker-credential-<store> as a fallback
//  3. auths[host] — base64-decoded user:pass, or explicit username/password
//
// Host matching tolerates scheme prefixes ("https://host", "host/v2/") because
// docker config keys are inconsistent in the wild.
//
// Returns a zero RegistryCreds (and no error) when no credential source is
// configured for the host — anonymous access is the legitimate fallback for
// public registries.
func LookupRegistryCreds(host string) (RegistryCreds, error) {
	cfg, err := loadDockerConfig()
	if err != nil {
		return RegistryCreds{}, err
	}
	if cfg == nil {
		return RegistryCreds{}, nil
	}

	if helper, ok := matchHost(cfg.CredHelpers, host); ok {
		return runCredHelper(helper, host)
	}

	if entry, ok := matchAuths(cfg.Auths, host); ok {
		creds, err := decodeAuthEntry(entry)
		if err != nil {
			return RegistryCreds{}, fmt.Errorf("auths[%s]: %w", host, err)
		}
		if creds.Username != "" {
			return creds, nil
		}
	}

	if cfg.CredsStore != "" {
		creds, err := runCredHelper(cfg.CredsStore, host)
		// A credsStore miss is not fatal — anonymous is still valid.
		if err == nil && creds.Username != "" {
			return creds, nil
		}
	}

	return RegistryCreds{}, nil
}

func loadDockerConfig() (*dockerConfig, error) {
	path := os.Getenv("DOCKER_CONFIG")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil
		}
		path = filepath.Join(home, ".docker")
	}
	data, err := os.ReadFile(filepath.Join(path, "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read docker config: %w", err)
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse docker config: %w", err)
	}
	return &cfg, nil
}

// matchHost picks an entry from a host-keyed map by trying the literal host
// then variants with/without scheme and trailing path. Docker hub's legacy key
// "https://index.docker.io/v1/" is handled by the variant fallback.
func matchHost(m map[string]string, host string) (string, bool) {
	if m == nil {
		return "", false
	}
	for _, candidate := range hostCandidates(host) {
		if v, ok := m[candidate]; ok {
			return v, true
		}
	}
	return "", false
}

func matchAuths(m map[string]dockerAuthEntry, host string) (dockerAuthEntry, bool) {
	if m == nil {
		return dockerAuthEntry{}, false
	}
	for _, candidate := range hostCandidates(host) {
		if v, ok := m[candidate]; ok {
			return v, true
		}
	}
	return dockerAuthEntry{}, false
}

func hostCandidates(host string) []string {
	host = strings.TrimSpace(host)
	bare := host
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		bare = u.Host
	}
	bare = strings.TrimSuffix(bare, "/")
	return []string{
		host,
		bare,
		"https://" + bare,
		"https://" + bare + "/",
		"https://" + bare + "/v1/",
		"https://" + bare + "/v2/",
	}
}

func decodeAuthEntry(e dockerAuthEntry) (RegistryCreds, error) {
	if e.Auth != "" {
		raw, err := base64.StdEncoding.DecodeString(e.Auth)
		if err != nil {
			return RegistryCreds{}, fmt.Errorf("decode auth: %w", err)
		}
		i := strings.IndexByte(string(raw), ':')
		if i < 0 {
			return RegistryCreds{}, fmt.Errorf("malformed auth (no colon)")
		}
		return RegistryCreds{Username: string(raw[:i]), Password: string(raw[i+1:])}, nil
	}
	if e.Username != "" {
		return RegistryCreds{Username: e.Username, Password: e.Password}, nil
	}
	return RegistryCreds{}, nil
}

// credHelperResponse matches the Docker credential helper protocol.
type credHelperResponse struct {
	ServerURL string `json:"ServerURL"`
	Username  string `json:"Username"`
	Secret    string `json:"Secret"`
}

func runCredHelper(name, host string) (RegistryCreds, error) {
	binary := "docker-credential-" + name
	cmd := exec.Command(binary, "get") //nolint:gosec // helper name comes from user's docker config
	cmd.Stdin = strings.NewReader(host + "\n")
	out, err := cmd.Output()
	if err != nil {
		// Helpers return non-zero with "credentials not found" on stderr for misses.
		// Treat any helper failure as "no creds available" — caller can fall through
		// to anonymous; a real auth failure surfaces later from the registry.
		return RegistryCreds{}, nil
	}
	var resp credHelperResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return RegistryCreds{}, fmt.Errorf("parse %s output: %w", binary, err)
	}
	if resp.Username == "" && resp.Secret == "" {
		return RegistryCreds{}, nil
	}
	return RegistryCreds{Username: resp.Username, Password: resp.Secret}, nil
}
