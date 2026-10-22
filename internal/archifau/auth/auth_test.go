package auth

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// --- test JWT / JWKS plumbing ----------------------------------------------

type fakeIssuer struct {
	key      *rsa.PrivateKey
	kid      string
	srv      *httptest.Server
	jwksHits int
	// down, when true, makes discovery and JWKS endpoints return 500 to
	// simulate a GitHub JWKS outage.
	down bool
}

// newFakeIssuer stands up an httptest OIDC issuer: discovery + JWKS endpoints
// backed by a fresh RSA key.
func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fi := &fakeIssuer{key: key, kid: "test-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if fi.down {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// go-oidc requires the discovery "issuer" to match the configured
		// issuer URL exactly, else NewProvider rejects the provider.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   fi.srv.URL,
			"jwks_uri": fi.srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		if fi.down {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fi.jwksHits++
		// go-oidc ignores Cache-Control (it caches keys by kid); kept only to
		// mimic GitHub's real JWKS response headers.
		w.Header().Set("Cache-Control", "max-age=300")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kid": fi.kid,
				"kty": "RSA",
				"alg": "RS256",
				"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
			}},
		})
	})
	fi.srv = httptest.NewServer(mux)
	t.Cleanup(fi.srv.Close)
	return fi
}

// sign produces an RS256 JWT over the claims with the issuer's key.
func (fi *fakeIssuer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	return signJWT(t, fi.key, fi.kid, claims)
}

func signJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signing := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body)
	hashed := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// forgeJWT builds a JWT with an attacker-chosen alg header and the given raw
// signature bytes — for alg-downgrade / alg:none tests.
func forgeJWT(t *testing.T, alg, kid string, claims map[string]any, sig []byte) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT", "kid": kid})
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signing := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body)
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// baseClaims returns a valid set of GitHub Actions OIDC claims for the issuer.
func (fi *fakeIssuer) baseClaims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":              fi.srv.URL,
		"aud":              "archifau.test",
		"exp":              now.Add(5 * time.Minute).Unix(),
		"nbf":              now.Add(-1 * time.Minute).Unix(),
		"repository":       "Delphos-Labs/Widget",
		"repository_owner": "Delphos-Labs",
		"ref":              "refs/heads/main",
		"actor":            "faye",
		"workflow":         "publish",
	}
}

func (fi *fakeIssuer) verifier() *OIDCVerifier {
	return &OIDCVerifier{
		Issuer:        fi.srv.URL,
		ExpectedAud:   "archifau.test",
		AllowedOwners: []string{"delphos-labs"},
	}
}

// --- OIDC verifier ----------------------------------------------------------

func TestOIDCVerifyValid(t *testing.T) {
	fi := newFakeIssuer(t)
	id, err := fi.verifier().Verify(fi.sign(t, fi.baseClaims()))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Kind != "oidc" {
		t.Errorf("kind = %q, want oidc", id.Kind)
	}
	// Mixed-case GitHub names must be lowercased for registry namespaces.
	if id.Repository != "delphos-labs/widget" {
		t.Errorf("repository = %q, want delphos-labs/widget", id.Repository)
	}
	if len(id.Namespaces) != 1 || id.Namespaces[0] != "delphos-labs/widget" {
		t.Errorf("namespaces = %v", id.Namespaces)
	}
	if id.Ref != "refs/heads/main" || id.Actor != "faye" || id.Workflow != "publish" {
		t.Errorf("claims not carried: %+v", id)
	}
}

func TestOIDCVerifyAudienceArray(t *testing.T) {
	fi := newFakeIssuer(t)
	claims := fi.baseClaims()
	claims["aud"] = []string{"other", "archifau.test"}
	if _, err := fi.verifier().Verify(fi.sign(t, claims)); err != nil {
		t.Fatalf("array audience should verify: %v", err)
	}
}

