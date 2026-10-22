package commands

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestEffectiveChunkSize(t *testing.T) {
	tests := []struct {
		name   string
		envSet bool
		env    string
		want   int
	}{
		{name: "unset uses default", envSet: false, want: chunkedBlobChunkSize},
		{name: "valid positive override (MiB)", envSet: true, env: "256", want: 256 * 1024 * 1024},
		{name: "zero falls back to default", envSet: true, env: "0", want: chunkedBlobChunkSize},
		{name: "negative falls back to default", envSet: true, env: "-5", want: chunkedBlobChunkSize},
		{name: "garbage falls back to default", envSet: true, env: "abc", want: chunkedBlobChunkSize},
		{name: "empty falls back to default", envSet: true, env: "", want: chunkedBlobChunkSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envSet {
				t.Setenv("ROAR_CHUNK_SIZE_MIB", tt.env)
			} else {
				// Ensure a stray ambient value doesn't leak in.
				t.Setenv("ROAR_CHUNK_SIZE_MIB", "")
			}
			if got := effectiveChunkSize(); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolveLocation(t *testing.T) {
	const base = "https://reg.example.com"
	tests := []struct {
		name     string
		location string
		want     string
		wantErr  string // substring expected in the error, "" means no error
	}{
		{
			name:     "path-only resolves against base",
			location: "/v2/repo/blobs/uploads/abc",
			want:     base + "/v2/repo/blobs/uploads/abc",
		},
		{
			name:     "absolute same-host is honored",
			location: "https://reg.example.com/v2/repo/blobs/uploads/abc",
			want:     "https://reg.example.com/v2/repo/blobs/uploads/abc",
		},
		{
			name:     "absolute different host is rejected",
			location: "https://attacker.example.net/v2/repo/blobs/uploads/abc",
			wantErr:  "different origin",
		},
		{
			name:     "absolute same host but downgraded scheme is rejected",
			location: "http://reg.example.com/v2/repo/blobs/uploads/abc",
			wantErr:  "different origin",
		},
		{
			name:     "explicit :443 is the same origin as bare https",
			location: "https://reg.example.com:443/v2/repo/blobs/uploads/abc",
			want:     "https://reg.example.com:443/v2/repo/blobs/uploads/abc",
		},
		{
			name:     "host case is origin-normalized",
			location: "https://REG.Example.com/v2/repo/blobs/uploads/abc",
			want:     "https://REG.Example.com/v2/repo/blobs/uploads/abc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveLocation(base, tt.location)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildFinalizeURL(t *testing.T) {
	const base = "https://reg.example.com"
	const digest = "sha256:abc123"
	// QueryEscape turns the digest's colon into %3A in the URL.
	const escapedDigest = "sha256%3Aabc123"
	tests := []struct {
		name     string
		location string
		want     string
		wantErr  string
	}{
		{
			name:     "path-only location",
			location: "/v2/repo/blobs/uploads/sess",
			want:     base + "/v2/repo/blobs/uploads/sess?digest=" + escapedDigest,
		},
		{
			name:     "absolute same-host location",
			location: "https://reg.example.com/v2/repo/blobs/uploads/sess",
			want:     "https://reg.example.com/v2/repo/blobs/uploads/sess?digest=" + escapedDigest,
		},
		{
			name:     "location with existing query uses ampersand",
			location: "/v2/repo/blobs/uploads/sess?_state=xyz",
			want:     base + "/v2/repo/blobs/uploads/sess?_state=xyz&digest=" + escapedDigest,
		},
		{
			name:     "absolute different host is rejected",
			location: "https://attacker.example.net/v2/repo/blobs/uploads/sess",
			wantErr:  "different origin",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildFinalizeURL(base, tt.location, digest)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveLocationNeverLeavesOrigin is the credential-leak regression: a
// malicious registry Location must never resolve to a foreign host (which would
// re-attach the credential off-origin). resolveLocation either rejects it or
// resolves it to a safe same-host path — never to the attacker host. These are
// the userinfo/host-confusion vectors a string-concatenation branch waved through.
func TestResolveLocationNeverLeavesOrigin(t *testing.T) {
	const base = "https://reg.example.com"
	vectors := []string{
		"@evil.com/v2/steal",
		":@evil.com/v2/steal",
		".evil.com/v2/steal",
		"//evil.com/v2/steal",
		"https://evil.com/v2/steal",
		"https://evil.reg.example.com/v2/steal", // subdomain
		"http://reg.example.com/v2/steal",       // https->http downgrade
	}
	for _, loc := range vectors {
		t.Run(loc, func(t *testing.T) {
			got, err := resolveLocation(base, loc)
			if err != nil {
				return // rejected outright — safe
			}
			u, perr := url.Parse(got)
			if perr != nil {
				t.Fatalf("resolveLocation(%q) returned unparseable %q: %v", loc, got, perr)
			}
			if !strings.EqualFold(u.Hostname(), "reg.example.com") {
				t.Errorf("resolveLocation(%q) = %q -> host %q; credential would leak off-origin",
					loc, got, u.Hostname())
			}
		})
	}
}

// TestCredsAllowedOverScheme covers the plaintext-credential gate: https always
// allowed; http only to a loopback host (any 127.0.0.0/8 or ::1) or under the
// ROAR_ALLOW_INSECURE_HTTP opt-in.
func TestCredsAllowedOverScheme(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		env    string
		want   bool
	}{
		{"https public", "https://reg.example.com", "", true},
		{"http localhost", "http://localhost:5000", "", true},
		{"http 127.0.0.1", "http://127.0.0.1:5000", "", true},
		{"http 127.0.0.2 (loopback)", "http://127.0.0.2:5000", "", true},
		{"http ::1", "http://[::1]:5000", "", true},
		{"http public refused", "http://reg.example.com", "", false},
		{"http public with opt-in", "http://reg.example.com", "1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ROAR_ALLOW_INSECURE_HTTP", tt.env)
			u, err := url.Parse(tt.rawURL)
			if err != nil {
				t.Fatal(err)
			}
			if got := credsAllowedOverScheme(u); got != tt.want {
				t.Errorf("credsAllowedOverScheme(%q) env=%q = %v, want %v", tt.rawURL, tt.env, got, tt.want)
			}
		})
	}
}

// TestCredentialedRedirectStaysOnOrigin asserts the CheckRedirect policy never
// re-sends the Authorization header to a cross-origin host on a registry 3xx
// (the second, transport-level credential-leak vector).
func TestCredentialedRedirectStaysOnOrigin(t *testing.T) {
	var leakedHost string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leakedHost = r.Host
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	req, err := http.NewRequest("GET", origin.URL+"/v2/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer SECRET")
	client := &http.Client{CheckRedirect: refuseCrossOriginRedirect}
	if resp, err := client.Do(req); err == nil {
		_ = resp.Body.Close()
	}
	if leakedHost != "" {
		t.Fatalf("Authorization header leaked to cross-origin host %q", leakedHost)
	}
}
