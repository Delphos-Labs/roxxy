# Airgap Deployment Guide

Roxxy supports airgapped deployments through two operational modes that can be used independently or together.

## Overview

Airgap deployment involves two phases:

1. **Preparation Phase** (`--airgap-proxy`): Cache all required files with no size limits
2. **Deployment Phase** (`--airgapped`): Run in airgapped mode with no external access

## Flags

### `--airgap-proxy`

**Purpose**: Cache all files with no size limits (for airgap preparation)

**Behavior**:
- Sets `MinCacheSize` to 0 for all package managers
- Caches all files regardless of size
- Still allows external access to fetch missing artifacts
- Useful for pre-populating the cache before moving to an airgapped environment

**Use Case**: Run this mode in a connected environment to build up a complete cache of all required artifacts before transferring to an airgapped network.

### `--airgapped`

**Purpose**: Airgapped mode - no external access, return 404 for missing artifacts

**Behavior**:
- Disables all external network access
- Returns HTTP 404 (Not Found) for cache misses
- Only serves artifacts that are already cached
- Applied to both Docker registry and package manager proxies

**Use Case**: Run this mode in an airgapped environment where external network access is not available or desired.

## Usage Examples

### Phase 1: Prepare Cache (Connected Environment)

```bash
# Start roxxy in airgap-proxy mode to cache everything
./roxxy --airgap-proxy server
```

During this phase:
- All Docker image pulls will be cached
- All package manager requests (APK, APT, NPM, PyPI, Go) will be cached
- No size limits are applied - even small files are cached
- External access is still available for cache misses

### Phase 2: Deploy to Airgapped Environment

```bash
# Start roxxy in airgapped mode (no external access)
./roxxy --airgapped server
```

During this phase:
- Only cached artifacts are served
- Missing artifacts return 404
- No external network requests are made
- Safe for use in airgapped networks

### Combined Usage

While you can combine both flags, it's typically not necessary:

```bash
# This combination is redundant - airgapped already prevents external access
./roxxy --airgap-proxy --airgapped server
```

The typical workflow is:
1. Use `--airgap-proxy` in a connected environment to build the cache
2. Transfer the cache directory to the airgapped environment
3. Use `--airgapped` in the airgapped environment

## Implementation Details

### Docker Registry Proxies

When `--airgapped` is enabled:
- Cache misses return HTTP 404 instead of fetching from upstream
- All Docker registry requests (manifests, blobs) are served from cache only
- No upstream authentication or token fetching occurs

### Package Manager Proxies

When `--airgap-proxy` is enabled:
- `MinCacheSize` is set to 0 for all package types (APK, APT, NPM, PyPI, Go)
- All files are cached regardless of size
- Index files and small metadata files are cached

When `--airgapped` is enabled:
- Cache misses return HTTP 404 before attempting upstream fetch
- All package requests are served from cache only
- No upstream network requests are made

## Cache Transfer

To move from a connected environment to an airgapped environment:

1. **In connected environment** (with `--airgap-proxy`):
   ```bash
   # Run roxxy and let it cache everything
   ./roxxy --airgap-proxy server
   
   # Use your build system to trigger all required artifact downloads
   # This will populate the cache
   ```

2. **Transfer the cache**:
   ```bash
   # Copy the entire cache directory
   tar -czf roxxy-cache.tar.gz /app/cache
   
   # Transfer to airgapped environment (via USB, network transfer, etc.)
   ```

3. **In airgapped environment**:
   ```bash
   # Extract the cache
   tar -xzf roxxy-cache.tar.gz -C /app
   
   # Start roxxy in airgapped mode
   ./roxxy --airgapped server
   ```

## Cache Directory Structure

The cache directory (`/app/cache` by default) contains:

- `docker-manifests/` - Docker image manifests
- `docker-blobs/` - Docker image blobs (layers)
- `packages/` - Package manager artifacts organized by type:
  - `apk/` - Alpine packages
  - `apt/` - Debian/Ubuntu packages
  - `npm/` - NPM packages
  - `pypi/` - Python packages
  - `go/` - Go modules
- `access-stats.json` - Access statistics
- `refcount/` - Reference counting for content-addressable storage

## Verification

### Check Cache Contents

```bash
# List Docker images
./roxxy docker ls

# List cached packages
./roxxy package ls

# List all cached content
./roxxy cache ls
```

### Test Airgapped Mode

```bash
# Start in airgapped mode
./roxxy --airgapped server

# Try to pull a cached image (should work)
docker pull nginx:alpine

# Try to pull an uncached image (should fail with 404)
docker pull some-uncached-image:tag
```

## Troubleshooting

### Missing Artifacts in Airgapped Mode

If you get 404 errors for artifacts you expect to be cached:

1. Verify the cache was properly transferred
2. Check cache contents: `./roxxy cache ls`
3. Ensure you ran `--airgap-proxy` mode long enough to cache everything
4. Check logs for cache hit/miss information

### Cache Not Populating in Airgap-Proxy Mode

If files aren't being cached:

1. Verify `--airgap-proxy` flag is set
2. Check that requests are actually going through roxxy
3. Review logs for cache storage messages
4. Ensure sufficient disk space is available

### Network Requests Still Occurring in Airgapped Mode

If you see network activity in airgapped mode:

1. Verify `--airgapped` flag is set correctly
2. Check that all proxy handlers received the flag
3. Review logs for upstream fetch attempts (should not occur)

## Best Practices

1. **Pre-populate thoroughly**: Run `--airgap-proxy` mode for an extended period to ensure all required artifacts are cached
2. **Test before deployment**: Verify all required artifacts are cached before moving to airgapped environment
3. **Monitor cache size**: Ensure sufficient storage is available for the complete cache
4. **Document requirements**: Keep track of which artifacts are needed for your airgapped deployment
5. **Version control**: Consider versioning your cache exports for reproducibility

## See Also

- [../README.md](../README.md) - General roxxy documentation
- [PROVENANCE.md](PROVENANCE.md) - Provenance & attestation model for airgap deliveries (content-addressing + SBOM + cosign)
- [DATA_FLOW.md](DATA_FLOW.md) - Data flow architecture
- [TROUBLESHOOTING.md](TROUBLESHOOTING.md) - General troubleshooting guide