func TestOIDCVerifyRejections(t *testing.T) {
	fi := newFakeIssuer(t)

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		token func() string
	}{
		{"expired", func() string {
			c := fi.baseClaims()
			c["exp"] = time.Now().Add(-10 * time.Minute).Unix()
			return fi.sign(t, c)
		}},
		{"wrong audience", func() string {
			c := fi.baseClaims()
			c["aud"] = "someone-else"
			return fi.sign(t, c)
		}},
		{"wrong issuer", func() string {
			c := fi.baseClaims()
			c["iss"] = "https://evil.example.com"
			return fi.sign(t, c)
		}},
		{"bad signature", func() string {
			return signJWT(t, otherKey, fi.kid, fi.baseClaims())
		}},
		{"disallowed owner", func() string {
			c := fi.baseClaims()
			c["repository"] = "mallory/widget"
			c["repository_owner"] = "mallory"
			return fi.sign(t, c)
		}},
		{"alg none rejected", func() string {
			// go-oidc must reject alg:none — SupportedSigningAlgs pins RS256.
			return forgeJWT(t, "none", fi.kid, fi.baseClaims(), nil)
		}},
		{"alg HS256 confusion rejected", func() string {
			// Classic RS256->HS256 confusion: HMAC the signing input with the
			// RSA public modulus. A verifier that trusted the header alg and
			// reused the public key as an HMAC secret would accept this; go-oidc
			// rejects it on the alg allow-list before any key is used. We sign a
			// *valid* HMAC so the only possible reason for rejection is the alg.
			c := fi.baseClaims()
			hdr, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT", "kid": fi.kid})
			body, _ := json.Marshal(c)
			signing := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body)
			mac := hmac.New(sha256.New, fi.key.N.Bytes())
			mac.Write([]byte(signing))
			return signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		}},
		{"future nbf rejected", func() string {
			// nbf beyond go-oidc's 5-minute leeway must be rejected.
			c := fi.baseClaims()
			c["nbf"] = time.Now().Add(10 * time.Minute).Unix()
			return fi.sign(t, c)
		}},
		{"not a JWT", func() string { return "gho_notajwt" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := fi.verifier().Verify(tc.token()); err == nil {
				t.Errorf("%s: expected rejection, got success", tc.name)
			}
		})
	}
}

func TestOIDCJWKSCached(t *testing.T) {
	fi := newFakeIssuer(t)
	v := fi.verifier()
	for i := 0; i < 3; i++ {
		if _, err := v.Verify(fi.sign(t, fi.baseClaims())); err != nil {
			t.Fatal(err)
		}
	}
	if fi.jwksHits != 1 {
		t.Errorf("JWKS fetched %d times, want 1 (cache should hold)", fi.jwksHits)
	}
}

// TestOIDCDiscoveryFailureIsNotSticky asserts that a discovery failure at first
// use is not cached: once the issuer recovers, the next Verify retries discovery
// and succeeds (a cached error would poison the verifier until restart).
func TestOIDCDiscoveryFailureIsNotSticky(t *testing.T) {
	fi := newFakeIssuer(t)
	v := fi.verifier()

	// Discovery is down on the first attempt -> Verify fails.
	fi.down = true
	if _, err := v.Verify(fi.sign(t, fi.baseClaims())); err == nil {
		t.Fatal("expected failure while issuer discovery is down")
	}

	// Issuer recovers; the next Verify must retry discovery and succeed.
	fi.down = false
	if _, err := v.Verify(fi.sign(t, fi.baseClaims())); err != nil {
		t.Fatalf("verify should recover after the issuer returns: %v", err)
	}
}

