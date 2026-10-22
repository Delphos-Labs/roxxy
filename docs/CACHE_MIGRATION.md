# Cache Migration Guide

The `/app/cache` directory is fully portable and can be migrated between registry-proxy instances. This enables easy on-premise deployment with pre-populated caches.

## Cache Structure

The cache directory contains:
- `/app/cache/packages/` - Package manager caches (APK, APT, NPM, PyPI, Go modules) with `.meta` files
- `/app/cache/docker-manifests/` - Docker registry manifests (organized by registry/repository)
- `/app/cache/docker-blobs/` - Docker registry blobs (content-addressable by digest)
- `/app/cache/content/` - Content-addressable binary file storage
- `/app/cache/refcount/` - Reference counting files for content deduplication
- `/app/cache/url/` - URL metadata files
- `/app/cache/access-stats.json` - Access statistics JSON file

All paths are relative to `/app/cache`, making the cache fully portable.

## Use Cases

### 1. On-Premise Deployment

Build up a cache in a development/staging environment, then deploy it to on-premise instances:

```bash
# On development/staging environment
docker compose exec -T registry-proxy tar czf - -C /app cache > cache-bundle.tar.gz

# Transfer to on-premise environment (via USB, network, etc.)
# Then restore:
cat cache-bundle.tar.gz | docker compose exec -T registry-proxy tar xzf - -C /app
```

The on-premise instance will start with a warm cache, avoiding initial upstream fetches.

### 2. Cache Backup/Restore

Backup your cache for disaster recovery or to clone to another instance:

```bash
# Backup
docker compose exec -T registry-proxy tar czf - -C /app cache > cache-backup-$(date +%Y%m%d).tar.gz

# Restore
cat cache-backup-20241228.tar.gz | docker compose exec -T registry-proxy tar xzf - -C /app
```

### 3. Cache Sharing Between Instances

Share a pre-populated cache across multiple registry-proxy instances:

```bash
# Export from source instance
docker compose exec -T registry-proxy tar czf - -C /app cache > shared-cache.tar.gz

# Import to destination instances
cat shared-cache.tar.gz | docker compose exec -T registry-proxy tar xzf - -C /app
```

## Commands

### Backup Cache

```bash
# Create a tarball of the cache directory
docker compose exec -T registry-proxy tar czf - -C /app cache > cache-backup.tar.gz
```

### Restore Cache

```bash
# Extract the tarball into the cache directory
cat cache-backup.tar.gz | docker compose exec -T registry-proxy tar xzf - -C /app
```

Or using stdin redirection:

```bash
docker compose exec -T registry-proxy tar xzf - -C /app < cache-backup.tar.gz
```

## Notes

- The `-T` flag tells docker exec not to allocate a TTY (required for piping)
- The `-` in tar commands means stdout/stdin
- File permissions are preserved automatically
- The cache will work immediately after extraction since all paths are relative
- The cache is content-addressable, so duplicate content is automatically deduplicated
- Docker registry images stored in `/app/cache/docker-manifests/` and `/app/cache/docker-blobs/` are fully portable

