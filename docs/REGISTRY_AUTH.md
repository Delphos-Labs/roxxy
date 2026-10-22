# Registry Authentication

`roar push` (and `roxxy docker push`) authenticate to remote registries using
your Docker config file at `$DOCKER_CONFIG/config.json` (default
`~/.docker/config.json`). No registry-specific flags are required — if
`docker push` works for the destination, roar push works.

## Resolution order

For a given registry hostname, credentials are resolved in this order:

1. **`credHelpers[<host>]`** — exec `docker-credential-<helper> get` and pass
   the host on stdin. The helper returns a JSON object with `Username` and
   `Secret` fields.
2. **`auths[<host>]`** — base64-decode the `auth` field as `username:password`,
   or use the explicit `username` / `password` fields.
3. **`credsStore`** — global fallback helper, same protocol as `credHelpers`.
4. **Anonymous** — no credentials sent. The registry's WWW-Authenticate
   challenge will surface a 401 if it required auth.

For each resolved credential, the client sends Basic auth on the initial
request. If the registry responds 401 with `WWW-Authenticate: Bearer realm=...`,
the client exchanges the Basic credential at the realm URL for a Bearer
token, then retries. This is the same flow `docker push` and `oras push`
use — it works for ECR, GHCR, GCR, ACR, Quay, Docker Hub, and any other
v2-compliant registry.

## ECR

Two equivalent options:

**Static credentials (12-hour validity):**

```bash
aws ecr get-login-password --region us-east-1 \
  | docker login --username AWS --password-stdin 123456789012.dkr.ecr.us-east-1.amazonaws.com
```

This writes a base64 `auth` entry into `auths` and is the simplest path for
one-off pushes.

**Credential helper (recommended for long-running use):**

Install [`docker-credential-ecr-login`](https://github.com/awslabs/amazon-ecr-credential-helper)
and add to `~/.docker/config.json`:

```json
{
  "credHelpers": {
    "123456789012.dkr.ecr.us-east-1.amazonaws.com": "ecr-login"
  }
}
```

The helper invokes the AWS SDK on each call, so credentials never go stale
and roar respects whatever AWS profile / IAM role / IRSA is active in the
environment.

## GHCR

```bash
echo $GITHUB_TOKEN | docker login ghcr.io -u <username> --password-stdin
```

The token must have `write:packages` scope.

## Docker Hub

```bash
docker login
```

## Localhost (roxxy itself)

When pushing back to a roxxy proxy on `localhost`, roar trusts the roxxy CA
certificate at `$ROXXY_CERTS_DIR/ca-cert.pem` (run `roxxy certs generate` if
it doesn't exist). If the CA certificate isn't found, roar falls back to
`InsecureSkipVerify` for `localhost` only and warns on stderr — this is to
keep development workflows unblocked. Non-localhost pushes always use the
system trust store.

## Cleartext (plain HTTP) registries

roar refuses to send registry credentials in cleartext over plain `http://` to a
non-loopback host — a credentialed push to `http://reg.example.com` fails fast with
*"refusing to send registry credentials in cleartext ... over http://"* rather than
leaking the token on the wire. Exceptions:

- **Loopback is always allowed** — any `127.0.0.0/8` address, `::1`, or `localhost`
  (local dev registries).
- **`ROAR_ALLOW_INSECURE_HTTP=1`** opts in to cleartext credentials for a
  non-loopback host on a trusted network (the analogue of archifau's
  `ARCHIFAU_INSECURE_HTTP`). The variable is process-wide — set it per-invocation,
  not globally, so it can't silently weaken a later production push.

`https://` is always permitted. (roar also refuses to follow a cross-origin or
https→http-downgrade redirect with credentials attached.)

## Troubleshooting

- **"401 Unauthorized" on push** — check that the host string in your
  `~/.docker/config.json` matches the registry argument exactly. ECR is
  registered as the bare host (`123.dkr.ecr.us-east-1.amazonaws.com`); Docker
  Hub uses the legacy `https://index.docker.io/v1/` key. roar tries both
  shapes automatically, but a mismatch elsewhere will fall back to anonymous.
- **"403 Forbidden" on a blob upload** — the credential is valid but the
  identity lacks `ecr:PutImage` / equivalent push permission for the target
  repository. This is an IAM/RBAC issue, not a roar bug.
- **Credential helper not found** — make sure `docker-credential-<helper>` is
  on `$PATH`. roar runs the helper as a subprocess; it doesn't link any
  cloud SDKs.
