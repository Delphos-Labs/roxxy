#!/bin/bash
# Wrapper script for docker-compose build that automatically sets BUILD_TIME and proxy settings
# Usage: ./docker/docker-compose-build.sh [docker compose build arguments...]

set -e

# Calculate BUILD_TIME if not already set
if [ -z "$BUILD_TIME" ]; then
  export BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
fi

# Get roxxy IP for proxy settings (if container is running)
test -n "$CLEAN" || PROXY_IP=$(docker inspect roxxy --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' 2>/dev/null || echo "")

if [ -n "$PROXY_IP" ]; then
  export HTTP_PROXY="http://${PROXY_IP}:3128"
  export HTTPS_PROXY="http://${PROXY_IP}:3128"
  # Add Alpine package repositories to NO_PROXY so apk can work before CA cert is installed
  # Include roxxy's network subnet (172.20.0.0/16) in NO_PROXY to prevent proxy loops
  # Also include the specific IP if we can get it
  ROXXY_IP_SUBNET="172.20.0.0/16"
  export NO_PROXY="github.com,auth.docker.io,localhost,127.0.0.1,${ROXXY_IP_SUBNET},${PROXY_IP},dl-cdn.alpinelinux.org,dl-2.alpinelinux.org,dl-3.alpinelinux.org,dl-4.alpinelinux.org,dl-5.alpinelinux.org,alpinelinux.org"
  echo "Building with BUILD_TIME=$BUILD_TIME"
  echo "Building with Nexus caching (proxy: $PROXY_IP:3128)"
  echo ""
else
  echo "Building with BUILD_TIME=$BUILD_TIME"
  echo "Note: roxxy not running, building without proxy caching"
  echo ""
fi

# Run docker-compose build with BUILD_TIME and proxy settings
# Run from root directory, but use docker-compose file from docker/ directory
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT_DIR"
docker compose -f docker/docker-compose.yml build "$@"
docker compose -f docker/docker-compose.yml up --build -d
if [ -f "./buildx/setup.sh" ]; then
    ./buildx/setup.sh
fi
