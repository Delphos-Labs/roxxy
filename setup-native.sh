#!/bin/bash
# Setup script for running roxxy natively (outside Docker)
# This avoids the proxy loop issue when Docker's proxy settings route traffic back through roxxy

set -e

# Detect platform and set appropriate directories
if [[ "$OSTYPE" == "darwin"* ]]; then
    # macOS
    ROXXY_DIR="$HOME/Library/Application Support/roxxy"
    CACHE_DIR="$ROXXY_DIR/Cache"
elif [[ "$OSTYPE" == "msys" || "$OSTYPE" == "win32" ]]; then
    # Windows (Git Bash or similar)
    if [ -n "$APPDATA" ]; then
        ROXXY_DIR="$APPDATA/roxxy"
        CACHE_DIR="$ROXXY_DIR/Cache"
    elif [ -n "$LOCALAPPDATA" ]; then
        ROXXY_DIR="$LOCALAPPDATA/roxxy"
        CACHE_DIR="$ROXXY_DIR/Cache"
    else
        ROXXY_DIR="$HOME/AppData/Roaming/roxxy"
        CACHE_DIR="$ROXXY_DIR/Cache"
    fi
else
    # Linux and others
    ROXXY_DIR="$HOME/.roxxy"
    # Use XDG_CACHE_HOME if set, otherwise ~/.cache
    if [ -n "$XDG_CACHE_HOME" ]; then
        CACHE_DIR="$XDG_CACHE_HOME/roxxy"
    else
        CACHE_DIR="$HOME/.cache/roxxy"
    fi
fi

CERTS_DIR="$ROXXY_DIR/certs"
CONFIG_FILE="$ROXXY_DIR/config.yaml"

echo "=== Roxxy Native Setup ==="
echo "Platform: $(uname -s)"
echo "Roxxy directory: $ROXXY_DIR"
echo ""

# Create directories
echo "Creating directories..."
mkdir -p "$ROXXY_DIR"
mkdir -p "$CACHE_DIR"
mkdir -p "$CERTS_DIR"
echo "  ✓ Config: $ROXXY_DIR"
echo "  ✓ Cache: $CACHE_DIR"
echo "  ✓ Certs: $CERTS_DIR"

# Copy or generate certificates
if [ -f "./certs/ca-cert.pem" ] && [ -f "./certs/ca-key.pem" ]; then
    echo ""
    echo "Copying certificates from ./certs/..."
    cp ./certs/ca-cert.pem "$CERTS_DIR/"
    cp ./certs/ca-key.pem "$CERTS_DIR/"
    echo "  ✓ Copied ca-cert.pem"
    echo "  ✓ Copied ca-key.pem"
elif [ -f "$CERTS_DIR/ca-cert.pem" ] && [ -f "$CERTS_DIR/ca-key.pem" ]; then
    echo ""
    echo "Using existing certificates in $CERTS_DIR"
else
    echo ""
    echo "Generating new CA certificate..."
    # Check if roxxy binary exists (might be in PATH or current directory)
    if command -v roxxy &> /dev/null; then
        roxxy certs generate
    elif [ -f "./roxxy" ]; then
        ./roxxy certs generate
    else
        echo "  ⚠️  roxxy binary not found. Building it first..."
        go build -o roxxy .
        ./roxxy certs generate
    fi
    echo ""
    echo "  ⚠️  You'll need to install this CA certificate in your system trust store:"
    echo "     roxxy certs install"
fi

# Create config file if it doesn't exist
if [ ! -f "$CONFIG_FILE" ]; then
    echo ""
    echo "Creating config file..."
    cat > "$CONFIG_FILE" << 'EOF'
# Roxxy Native Configuration
# Location: ~/.roxxy/config.yaml

global:
  # Cache directory - can be overridden to use /data, /var/roxxy, etc.
  # Default: platform-specific roxxy directory/cache
  # cache_dir: /data/roxxy/cache
  # Certificates directory
  # certs_dir: ~/.roxxy/certs
  # Listen address
  listen_addr: :3128

# Package manager proxies
package_managers:
  - type: apk
    upstream_url: https://dl-cdn.alpinelinux.org
    hostnames:
      - dl-cdn.alpinelinux.org
      - dl-2.alpinelinux.org
      - dl-3.alpinelinux.org
      - dl-4.alpinelinux.org
      - dl-5.alpinelinux.org

  - type: go
    upstream_url: https://proxy.golang.org
    hostnames:
      - proxy.golang.org

  - type: npm
    upstream_url: https://registry.npmjs.org
    hostnames:
      - registry.npmjs.org

  - type: pypi
    upstream_url: https://pypi.org
    hostnames:
      - pypi.org
      - files.pythonhosted.org

# Docker registries
docker_registries:
  - upstream_url: https://registry-1.docker.io
    hostnames:
      - docker.io
      - registry-1.docker.io
      - index.docker.io
    proxy_pushes: false
    local_only_paths: []

  - upstream_url: https://ghcr.io
    hostnames:
      - ghcr.io
    proxy_pushes: false

# Binary caching for other hosts
registries:
  binaries.prisma.sh:
    cache_type: "local"
    min_cache_size: 1048576

  unofficial-builds.npmjs.org:
    cache_type: "local"
    min_cache_size: 1048576
EOF
    echo "  ✓ Created $CONFIG_FILE"
else
    echo ""
    echo "Config file already exists: $CONFIG_FILE"
fi

# Build roxxy
echo ""
echo "Building roxxy..."
if [ -f "./roxxy" ]; then
    # Use existing binary if available
    cp ./roxxy "$ROXXY_DIR/roxxy"
    echo "  ✓ Copied existing roxxy binary"
else
    go build -o "$ROXXY_DIR/roxxy" .
    echo "  ✓ Built $ROXXY_DIR/roxxy"
fi

# Create a convenience symlink
if [ -w /usr/local/bin ]; then
    echo ""
    echo "Creating symlink in /usr/local/bin..."
    ln -sf "$ROXXY_DIR/roxxy" /usr/local/bin/roxxy
    echo "  ✓ Created /usr/local/bin/roxxy"
fi

echo ""
echo "=== Setup Complete ==="
echo ""
echo "To start roxxy:"
echo "  $ROXXY_DIR/roxxy server"
echo ""
echo "Or if symlinked:"
echo "  roxxy server"
echo ""
echo "To configure Docker to use roxxy as proxy:"
echo "  1. Open Docker Desktop → Settings → Resources → Proxies"
echo "  2. Set HTTP Proxy: http://host.docker.internal:3128"
echo "  3. Set HTTPS Proxy: http://host.docker.internal:3128"
echo "  4. Add to No Proxy: localhost,127.0.0.1"
echo ""
echo "Since roxxy runs OUTSIDE Docker, its upstream fetches won't loop!"
echo ""

