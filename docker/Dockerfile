FROM golang:1.25-alpine AS builder
# Accept build args for proxy configuration FIRST (before any RUN commands that might use them)
ARG HTTP_PROXY
ARG HTTPS_PROXY
ARG NO_PROXY
# Set them as environment variables so they're available during build
ENV HTTP_PROXY=${HTTP_PROXY}
ENV HTTPS_PROXY=${HTTPS_PROXY}
ENV NO_PROXY=${NO_PROXY}
# Install ca-certificates FIRST (before trying to add roxxy's cert)
# This provides the base system root CA certificates
# Bypass proxy for this initial install since we don't have roxxy's CA cert yet
RUN wget -q -O /usr/local/share/ca-certificates/upstream_ca.crt http://host.docker.internal:3128/api/v1/certs/ca \
 && cat /usr/local/share/ca-certificates/upstream_ca.crt >> /etc/ssl/certs/ca-certificates.crt
RUN apk add --no-cache ca-certificates wget

# Optionally copy CA cert and add it to Alpine's cert store
# This allows apk to work through roxxy's proxy (if Docker is using roxxy as proxy)
# Try multiple sources: build context, then roxxy's API if proxy is configured
RUN --mount=type=bind,source=.,target=/buildctx \
    mkdir -p /usr/local/share/ca-certificates && \
    CERT_ADDED=0 && \
    if [ -f /buildctx/certs/ca-cert.pem ]; then \
        cp /buildctx/certs/ca-cert.pem /usr/local/share/ca-certificates/roxxy-ca.crt && \
        echo "✓ Found roxxy CA cert in build context"; \
        CERT_ADDED=1; \
    elif [ -n "$HTTP_PROXY" ]; then \
        echo "Attempting to fetch roxxy CA cert from API..."; \
        # Extract host from HTTP_PROXY (e.g., http://host.docker.internal:3128 -> host.docker.internal) \
        PROXY_HOST=$(echo "$HTTP_PROXY" | sed -E 's|^https?://([^:/]+).*|\1|') && \
        # Try to fetch from roxxy's API (bypass proxy, connect directly by unsetting proxy env vars) \
        if (HTTP_PROXY= HTTPS_PROXY= http_proxy= https_proxy= wget -q -O /tmp/roxxy-ca.crt "http://${PROXY_HOST}/api/v1/certs/ca" 2>/dev/null) || \
           (HTTP_PROXY= HTTPS_PROXY= http_proxy= https_proxy= wget -q -O /tmp/roxxy-ca.crt "http://${PROXY_HOST}:3128/api/v1/certs/ca" 2>/dev/null); then \
            if [ -s /tmp/roxxy-ca.crt ] && grep -q "BEGIN CERTIFICATE" /tmp/roxxy-ca.crt 2>/dev/null; then \
                cp /tmp/roxxy-ca.crt /usr/local/share/ca-certificates/roxxy-ca.crt && \
                echo "✓ Fetched roxxy CA cert from API (${PROXY_HOST})"; \
                CERT_ADDED=1; \
            else \
                echo "Warning: Fetched file doesn't look like a valid certificate"; \
                rm -f /tmp/roxxy-ca.crt; \
            fi; \
        else \
            echo "Note: Could not fetch roxxy CA cert from API (${PROXY_HOST})"; \
        fi; \
    fi && \
    if [ "$CERT_ADDED" = "1" ]; then \
        chmod 644 /usr/local/share/ca-certificates/roxxy-ca.crt && \
        update-ca-certificates 2>/dev/null && \
        cat /usr/local/share/ca-certificates/roxxy-ca.crt >> /etc/ssl/certs/ca-certificates.crt && \
        touch /tmp/roxxy_cert_added && \
        echo "✓ Added roxxy CA cert to system trust store"; \
    else \
        echo "Note: roxxy CA cert not available, apk will connect directly to Alpine repos"; \
        rm -f /tmp/roxxy_cert_added; \
    fi

# Install remaining packages
# If roxxy cert was added, apk can use roxxy's proxy
# If roxxy cert was NOT added but proxy is set, bypass proxy for apk
RUN if [ -n "$HTTP_PROXY" ] && [ ! -f /tmp/roxxy_cert_added ]; then \
        echo "Warning: Proxy is set but roxxy CA cert not found, bypassing proxy for apk operations"; \
        HTTP_PROXY= HTTPS_PROXY= apk add --no-cache nodejs npm; \
    else \
        apk add --no-cache nodejs npm; \
    fi

WORKDIR /build

COPY package.json package-lock.json ./
RUN npm ci

# Copy all source files first (including UI files)
COPY go.mod go.sum ./
COPY *.go ./
COPY internal/ ./internal/
COPY cmd/ ./cmd/
# Use proxy for Go module operations (this tests that the proxy works correctly)
RUN wget --post-data='{"name":"roxxy"}' http://host.docker.internal:3128/api/v1/sbom/start || true
RUN go mod download
RUN wget --post-data='{"name":"roxxy"}' http://host.docker.internal:3128/api/v1/sbom/stop || true
# Generate embedded UI assets (minifies js/*.js -> js/*.min.js via build-js.js)
RUN go generate ./...
# Build the main binary with command routing built-in
# Set BuildTime during build for debugging (can be overridden with --build-arg BUILD_TIME=...)
ARG BUILD_TIME=unknown
RUN go build -ldflags "-X main.BuildTime=$BUILD_TIME" -o roxxy . && \
    echo "Build completed at: $BUILD_TIME"
