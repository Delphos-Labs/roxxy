# AGENTS.md

Working notes for agents and humans in this repo. `README.md` owns **what / why**;
this file owns **how** — the build/test/lint commands and the conventions that
aren't obvious from reading a single file.

## Repository shape

One Go module (`github.com/Delphos-Labs/roxxy`, Go 1.25) producing three roles from a
shared core under `internal/`:

- **roxxy** (`main.go`) — the caching / airgap HTTPS proxy the README describes.
- **roar** (`cmd/roar`) — the airgap bundler (export / import / push image + chart bundles).
- **archifau** (`cmd/archifau`) — the internet-facing origin registry (docker + PyPI +
  bare files behind GitHub-derived auth). It is a *stripped* build: it imports only the
  packages it needs, so the proxy / UI / SBOM machinery never links in. The auth deps
  (`go-oidc`, `golang-jwt`) link into archifau only — not the proxy binary — though they
  do enter the shared `go.mod` / `go.sum`. Clients fetch fresh per-request credentials via
  the `cmd/docker-credential-archifau` helper.

Other `cmd/*` are operational tools (cache inspection, license checks, refcount fixes, …).

## Build / test / lint

- **Build:** `make build` (`go generate` + template/JS validation, then builds roxxy +
  roar). archifau: `go build ./cmd/archifau`.
- **Test:** `go test -race ./...` (CI runs `-race` with coverage). Prefer **behavioral
  tests against `httptest` fakes** over table tests — the fake-issuer OIDC harness in
  `internal/archifau/auth` and the httptest registry harness in `internal/registry` are
  the model. New concurrent code should come with a `-race` test.
- **Lint:** `golangci-lint run` (config `.golangci.yml`) must be clean, and `gofmt` is
  enforced — run `gofmt -w` before committing.
- **Templates** (proxy UI only): `make validate`.

## Conventions worth knowing

- **Build-time stamp:** a `main` that wants a version uses
  `-ldflags "-X main.BuildTime=…"` and must declare `var BuildTime = "unknown"` (the
  linker silently ignores `-X` for a missing symbol). Surface it (e.g. a `version` verb)
  so the var isn't flagged unused.
- **Error handling:** write-path `Close()` errors that gate data integrity are checked
  explicitly; only read-side closers use `util.DeferClose`. Don't swallow errors.
- **`internal/commands` exits via `os.Exit`** as the package-wide error idiom (~175
  sites). That code is tested *behaviorally* (assert outcomes / exit gates), not by
  unit-testing exit paths; a clean "only `main` exits" refactor is a known, deferred,
  cross-cutting change — don't half-do it.
- **Content-addressed storage:** artifacts are keyed by sha256 and downloads are verified
  against the digest before they're committed to local storage.
- **Design notes and decisions** live in `docs/` (e.g. `docs/REGISTRY_AUTH.md` for the
  auth model); architecture decisions are recorded as ADRs under `docs/adr/`.

## Security review — vulnerabilities are present until proven absent

For code that handles **credentials, auth, or untrusted input** (an HTTP client that
attaches tokens; URL / redirect / `Location` parsing; signature or permission checks),
read-only review — including the multi-agent pre-submit pass — is **not** enough. It
judges what's on the page and tends to trust reassuring comments, so it misses *absent*
controls and parsing gotchas. Before pushing such code, prove the absence of the bug:

- **Write the failing exploit first.** Construct the adversarial input and run it: feed
  host-confusion strings (`@evil.com`, `.evil.com`, `//evil.com`) to a URL resolver;
  stand up an `httptest` server that redirects a credentialed request cross-origin and
  assert the `Authorization` header never arrives; force the concurrent/rollback race.
  The test must **fail without the control** (non-vacuous), or it proves nothing.
- **Audit the negative space** — ask "what control *should* be here and isn't?" For an
  HTTP client carrying credentials: is `CheckRedirect` set (Go's default re-sends auth on
  a same-host https→http downgrade and on subdomains)? Is every credential send-path
  same-origin-checked? Are URLs parsed via `url.ResolveReference`, never concatenated?
- **Distrust security-claim comments** ("same origin by construction") — verify them, don't accept them.
- **Confirm the test harness can even produce the threat** (a fake server that emits 3xx
  / malformed Locations). If it can't, that's a gap to fix, not a reason to skip the test.

Yes, this means writing more tests. That's the point: a security control with no test
that fails when the control is removed is unverified.

## Safety

- The canonical repo is `Delphos-Labs/roxxy`. Treat its branches as shared: rebase or
  force-push only deliberately and with `--force-with-lease`.
- Shared infrastructure (S3 buckets, etc.) is **terraform-managed** — don't provision it
  ad hoc from the CLI.
