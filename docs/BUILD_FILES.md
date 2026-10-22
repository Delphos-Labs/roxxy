# Build files: generated and committed

Which UI files are generated during the build (and gitignored) versus committed
sources. JavaScript minification has **one source of truth**:
`internal/ui/static/build-js.js`, invoked by `go generate`.

## Generated at build time

- `internal/ui/static/js/*.min.js`: minified JavaScript, produced from the
  `js/*.js` sources by `go generate` (the `//go:generate node static/build-js.js`
  directive in `internal/ui/server.go`). `index.html` references these `.min.js`.
- `sbom.json` / `sbom.json.gz`: produced during Docker builds.

These outputs are ignored by Git. Do not commit them.

## Committed sources (SHOULD be committed)

- `internal/ui/static/js/*.js`: unminified JS sources. **Edit these directly.**
  the `.min.js` outputs are regenerated, never hand-edited or minified in place.
- `internal/ui/static/index.html`: references the generated `js/*.min.js`.
- `internal/ui/static/components/**/*.html`: component HTML.
- `internal/ui/static/components/templates/**/*.tmpl`: Go templates.

## Build process

Every build path runs the same `go generate` step before `go build`:

1. **`make build`** → `generate` (`go generate ./...` → `build-js.js`) → `validate`
   → build the binaries.
2. **`build.sh`** (native build outside Docker) → `go generate ./...` → build.
3. **Docker builds** (`docker/Dockerfile`, `Dockerfile.scratch`, `Dockerfile.release`)
   → `go generate ./...` in the builder stage before `go build`.
4. **CI** (`ci.yml`, `release.yml`) → `make generate` in the binary-producing jobs;
   the test job goes through `make build`.

UI generation requires Node.js and the local esbuild package installed by `npm ci`.
`build-js.js` calls esbuild's synchronous API. It does not invoke npm or fetch a missing dependency.
Missing dependencies fail the build instead of producing an interface with missing JavaScript.

## Embedding

`internal/ui/server.go` embeds `internal/ui/static/**` via `//go:embed`. The
embedded set is whatever exists when `go build` runs, so `go generate` must run
first to produce the `js/*.min.js` that `index.html` loads.