// TestOIDCStaleCacheSurvivesOutage covers the "cache beats an outage" property:
// once a kid is cached, verification keeps working while the issuer's JWKS is
// unreachable, and an unknown kid still fails because it forces a (failing)
// refetch.
//
// The hand-rolled verifier expired its JWKS cache on a TTL and fell back to the
// stale entry on a fetch error. go-oidc's remote key set instead keeps cached
// keys indefinitely and only refetches when it sees an unknown kid (per the
// OIDC key-rotation guidance), so there is no TTL to advance past — but the
// observable behavior this test asserts is identical and remains correct: known
// kid verifies offline, unknown kid fails during the outage. The clock is still
// driven so the in-flight tokens stay within their own exp/nbf window.
func TestOIDCStaleCacheSurvivesOutage(t *testing.T) {
	fi := newFakeIssuer(t)
	v := fi.verifier()

	// Drive the clock so the tokens below stay valid deterministically.
	now := time.Now()
	v.Now = func() time.Time { return now }

	// Prime the cache with one successful verify.
	if _, err := v.Verify(fi.sign(t, fi.baseClaims())); err != nil {
		t.Fatalf("priming Verify: %v", err)
	}

	// The issuer goes dark. go-oidc keeps the cached key (it caches by kid and
	// ignores Cache-Control), so the already-seen kid still verifies offline.
	fi.down = true
	now = now.Add(10 * time.Minute)

	t.Run("cached kid still verifies", func(t *testing.T) {
		claims := fi.baseClaims()
		// Keep the token itself unexpired despite the advanced clock.
		claims["exp"] = now.Add(5 * time.Minute).Unix()
		claims["nbf"] = now.Add(-1 * time.Minute).Unix()
		if _, err := v.Verify(fi.sign(t, claims)); err != nil {
			t.Errorf("stale cache should still verify cached kid: %v", err)
		}
	})

	t.Run("unknown kid fails during outage", func(t *testing.T) {
		token := signJWT(t, fi.key, "unseen-kid", fi.baseClaims())
		if _, err := v.Verify(token); err == nil {
			t.Error("expected failure for kid absent from the stale cache")
		}
	})
}

// TestOIDCExpiryUsesInjectedClockAndSkew proves two things at once: expiry is
// evaluated against the injected clock (not wall-clock), and ClockSkew is
// actually honored as leeway past exp.
func TestOIDCExpiryUsesInjectedClockAndSkew(t *testing.T) {
	fi := newFakeIssuer(t)
	v := fi.verifier()
	v.ClockSkew = 60 * time.Second
	// A fixed clock far in the future. A token whose exp is "10 minutes ago"
	// relative to this clock is still in the *future* relative to wall-clock,
	// so a rejection can only happen if the injected clock drives expiry.
	fixed := time.Now().Add(48 * time.Hour)
	v.Now = func() time.Time { return fixed }

	t.Run("expired against injected clock is rejected", func(t *testing.T) {
		c := fi.baseClaims()
		c["exp"] = fixed.Add(-10 * time.Minute).Unix()
		c["nbf"] = fixed.Add(-20 * time.Minute).Unix()
		if _, err := v.Verify(fi.sign(t, c)); err == nil {
			t.Error("token expired past the skew window should be rejected")
		}
	})

	t.Run("within ClockSkew grace is accepted", func(t *testing.T) {
		c := fi.baseClaims()
		c["exp"] = fixed.Add(-30 * time.Second).Unix() // expired, but < 60s skew
		c["nbf"] = fixed.Add(-20 * time.Minute).Unix()
		if _, err := v.Verify(fi.sign(t, c)); err != nil {
			t.Errorf("token within ClockSkew grace should verify: %v", err)
		}
	})

	// Pins the window magnitude/sign: 90s past exp is beyond the 60s skew, so a
	// regression that widened the skew (or flipped its sign) would wrongly
	// accept this.
	t.Run("just past the skew window is rejected", func(t *testing.T) {
		c := fi.baseClaims()
		c["exp"] = fixed.Add(-90 * time.Second).Unix()
		c["nbf"] = fixed.Add(-20 * time.Minute).Unix()
		if _, err := v.Verify(fi.sign(t, c)); err == nil {
			t.Error("token 90s past exp (beyond 60s skew) should be rejected")
		}
	})

	// nbf accept side + the documented skew/nbf interaction. The 60s back-shift
	// also subtracts from go-oidc's fixed 5m nbf leeway, narrowing the accept
	// window for not-yet-valid tokens to ~4m. These cases pin the nbf leeway's
	// magnitude AND sign on the accept side (the reject-only "future nbf" case in
	// TestOIDCVerifyRejections cannot). They assume go-oidc's 5m nbf leeway; if
	// the library changed it, the boundary case below would flip.
	t.Run("near-future nbf within the narrowed leeway is accepted", func(t *testing.T) {
		c := fi.baseClaims()
		c["exp"] = fixed.Add(10 * time.Minute).Unix()
		c["nbf"] = fixed.Add(3 * time.Minute).Unix() // < ~4m narrowed leeway
		if _, err := v.Verify(fi.sign(t, c)); err != nil {
			t.Errorf("nbf 3m out (within the ~4m narrowed leeway) should verify: %v", err)
		}
	})

	t.Run("near-future nbf beyond the narrowed leeway is rejected", func(t *testing.T) {
		c := fi.baseClaims()
		c["exp"] = fixed.Add(10 * time.Minute).Unix()
		// 4m30s out: beyond the 60s-narrowed ~4m window, but inside go-oidc's raw
		// 5m leeway — so accepting this would mean the skew failed to narrow nbf.
		c["nbf"] = fixed.Add(4*time.Minute + 30*time.Second).Unix()
		if _, err := v.Verify(fi.sign(t, c)); err == nil {
			t.Error("nbf 4m30s out (beyond the skew-narrowed leeway) should be rejected")
		}
	})
}

