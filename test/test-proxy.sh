#!/bin/bash
# Quick test script for roxxy using the test container

set -e

echo "=== Testing Registry Proxy ==="
echo ""

# Flush cache first
echo "1. Flushing roxxy cache..."
docker exec roxxy sh /app/flush-cache.sh || echo "  (cache flush failed, continuing anyway)"
echo ""

# Extract CA cert from roxxy container
echo "2. Extracting CA certificate from roxxy..."
TEMP_CERT=$(mktemp)
docker exec roxxy cat /app/certs/ca-cert.pem > "$TEMP_CERT" 2>/dev/null || {
    echo "  ⚠ Failed to extract CA cert, tests may fail for HTTPS"
    TEMP_CERT=""
}
echo ""

# Run test container with CA cert mounted
echo "3. Running test container..."
if [ -n "$TEMP_CERT" ] && [ -f "$TEMP_CERT" ]; then
    docker run --rm \
      --network roxxy_roxxy-net \
      -v "$TEMP_CERT:/tmp/ca-cert.pem:ro" \
      -e HTTP_PROXY=http://roxxy:3128 \
      -e HTTPS_PROXY=http://roxxy:3128 \
      -e http_proxy=http://roxxy:3128 \
      -e https_proxy=http://roxxy:3128 \
      -e NO_PROXY=localhost,127.0.0.1,roxxy,nexus \
      -e no_proxy=localhost,127.0.0.1,roxxy,nexus \
      roxxy-test \
      /test/test-components.sh
    rm -f "$TEMP_CERT"
else
    docker run --rm \
      --network roxxy_roxxy-net \
      -e HTTP_PROXY=http://roxxy:3128 \
      -e HTTPS_PROXY=http://roxxy:3128 \
      -e http_proxy=http://roxxy:3128 \
      -e https_proxy=http://roxxy:3128 \
      -e NO_PROXY=localhost,127.0.0.1,roxxy,nexus \
      -e no_proxy=localhost,127.0.0.1,roxxy,nexus \
      roxxy-test \
      /test/test-components.sh
fi

echo ""
echo "=== Test completed ==="

