package auth

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// defaultUserTTL is how long a validated user token's identity is cached
// before re-checking with the GitHub API.
const defaultUserTTL = 5 * time.Minute

// UserVerifier validates GitHub user tokens (gh auth token / PATs) by calling
// the GitHub API: GET /user identifies the caller, GET /user/orgs establishes
// which allowed orgs they belong to. Results are cached briefly, keyed by a
// hash of the token, so hot paths don't hit api.github.com per request.
//
// Note: org membership discovery needs the token to carry the read:org scope
// (gh auth token has it by default). A user whose own login is an allowed
// owner needs no org scope at all.
type UserVerifier struct {
	// APIBase is the GitHub API root. Defaults to https://api.github.com.
	// Override in tests to point at an httptest server.
	APIBase string
	// AllowedOwners is the set of orgs/users (lowercase) whose members may
	// authenticate. Required — an empty list denies all user tokens.
	AllowedOwners []string
	// Client is the HTTP client for API calls.
	Client *http.Client
	// TTL is how long validated identities are cached (default 5m).
	TTL time.Duration
	// Now is the clock, injectable for tests.
	Now func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]userCacheEntry
}

type userCacheEntry struct {
	id  *Identity
	exp time.Time
}

func (v *UserVerifier) apiBase() string {
	if v.APIBase != "" {
		return strings.TrimSuffix(v.APIBase, "/")
	}
	return "https://api.github.com"
}

func (v *UserVerifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *UserVerifier) client() *http.Client {
	if v.Client != nil {
		return v.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (v *UserVerifier) ttl() time.Duration {
	if v.TTL > 0 {
		return v.TTL
	}
	return defaultUserTTL
}

// Verify validates the user token and returns the resulting Identity with
// one namespace per allowed owner the user belongs to.
func (v *UserVerifier) Verify(token string) (*Identity, error) {
	if len(v.AllowedOwners) == 0 {
		return nil, fmt.Errorf("user: no allowed owners configured")
	}

	key := sha256.Sum256([]byte(token))
	v.mu.Lock()
	if e, ok := v.cache[key]; ok && v.now().Before(e.exp) {
		v.mu.Unlock()
		return e.id, nil
	}
	v.mu.Unlock()

	login, err := v.fetchLogin(token)
	if err != nil {
		return nil, err
	}

	var namespaces []string
	if containsTrimmed(v.AllowedOwners, login) {
		namespaces = append(namespaces, login)
	}

	orgs, err := v.fetchOrgs(token)
	if err != nil {
		// Tokens without read:org can still authenticate when the user's
		// own login is an allowed owner; otherwise surface the error.
		if len(namespaces) == 0 {
			return nil, err
		}
		// Discard any partial page so behavior matches the log: a mid-pagination
		// failure must not silently grant a subset of orgs (login + an arbitrary
		// prefix), which would cache an incomplete identity for the full TTL and
		// flap 403s on the orgs that didn't make it. Proceed strictly login-only.
		orgs = nil
		slog.Warn("user: org listing failed; proceeding with login-only identity, org namespaces skipped",
			"login", login, "err", err)
	}
	for _, org := range orgs {
		if containsTrimmed(v.AllowedOwners, org) && !containsTrimmed(namespaces, org) {
			namespaces = append(namespaces, org)
		}
	}

	if len(namespaces) == 0 {
		return nil, fmt.Errorf("user: %q is not a member of any allowed owner", login)
	}

	id := &Identity{
		Kind:       "user",
		Subject:    login,
		Actor:      login,
		Namespaces: namespaces,
	}

	v.mu.Lock()
	if v.cache == nil {
		v.cache = make(map[[32]byte]userCacheEntry)
	}
	v.cache[key] = userCacheEntry{id: id, exp: v.now().Add(v.ttl())}
	v.mu.Unlock()

	return id, nil
}

func (v *UserVerifier) fetchLogin(token string) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	if err := v.getJSON(token, v.apiBase()+"/user", &user); err != nil {
		return "", fmt.Errorf("user: identifying token: %w", err)
	}
	if user.Login == "" {
		return "", fmt.Errorf("user: GitHub returned no login for token")
	}
	return strings.ToLower(user.Login), nil
}

// fetchOrgs lists the orgs the token's user belongs to (requires read:org).
func (v *UserVerifier) fetchOrgs(token string) ([]string, error) {
	var orgs []string
	// Allowed-owner lists are short; a couple of pages is plenty.
	for page := 1; page <= 5; page++ {
		var batch []struct {
			Login string `json:"login"`
		}
		url := fmt.Sprintf("%s/user/orgs?per_page=100&page=%d", v.apiBase(), page)
		if err := v.getJSON(token, url, &batch); err != nil {
			return orgs, fmt.Errorf("user: listing orgs: %w", err)
		}
		for _, o := range batch {
			orgs = append(orgs, strings.ToLower(o.Login))
		}
		if len(batch) < 100 {
			break
		}
	}
	return orgs, nil
}

func (v *UserVerifier) getJSON(token, url string, out any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := v.client().Do(req)
	if err != nil {
		return err
	}
	defer util.DeferClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
