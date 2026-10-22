# archifau

The origin-server build of roxxy: a **docker registry**, a **PyPI registry**,
and a **bare file store** behind GitHub-derived authentication. No proxying,
no TLS interception, one fixed certificate, one listener. The operator selects
the advertised registry hostname and the authorized GitHub owners explicitly.

Only the packages this binary needs are imported — the proxy/UI/SBOM
machinery in the rest of the repo never builds in (binary is ~40% smaller
than roxxy). The docker registry reuses roxxy's `internal/registry` in
airgapped/local-push mode: pushes store locally, pulls never contact an
upstream, `/v2/_catalog` and tag listing work from local manifests.

## Identity model

GitHub is the only identity provider. Two credential kinds are accepted, and
**all authorization derives from the identity**:

| Credential                      | Verified by                              | Identity            | Namespace                  |
| ------------------------------- | ---------------------------------------- | ------------------- | -------------------------- |
| GitHub Actions OIDC token (JWT) | offline, against GitHub's JWKS (cached)  | the repository      | `org/repo` (its own repo)  |
| `gh auth token` / PAT           | GitHub API (`/user`, `/user/orgs`), cached | the user          | each allowed org they're in |

ACL (mirrors the original `docker_auth` design):

- CI on `refs/heads/main`: **push + pull** inside `org/repo/*`.
- CI on any other ref: **pull only**, still confined to `org/repo/*`.
- Org humans: push + pull anywhere in `org/*`.
- PyPI is a flat namespace (like pypi.org): reads for any authenticated
  identity, uploads for write-capable identities (CI@main / org humans).
- Everything else: denied. A job cannot touch another repo's namespace.

After the initial credential exchange at `/token`, archifau mints its own
short-lived (5 min) HS256 token embedding the identity; per-request checks
are then local — no network on the hot path. Simple clients (curl, pip,
twine) can skip the exchange and present the raw GitHub credential as a
Bearer token or Basic password.

## Endpoints

| Path                          | What                                              |
| ----------------------------- | ------------------------------------------------- |
| `/v2/...`                     | docker registry API v2 (push stored locally)      |
| `/token`                      | registry token endpoint (docker login flow)       |
| `/pypi/simple/...`            | PEP 503 index (pip `--index-url`)                 |
| `/pypi/legacy/`               | twine-compatible upload                           |
| `/pypi/packages/<name>/<file>`| package downloads                                 |
| `/files/<org>/<repo>/...`     | bare file PUT/GET/DELETE + JSON dir listing       |
| `/healthz`                    | unauthenticated liveness                          |

Uploads record provenance (identity, ref, sha256, timestamp) in sidecar
metadata (`files-meta/`, `pypi-meta/`) — the OIDC claims are the audit trail.

## Configuration (environment)

Choose a registry address that your clients resolve and whose TLS certificate they trust.
Set `ARCHIFAU_HOSTNAME` and `ALLOWED_OWNERS` before starting the server or rendering Compose configuration.
The examples use reserved domains and placeholder owners. Replace them with your deployment values before authenticating.

```
ARCHIFAU_LISTEN        listen address                       (default ":8443")
ARCHIFAU_HOSTNAME      advertised registry host[:port]      (required)
ARCHIFAU_DATA_DIR      storage root                         (default "./archifau-data")
ARCHIFAU_TLS_CERT/_KEY server certificate + key PEM         (required; or ARCHIFAU_INSECURE_HTTP=1 for tests)
ARCHIFAU_TOKEN_SECRET  token-signing secret                 (default: random per process — MUST be one shared value across all live replicas; random per-process only works for a single instance, and a restart then forces clients to re-exchange at /token)
ARCHIFAU_LOG_LEVEL     "debug" enables debug logs           (default info; all output is JSON lines on stderr)
ARCHIFAU_S3_BUCKET     S3 bucket for the second level       (unset => disabled)
ARCHIFAU_S3_PREFIX     key prefix within that bucket        (default ""; AWS creds/region via the standard AWS_* chain)
ARCHIFAU_MAX_UPLOAD_BYTES per-upload body cap, bytes        (default 0 = unlimited; >0 => 413 on oversize)
ARCHIFAU_READ_TIMEOUT_SECS  whole-request read timeout      (default 0 = off; large blobs need it off)
ARCHIFAU_WRITE_TIMEOUT_SECS response write timeout          (default 0 = off; large blobs need it off)
EXPECTED_AUD           OIDC audience to require             (default: ARCHIFAU_HOSTNAME)
ALLOWED_OWNERS         comma-separated GitHub orgs/users    (required)
CLOCK_SKEW_SECS        OIDC exp leeway; also narrows the 5m nbf leeway, so larger = stricter on not-yet-valid tokens; must be < 300 (default 60)
TOKEN_TTL_SECS         minted token lifetime                (default 300)
GITHUB_OIDC_ISSUER     issuer override                      (tests only)
GITHUB_API             GitHub API base override             (tests only)
ARCHIFAU_S3_ENDPOINT   custom S3 endpoint                   (tests only)
```