// TestOIDCExplicitRS256PinIsLoadBearing proves that the explicit
// SupportedSigningAlgs:[RS256] in idTokenVerifier — not go-oidc's hardcoded
// RS256 fallback — is what rejects a non-RS256 token. The standard fakeIssuer's
// discovery advertises no algs, so go-oidc falls back to ["RS256"] and would
// reject an ES256 token even if the pin were deleted (making the alg:none /
// HS256 cases unable to catch removal of the pin). Here discovery advertises
// ES256 and the JWKS serves a valid EC key, so absent the pin go-oidc would
// accept this token; the only thing rejecting it is the explicit pin.
//
// Verified non-vacuous during development: temporarily removing
// SupportedSigningAlgs from idTokenVerifier makes this token verify.
func TestOIDCExplicitRS256PinIsLoadBearing(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const kid = "ec-key-1"

	var issuerURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		// Advertise ES256 (with RS256): absent the explicit pin, go-oidc fills
		// its supported-alg set from this list and would accept the ES256 token.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuerURL,
			"jwks_uri":                              issuerURL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256", "ES256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "EC",
				"crv": "P-256",
				"alg": "ES256",
				"kid": kid,
				"x":   base64.RawURLEncoding.EncodeToString(ecKey.X.FillBytes(make([]byte, 32))),
				"y":   base64.RawURLEncoding.EncodeToString(ecKey.Y.FillBytes(make([]byte, 32))),
			}},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuerURL = srv.URL

	// A fully valid ES256-signed token: right issuer/audience, fresh exp/nbf.
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss":              issuerURL,
		"aud":              "archifau.test",
		"exp":              now.Add(5 * time.Minute).Unix(),
		"nbf":              now.Add(-1 * time.Minute).Unix(),
		"repository":       "Delphos-Labs/Widget",
		"repository_owner": "Delphos-Labs",
	})
	tok.Header["kid"] = kid
	es256Token, err := tok.SignedString(ecKey)
	if err != nil {
		t.Fatal(err)
	}

	v := &OIDCVerifier{Issuer: issuerURL, ExpectedAud: "archifau.test", AllowedOwners: []string{"delphos-labs"}}
	_, err = v.Verify(es256Token)
	if err == nil {
		t.Fatal("ES256 token must be rejected by the explicit SupportedSigningAlgs:[RS256] pin")
	}
	// Non-vacuity guard: the rejection must be the alg pin, not a missing key,
	// bad signature, or claim failure. go-oidc names the algorithm in that error.
	if !strings.Contains(strings.ToLower(err.Error()), "algorithm") {
		t.Errorf("expected an algorithm-pin rejection, got a different failure: %v", err)
	}
}

