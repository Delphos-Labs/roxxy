#!/bin/bash
# Wrapper for docker buildx build that adds Nexus proxy configuration

set -e

# Get roxxy IP
PROXY_IP=$(docker inspect roxxy --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' 2>/dev/null)

if [ -z "$PROXY_IP" ]; then
  echo "Error: Could not get roxxy IP. Is the container running?"
  exit 1
fi

echo "Building with Nexus caching (proxy: $PROXY_IP:3128)"

test -z "${SKIP_LOAD}" && LOAD_FLAG=--load  || LOAD_FLAG=--push 
docker buildx build \
  ${FROM:+--cache-from=type=registry,ref=$FROM} \
  ${BUILD_CONTEXT:+--build-context $BUILD_CONTEXT} \
  --builder=roxxy-builder \
  --build-arg HTTP_PROXY=http://${PROXY_IP}:3128 \
  --build-arg HTTPS_PROXY=http://${PROXY_IP}:3128 \
  --build-arg NO_PROXY=github.com,auth.docker.io \
  $LOAD_FLAG \
  "$@"