## Upgrade from implicit configuration

This release removes implicit registry and certificate hostnames and the Compose owner fallback.
Before deploying the new image or binary:

1. Inspect the existing deployment configuration. Set `ARCHIFAU_HOSTNAME` explicitly to the registry host clients already use, including its port when not 443.
2. Set `ALLOWED_OWNERS` explicitly to the GitHub owners already authorized for the registry. Do not copy placeholder owners.
3. Run `docker compose config --quiet` with the deployment environment to validate Compose inputs. Do not publish rendered configuration or secrets.
4. Confirm the registry hostname matches the existing TLS certificate and credential-helper configuration. Do not rename the registry as part of this upgrade.
5. Pass `-hostname <certificate-host>` when running `archifau gencert`. Existing certificates do not need replacement solely for this configuration change.
6. Restart only after these checks pass, then verify `/healthz` and an authenticated registry request through the normal deployment process.

The server refuses to start without `ARCHIFAU_HOSTNAME`.
Compose refuses to render without `ARCHIFAU_HOSTNAME` and `ALLOWED_OWNERS`.
Deployment owners must confirm these values before upgrading. Source tests do not establish live configuration.

## Logging

Everything goes to stderr as JSON lines (12-factor). Each request emits one
`"msg":"access"` record with weblog fields (`remote_addr`, `method`, `path`,
`status`, `bytes`, `duration_ms`, `user_agent`) plus the authenticated
identity (`id_kind`, `id_subject`, `id_actor`, `id_ref`) and the audit
outcome (`event` = authenticated/denied/anonymous/upload/download/delete,
`sha256`, `object_size`). Token issuance and rejection are separate
`auth token issued` / `auth token rejected` records. Lines from the shared
registry code are bridged into the same stream tagged `"source":"stdlog"`.
`/healthz` logs at debug only, so healthcheck polling stays out of the
default stream.

## S3 second level

With `ARCHIFAU_S3_BUCKET` set, S3 is the durable store and the local data
dir becomes a hot cache (the bucket is terraform-managed; archifau only
consumes it — versioning on, since name→content pointers are overwritable
on the files surface):

```
cas/sha256/<aa>/<bb>/<digest>                  immutable content (dedup across surfaces)
pypi-meta/<name>/<file>.json                   name index, mirrors the local sidecars
files-meta/<org>/<repo>/<path>.json
```

- **Write-through**: uploads return 201/200 after the local commit, then
  replicate asynchronously (CAS blob first, then the meta pointer). Until
  the PutObject lands, the only copy is local disk — that window is the
  accepted durability gap. Replication serializes per name and re-reads
  local state at send time, so concurrent writes to one file name converge
  on the last local commit; the losing content stays recoverable in the
  bucket's meta version history.
- **Read-through**: a local miss streams the object from S3 to the client
  while a verified copy (sha256 checked against the meta pointer) fills the
  local cache. Mismatched or truncated fetches are never committed locally;
  the client sees a failed transfer via the Content-Length mismatch.
- **Deletes** remove the S3 meta pointer but never CAS blobs (immutable,
  possibly shared).
- **Known gap**: index pages and directory listings reflect local content
  only. On a cold node a package is fetchable by exact name (which re-warms
  the index) but not yet discoverable via `/pypi/simple/`. A future
  `archifau sync` subcommand will warm a cold node and reconcile missed
  replications.

## Certificate

Use a server certificate for the DNS name in your advertised registry address.
The required `-hostname` argument is the certificate name, without a scheme or port.
For local testing, sign with a CA that your clients already trust.
An installed Roxxy CA can be used. Supply its certificate and key paths for your platform:

