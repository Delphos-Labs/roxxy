# ADR 0001 — archifau identity model and token verification

- **Status:** Accepted
- **Date:** 2026-06-16
- **Deciders:** INF / archifau maintainers
- **Scope:** archifau (the internet-facing origin: docker registry + PyPI + bare files). Does not cover the roxxy proxy or roar.

## Context

archifau authenticates every request from a **GitHub-derived identity**:

- **CI** presents a GitHub Actions **OIDC ID token** (RS256 JWT), audience-bound to
  archifau, verified offline against GitHub's JWKS. Identity = the repository
  (`org/repo`, with `ref`/`workflow` provenance).
- **Humans** present a `gh auth token` / PAT, verified via the GitHub API. Identity
  = the user plus the allowed orgs they belong to, which gate their **read** access
  (humans are read-only — see Decision 5).

After an exchange at `/token`, archifau mints its own short-lived **HS256** session
token; per-request checks are then local.

Several concerns were raised about this design:

1. *"Make auth depend on Keycloak/Zitadel — more k8s-like."*
2. We send **bare bearer tokens** to a service we run, not to GitHub.
3. You can't **downscope** a `gh auth token` into something narrower.
4. We appear to violate **"don't roll your own auth / crypto"** — the OIDC/JWT
   verification (`internal/archifau/auth/oidc.go`) and HS256 minting
   (`mint.go`) are hand-written against `crypto/rsa` and `crypto/hmac`.

Key facts that shaped the decision:

- archifau is a **relying party / resource server**: it *verifies* identities
  issued elsewhere. An IdP (Keycloak/Zitadel) *issues* identities. These are
  different layers.
- Every current principal's identity **already lives in GitHub** (CI, humans) or,
  prospectively, the **k8s cluster** (in-cluster workloads → ServiceAccount OIDC).
  A self-hosted IdP would be a *broker* re-deriving identity we already hold.
- The hand-rolled verifier is, on review, **competent**: it pins `alg=RS256`
  (defeating the alg-confusion / `none` attacks), validates `iss`/`aud`/`exp`/`nbf`,
  fails closed when the audience is unconfigured, and uses constant-time HMAC
  compare. It does **not** implement crypto primitives (those are Go stdlib) and
  does **not** invent a protocol (it speaks standard OIDC + the registry token
  flow). What it *does* do is hand-implement standard JWT/OIDC **verification**,
  the part the maxim legitimately covers.

## Decision

1. **archifau stays a relying party. We do not run a self-hosted IdP
   (Keycloak/Zitadel) at this time.** GitHub remains the issuer for CI and humans;
   the cluster's OIDC issuer is the natural issuer for in-cluster workloads.

