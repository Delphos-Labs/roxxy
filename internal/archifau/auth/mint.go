package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// defaultTokenTTL matches the original docker_auth design: short-lived
// registry tokens, re-fetched by clients as needed.
const defaultTokenTTL = 5 * time.Minute

// Minter issues and verifies archifau's own short-lived bearer tokens
// (HS256 JWTs embedding the authenticated Identity). After the initial
// GitHub credential exchange at /token, every request is verified locally
// against these — no network access on the hot path.
type Minter struct {
	// Secret signs the tokens. With an empty config the caller should
	// generate a random per-process secret; set it explicitly only to keep
	// tokens valid across restarts/replicas.
	Secret []byte
	// Issuer names the token issuer (default "archifau").
	Issuer string
	// Service is the audience — the registry's public hostname.
	Service string
	// TTL is the token lifetime (default 5m). Deliberately short: the
	// GitHub credentials behind it are themselves short-lived or revocable.
	TTL time.Duration
	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// mintedClaims carries the standard registered claims (iss/aud/sub/iat/exp)
// plus the embedded Identity. Under golang-jwt v5 — whose package-global
// MarshalSingleStringAsArray defaults to true — the single-element audience
// serializes as a JSON array (["<service>"]), not a bare string, and exp/iat
// as numeric unix-seconds. Verify accepts both the array and bare-string
// audience forms (jwt.ClaimStrings.UnmarshalJSON), so reads stay compatible
// with either shape; only this minter and Minter.Verify produce/consume these
// tokens, so the array encoding is internal.
type mintedClaims struct {
	Identity *Identity `json:"identity"`
	jwt.RegisteredClaims
}

func (m *Minter) issuer() string {
	if m.Issuer != "" {
		return m.Issuer
	}
	return "archifau"
}

func (m *Minter) ttl() time.Duration {
	if m.TTL > 0 {
		return m.TTL
	}
	return defaultTokenTTL
}

func (m *Minter) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Mint issues a token for the identity. Returns the token, its lifetime in
// seconds, and the issue time (the shape the docker token endpoint needs).
func (m *Minter) Mint(id *Identity) (token string, expiresIn int, issuedAt time.Time, err error) {
	if len(m.Secret) == 0 {
		return "", 0, time.Time{}, fmt.Errorf("mint: no signing secret configured")
	}
	issuedAt = m.now()
	claims := mintedClaims{
		Identity: id,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer(),
			Audience:  jwt.ClaimStrings{m.Service},
			Subject:   id.Subject,
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			ExpiresAt: jwt.NewNumericDate(issuedAt.Add(m.ttl())),
		},
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.Secret)
	if err != nil {
		return "", 0, time.Time{}, fmt.Errorf("mint: signing token: %w", err)
	}
	return signed, int(m.ttl().Seconds()), issuedAt, nil
}

// Verify checks a minted token and returns the embedded Identity. It rejects
// tampered signatures, expired tokens, wrong issuer/audience, and any signing
// method other than HS256.
func (m *Minter) Verify(token string) (*Identity, error) {
	if len(m.Secret) == 0 {
		return nil, fmt.Errorf("mint: no signing secret configured")
	}

	var claims mintedClaims
	// keyFunc pins HMAC: any non-HMAC alg (e.g. an RS256/none downgrade) is
	// rejected before the secret is ever used as a key. WithValidMethods
	// further restricts the accepted alg header to HS256.
	keyFunc := func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("mint: unexpected signing method %q", t.Header["alg"])
		}
		return m.Secret, nil
	}

	parsed, err := jwt.ParseWithClaims(token, &claims, keyFunc,
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(m.issuer()),
		jwt.WithAudience(m.Service),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(m.now),
	)
	if err != nil {
		return nil, fmt.Errorf("mint: %w", err)
	}
	if !parsed.Valid {
		return nil, fmt.Errorf("mint: token invalid")
	}
	if claims.Identity == nil {
		return nil, fmt.Errorf("mint: token has no identity")
	}
	return claims.Identity, nil
}
