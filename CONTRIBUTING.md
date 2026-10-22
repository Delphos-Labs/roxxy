# Contributing to Roxxy

Use [Delphos-Labs/roxxy](https://github.com/Delphos-Labs/roxxy) for issues and pull requests.
Read [SUPPORT.md](SUPPORT.md) before asking for help.
Follow [SECURITY.md](SECURITY.md) for vulnerabilities. Do not disclose them in public issues or pull requests.

## Propose a change

For a large change, open an issue that explains the problem and expected behavior before implementation.
For a bug, include the affected version, a minimal reproduction, and the expected result.
Remove credentials, private registry addresses, certificate keys, and confidential data from examples and logs.

Fork the repository and submit a focused pull request against `main`.
Explain the change, its user impact, and the checks you ran.
State any checks you could not run. Include documentation when behavior changes.
Only submit material you have authority to contribute under the repository's [license](LICENSE).
Preserve third-party notices and identify third-party code or assets you add.

## Build and check

Read [AGENTS.md](AGENTS.md) for repository conventions.
Install Go 1.25 or newer, Node.js with npm, Python 3, GNU Make, and golangci-lint compatible with the repository configuration.
CI currently uses Node.js 20 and golangci-lint v2.14.0.

```bash
npm ci
make build
make validate
go test -race ./...
golangci-lint run
node --test test/test-build-js.js
python3 test/test-release-checksums.py
```

Run `gofmt` on changed Go files. Add behavioral tests for changed behavior.
JavaScript builds use the locally installed esbuild API. Run `npm ci` first. Missing dependencies must not trigger a registry download.
See [generated build files](docs/BUILD_FILES.md) for source and generated-asset boundaries.
For security controls, include an adversarial test that fails when the control is removed.
Do not use `build.sh` for ordinary contribution checks. Its default path can publish a container image.

## Review and merge

An independent maintainer must review a change before merge. Authors do not approve their own changes.
Required CI checks must pass, and review conversations must be resolved.
Code Wizards (`@Delphos-Labs/code-wizards`) owns all paths.
Faye Salwin (`@beiriannydd`) can be added as an owner after she accepts write-capable repository access.

Ownership declarations do not grant permissions. Branch protection must require code-owner review to enforce ownership approval.
See [MAINTAINERS.md](MAINTAINERS.md) for the intended roles and activation requirements.
Maintainers can request changes or decline a proposal. No review or response deadline is promised.