```sh
mkdir -p cmd/archifau/certs
archifau gencert \
  -ca-cert "${CA_CERT:?Set CA_CERT to your trusted CA certificate}" \
  -ca-key "${CA_KEY:?Set CA_KEY to that CA private key}" \
  -hostname registry.example.com -out-prefix cmd/archifau/certs/archifau
```

Verify trust in Docker's registry and token clients, not only in curl.
Trust configuration differs between Docker Engine and Docker Desktop installations.

## Build & run

The Compose file in this directory is the canonical deployment.
Put the certificate pair in `cmd/archifau/certs/`, then set your registry address and allowed GitHub owners.
For example, after replacing the domain and owner below:

```sh
export ARCHIFAU_HOSTNAME=registry.example.com:8443
export ALLOWED_OWNERS=example-owner
docker compose -f cmd/archifau/docker-compose.yaml config -q
docker compose -f cmd/archifau/docker-compose.yaml up -d --wait
```

Port 8443 and the `archifau-data` named volume remain defaults.
There is no hostname or authorization-owner default. Compose rejects unset or empty values.
If clients connect on a non-443 port, include that port in `ARCHIFAU_HOSTNAME` so the advertised token realm matches.
Other settings include `ARCHIFAU_PORT`, `ARCHIFAU_CERTS_DIR`, and `ARCHIFAU_TOKEN_SECRET`.

Deployments upgrading from implicit settings must explicitly supply their existing hostname and owner list.
Keep those values unchanged to preserve existing token audiences and hostname-derived storage paths.

Building behind a TLS-intercepting proxy (roxxy itself, corporate MITM):
drop the intercepting CA as a `*.crt` into `docker/archifau/ca-certs/`
before building — both build and runtime stages trust it. Note the live
proxy's CA may differ from `certs/ca-cert.pem` in the repo checkout (e.g. a
long-running instance using `~/Library/Application Support/roxxy/certs/`).

Plain `go build ./cmd/archifau` and a bare
`docker build -f docker/archifau/Dockerfile -t archifau .` also work.

## CI usage

```yaml
env:
  ARCHIFAU_HOSTNAME: ${{ vars.ARCHIFAU_HOSTNAME }}
permissions:
  id-token: write
steps:
  - name: Log in with the job's OIDC identity
    run: |
      : "${ARCHIFAU_HOSTNAME:?Configure your registry hostname}"
      OIDC_TOKEN=$(curl -fsSL -H "Authorization: bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}" \
        "${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=${ARCHIFAU_HOSTNAME}" | jq -r '.value')
      echo "${OIDC_TOKEN}" | docker login "${ARCHIFAU_HOSTNAME}" -u github-oidc --password-stdin
  - run: docker push "${ARCHIFAU_HOSTNAME}/${GITHUB_REPOSITORY,,}/app:${GITHUB_SHA}"
```

For long jobs, install `docker-credential-archifau` instead of `docker login`
(`{"credHelpers": {"registry.example.com:8443": "archifau"}}` in
`~/.docker/config.json`) — it fetches a fresh OIDC token per operation, so
nothing expires mid-push. Outside Actions it falls back to `gh auth token`,
which is also what humans use directly:

Replace the helper key with your configured registry host and port.

```sh
: "${ARCHIFAU_HOSTNAME:?Set your registry hostname}"
gh auth token | docker login "${ARCHIFAU_HOSTNAME}" -u "$(gh api user -q .login)" --password-stdin
pip install --index-url "https://x:$(gh auth token)@${ARCHIFAU_HOSTNAME}/pypi/simple/" widget-tools
curl -u "x:$(gh auth token)" -T dump.parquet "https://${ARCHIFAU_HOSTNAME}/files/example-owner/widget/dumps/dump.parquet"
```

## PyPI

No PyPI-specific credential exists — the GitHub credential goes in the Basic
password (the username is ignored). Reads are open to any authenticated
identity; uploads require a write-capable one (CI on `main`, or an org
human), since the namespace is flat like pypi.org.

```sh
# install (human)
pip install --index-url "https://x:$(gh auth token)@${ARCHIFAU_HOSTNAME:?}/pypi/simple/" widget-tools

# publish (human)
TWINE_USERNAME=x TWINE_PASSWORD=$(gh auth token) \
  twine upload --repository-url "https://${ARCHIFAU_HOSTNAME:?}/pypi/legacy/" dist/*
```