// --- user (gh token) verifier ------------------------------------------------

// newFakeGitHubAPI serves /user and /user/orgs for a fixed token.
func newFakeGitHubAPI(t *testing.T, token, login string, orgs []string, hits *int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	authed := func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer "+token
	}
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if !authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"login": login})
	})
	mux.HandleFunc("/user/orgs", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var out []map[string]string
		for _, o := range orgs {
			out = append(out, map[string]string{"login": o})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestUserVerifyOrgMember(t *testing.T) {
	hits := 0
	srv := newFakeGitHubAPI(t, "gho_good", "Faye", []string{"Delphos-Labs", "other-org"}, &hits)
	v := &UserVerifier{APIBase: srv.URL, AllowedOwners: []string{"delphos-labs"}}

	id, err := v.Verify("gho_good")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Kind != "user" || id.Subject != "faye" {
		t.Errorf("identity = %+v", id)
	}
	if len(id.Namespaces) != 1 || id.Namespaces[0] != "delphos-labs" {
		t.Errorf("namespaces = %v, want [delphos-labs]", id.Namespaces)
	}

	// Second verify must come from cache, not the API.
	if _, err := v.Verify("gho_good"); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("GitHub API hit %d times, want 1 (cache should hold)", hits)
	}
}

func TestUserVerifyRejections(t *testing.T) {
	hits := 0
	srv := newFakeGitHubAPI(t, "gho_good", "mallory", []string{"mallorys-org"}, &hits)

	t.Run("not in any allowed org", func(t *testing.T) {
		v := &UserVerifier{APIBase: srv.URL, AllowedOwners: []string{"delphos-labs"}}
		if _, err := v.Verify("gho_good"); err == nil {
			t.Error("expected rejection for non-member")
		}
	})
	t.Run("invalid token", func(t *testing.T) {
		v := &UserVerifier{APIBase: srv.URL, AllowedOwners: []string{"delphos-labs"}}
		if _, err := v.Verify("gho_bad"); err == nil {
			t.Error("expected rejection for invalid token")
		}
	})
	t.Run("no allowed owners", func(t *testing.T) {
		v := &UserVerifier{APIBase: srv.URL}
		if _, err := v.Verify("gho_good"); err == nil {
			t.Error("expected rejection with empty allow-list")
		}
	})
}

func TestUserVerifyOwnLoginAllowed(t *testing.T) {
	hits := 0
	srv := newFakeGitHubAPI(t, "gho_good", "Faye", nil, &hits)
	v := &UserVerifier{APIBase: srv.URL, AllowedOwners: []string{"faye"}}
	id, err := v.Verify("gho_good")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(id.Namespaces) != 1 || id.Namespaces[0] != "faye" {
		t.Errorf("namespaces = %v, want [faye]", id.Namespaces)
	}
}

// TestUserVerifyPartialOrgPageFailureIsLoginOnly pins S2: when org pagination
// fails mid-way but the user's own login is an allowed owner, the verifier must
// proceed strictly login-only — it must NOT grant the orgs from the pages that
// happened to succeed before the failure, which would cache a subset identity
// for the full TTL and flap 403s on the orgs that didn't make it.
func TestUserVerifyPartialOrgPageFailureIsLoginOnly(t *testing.T) {
	const token, login = "gho_good", "faye"
	allowedOrgPage1 := "delphos-labs" // allowed, returned on the first (full) page

	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"login": login})
	})
	mux.HandleFunc("/user/orgs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			// A full page (100 entries) forces a second page fetch.
			var out []map[string]string
			out = append(out, map[string]string{"login": allowedOrgPage1})
			for i := 1; i < 100; i++ {
				out = append(out, map[string]string{"login": fmt.Sprintf("filler-%d", i)})
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError) // page 2 fails
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// login "faye" is itself an allowed owner, so login-only auth succeeds even
	// though org listing failed.
	v := &UserVerifier{APIBase: srv.URL, AllowedOwners: []string{"faye", allowedOrgPage1}}
	id, err := v.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, ns := range id.Namespaces {
		if ns == allowedOrgPage1 {
			t.Errorf("granted org %q from a partial page; must be login-only on pagination failure (namespaces=%v)",
				allowedOrgPage1, id.Namespaces)
		}
	}
	if len(id.Namespaces) != 1 || id.Namespaces[0] != login {
		t.Errorf("namespaces = %v, want exactly [%s]", id.Namespaces, login)
	}
}

