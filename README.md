# Roxxy - Intelligent Registry and Package Manager Proxy

Roxxy is a transparent HTTPS proxy that caches Docker registry images and package manager artifacts (APT, APK, NPM, PyPI, Go modules) locally. It intelligently routes requests, caches content, and can operate in airgapped environments.

Canonical repository: [Delphos-Labs/roxxy](https://github.com/Delphos-Labs/roxxy).
See [Contributing](CONTRIBUTING.md), [Maintainers](MAINTAINERS.md),
[Support](SUPPORT.md), and [Security](SECURITY.md) for project guidance.

## Features

- **Single Proxy Endpoint**: Configure clients with one `HTTP_PROXY` setting
- **Multi-Protocol Support**: Docker registries, APT, APK, NPM, PyPI, Go modules
- **Local Filesystem Cache**: All artifacts cached locally, no external dependencies
- **TLS Interception**: Terminates HTTPS to inspect and route requests
- **Airgap Support**: Can operate in airgapped mode or prepare caches for airgap deployment
- **Proxy Chaining**: Chain multiple roxxy instances together (airgap-proxy → system → internet)
- **Docker Build Integration**: Works transparently with `docker build` - no special buildx setup needed
- **Lightweight**: ~10MB Go binary, minimal resource usage
- **Web UI**: Built-in dashboard for viewing cached content
- **Service Management**: Install as system service for automatic startup
- **Cache Bootstrap**: Initialize cache from tarball on first startup (PostgreSQL-style)

## Quick Start

### Local Installation

First build from a clean checkout. Install Git, Go 1.25 or newer, Node.js with npm,
and GNU Make. Node.js 20 is the version currently used by CI.

```bash
git clone https://github.com/Delphos-Labs/roxxy.git
cd roxxy
npm ci
make build
make validate
```

`npm ci` installs the locked JavaScript build dependencies. `make build` generates
embedded UI assets, validates templates and JavaScript, and builds `roxxy` and `roar`.
`make validate` can also run validation separately. These commands do not publish artifacts.

**macOS/Linux:**
```bash
# Configure and install the binary built above
./setup-native.sh

# Start locally, or use the installed binary if it is on PATH
./roxxy server
```

**Windows:**
```powershell
# Build first with npm ci and make build in Git Bash (outputs roxxy.exe)

# Install (interactive - prompts to add to PATH)
powershell -ExecutionPolicy Bypass -File scripts\install.ps1

# Start roxxy
roxxy server
```

**Manual Installation (macOS/Linux):**
```bash
# Build with generated UI assets
npm ci
make build

# Install to system path (optional)
sudo cp roxxy /usr/local/bin/
```

The setup script creates:
- Config directory: `~/.roxxy/` (Linux) or `~/Library/Application Support/roxxy/` (macOS)
- Cache directory: `~/.cache/roxxy/` (Linux) or `~/Library/Application Support/roxxy/Cache/` (macOS)
- Certificates directory: `~/.roxxy/certs/`
- Default config file: `~/.roxxy/config.yaml`

**Start Roxxy:**
```bash
# Run directly
roxxy server

# Or install as a service (starts automatically)
roxxy service install
```

**Install CA Certificate:**
```bash
# Generate CA certificate (if not already generated)
roxxy certs generate

# Install CA certificate in system trust store
roxxy certs install
```

**Container Deployment (Automatic Cert Generation):**
```bash
# Start server with automatic certificate generation and installation
# This generates the CA cert if missing and adds it to the container's trust store
roxxy server --generate

# In Docker Compose or Dockerfile CMD:
CMD ["server", "--generate"]
```

The `--generate` flag:
- Generates roxxy's CA certificate if it doesn't exist
- Adds it to the system trust store (`/usr/local/share/ca-certificates/` on Alpine/Debian)
- Updates the certificate bundle (`/etc/ssl/certs/ca-certificates.crt`)
- Starts the server normally

This ensures that:
- Roxxy can verify upstream TLS connections (uses system root CA certs)
- Roxxy's own CA cert is trusted by processes in the container
- Other tools in the container (like `docker pull` via roxxy's proxy) trust roxxy's MITM certificate

### Docker Installation

**Using Docker Compose:**
```bash
docker compose -f docker/docker-compose.yml up -d
```

**Using Docker directly:**
```bash
docker build -f docker/Dockerfile -t roxxy:latest .
docker run -d \
  -p 3128:3128 \
  -v roxxy-cache:/app/cache \
  -v ./certs:/app/certs:ro \
  -v ./config.yaml:/app/config.yaml:ro \
  roxxy:latest
```

### Configure Docker to Use Roxxy

Roxxy works as a standard HTTP proxy, so Docker can use it without any special configuration.

**Option 1: Docker Desktop (macOS/Windows)**

1. Open Docker Desktop → Settings → Resources → Proxies
2. Set HTTP Proxy: `http://host.docker.internal:3128`
3. Set HTTPS Proxy: `http://host.docker.internal:3128`
4. Add to No Proxy: `localhost,127.0.0.1`

**Option 2: Docker Daemon Configuration (Linux)**

Edit `/etc/docker/daemon.json`:
```json
{
  "proxies": {
    "http-proxy": "http://localhost:3128",
    "https-proxy": "http://localhost:3128",
    "no-proxy": "localhost,127.0.0.1"
  }
}
```

Then restart Docker:
```bash
sudo systemctl restart docker
```

**Option 3: Environment Variables (Per-Container)**

```bash
docker run --env HTTP_PROXY=http://host.docker.internal:3128 \
           --env HTTPS_PROXY=http://host.docker.internal:3128 \
           myimage
```

### Docker Build Integration

Roxxy works transparently with `docker build` - no special buildx setup needed!

**Simple Usage:**
```bash
# Just configure Docker to use roxxy as proxy (see above)
# Then build normally - all pulls will be cached
docker build -t myapp:latest .
```

**What Gets Cached:**
- All `FROM` image pulls (Docker Hub, Quay.io, GHCR, etc.)
- Package manager downloads in `RUN` commands (if configured):
  - `apt-get install` (when APT proxy is configured)
  - `apk add` (when APK proxy is configured)
  - `npm install` (when NPM proxy is configured)
  - `pip install` (when PyPI proxy is configured)
  - `go mod download` (when Go proxy is configured)

**No Buildx Required:**
Unlike previous versions, you don't need a special buildx builder. Just configure Docker to use roxxy as a proxy, and all builds will automatically cache through roxxy.

### Test It

```bash
# Pull from Docker Hub (will be cached)
docker pull nginx:alpine

# Build a Dockerfile (FROM pulls will be cached)
docker build -t myapp:latest .

# View cached content via CLI
roxxy docker ls
roxxy package ls

# Or visit the web UI
open http://localhost:3128/ui/  # macOS
xdg-open http://localhost:3128/ui/  # Linux
```

## Architecture

```
Docker Client / Package Manager
    ↓ (HTTP_PROXY=localhost:3128)
Roxxy Proxy (Port 3128)
    ↓
    ├─ Docker Registry Request
    │    ↓
    │  Check Cache
    │    ├─ Cache Hit → Return from cache
    │    └─ Cache Miss → Fetch from upstream → Cache → Return
    │
    ├─ Package Manager Request (APT/APK/NPM/PyPI/Go)
    │    ↓
    │  Check Cache
    │    ├─ Cache Hit → Return from cache
    │    └─ Cache Miss → Fetch from upstream → Cache → Return
    │
    └─ Unknown Request
         ↓
       Pass-through (direct to internet)
```

## Configuration

### Config File

Roxxy uses a YAML configuration file. Default locations (checked in order):
1. `./config.yaml` (current directory)
2. `~/.roxxy.conf`
3. Platform-specific:
   - macOS: `~/Library/Application Support/roxxy/config.yaml`
   - Windows: `%APPDATA%/roxxy/config.yaml`
   - Linux: `~/.roxxy/config.yaml`

Or set `CONFIG_FILE` environment variable.

**Example config.yaml:**
```yaml
global:
  cache_dir: /app/cache
  certs_dir: /app/certs
  listen_addr: :3128  # Single address (deprecated, use listen_addrs)
  listen_addrs:       # Multiple listen addresses (recommended)
    - :3128           # Listen on all interfaces, port 3128
    - 127.0.0.1:3128 # Listen on localhost only, port 3128
    - 192.168.1.100:3128  # Listen on specific interface

# Package manager proxies
package_managers:
  - type: apk
    upstream_url: https://dl-cdn.alpinelinux.org
    hostnames:
      - dl-cdn.alpinelinux.org

  - type: go
    upstream_url: https://proxy.golang.org
    hostnames:
      - proxy.golang.org

# Docker registries
docker_registries:
  - upstream_url: https://registry-1.docker.io
    hostnames:
      - docker.io
      - registry-1.docker.io
    proxy_pushes: false
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CONFIG_FILE` | (auto-detected) | Path to config file |
| `LISTEN_ADDR` | `:3128` | Proxy listen address |
| `ROXXY_CACHE_INIT_TARBALL` | (none) | Path to tarball for cache bootstrap |
| `HTTP_PROXY` | (none) | Proxy URL for upstream requests (used if `upstream_proxy` not set in config) |

**Config File Option:**
```yaml
global:
  upstream_proxy: "http://proxy:3128"  # Use this proxy for upstream requests
  # Or set to "none" to explicitly bypass proxy
```

## Airgap Deployment

Roxxy supports airgapped deployments through two operational modes:

### Preparation Phase (`--airgap-proxy`)

Cache all required files with no size limits:

```bash
roxxy --airgap-proxy server
```

This mode:
- Caches all files regardless of size
- Still allows external access for cache misses
- Use this in a connected environment to build up a complete cache

### Deployment Phase (`--airgapped`)

Run in airgapped mode with no external access:

```bash
roxxy --airgapped server
```

This mode:
- Returns 404 for cache misses (no external access)
- Only serves artifacts that are already cached
- Safe for use in airgapped networks

### Airgap Proxy Chaining

You can chain roxxy instances for airgap preparation:

```
[airgap-proxy roxxy] → [system roxxy] → [internet]
```

**Setup:**

1. Start system roxxy (connected to internet):
   ```bash
   roxxy server
   ```

2. Start airgap-proxy roxxy (pointing to system roxxy):
   
   **Option A: Environment Variable**
   ```bash
   export HTTP_PROXY=http://system-roxxy:3128
   export HTTPS_PROXY=http://system-roxxy:3128
   roxxy --airgap-proxy server
   ```
   
   **Option B: Config File**
   ```yaml
   global:
     upstream_proxy: "http://system-roxxy:3128"
   ```
   Then:
   ```bash
   roxxy --airgap-proxy server
   ```

**How It Works:**
- System roxxy unsets its own `HTTP_PROXY` to prevent loops
- System roxxy connects directly to the internet
- Airgap-proxy roxxy uses system roxxy as its proxy (via `upstream_proxy` config or `HTTP_PROXY` env var)
- All requests are cached in both instances
- Loop prevention: If `upstream_proxy` points to roxxy's own listen address, it's automatically bypassed

**Note:** To explicitly bypass proxy even if `HTTP_PROXY` is set, use `upstream_proxy: "none"` in config.

See [AIRGAP.md](docs/AIRGAP.md) for detailed airgap deployment instructions.

## Cache Bootstrap

Roxxy can bootstrap its cache from a tarball on first startup, similar to PostgreSQL's `/docker-entrypoint-initdb.d/` functionality.

**Method 1: Environment Variable**
```bash
docker run -e ROXXY_CACHE_INIT_TARBALL=/path/to/cache.tar.gz roxxy
```

**Method 2: Init Directory (PostgreSQL-style)**
```bash
# Mount tarball into init directory
docker run -v /path/to/cache.tar.gz:/docker-entrypoint-initcache.d/01-cache.tar.gz roxxy
```

**Method 3: Multiple Tarballs (processed in alphabetical order)**
```bash
docker run \
  -v /path/to/01-packages.tar.gz:/docker-entrypoint-initcache.d/01-packages.tar.gz \
  -v /path/to/02-docker.tar.gz:/docker-entrypoint-initcache.d/02-docker.tar.gz \
  roxxy
```

The bootstrap:
- Only runs if cache is empty
- Creates a `.roxxy-bootstrapped` marker file to prevent re-bootstrapping
- Supports `.tar.gz`, `.tgz`, and `.tar` formats
- Works in both Alpine and scratch containers

## Web UI

Roxxy includes a built-in web UI for viewing cached content:

- **Dashboard**: Overview of cached content
- **Packages**: Browse cached packages (APT, APK, NPM, PyPI, Go)
- **Docker Images**: Browse cached Docker images with hierarchical view
- **Statistics**: Cache usage and access statistics

Access at: `http://localhost:3128/ui/`

## Service Management

Roxxy can be installed as a system service for automatic startup and management. **No root privileges required** on Linux and macOS (port 3128 is non-privileged).

### Install Service

**macOS (LaunchAgent):**
```bash
roxxy service install
```

**Linux (systemd user service):**
```bash
roxxy service install  # No sudo required!
```

**Windows:**
```bash
# Run as Administrator (required for service installation only)
roxxy service install
```

### Manage Service

```bash
# Restart the service
roxxy service restart        # macOS/Linux (no sudo needed)

# Uninstall the service
roxxy service uninstall     # macOS/Linux (no sudo needed)
```

**macOS:**
- Service runs as a LaunchAgent in `~/Library/LaunchAgents/com.roxxy.proxy.plist`
- Start: `launchctl load ~/Library/LaunchAgents/com.roxxy.proxy.plist`
- Stop: `launchctl unload ~/Library/LaunchAgents/com.roxxy.proxy.plist`
- Status: `launchctl list | grep roxxy`

**Linux:**
- Service runs as a **user systemd service** (no root required)
- Service file: `~/.config/systemd/user/roxxy.service`
- Start: `systemctl --user start roxxy`
- Stop: `systemctl --user stop roxxy`
- Status: `systemctl --user status roxxy`
- Logs: `journalctl --user -u roxxy -f`
- **Enable lingering** (run when user is not logged in):
  ```bash
  systemctl --user enable roxxy.service
  loginctl enable-linger $USER
  ```

**Windows:**
- Service runs as a Windows service (requires NSSM)
- Service runs as current user (not SYSTEM) for better security
- Note: Service installation requires admin, but service runs as your user account

### View Logs

```bash
# macOS/Linux
roxxy logs

# Or view log file directly
# macOS: ~/Library/Logs/roxxy/roxxy.log
# Linux: ~/.local/state/roxxy/logs/roxxy.log
```

## Viewing Cache Content

### Web UI

The easiest way to view cached content is through the web UI:

```bash
# Open in browser
open http://localhost:3128/ui/  # macOS
xdg-open http://localhost:3128/ui/  # Linux
```

The UI shows:
- **Dashboard**: Overview with statistics
- **Packages**: Browse cached packages (APT, APK, NPM, PyPI, Go) in a hierarchical tree
- **Docker Images**: Browse cached Docker images organized by registry/namespace/repository

### CLI Commands

**Docker Images:**
```bash
# List all cached Docker images
roxxy docker ls

# Show details and layers for a specific image
roxxy docker ls <image:tag>
# Example: roxxy docker ls nginx:alpine

# Pull image from registry into roxxy cache
roxxy docker pull <image:tag>
# Example: roxxy docker pull library/golang:1.25-alpine

# Pull specific platforms for multi-arch images
roxxy docker pull library/golang:1.25-alpine --platform linux/amd64 --platform linux/arm64

# Push image to registry
roxxy docker push <registry> <image:tag>
# Example: roxxy docker push docker.io library/golang:1.25-alpine

# Push from tarball
roxxy docker push docker.io library/golang:1.25-alpine --from-tarball cache.tar

# Export image as Docker tarball (compatible with docker load)
roxxy docker export <image:tag> [output.tar]
# Example: roxxy docker export library/golang:1.25-alpine image.tar

# Delete a cached Docker image
roxxy docker rmi <image:tag>

# Prune unreferenced blobs
roxxy docker prune

# Discover tags for digest-only manifests
roxxy docker discover
```

**Batch Operations:**
```bash
# Create a file with image list (one per line, optional platforms)
cat > images.txt <<EOF
alpine:latest linux/amd64
golang:1.25-alpine linux/arm64 linux/amd64
library/nginx:latest
ghcr.io/myorg/myimage:v1.0 linux/amd64
EOF

# Pull all images from file
roxxy docker pull --file images.txt

# Push all images from file to registry
roxxy docker push docker.io --file images.txt

# Push all images from tarball
roxxy docker push docker.io --file images.txt --from-tarball cache.tar

# Export all images from file
roxxy docker export --file images.txt

# Export with custom output prefix
roxxy docker export --file images.txt output.tar
# Creates: output_alpine_latest.tar, output_golang_1.25-alpine.tar, etc.
```

**File Format:**
The batch file format is simple: one image per line, with optional platform specifications:
```
<image:tag> [platform1] [platform2] ...
```

- Empty lines and lines starting with `#` are ignored
- First field is the image name (repo:tag)
- Remaining fields are platform specifications (os/arch format, e.g., `linux/amd64`)
- If no platforms are specified, all platforms will be pulled for multi-arch images

**Packages:**
```bash
# List all cached packages
roxxy package ls

# List packages by type
roxxy package ls --type go
roxxy package ls --type npm
roxxy package ls --type pypi
roxxy package ls --type apt
roxxy package ls --type apk

# List with sorting options
roxxy package ls --sort name      # Sort by name (default)
roxxy package ls --sort size      # Sort by size
roxxy package ls --sort cached_at # Sort by cache time
roxxy package ls --sort accessed  # Sort by access count

# Parse package indices (for better metadata)
roxxy package parse
roxxy package parse --type go
```

**General Cache:**
```bash
# List all cached content (raw URLs, packages, Docker)
roxxy cache ls

# List by hostname
roxxy cache ls <hostname>
# Example: roxxy cache ls registry-1.docker.io

# List with sorting
roxxy cache ls --sort size      # Sort by size
roxxy cache ls --sort accessed   # Sort by access count
roxxy cache ls --sort host       # Sort by hostname (default)

# Tree view
roxxy cache ls --tree

# Flush all cache
roxxy cache flush
```

**Cache Statistics:**
```bash
# View via web UI dashboard
# Or check access-stats.json in cache directory
cat ~/.cache/roxxy/access-stats.json  # Linux
cat ~/Library/Application\ Support/roxxy/Cache/access-stats.json  # macOS
```

## Roxxy Archiver (roar)

Roxxy includes a separate lightweight binary called `roar` (Roxxy Archiver) for interacting with roxxy cache tarballs without running the proxy server. This is useful for:

- Inspecting tarball contents
- Pushing images from tarballs to registries
- Exporting images from tarballs as Docker-compatible tarballs
- Installing and running Docker containers from tarballs
- Working with tarballs in CI/CD pipelines or airgapped environments

### Building roar

```bash
# From the repository root, build both roxxy and roar
npm ci
make build
```

Do not use `build.sh` as the clean local build command. Its default path calls
a local Roxxy API and attempts to push a container image.

### roar Commands

**List Contents:**
```bash
# List all Docker images in tarball
roar ls --from-tarball cache.tar --docker

# Show layers for specific image
roar ls --from-tarball cache.tar --docker --image ghcr.io/org/image:tag

# Show specific platform
roar ls --from-tarball cache.tar --docker --image ghcr.io/org/image:tag --platform linux/arm64
```

**Import Tarball:**
```bash
# List contents of tarball
roar import cache.tar.gz
```

**Push Images and Charts:**
```bash
# Replay a cached manifest tree (image or helm chart) from a tarball.
# Works for docker images, OCI images, and helm OCI charts — the manifest
# media type is preserved from the tarball.
roar push <registry> <repo:tag> --from-tarball cache.tar

# Pluck a single platform from a multi-arch index and push it as a
# single-platform image under the same tag.
roar push <registry> <repo:tag> --from-tarball cache.tar --platform linux/amd64

# Push every manifest in the tarball in one shot, with byte-based progress.
# --platform applies to every multi-arch index encountered.
roar push <registry> --from-tarball cache.tar --all
roar push <registry> --from-tarball cache.tar --all --platform linux/amd64

# Build and push a helm OCI manifest from a locally-packaged chart.
# <repo:tag> defaults to the chart's name:version from Chart.yaml.
roar push <registry> --chart mychart-0.1.0.tgz
roar push <registry> charts/mychart:0.1.0 --chart mychart-0.1.0.tgz
```

**`push --all` exit codes** (for CI gating on `$?`): **0** = every ref landed
(this includes benign tag conflicts — the destination already has that tag
pointing at different content, which leaves the existing content intact); **2**
= one or more refs FAILED to push or a manifest path was unparseable, so the
destination registry is **INCOMPLETE** — treat non-zero as a failed/partial
delivery and do not deploy.

Authentication uses the docker config file (`~/.docker/config.json`):
credential helpers are tried first, then the `auths` block, then anonymous.
For ECR push, run `aws ecr get-login-password | docker login` once (or
configure `docker-credential-ecr-login` as a `credHelper`); roar picks up
the resulting credentials automatically. See [docs/REGISTRY_AUTH.md](docs/REGISTRY_AUTH.md)
for details.

**Export Images:**
```bash
# Export Docker image as Docker tarball (compatible with docker load)
roar export docker <image:tag> [output.tar] --from-tarball cache.tar
# Example: roar export docker library/golang:1.25-alpine image.tar --from-tarball cache.tar
```

**Tarball Information:**
```bash
# Show tarball metadata (size, manifest count, blob count)
roar info cache.tar
```

**Install from Tarball:**
```bash
# Install using install.json from tarball
roar install cache.tar

# Install with extra arguments passed to container
roar install cache.tar --extra-arg --another-arg
```

The `install` command reads an `install.json` file from the tarball and runs the specified Docker container with the tarball mounted. The tarball is mounted as `/cache.tar:ro` (read-only) in the container.

**install.json Format:**
```json
{
  "image": "roxxy:latest",
  "command": ["-it", "--rm", "--name", "roxxy-install"],
  "mounts": ["-v", "/var/run/docker.sock:/var/run/docker.sock"],
  "env": ["-e", "ROXXY_CACHE=/cache.tar"],
  "args": ["server"]
}
```

- **image**: Docker image to run (required)
- **command**: Docker run flags (e.g., `-it`, `--rm`, `--name`)
- **mounts**: Additional volume mounts (e.g., `["-v", "/host:/container"]`)
- **env**: Environment variables (e.g., `["-e", "KEY=value"]`)
- **args**: Default arguments passed to the container command

**Example install.json:**
```json
{
  "image": "roxxy:latest",
  "command": ["-it", "--rm"],
  "mounts": ["-v", "/var/run/docker.sock:/var/run/docker.sock"],
  "env": ["-e", "ROXXY_CACHE=/cache.tar"],
  "args": ["server"]
}
```

This would execute:
```bash
docker run -it --rm \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /absolute/path/to/cache.tar:/cache.tar:ro \
  -e ROXXY_CACHE=/cache.tar \
  roxxy:latest server [extra args...]
```

### Use Cases

1. **CI/CD Pipelines**: Export roxxy cache tarballs and use `roar` to push specific images to registries
2. **Airgapped Environments**: Transfer tarballs to airgapped systems and use `roar` to inspect or export images
3. **Image Migration**: Extract specific images from large cache tarballs and push them to different registries
4. **Tarball Inspection**: Quickly check what images are in a tarball without extracting it
5. **Automated Installation**: Use `roar install` to automatically run Docker containers from tarballs with pre-configured settings

### roar vs roxxy docker

- **roar**: Works with tarballs only, no proxy server needed, smaller binary (~6MB)
- **roxxy docker**: Works with live cache and tarballs, requires roxxy to be running or cache directory access

Use `roar` when you only need to work with tarballs. Use `roxxy docker` when you want to interact with the live cache or need batch operations.

## Supported Registries and Package Managers

### Docker Registries
- Docker Hub (`docker.io`, `registry-1.docker.io`)
- GitHub Container Registry (`ghcr.io`)
- Quay.io (`quay.io`)
- Kubernetes Registry (`registry.k8s.io`)
- Any custom registry (configurable)

### Package Managers
- **APT**: Debian/Ubuntu packages
- **APK**: Alpine packages
- **NPM**: Node.js packages
- **PyPI**: Python packages
- **Go Modules**: Go packages

## Development

### Building

Use Go 1.25 or newer, Node.js with npm, and GNU Make. From the repository root:

```bash
# Install locked JavaScript dependencies and generate embedded assets before compiling
npm ci
make build
make validate

# Build with Docker
docker build -f docker/Dockerfile -t roxxy:latest .

# Build scratch image (minimal, ~10MB)
docker build -f docker/Dockerfile.scratch -t roxxy:scratch .
```

### Testing

```bash
# Start roxxy
roxxy server

# In another terminal, test Docker pull
export HTTP_PROXY=http://localhost:3128
export HTTPS_PROXY=http://localhost:3128
docker pull nginx:alpine

# Check cache
roxxy docker ls
```

## Troubleshooting

### Certificate Errors

**Error**: `x509: certificate signed by unknown authority`

**Solution**: Install the CA certificate:

**For local installation:**
```bash
# macOS
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain certs/ca-cert.pem

# Linux
sudo cp certs/ca-cert.pem /usr/local/share/ca-certificates/roxxy-ca.crt
sudo update-ca-certificates
```

**For container deployment:**
```bash
# Use --generate flag to automatically generate and install
docker run roxxy:latest server --generate

# Or manually generate and install
docker exec roxxy roxxy certs generate --force
docker exec roxxy sh -c 'cp /app/certs/ca-cert.pem /usr/local/share/ca-certificates/roxxy-ca.crt && update-ca-certificates'
```

**Note:** Roxxy automatically trusts certificates from the system trust store (`/usr/local/share/ca-certificates/` on Alpine/Debian). Any certificates added there via `update-ca-certificates` will be trusted by roxxy's upstream connections.

### Docker Build Not Using Proxy

**Solution**: Ensure Docker is configured to use roxxy:
- Check `/etc/docker/daemon.json` (Linux)
- Check Docker Desktop proxy settings (macOS/Windows)
- Verify `HTTP_PROXY`/`HTTPS_PROXY` environment variables

### Cache Not Populating

**Check**:
1. Verify requests are going through roxxy (check logs)
2. Check cache directory permissions
3. Ensure sufficient disk space
4. Review config file for correct upstream URLs

## Performance

- Adds ~10-50ms latency per request (TLS termination + routing)
- Negligible CPU usage (<1% for typical workloads)
- Memory: ~20MB base + ~1MB per active connection
- Local caching provides significant speedup for repeated pulls

## Limitations

- Requires CA certificate installation on clients
- Only supports HTTP/HTTPS proxies (no SOCKS5)
- Docker BuildKit may need additional configuration for some use cases

## See Also

- [docs/CI_OPTIMIZATION.md](docs/CI_OPTIMIZATION.md) - Speed up CI/CD pipelines with Docker-in-Docker and airgapped environments
- [docs/AIRGAP.md](docs/AIRGAP.md) - Airgap deployment guide
- [docs/SCRATCH.md](docs/SCRATCH.md) - Scratch container deployment
- [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md) - Detailed troubleshooting
- [docs/adr/](docs/adr/) - Architecture Decision Records (ADRs)

## License

See [LICENSE](LICENSE) for Apache License 2.0 terms. Dependencies and bundled
assets may have separate terms.