From GitHub Actions, publish with the job's OIDC identity — uploads succeed
only from `refs/heads/main`, any other ref gets 403:

```yaml
env:
  ARCHIFAU_HOSTNAME: ${{ vars.ARCHIFAU_HOSTNAME }}
permissions:
  id-token: write
steps:
  - run: |
      : "${ARCHIFAU_HOSTNAME:?Configure your registry hostname}"
      OIDC_TOKEN=$(curl -fsSL -H "Authorization: bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}" \
        "${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=${ARCHIFAU_HOSTNAME}" | jq -r '.value')
      TWINE_USERNAME=x TWINE_PASSWORD="${OIDC_TOKEN}" \
        twine upload --repository-url "https://${ARCHIFAU_HOSTNAME}/pypi/legacy/" dist/*
```

## Bare files

`/files/<org>/<repo>/...` is plain HTTP PUT/GET/DELETE — for release
binaries, datasets, vendor installers (Binary Ninja and friends), anything
that isn't an image or a wheel. Unlike PyPI, authorization here is the path
prefix: CI writes only under its own `org/repo/`, org humans anywhere under
their `org/`. A `Content-Type` set on upload is stored and replayed on
download.

```sh
BASE="https://${ARCHIFAU_HOSTNAME:?}/files/example-owner/roxxy"

# upload — 201; response body and X-Content-SHA256 header carry the digest
curl -u "x:$(gh auth token)" -T roar-linux-amd64 \
  "$BASE/releases/v1.2.3/roar-linux-amd64"

# download
curl -fsSL -u "x:$(gh auth token)" -o roar "$BASE/releases/v1.2.3/roar-linux-amd64"

# list a directory — JSON {"path": ..., "entries": [...]}
curl -u "x:$(gh auth token)" "$BASE/releases/"

# delete
curl -u "x:$(gh auth token)" -X DELETE "$BASE/releases/v1.2.3/roar-linux-amd64"
```

From CI, same OIDC token as the PyPI example, confined to the job's repo:

```yaml
  - run: |
      curl -fsS -u "x:${OIDC_TOKEN}" -T dist/app \
        "https://${ARCHIFAU_HOSTNAME:?}/files/${GITHUB_REPOSITORY,,}/builds/${GITHUB_SHA}/app"
```

## Verification status

- `go build` / `go vet` / `go test ./internal/archifau/...` — green.
- Unit tests cover: OIDC valid/expired/wrong-aud/wrong-iss/bad-signature/
  disallowed-owner/audience-array, JWKS caching, gh-token org membership +
  caching, mint/verify roundtrip + tampering, the full ACL table, and
  handler-level push/pull/upload/denial flows for all three surfaces.
- End-to-end (containerized archifau + fake OIDC issuer): real `docker
  login`/`push`/`pull`, 403 on non-main-ref push, 403 on foreign-namespace
  push, login rejected for disallowed owner; wheel upload via the twine API
  and a real `pip install` from the index; file PUT/GET/list/DELETE with
  namespace denials. Human path verified against the **real** GitHub API
  with a live `gh auth token`.

## Security posture (docker registry)

- `/v2/_catalog` is filtered per request to the caller's readable
  namespaces, so it cannot enumerate other tenants' repositories. The
  airgapped cached-catalog fast path is bypassed whenever the authorizer
  is set (the cache is unfiltered).
- Cross-repo blob mounts (`?mount=<digest>&from=<repo>`) are honored only
  when the caller can read a well-formed `from=` repo **and** the digest is
  actually a member of a manifest stored in `from=` — the attacker-controlled
  `from=` alone is not trusted.
- Known gap: blobs live in a single global content-addressable store, and
  a by-digest read (`GET /v2/<own-repo>/blobs/<digest>`) is authorized only
  on the URL repo, not on blob membership. So a caller who knows a foreign
  digest can still read the raw bytes via its own repo. Gating by-digest
  reads on membership is a separate follow-up (tracked, not in this PR).

## Known gaps / next steps

- Minted tokens default to a per-process secret: restarts invalidate
  in-flight tokens (clients re-exchange automatically). Pin
  `ARCHIFAU_TOKEN_SECRET` for multi-replica setups.
- PyPI uploads are flat-namespace; no per-project ownership yet.
- Swap in the official certificate at go-live (config change only).
