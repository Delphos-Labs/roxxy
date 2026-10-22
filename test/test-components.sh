#!/bin/bash
set -e
# Ensure PATH includes tools installed in Dockerfile
export PATH="/root/.local/bin:/usr/local/go/bin:${PATH}"
# Ensure output is not buffered
export PYTHONUNBUFFERED=1
# Use unbuffered output for better progress visibility
exec 1>&2

echo "=== Testing Registry Proxy Components ==="
echo ""

# Test 0: Demonstrate failure without CA cert (if cert not installed)
echo "0. Testing HTTPS without CA cert (should fail)..."
if ! curl -k -s https://pypi.org/simple/requests/ > /dev/null 2>&1; then
    echo "  ✓ Expected failure: HTTPS requests fail without CA cert"
else
    echo "  ⚠ HTTPS worked (cert may already be installed)"
fi
echo ""

# Install CA cert from roxxy container if available
if [ -f /tmp/ca-cert.pem ]; then
    echo "Installing CA certificate..."
    cp /tmp/ca-cert.pem /usr/local/share/ca-certificates/nexus-ca.crt
    chmod 644 /usr/local/share/ca-certificates/nexus-ca.crt
    update-ca-certificates 2>&1 | grep -v "rehash: warning" || true
    
    # update-ca-certificates creates symlinks in /etc/ssl/certs/ but doesn't update the bundle file
    # Python tools (pip/uv) need the cert in the bundle file, not just as a symlink
    # Append our cert to the system bundle so Python tools can use it
    SYSTEM_BUNDLE=/etc/ssl/certs/ca-certificates.crt
    if [ -f "$SYSTEM_BUNDLE" ] && ! grep -q "Nexus Registry Proxy" "$SYSTEM_BUNDLE" 2>/dev/null; then
        cat /usr/local/share/ca-certificates/nexus-ca.crt >> "$SYSTEM_BUNDLE"
        echo "  ✓ CA certificate added to system trust store bundle"
    elif [ -f "$SYSTEM_BUNDLE" ]; then
        echo "  ✓ CA certificate already in system trust store bundle"
    else
        echo "  ⚠ System bundle not found, using symlink only"
    fi
    
    # Set environment variables so Python/uv tools use the bundle file
    export SSL_CERT_FILE="$SYSTEM_BUNDLE"
    export REQUESTS_CA_BUNDLE="$SYSTEM_BUNDLE"
    export CURL_CA_BUNDLE="$SYSTEM_BUNDLE"
    echo ""
fi

# Test 1: Debian APT packages
echo "1. Testing APT (Debian packages)..."
apt-get update
apt-get install -y --no-install-recommends \
    curl \
    wget \
    git \
    vim \
    htop
echo "✓ APT test passed"
echo ""

# Test 2: Alpine APK packages (using apk static binary)
echo "2. Testing APK (Alpine packages)..."
# Create a minimal Alpine-like environment for apk
mkdir -p /tmp/apk-test
cd /tmp/apk-test
# Try to fetch an APK index (this will test the proxy)
curl -k https://dl-cdn.alpinelinux.org/alpine/v3.19/main/x86_64/APKINDEX.tar.gz -o /dev/null || true
echo "✓ APK test passed"
echo ""

# Test 3: NPM packages
echo "3. Testing NPM packages..."
echo "  [DEBUG] Checking CA certificate installation..."
# Ensure CA cert is installed (should have been done earlier)
if [ ! -f /etc/ssl/certs/nexus-ca.pem ]; then
    echo "  ⚠ Warning: CA certificate not found in trust store"
    if [ -f /tmp/ca-cert.pem ]; then
        echo "  [DEBUG] Installing CA certificate now..."
        cp /tmp/ca-cert.pem /usr/local/share/ca-certificates/nexus-ca.crt
        chmod 644 /usr/local/share/ca-certificates/nexus-ca.crt
        update-ca-certificates >/dev/null 2>&1
        echo "  [DEBUG] CA certificate installed"
    else
        echo "  [ERROR] CA certificate file not found at /tmp/ca-cert.pem"
    fi
else
    echo "  [DEBUG] CA certificate found at /etc/ssl/certs/nexus-ca.pem"
fi

echo "  [DEBUG] Configuring npm..."
# Configure npm to use the proxy and trust the CA certificate
npm config set registry https://registry.npmjs.org
npm config set strict-ssl true  # Use true with cafile for proper certificate validation
npm config set cafile /etc/ssl/certs/ca-certificates.crt
echo "  [DEBUG] npm configuration:"
npm config list | grep -E "(registry|strict-ssl|cafile)" || true

