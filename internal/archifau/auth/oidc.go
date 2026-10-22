package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// GitHubIssuer is the issuer of GitHub Actions OIDC tokens.
const GitHubIssuer = "https://token.actions.githubusercontent.com"

// OIDCVerifier verifies GitHub Actions OIDC ID tokens (RS256 JWTs) against
// the issuer's published JWKS. Discovery and JWKS fetching/caching are handled
// by go-oidc: the provider is built once (lazily) and the remote key set caches
// keys in-process, refetching only when it sees an unknown kid.
type OIDCVerifier struct {
	// Issuer is the OIDC issuer URL. Defaults to GitHubIssuer. Override in
	// tests to point at an httptest server.
	Issuer string
	// ExpectedAud is the audience the token must carry. Required.
	ExpectedAud string
	// AllowedOwners optionally restricts repository_owner (lowercase).
	AllowedOwners []string
	// ClockSkew is the leeway applied to the token's exp so brief clock drift
	// between archifau and the issuer doesn't reject still-fresh tokens. go-oidc
	// has no native exp-leeway option, so it is applied by handing go-oidc a
	// clock shifted back by this amount (see verifyClock). That shift also
	// subtracts from go-oidc's fixed 5-minute nbf leeway, so keep this well
	// under 5m. A zero value defaults to 60s (see clockSkew), which is plenty
	// for real drift. (main.go rejects CLOCK_SKEW_SECS >= 300.)
	ClockSkew time.Duration
	// Client is the HTTP client for discovery/JWKS fetches.
	Client *http.Client
	// Now is the clock, injectable for tests.
	Now func() time.Time

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
}

type ghClaims struct {
	Repository      string `json:"repository"`
	RepositoryOwner string `json:"repository_owner"`
	Ref             string `json:"ref"`
	Actor           string `json:"actor"`
	Workflow        string `json:"workflow"`
}

func (v *OIDCVerifier) issuer() string {
	if v.Issuer != "" {
		return v.Issuer
	}
	return GitHubIssuer
}

func (v *OIDCVerifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// clockSkew is the effective exp leeway. A zero ClockSkew defaults to 60s, so a
// directly-constructed OIDCVerifier that omits the field (tests, or any future
// caller) gets the documented default rather than silently zero leeway. The
// shipped binary always passes an explicit value from CLOCK_SKEW_SECS.
func (v *OIDCVerifier) clockSkew() time.Duration {
	if v.ClockSkew == 0 {
		return 60 * time.Second
	}
	return v.ClockSkew
}

// verifyClock is the clock handed to go-oidc: the real clock shifted back by
// clockSkew, so a token is still accepted until that much past its exp. This is
// how the exp leeway is applied (go-oidc has no leeway option) without ever
// disabling the library's own expiry check.
func (v *OIDCVerifier) verifyClock() time.Time {
	return v.now().Add(-v.clockSkew())
}

func (v *OIDCVerifier) client() *http.Client {
	if v.Client != nil {
		return v.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// idTokenVerifier builds (once) and returns the go-oidc verifier. Provider
// discovery and JWKS caching live behind it, so it is constructed lazily and
// reused across requests. The lock single-flights construction so a concurrent
// cold burst triggers one discovery, not N. A discovery failure is deliberately
// NOT cached: only the successful verifier is memoized, so a transient issuer
// outage at first use self-heals on the next request rather than poisoning the
// verifier until restart.
func (v *OIDCVerifier) idTokenVerifier() (*oidc.IDTokenVerifier, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.verifier != nil {
		return v.verifier, nil
	}

	// go-oidc reads the HTTP client from the context (same key as x/oauth2).
	// The verifier captures this context for its background JWKS fetches, so
	// the test client/clock keep flowing through to discovery and key lookups.
	ctx := oidc.ClientContext(context.Background(), v.client())
	provider, err := oidc.NewProvider(ctx, v.issuer())
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery: %w", err)
	}

	// ClientID enforces the audience; an empty value would require
	// SkipClientIDCheck, which we never set — keep the fail-closed behavior.
	// SupportedSigningAlgs pins RS256 so the signature alg cannot be downgraded.
	// Now uses a clock shifted back by ClockSkew (verifyClock) so exp is checked
	// with that much leeway — go-oidc has no exp-leeway option of its own.
	v.verifier = provider.VerifierContext(ctx, &oidc.Config{
		ClientID:             v.ExpectedAud,
		SupportedSigningAlgs: []string{oidc.RS256},
		Now:                  v.verifyClock,
	})
	return v.verifier, nil
}

// Verify checks the token's signature and claims and returns the resulting
// Identity, confined to the repository's namespace.
func (v *OIDCVerifier) Verify(token string) (*Identity, error) {
	if v.ExpectedAud == "" {
		return nil, fmt.Errorf("oidc: ExpectedAud is not configured")
	}

	verifier, err := v.idTokenVerifier()
	if err != nil {
		return nil, err
	}

	// go-oidc enforces signature (RS256, via JWKS), issuer, audience and expiry.
	idToken, err := verifier.Verify(context.Background(), token)
	if err != nil {
		return nil, fmt.Errorf("oidc: %w", err)
	}

	var c ghClaims
	if err := idToken.Claims(&c); err != nil {
		return nil, fmt.Errorf("oidc: claims: %w", err)
	}

	owner := strings.ToLower(c.RepositoryOwner)
	if len(v.AllowedOwners) > 0 && !containsTrimmed(v.AllowedOwners, owner) {
		return nil, fmt.Errorf("oidc: owner %q not allowed", owner)
	}

	// Registry/namespace names must be lowercase; GitHub names may not be.
	repo := strings.ToLower(c.Repository)
	if repo == "" {
		return nil, fmt.Errorf("oidc: token has no repository claim")
	}

	return &Identity{
		Kind:       "oidc",
		Subject:    repo,
		Repository: repo,
		Ref:        c.Ref,
		Actor:      c.Actor,
		Workflow:   c.Workflow,
		Namespaces: []string{repo},
	}, nil
}

func containsTrimmed(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.TrimSpace(h) == needle {
			return true
		}
	}
	return false
}
