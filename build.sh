#!/bin/bash
# Build script for roxxy (native builds outside Docker)
# Usage: ./build.sh [output-path] [--buildtime TIMESTAMP] [--release]

set -e

OUTPUT="./roxxy"
BUILD_TIME=""
RELEASE=false

# Parse arguments
while [[ $# -gt 0 ]]; do
	case $1 in
		--buildtime)
			BUILD_TIME="$2"
			shift 2
			;;
		--release)
			RELEASE=true
			shift
			;;
		--no-docker)
			NODOCKER=true
			shift
			;;
		--no-cache)
			NOCACHE=true
			shift
			;;
		*)
			if [[ -z "$OUTPUT" || "$OUTPUT" == "./roxxy" ]]; then
				OUTPUT="$1"
			fi
			shift
			;;
	esac
done

# Default output if not specified
if [[ "$OUTPUT" == "./roxxy" && -n "$1" ]]; then
	OUTPUT="$1"
fi

# Set build time if not provided
if [[ -z "$BUILD_TIME" ]]; then
	BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)
fi

echo "Building roxxy..."
echo "Build time: $BUILD_TIME"
echo "Output: $OUTPUT"
if [[ "$RELEASE" == "true" ]]; then
	echo "Mode: RELEASE (stripped, optimized)"
	LDFLAGS="-X main.BuildTime=$BUILD_TIME -s -w"
	BUILD_FLAGS="-trimpath"
else
	echo "Mode: DEBUG (with debug info)"
	LDFLAGS="-X main.BuildTime=$BUILD_TIME"
	BUILD_FLAGS=""
fi
echo ""

# Generate embedded UI assets (minifies js/*.js -> js/*.min.js via build-js.js).
# Single source of truth: the go:generate directive in internal/ui — not a copy
# here. Requires node + esbuild; fails loudly rather than shipping a UI whose
# index.html references missing .min.js.
echo "Generating embedded UI assets..."
go generate ./...
echo "✓ UI assets generated"
echo ""

# Validate HTML templates and JavaScript before building
echo "Validating HTML templates and JavaScript..."
if ! go test ./internal/ui/static -run TestValidate -v 2>&1; then
	echo "ERROR: Template/JavaScript validation failed (see output above)"
	echo "Build aborted due to validation errors"
	exit 1
fi
echo "✓ Templates and JavaScript validated"
echo ""

go build -ldflags "$LDFLAGS" $BUILD_FLAGS -o "$OUTPUT" .

echo ""
echo "Building roar (roxxy archiver)..."
ROAR_OUTPUT="./roar"
go build -ldflags "$LDFLAGS" $BUILD_FLAGS -o "$ROAR_OUTPUT" ./cmd/roar

if [ -z "$NODOCKER" ]; then
	echo ""
	echo "Building docker container"
	curl -d '{"name": "roxxy"}' http://localhost:3128/api/v1/sbom/start
	docker build -f docker/Dockerfile -t delphos/roxxy:latest . --push ${NOCACHE+--no-cache}
	curl -d '{"name": "roxxy"}' http://localhost:3128/api/v1/sbom/stop
	curl -d '{"name": "roxxy"}' http://localhost:3128/api/v1/sbom/output -o sbom.json
fi

echo ""
echo "✓ Build complete:"
echo "  - roxxy: $OUTPUT"
echo "  - roar:  $ROAR_OUTPUT"
echo ""
echo "To install to /usr/local/bin:"
echo "  sudo cp $OUTPUT /usr/local/bin/roxxy"
echo "  sudo cp $ROAR_OUTPUT /usr/local/bin/roar"
echo ""
echo "To install roxxy as a service:"
echo "  roxxy service install"