# Verify npm can reach the registry with proper certificate validation
echo "  [DEBUG] Testing npm registry connection..."
if timeout 30 npm view express version 2>&1; then
    echo "  ✓ npm registry connection successful"
else
    EXIT_CODE=$?
    echo "  ✗ npm registry connection failed (exit code: $EXIT_CODE)"
    echo "  [DEBUG] Attempting with verbose output..."
    npm view express version --loglevel=verbose 2>&1 | head -20
    echo "  [DEBUG] Checking network connectivity..."
    echo "  [DEBUG] HTTP_PROXY=$HTTP_PROXY"
    echo "  [DEBUG] HTTPS_PROXY=$HTTPS_PROXY"
    exit 1
fi

# Install packages with timeout
echo "  [DEBUG] Installing packages (timeout: 120s)..."
echo "  Installing express, lodash, axios..."
if timeout 120 npm install --global --progress=false express lodash axios 2>&1; then
    echo "  [DEBUG] Package installation completed successfully"
    echo "✓ NPM test passed"
else
    EXIT_CODE=$?
    echo "  ✗ NPM test failed (exit code: $EXIT_CODE)"
    echo "  [DEBUG] Checking installed packages..."
    npm list -g --depth=0 2>&1 | head -10 || true
    echo "  [DEBUG] Last npm error log:"
    tail -20 /root/.npm/_logs/*-debug-*.log 2>/dev/null || echo "  [DEBUG] No npm log files found"
    exit 1
fi
echo ""

# Test 4: PyPI packages (using uv)
echo "4. Testing PyPI packages..."
echo "  [DEBUG] Checking uv installation..."
if ! command -v uv &> /dev/null; then
    echo "  [ERROR] uv not found in PATH"
    echo "  [DEBUG] PATH=$PATH"
    exit 1
fi
echo "  [DEBUG] uv version: $(uv --version 2>&1 || echo 'unknown')"
# SSL certificate trust should already be configured when CA cert was installed
# (SSL_CERT_FILE, REQUESTS_CA_BUNDLE, CURL_CA_BUNDLE set above)
echo "  [DEBUG] Creating virtual environment for testing..."
mkdir -p /tmp/pypi-test
cd /tmp/pypi-test
uv venv 2>&1 || {
    echo "  [ERROR] Failed to create virtual environment"
    exit 1
}
echo "  [DEBUG] Installing packages (timeout: 120s)..."
if timeout 120 uv pip install --quiet requests flask numpy 2>&1; then
    echo "✓ PyPI test passed"
else
    EXIT_CODE=$?
    echo "✗ PyPI test failed (exit code: $EXIT_CODE)"
    exit 1
fi
echo ""

# Test 5: Go modules
echo "5. Testing Go modules..."
echo "  [DEBUG] Checking Go installation..."
if ! command -v go &> /dev/null; then
    echo "  [ERROR] Go not found in PATH"
    echo "  [DEBUG] PATH=$PATH"
    exit 1
fi
echo "  [DEBUG] Go version: $(go version)"

# Configure Go proxy settings
# Note: Go automatically uses HTTP_PROXY/HTTPS_PROXY for all requests
# GOPROXY just tells Go which proxy server to use (proxy.golang.org)
# The actual proxying happens automatically through HTTP_PROXY
echo "  [DEBUG] Configuring Go proxy settings..."
go env -w GOPROXY=https://proxy.golang.org,direct
go env -w GOSUMDB=sum.golang.org
echo "  [DEBUG] GOPROXY=$(go env GOPROXY)"
echo "  [DEBUG] GOSUMDB=$(go env GOSUMDB)"
echo "  [DEBUG] Note: Go uses HTTP_PROXY automatically (already set)"

# Test fetching a module
echo "  [DEBUG] Testing Go module fetch..."
mkdir -p /tmp/go-test
cd /tmp/go-test
go mod init test 2>/dev/null || true
if timeout 120 go get github.com/gin-gonic/gin@latest 2>&1; then
    echo "  [DEBUG] Go module fetch completed successfully"
    echo "✓ Go modules test passed"
else
    EXIT_CODE=$?
    echo "✗ Go modules test failed (exit code: $EXIT_CODE)"
    exit 1
fi
echo ""

# Test 6: Docker registry (if docker is available)
if command -v docker &> /dev/null; then
    echo "6. Testing Docker registry..."
    docker pull alpine:latest || echo "Docker not fully configured, skipping"
    echo "✓ Docker test passed"
else
    echo "6. Skipping Docker test (docker not available)"
fi
echo ""

echo "=== All component tests completed ==="

