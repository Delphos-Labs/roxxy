# Scratch Container Deployment

This directory contains a minimal scratch-based Dockerfile for roxxy that produces an extremely small, secure container image.

## What is Scratch?

Scratch is the smallest possible Docker base image - it's completely empty. This means:
- **No shell** - no `/bin/sh`, no shell scripts
- **No libc** - only statically compiled binaries work
- **Minimal attack surface** - no unnecessary software
- **Tiny image size** - typically < 10MB vs ~50MB for Alpine

## Requirements

Since scratch has no shell or libc:
- ✅ Go binaries (statically compiled with `CGO_ENABLED=0`)
- ✅ CA certificates (copied from Alpine)
- ❌ Shell scripts (replaced with Go CLI commands)

## Building

```bash
docker build -f docker/Dockerfile.scratch -t roxxy:scratch .
```

## Running

```bash
# Using docker-compose
docker compose -f docker/docker-compose.scratch.yml up -d

# Or manually:
docker run -d --name roxxy \
  -p 3128:3128 \
  -v ./certs:/app/certs:ro \
  -v ./config.yaml:/app/config.yaml:ro \
  -v cache-data:/app/cache \
  roxxy:scratch
```

## CLI Commands

The binary can be called directly or via convenience hardlinks:

```bash
# Using convenience commands (recommended)
docker exec roxxy roxxy docker ls
docker exec roxxy roxxy docker rmi library/alpine:latest
docker exec roxxy roxxy docker prune
docker exec roxxy roxxy cache ls
docker exec roxxy roxxy cache flush
docker exec roxxy roxxy package ls

# Or using subcommands (also works)
docker exec roxxy roxxy docker ls
docker exec roxxy roxxy cache ls
docker exec roxxy roxxy package ls
```

The binary detects its name (`os.Args[0]`) and routes accordingly, so hardlinks work perfectly in scratch containers.

## Configuration

Roxxy uses standard HTTP proxy configuration. See the main README for Docker configuration options.

## Image Size Comparison

- **Alpine-based**: ~50MB (includes shell, libc)
- **Scratch-based**: ~8-10MB (just the Go binaries + CA certs)

## Trade-offs

**Pros:**
- Extremely small image size
- Minimal attack surface
- Fast container startup
- No unnecessary dependencies

**Cons:**
- No shell for debugging (use `docker exec` with binaries)
- No shell scripts (use Go CLI commands instead)

## Migration from Alpine

1. Replace shell scripts with Go CLI commands (already done)
2. Update any scripts that reference `/bin/sh` or shell commands

