#!/bin/bash
# Build the roxxy-test container

set -e

echo "Building roxxy-test container..."
cd "$(dirname "$0")"
if [ -f "docker/Dockerfile.test" ]; then
    docker build -f docker/Dockerfile.test -t roxxy-test .
else
    echo "Error: docker/Dockerfile.test not found"
    exit 1
fi
echo "✓ roxxy-test container built successfully"
echo ""
echo "To run tests:"
echo "  ./test-proxy.sh"
echo ""
echo "Or run the container manually:"
echo "  docker run --rm --network roxxy_roxxy-net \\"
echo "    -e HTTP_PROXY=http://roxxy:3128 \\"
echo "    -e HTTPS_PROXY=http://roxxy:3128 \\"
echo "    roxxy-test"

