# Running Roxxy with Docker Compose

This docker-compose configuration provides a simple way to run roxxy in a container with persistent storage, perfect for users who don't want to run native services.

## Quick Start

1. **Copy the example configuration:**
   ```bash
   cp docker-compose.example.yml docker-compose.yml
   ```

2. **Build the image:**
   ```bash
   docker compose build
   ```

3. **Start roxxy:**
   ```bash
   docker compose up -d
   ```

4. **View logs:**
   ```bash
   docker compose logs -f roxxy
   ```

5. **Access the web UI:**
   Open http://localhost:3128/ui in your browser

## Configuration

### Volumes

The docker-compose file uses named volumes for persistence:

- **roxxy-cache**: Stores all cached artifacts (Docker images, packages, etc.)
- **roxxy-certs**: Stores CA certificate and key for MITM TLS

### Ports

- **3128**: HTTP/HTTPS proxy port (for HTTP_PROXY/HTTPS_PROXY)
- **443**: HTTPS SNI routing port (for direct HTTPS connections)

### Using Roxxy as a Proxy

To use roxxy as a proxy in other containers, set these environment variables:

```yaml
environment:
  HTTP_PROXY: http://roxxy:3128
  HTTPS_PROXY: http://roxxy:3128
```

Or in docker-compose:

```yaml
services:
  my-app:
    environment:
      HTTP_PROXY: http://roxxy:3128
      HTTPS_PROXY: http://roxxy:3128
    depends_on:
      - roxxy
```

### Custom Configuration

To use a custom `config.yaml`:

1. Create or copy your `config.yaml` to the current directory
2. Uncomment this line in `docker-compose.yml`:
   ```yaml
   - ./config.yaml:/app/config.yaml:ro
   ```

### Bootstrap Cache

To bootstrap the cache from a tarball on first startup:

1. Create a `cache-bootstrap` directory:
   ```bash
   mkdir -p cache-bootstrap
   ```

2. Place `.tar` or `.tar.gz` files in `cache-bootstrap/`
3. Uncomment this line in `docker-compose.yml`:
   ```yaml
   - ./cache-bootstrap:/docker-entrypoint-initcache.d:ro
   ```

The tarballs will be automatically imported when the container starts for the first time.

### Accessing the CA Certificate

To download the CA certificate for MITM TLS:

```bash
curl http://localhost:3128/api/v1/certs/ca -o roxxy-ca.crt
```

Then install it in your system's trust store (varies by OS).

### Using Bind Mounts Instead of Named Volumes

If you prefer to use bind mounts for direct access to files:

```yaml
volumes:
  - ./cache:/app/cache
  - ./certs:/app/certs
```

This allows you to directly access the cache and certificates from the host filesystem.

## Maintenance

### Viewing Cache Statistics

Access the web UI at http://localhost:3128/ui or use the API:

```bash
curl http://localhost:3128/api/v1/stats/summary
```

### Exporting Cache

Export the entire cache:

```bash
curl http://localhost:3128/api/v1/export -o roxxy-cache.tar
```

Export only Docker images:

```bash
curl http://localhost:3128/api/v1/export/docker -o docker-cache.tar
```

### Updating Roxxy

1. Pull the latest code or rebuild:
   ```bash
   docker compose build
   ```

2. Restart the container:
   ```bash
   docker compose up -d
   ```

The cache and certificates will persist across updates.

## Troubleshooting

### Container won't start

Check the logs:
```bash
docker compose logs roxxy
```

### Port already in use

If port 3128 or 443 is already in use, change the port mappings:

```yaml
ports:
  - "3129:3128"  # Use 3129 on host instead of 3128
  - "4443:443"   # Use 4443 on host instead of 443
```

### Certificate issues

If you need to regenerate certificates:

1. Stop the container:
   ```bash
   docker compose down
   ```

2. Remove the certs volume (or delete `./certs` if using bind mount):
   ```bash
   docker volume rm roxxy_certs
   ```

3. Start the container again - it will generate new certificates

### Cache location

To find where the cache volume is stored:

```bash
docker volume inspect roxxy_cache
```

Look for the `Mountpoint` field.