2. **Replace the hand-rolled OIDC/JWT verification with a vetted library**
   ([`github.com/coreos/go-oidc/v3`](https://github.com/coreos/go-oidc)), and the
   hand-rolled HS256 minting with `github.com/golang-jwt/jwt/v5`. This is the
   correct application of "don't roll your own": not because the current code is
   known-broken, but because a library keeps verification correct across future
   edits, maintainers, and newly-discovered attacks. **This is the first concrete
   step** (see Implementation).

3. **Move human auth off broad `gh auth token`/PAT to a GitHub App device flow.**
   archifau is registered as a GitHub App with minimal permissions; the
   `docker-credential-archifau` helper runs the device flow and obtains a
   **narrow, App-scoped** user token (GitHub App user tokens are bounded by
   `app ∩ user` permissions), instead of the user's account-wide token. This
   minimizes the blast radius of any credential archifau sees or a client caches.
   *(`gh auth token` cannot be downscoped after the fact — GitHub has no
   token-exchange/downscoping endpoint — so the fix is to obtain a narrow token
   from the start.)*

4. **Keep the verifier multi-issuer-ready.** `go-oidc` verifies per issuer, so
   additional trusted issuers (the k8s cluster OIDC issuer for in-cluster pulls,
   or an external IdP if ever needed) are an additive change, not a rewrite.

5. **Write authority is CI-only; humans are read-only outside a dev sandbox (ECR
   model).** Only OIDC identities (CI) may *write* blessed artifacts — their
   provenance (repository / ref / workflow) is cryptographically verifiable. Humans
   (`gh`/PAT) get **read-only**, org-wide access to the shared store, **plus write
   access to a per-user dev/test sandbox** under a reserved `dev/<login>/*` prefix.
   The sandbox is **firewalled from the trusted path**: CI and lockfile/manifest-
   pinned resolution never consume `dev/*`, so a dev-box build (which carries no
   verifiable provenance) cannot masquerade as a CI-built artifact — it is readable
   but never auto-trusted, and must never enter the cache CI depends on. Upstream
   (third-party) tools are seeded not by human pushes but from a *committed* manifest
   (`tool + os/arch → source URL`) that archifau fetches and content-addresses —
   provenance is the reviewed commit. *(The current code over-grants: humans get
   org-wide write. The read-only + per-user-dev-sandbox model is tracked in INF-1233;
   the committed-manifest seeding lives in the artifact-client / mise plan.)*

## Consequences

**Positive**

- No IdP to operate, secure, HA, patch, or expose, a real asset for an
  internet-facing service. Zero standing infra dependency on the auth hot path.
- Security-critical token verification lives in a vetted, maintained library.
- CI keeps its zero-secret, audience-bound OIDC path (already the ideal model).
- Human credentials shrink to a narrow App-scoped token; the broad account token
  never reaches archifau or a client cred cache.
- Extensible to new issuers without bespoke crypto per issuer.

**Negative / costs (accepted)**

- New dependencies (`go-oidc`, `golang-jwt`) cut against archifau's lean-binary
  ethos. **Accepted:** auth is exactly where "use a vetted library" outranks
  dependency minimalism.
- Requires registering and operating a GitHub App, and a device-flow UX for
  humans.
- archifau still *receives* a GitHub-scoped token at exchange time (it must, to
  verify identity), and even the narrow App token isn't archifau-exclusive the way
  CI's audience-bound OIDC is. Only the minted session token is archifau-only.
- **Airgap is unsolved by this ADR** (see triggers): inside an airgap GitHub's
  OIDC issuer is unreachable, so airgapped deployments need a local issuer story.

## Alternatives considered

- **Self-hosted IdP (Keycloak / Zitadel) now.** Rejected: for principals whose
  identity already lives in GitHub/the cluster, an IdP is a broker hop that adds a
  stateful, internet-facing dependency *and does not address the actual concerns*
  — it doesn't remove bare tokens (clients still send bearer tokens to archifau)
  and it doesn't remove verification (we'd still verify Keycloak's tokens, ideally
  with `go-oidc`). If we do adopt one later, **Zitadel** (Go, lighter, cloud-native)
  is preferred over Keycloak (JVM, heavier).
- **Status quo (keep hand-rolled verification).** Rejected on the
  "don't-roll-your-own" principle: the standing audit burden is not worth it when
  `go-oidc` exists.
- **Fine-grained PATs only.** Partial: manual to provision and still a standing
  account credential. The GitHub App device flow is the better human path.

## When to revisit (triggers for a dedicated IdP)

Adopt an issuer (preferably Zitadel) **only** when a concrete need appears:

1. **Non-GitHub principals** — external partners/customers or machine identities
   not in GitHub or the cluster need to authenticate.
2. **Central SSO/RBAC mandate** across many internal services (MFA, consent,
   central policy, introspection/revocation).
3. **Airgap** — GitHub OIDC is unreachable in-airgap, so airgapped archifau needs
   a local issuer. Even here, evaluate lighter options first: the cluster's own
   OIDC issuer, statically-minted tokens, or mTLS — before standing up an IdP.

## Implementation

_Status: Step 1 shipped in PR #10 (the `go-oidc` / `golang-jwt` swap). Steps 2–3 are not yet built._

### Step 1 — `go-oidc` verification swap (shipped in PR #10)

- **Replace** the internals of `internal/archifau/auth/oidc.go` `OIDCVerifier`
  with `coreos/go-oidc/v3`: construct a provider for the GitHub Actions issuer
  (`https://token.actions.githubusercontent.com`), build a verifier with
  `oidc.Config{ClientID: <ExpectedAud>}`, and verify with
  `verifier.Verify(ctx, rawToken)`. Extract GitHub's custom claims
  (`repository`, `repository_owner`, `ref`, `actor`, `workflow`) via
  `idToken.Claims(&ghClaims)`.
- **Keep unchanged:** the `Identity` struct and ACL (`identity.go`), the
  `AllowedOwners` check, the `UserVerifier` (it's a GitHub API call, no JWT), and
  the `Authenticator` wiring. The public `Verify(token) (*Identity, error)`
  signature stays so callers/tests are unaffected.
- **Minter** (`mint.go`): re-issue/verify the HS256 session token via
  `golang-jwt/jwt/v5` (standardized registered-claims parsing/encoding), keeping the
  same token shape and TTL. The HMAC itself is already stdlib + constant-time; the win
  is not hand-rolling claim/segment handling, not stronger crypto.
- **JWKS caching / discovery / key rotation / `alg` pinning** move to the library;
  `exp`/`nbf` enforcement becomes **unconditional** and clock-skew-aware — the
  hand-rolled verifier checked `exp` only when present (`oidc.go:149`), a gap the swap
  closes.
- **Clock-skew semantics (decided, shipped in #10).** go-oidc has no exp-leeway
  hook and applies a fixed ~5-minute nbf leeway, so `CLOCK_SKEW_SECS` was *retained*
  rather than silently dropped: the verifier hands go-oidc a clock shifted back by
  the skew (`verifyClock`), applying the leeway to `exp` without disabling the
  library's expiry check. The same back-shift *narrows* go-oidc's nbf leeway (a
  larger skew is stricter about not-yet-valid tokens), an unset value still defaults
  to 60s, and `main.go` rejects `CLOCK_SKEW_SECS >= 300`. The README and field godoc
  were updated to match, and new tests pin the exp window and the nbf interaction —
  the original regression tests sat outside both leeway windows and would not have
  caught this drift.
- **Safety net:** the existing `auth_test.go` (valid/expired/wrong-aud/wrong-iss/
  bad-signature/disallowed-owner/audience-array, JWKS caching, mint/verify
  roundtrip + tampering, the ACL table) is the regression gate. The fake-issuer
  harness already served `/.well-known/openid-configuration`; the real change was
  emitting an `issuer` field in that document so `go-oidc`'s `NewProvider`
  issuer-match check passes offline (the discovery path was used, not static
  `oidc.ProviderConfig`). A new test advertises a second signing alg in discovery
  to prove the explicit RS256 pin, not go-oidc's RS256 fallback, is load-bearing.
- **go.mod:** adds `go-oidc/v3`, `golang-jwt/jwt/v5` (+ `go-jose/v4` transitively).
  go-oidc does not link into the proxy binary (it isn't in that binary's import
  graph), but the deps do enter the shared `go.mod`/`go.sum` and the module-wide
  supply-chain surface.

### Step 2 — GitHub App device flow for humans (follow-on PR)

- Register the **archifau GitHub App** with minimal permissions.
- Add a device-flow path to `docker-credential-archifau` (and a corresponding
  verifier path) so humans obtain an App-scoped user token instead of
  `gh auth token`.
- Update `cmd/archifau/README.md` human-auth guidance.
- **Re-validate the read ACL under App tokens.** A GitHub App user-to-server token
  enumerates `/user/orgs` as only the orgs where the App is *installed*, not the
  user's full membership — so before cutover, install the App on each allowed org
  (single org today: `delphos-labs`), or switch the membership check to a GitHub App
  *installation* lookup. Otherwise a currently-authorized human resolves to zero orgs
  and loses read access. (An authorization consequence, not just UX.)

### Step 3 (future, on trigger) — additional issuers

- Add the k8s cluster OIDC issuer (in-cluster ServiceAccount tokens) as a second
  trusted issuer via `go-oidc` when in-cluster pull becomes a primary client; or
  an external issuer per the triggers above.