// --- minter -------------------------------------------------------------------

func testMinter() *Minter {
	return &Minter{Secret: []byte("test-secret"), Service: "archifau.test"}
}

func TestMintRoundtrip(t *testing.T) {
	m := testMinter()
	in := &Identity{Kind: "oidc", Subject: "delphos-labs/widget", Repository: "delphos-labs/widget",
		Ref: "refs/heads/main", Namespaces: []string{"delphos-labs/widget"}}

	token, expiresIn, _, err := m.Mint(in)
	if err != nil {
		t.Fatal(err)
	}
	if expiresIn != 300 {
		t.Errorf("expiresIn = %d, want 300", expiresIn)
	}
	out, err := m.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Subject != in.Subject || out.Ref != in.Ref || len(out.Namespaces) != 1 {
		t.Errorf("roundtrip mismatch: %+v", out)
	}
}

func TestMintRejections(t *testing.T) {
	m := testMinter()
	id := &Identity{Kind: "user", Subject: "faye", Namespaces: []string{"delphos-labs"}}
	token, _, _, err := m.Mint(id)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("tampered payload", func(t *testing.T) {
		tampered := token[:len(token)/2] + "x" + token[len(token)/2+1:]
		if _, err := m.Verify(tampered); err == nil {
			t.Error("expected rejection of tampered token")
		}
	})
	t.Run("wrong secret", func(t *testing.T) {
		other := &Minter{Secret: []byte("other"), Service: "archifau.test"}
		if _, err := other.Verify(token); err == nil {
			t.Error("expected rejection with different secret")
		}
	})
	t.Run("expired", func(t *testing.T) {
		past := &Minter{Secret: []byte("test-secret"), Service: "archifau.test",
			Now: func() time.Time { return time.Now().Add(-time.Hour) }}
		oldToken, _, _, err := past.Mint(id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Verify(oldToken); err == nil {
			t.Error("expected rejection of expired token")
		}
	})
	t.Run("wrong service", func(t *testing.T) {
		other := &Minter{Secret: []byte("test-secret"), Service: "elsewhere"}
		if _, err := other.Verify(token); err == nil {
			t.Error("expected rejection for wrong audience")
		}
	})

	// Alg-downgrade defenses on the minter's own tokens. The minter signs with a
	// symmetric secret, so it is the natural target of an alg:none strip or an
	// RS256 downgrade (an attacker treating the HMAC secret as a public key).
	// Unlike the OIDC path there is no library default that also pins RS256:
	// golang-jwt will verify HS256 happily, so the WithValidMethods + keyFunc
	// guards in Minter.Verify are the only thing rejecting these. The forged
	// claims are otherwise valid so the only possible reason for rejection is alg.
	mintedShape := map[string]any{
		"iss":      "archifau",
		"aud":      "archifau.test",
		"sub":      "faye",
		"exp":      time.Now().Add(5 * time.Minute).Unix(),
		"identity": map[string]any{"Kind": "user", "Subject": "faye", "Namespaces": []string{"delphos-labs"}},
	}
	t.Run("alg none rejected", func(t *testing.T) {
		if _, err := m.Verify(forgeJWT(t, "none", "", mintedShape, nil)); err == nil {
			t.Error("expected rejection of alg:none token")
		}
	})
	t.Run("alg RS256 downgrade rejected", func(t *testing.T) {
		// Signature bytes are irrelevant: the method pin must reject before the
		// signature is checked and before the secret is ever used as a key.
		if _, err := m.Verify(forgeJWT(t, "RS256", "", mintedShape, []byte("irrelevant"))); err == nil {
			t.Error("expected rejection of RS256-downgraded token")
		}
	})
}

// --- identity authorization -----------------------------------------------------

func TestIdentityAuthorization(t *testing.T) {
	ciMain := &Identity{Kind: "oidc", Repository: "delphos-labs/widget",
		Ref: "refs/heads/main", Namespaces: []string{"delphos-labs/widget"}}
	ciBranch := &Identity{Kind: "oidc", Repository: "delphos-labs/widget",
		Ref: "refs/heads/feature", Namespaces: []string{"delphos-labs/widget"}}
	human := &Identity{Kind: "user", Subject: "faye", Namespaces: []string{"delphos-labs"}}

	cases := []struct {
		name        string
		id          *Identity
		resource    string
		read, write bool
	}{
		{"ci main own namespace", ciMain, "delphos-labs/widget/app", true, true},
		{"ci main own repo exact", ciMain, "delphos-labs/widget", true, true},
		{"ci main foreign repo", ciMain, "delphos-labs/other/app", false, false},
		{"ci main foreign owner", ciMain, "mallory/widget/app", false, false},
		{"ci main prefix is not namespace", ciMain, "delphos-labs/widgetx/app", false, false},
		{"ci branch reads own", ciBranch, "delphos-labs/widget/app", true, false},
		{"human org-wide", human, "delphos-labs/anything/at/all", true, true},
		{"human foreign org", human, "mallory/thing", false, false},
		{"case folding", ciMain, "Delphos-Labs/Widget/App", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.CanRead(tc.resource); got != tc.read {
				t.Errorf("CanRead(%q) = %v, want %v", tc.resource, got, tc.read)
			}
			if got := tc.id.CanWrite(tc.resource); got != tc.write {
				t.Errorf("CanWrite(%q) = %v, want %v", tc.resource, got, tc.write)
			}
		})
	}

	if ciBranch.CanWriteFlat() {
		t.Error("non-main CI must not have flat write")
	}
	if !ciMain.CanWriteFlat() || !human.CanWriteFlat() {
		t.Error("main CI and org humans must have flat write")
	}
	// An on-main OIDC identity with no namespaces resolves to no flat write:
	// the main-ref gate alone must not grant writes without a namespace.
	noNamespaces := &Identity{Kind: "oidc", Ref: mainRef, Namespaces: nil}
	if noNamespaces.CanWriteFlat() {
		t.Error("oidc identity with no namespaces must not have flat write")
	}
}

