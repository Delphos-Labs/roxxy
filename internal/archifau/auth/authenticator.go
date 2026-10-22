package auth

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/archifau/audit"
)

// Authenticator ties the credential verifiers together and implements the
// HTTP-facing pieces: the docker token endpoint and per-request identity
// resolution.
type Authenticator struct {
	OIDC   *OIDCVerifier
	Users  *UserVerifier
	Minter *Minter
	// Realm is the absolute URL of the token endpoint, advertised in
	// WWW-Authenticate challenges.
	Realm string
	// Service is the registry service name (the public hostname).
	Service string
}

// looksLikeJWT distinguishes a GitHub OIDC ID token from an opaque user
// token (gho_…/ghp_…/github_pat_…).
func looksLikeJWT(cred string) bool {
	return strings.Count(cred, ".") == 2 && strings.HasPrefix(cred, "eyJ")
}

// CredentialIdentity validates a raw GitHub credential — an Actions OIDC JWT
// or a user token — and returns the established Identity.
func (a *Authenticator) CredentialIdentity(cred string) (*Identity, error) {
	cred = strings.TrimSpace(cred)
	if cred == "" {
		return nil, fmt.Errorf("auth: empty credential")
	}
	if looksLikeJWT(cred) {
		return a.OIDC.Verify(cred)
	}
	return a.Users.Verify(cred)
}

// RequestIdentity authenticates an incoming API request. The fast path is a
// minted archifau token in the Authorization header (Bearer, or the password
// of Basic for clients like pip/twine that only speak Basic). Raw GitHub
// credentials are accepted in the same positions so simple clients can skip
// the /token exchange.
func (a *Authenticator) RequestIdentity(r *http.Request) (*Identity, error) {
	cred, ok := requestCredential(r)
	if !ok {
		return nil, fmt.Errorf("auth: no credentials presented")
	}
	if id, err := a.Minter.Verify(cred); err == nil {
		return id, nil
	}
	return a.CredentialIdentity(cred)
}

// requestCredential pulls the bearer credential out of the request: the
// Bearer token, or the Basic password (the username is cosmetic, matching
// the docker-login convention from the original design).
func requestCredential(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if bearer, ok := cutPrefixFold(h, "Bearer "); ok {
		bearer = strings.TrimSpace(bearer)
		return bearer, bearer != ""
	}
	if _, pass, ok := r.BasicAuth(); ok && pass != "" {
		return pass, true
	}
	return "", false
}

// cutPrefixFold is strings.CutPrefix with ASCII case-insensitive matching of
// the prefix (auth scheme names are case-insensitive per RFC 7235).
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// TokenHandler implements the docker registry token endpoint: clients
// present a GitHub credential (docker login password) and receive a
// short-lived archifau token. Mirrors `aws ecr get-login-password` ergonomics.
func (a *Authenticator) TokenHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := requestCredential(r)
		if !ok {
			audit.Anonymous(r.Context())
			a.unauthorized(w, "credentials required")
			return
		}
		id, err := a.CredentialIdentity(cred)
		if err != nil {
			slog.Warn("auth token rejected", "err", err)
			audit.Anonymous(r.Context())
			a.unauthorized(w, "invalid credentials")
			return
		}

		token, expiresIn, issuedAt, err := a.Minter.Mint(id)
		if err != nil {
			slog.Error("auth minting token", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// The requested scope is the audit trail of intent; enforcement is
		// per-request from the identity, so it is logged, not embedded.
		scope := strings.Join(r.URL.Query()["scope"], " ")
		slog.Info("auth token issued",
			"id_kind", id.Kind, "id_subject", id.Subject,
			"id_actor", id.Actor, "id_ref", id.Ref, "scope", scope)
		audit.SetIdentity(r.Context(), id.Kind, id.Subject, id.Actor, id.Ref)
		audit.Event(r.Context(), "token_issued", "")

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"token":        token,
			"access_token": token, // some clients expect the OAuth2 name
			"expires_in":   expiresIn,
			"issued_at":    issuedAt.UTC().Format(time.RFC3339),
		}); err != nil {
			slog.Error("auth writing token response", "err", err)
		}
	})
}

func (a *Authenticator) unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"code": "UNAUTHORIZED", "message": msg}},
	}); err != nil {
		slog.Error("auth writing error response", "err", err)
	}
}

// ChallengeDocker writes the 401 + WWW-Authenticate Bearer challenge that
// points docker clients at the token endpoint.
func (a *Authenticator) ChallengeDocker(w http.ResponseWriter, scope string) {
	challenge := fmt.Sprintf("Bearer realm=%q,service=%q", a.Realm, a.Service)
	if scope != "" {
		challenge += fmt.Sprintf(",scope=%q", scope)
	}
	w.Header().Set("WWW-Authenticate", challenge)
	a.unauthorized(w, "authentication required")
}

// ChallengeBasic writes the 401 + WWW-Authenticate Basic challenge used by
// the PyPI and file endpoints (pip/twine/curl speak Basic, not the docker
// token flow).
func (a *Authenticator) ChallengeBasic(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="archifau"`)
	http.Error(w, "authentication required", http.StatusUnauthorized)
}