RUN go build -ldflags "-X main.BuildTime=$BUILD_TIME" -o roar ./cmd/roar && \
    echo "Build completed at: $BUILD_TIME"

FROM alpine:latest

# Ensure directories exist first
RUN mkdir -p /etc/ssl/certs /usr/local/share/ca-certificates

# Copy CA certificate bundle from builder stage FIRST
# The builder stage already has roxxy's CA cert added (fetched from API or build context)
# This includes system root CAs and roxxy's CA cert, allowing apk to verify TLS through roxxy's proxy
COPY --from=builder --chown=root:root /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# Copy the roxxy CA cert file from builder if it exists (named upstream_ca.crt in builder)
# Use RUN with mount to access builder filesystem since COPY doesn't support optional files
RUN --mount=type=bind,from=builder,source=/usr/local/share/ca-certificates,target=/builder-certs \
    if [ -f /builder-certs/upstream_ca.crt ]; then \
        cp /builder-certs/upstream_ca.crt /usr/local/share/ca-certificates/upstream_ca.crt && \
        chmod 644 /usr/local/share/ca-certificates/upstream_ca.crt && \
        cat /usr/local/share/ca-certificates/upstream_ca.crt >> /etc/ssl/certs/ca-certificates.crt && \
        echo "✓ Copied upstream_ca.crt from builder"; \
    elif [ -f /builder-certs/roxxy-ca.crt ]; then \
        cp /builder-certs/roxxy-ca.crt /usr/local/share/ca-certificates/roxxy-ca.crt && \
        chmod 644 /usr/local/share/ca-certificates/roxxy-ca.crt && \
        cat /usr/local/share/ca-certificates/roxxy-ca.crt >> /etc/ssl/certs/ca-certificates.crt && \
        echo "✓ Copied roxxy-ca.crt from builder"; \
    fi

RUN wget --post-data='{"name":"roxxy"}' http://host.docker.internal:3128/api/v1/sbom/start || true
# Now install ca-certificates package (this will work because we already have a valid cert bundle)
# This ensures the package is installed and update-ca-certificates is available
# The bundle we copied above will be preserved/updated by this installation
RUN apk add --no-cache ca-certificates

# Ensure roxxy's cert is properly in the trust store after ca-certificates installation
RUN if [ -f /usr/local/share/ca-certificates/upstream_ca.crt ]; then \
        update-ca-certificates 2>/dev/null || true && \
        cat /usr/local/share/ca-certificates/upstream_ca.crt >> /etc/ssl/certs/ca-certificates.crt; \
        echo "✓ Re-added upstream_ca.crt to trust store"; \
    elif [ -f /usr/local/share/ca-certificates/roxxy-ca.crt ]; then \
        update-ca-certificates 2>/dev/null || true && \
        cat /usr/local/share/ca-certificates/roxxy-ca.crt >> /etc/ssl/certs/ca-certificates.crt; \
        echo "✓ Re-added roxxy-ca.crt to trust store"; \
    fi

# Optionally copy roxxy's CA cert at build time (if it exists in build context)
# This allows apk to work through roxxy's proxy during build (if Docker is using it)
# If the cert doesn't exist at build time, it can be generated at runtime with:
#   roxxy server --generate
# This will generate the cert and add it to the system trust store automatically.
# Note: The root CA certs from ca-certificates package are already in the bundle,
# so roxxy can verify upstream TLS connections (quay.io, docker.io, etc.) even if 
# roxxy's own CA cert isn't added yet. The root CA certs are what Go's TLS library
# uses to verify upstream connections, and those come from Alpine's ca-certificates package.
# We use a shell script to make the copy optional (won't fail build if cert doesn't exist)
RUN --mount=type=bind,source=.,target=/buildctx \
    if [ -f /buildctx/certs/ca-cert.pem ]; then \
        cp /buildctx/certs/ca-cert.pem /usr/local/share/ca-certificates/roxxy-ca.crt && \
        chmod 644 /usr/local/share/ca-certificates/roxxy-ca.crt && \
        update-ca-certificates 2>/dev/null && \
        cat /usr/local/share/ca-certificates/roxxy-ca.crt >> /etc/ssl/certs/ca-certificates.crt && \
        echo "✓ Added roxxy CA cert from build context to system trust store"; \
    else \
        echo "Note: roxxy CA cert not found at build time, can be generated at runtime with 'roxxy server --generate'"; \
    fi

# Install runtime dependencies: xz for APT index decompression, jq for JSON inspection
RUN apk add --no-cache xz jq

# Set environment variable so Go's TLS library can find the root CA certificates
# This ensures Go can verify TLS connections to upstream registries (quay.io, docker.io, etc.)
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt

WORKDIR /app

RUN wget --post-data='{"name":"roxxy"}' http://host.docker.internal:3128/api/v1/sbom/stop || true
COPY --from=builder /build/roxxy /build/roar .
# Note: UI assets (incl. the generated js/*.min.js) are embedded in the binary via //go:embed

# Create cache directory and bootstrap init directory
RUN mkdir -p /app/cache /docker-entrypoint-initcache.d

# Create hardlinks for convenience commands
# The binary checks os.Args[0] to determine behavior
ENV PATH=$PATH:/app
RUN /app/roxxy hardlink

EXPOSE 3128 443

# Volume for cache bootstrap tarballs (similar to PostgreSQL's /docker-entrypoint-initdb.d/)
# Place .tar.gz or .tar files in this directory to bootstrap the cache on first startup
VOLUME ["/docker-entrypoint-initcache.d"]

ENTRYPOINT ["/app/roxxy"]
CMD ["server"]