// --- token endpoint ---------------------------------------------------------------

func TestTokenEndpoint(t *testing.T) {
	fi := newFakeIssuer(t)
	a := &Authenticator{
		OIDC:    fi.verifier(),
		Users:   &UserVerifier{AllowedOwners: []string{"delphos-labs"}},
		Minter:  testMinter(),
		Realm:   "https://archifau.test/token",
		Service: "archifau.test",
	}
	ts := httptest.NewServer(a.TokenHandler())
	t.Cleanup(ts.Close)

	t.Run("valid oidc credential", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"?service=archifau.test&scope=repository:delphos-labs/widget/app:push,pull", nil)
		req.SetBasicAuth("github-oidc", fi.sign(t, fi.baseClaims()))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var body struct {
			Token     string `json:"token"`
			ExpiresIn int    `json:"expires_in"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		id, err := a.Minter.Verify(body.Token)
		if err != nil {
			t.Fatalf("minted token does not verify: %v", err)
		}
		if id.Repository != "delphos-labs/widget" {
			t.Errorf("minted identity = %+v", id)
		}
	})

	t.Run("no credentials", func(t *testing.T) {
		resp, err := http.Get(ts.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("garbage credential", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
		req.SetBasicAuth("x", "eyJhbGciOiJSUzI1NiJ9.bogus.sig")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})
}

// captureAuthLogs swaps the default slog logger for a buffer-backed JSON
// handler. Tests using it must not run in parallel.
func captureAuthLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func authLogRecords(buf *bytes.Buffer, msg string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

func TestTokenLifecycleLogged(t *testing.T) {
	fi := newFakeIssuer(t)
	a := &Authenticator{
		OIDC:    fi.verifier(),
		Users:   &UserVerifier{AllowedOwners: []string{"delphos-labs"}},
		Minter:  testMinter(),
		Realm:   "https://archifau.test/token",
		Service: "archifau.test",
	}
	handler := a.TokenHandler()

	t.Run("issued", func(t *testing.T) {
		buf := captureAuthLogs(t)
		req := httptest.NewRequest(http.MethodGet, "/token?scope=repository:delphos-labs/widget/app:push,pull", nil)
		req.SetBasicAuth("github-oidc", fi.sign(t, fi.baseClaims()))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d", rr.Code)
		}
		recs := authLogRecords(buf, "auth token issued")
		if len(recs) != 1 {
			t.Fatalf("issued records = %d, want 1", len(recs))
		}
		rec := recs[0]
		for k, want := range map[string]any{
			"id_kind": "oidc", "id_subject": "delphos-labs/widget",
			"id_ref": "refs/heads/main",
			"scope":  "repository:delphos-labs/widget/app:push,pull",
		} {
			if rec[k] != want {
				t.Errorf("%s = %v, want %v", k, rec[k], want)
			}
		}
	})

	t.Run("rejected", func(t *testing.T) {
		buf := captureAuthLogs(t)
		req := httptest.NewRequest(http.MethodGet, "/token", nil)
		req.SetBasicAuth("x", "eyJhbGciOiJSUzI1NiJ9.bogus.sig")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", rr.Code)
		}
		recs := authLogRecords(buf, "auth token rejected")
		if len(recs) != 1 {
			t.Fatalf("rejected records = %d, want 1", len(recs))
		}
		if recs[0]["level"] != "WARN" || recs[0]["err"] == nil {
			t.Errorf("record = %+v", recs[0])
		}
	})
}

func TestRequestIdentityFallsBackToGitHubCredential(t *testing.T) {
	fi := newFakeIssuer(t)
	a := &Authenticator{OIDC: fi.verifier(), Users: &UserVerifier{AllowedOwners: []string{"delphos-labs"}},
		Minter: testMinter(), Service: "archifau.test"}

	// A raw OIDC JWT in the Bearer slot must authenticate (simple clients
	// skip the /token exchange).
	r := httptest.NewRequest(http.MethodGet, "/files/delphos-labs/widget/x", nil)
	r.Header.Set("Authorization", "Bearer "+fi.sign(t, fi.baseClaims()))
	id, err := a.RequestIdentity(r)
	if err != nil {
		t.Fatalf("RequestIdentity: %v", err)
	}
	if id.Repository != "delphos-labs/widget" {
		t.Errorf("identity = %+v", id)
	}

	// And a minted token must take the fast path.
	token, _, _, err := a.Minter.Mint(id)
	if err != nil {
		t.Fatal(err)
	}
	r2 := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	r2.SetBasicAuth("x", token)
	if _, err := a.RequestIdentity(r2); err != nil {
		t.Errorf("minted token via Basic password: %v", err)
	}
}

// Guard against fmt import drift in helpers.
var _ = fmt.Sprintf
