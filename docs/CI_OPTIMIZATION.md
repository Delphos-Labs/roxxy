# CI/CD Optimization with Roxxy

Roxxy significantly speeds up CI/CD pipelines by caching Docker images and package manager artifacts, reducing build times and network bandwidth usage. This is especially valuable in Docker-in-Docker (DinD) scenarios and airgapped environments.

## Overview

CI/CD pipelines often:
- Pull the same Docker images repeatedly across builds
- Download the same packages (APT, APK, NPM, PyPI, Go) for every job
- Run in isolated environments where each build starts from scratch
- Operate in airgapped networks where external access is limited or slow

Roxxy solves these problems by:
- **Caching Docker images locally** - Subsequent pulls are instant
- **Caching package manager artifacts** - Package downloads are served from cache
- **Reducing network bandwidth** - Only fetch once, serve from cache forever
- **Enabling airgap workflows** - Pre-populate cache, then run builds offline

## Docker-in-Docker (DinD) Scenarios

Docker-in-Docker is common in CI/CD pipelines where:
- Build jobs run inside Docker containers
- Each build needs to pull base images
- Builds create and push new images
- Multiple parallel jobs run simultaneously

### Problem Without Roxxy

```
Build Job 1: docker pull nginx:alpine  → 50MB download, 10s
Build Job 2: docker pull nginx:alpine  → 50MB download, 10s
Build Job 3: docker pull nginx:alpine  → 50MB download, 10s
...
Build Job N: docker pull nginx:alpine  → 50MB download, 10s

Total: N × 50MB bandwidth, N × 10s time
```

### Solution With Roxxy

```
Build Job 1: docker pull nginx:alpine  → 50MB download, 10s (cache miss)
Build Job 2: docker pull nginx:alpine  → 0MB download, <1s (cache hit)
Build Job 3: docker pull nginx:alpine  → 0MB download, <1s (cache hit)
...
Build Job N: docker pull nginx:alpine  → 0MB download, <1s (cache hit)

Total: 50MB bandwidth, 10s + (N-1) × <1s time
```

### Setup for Docker-in-Docker

**Option 1: Roxxy as a Service Container (Recommended)**

```yaml
# GitHub Actions example
services:
  roxxy:
    image: roxxy:latest
    ports:
      - 3128:3128
    volumes:
      - roxxy-cache:/app/cache
    environment:
      - CONFIG_FILE=/app/config.yaml

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - name: Configure Docker to use roxxy
        run: |
          echo '{"proxies":{"http-proxy":"http://roxxy:3128","https-proxy":"http://roxxy:3128"}}' | sudo tee /etc/docker/daemon.json
          sudo systemctl restart docker
      
      - name: Build Docker image
        run: docker build -t myapp:latest .
```

**Option 2: Roxxy in Docker Compose**

```yaml
version: '3.8'
services:
  roxxy:
    image: roxxy:latest
    ports:
      - "3128:3128"
    volumes:
      - roxxy-cache:/app/cache
      - ./config.yaml:/app/config.yaml:ro
    restart: unless-stopped

  # Your CI build service
  builder:
    image: docker:dind
    privileged: true
    environment:
      - HTTP_PROXY=http://roxxy:3128
      - HTTPS_PROXY=http://roxxy:3128
      - NO_PROXY=localhost,127.0.0.1
    depends_on:
      - roxxy
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock

volumes:
  roxxy-cache:
```

**Option 3: Kubernetes Sidecar**

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: ci-builder
spec:
  containers:
  - name: roxxy
    image: roxxy:latest
    ports:
    - containerPort: 3128
    volumeMounts:
    - name: roxxy-cache
      mountPath: /app/cache
    env:
    - name: CONFIG_FILE
      value: /app/config.yaml
  
  - name: builder
    image: docker:dind
    securityContext:
      privileged: true
    env:
    - name: HTTP_PROXY
      value: http://localhost:3128
    - name: HTTPS_PROXY
      value: http://localhost:3128
    volumeMounts:
    - name: docker-sock
      mountPath: /var/run/docker.sock
  
  volumes:
  - name: roxxy-cache
    persistentVolumeClaim:
      claimName: roxxy-cache-pvc
  - name: docker-sock
    emptyDir: {}
```

### Benefits in DinD

1. **Shared Cache Across Jobs**: All build jobs share the same roxxy cache
2. **Parallel Build Speedup**: Multiple jobs pulling the same image get instant cache hits
3. **Persistent Cache**: Cache survives between CI runs (with persistent volumes)
4. **Bandwidth Savings**: Only the first job downloads, others use cache

## Airgapped Environments

Airgapped CI/CD environments have no direct internet access, making roxxy essential for:
- Pre-populating caches before deployment
- Serving cached artifacts
- Maintaining build reproducibility

### Workflow: Preparation → Deployment

**Phase 1: Preparation (Connected Environment)**

```bash
# Start roxxy in airgap-proxy mode (caches everything, no size limits)
roxxy --airgap-proxy server

# Run your CI pipeline to populate cache
# All Docker pulls and package downloads will be cached
./run-ci-pipeline.sh

# Export the cache
tar -czf roxxy-cache.tar.gz /app/cache

# Transfer to airgapped environment (USB, network transfer, etc.)
```

**Phase 2: Deployment (Airgapped Environment)**

```bash
# Extract cache
tar -xzf roxxy-cache.tar.gz -C /app

# Start roxxy in airgapped mode (no external access)
roxxy --airgapped server

# Run CI pipeline - all builds use cached artifacts
./run-ci-pipeline.sh
```

### Docker Compose for Airgapped CI

```yaml
version: '3.8'
services:
  roxxy:
    image: roxxy:latest
    ports:
      - "3128:3128"
    volumes:
      - ./cache:/app/cache  # Pre-populated cache
      - ./config.yaml:/app/config.yaml:ro
    command: ["--airgapped", "server"]
    restart: unless-stopped

  builder:
    image: docker:dind
    privileged: true
    environment:
      - HTTP_PROXY=http://roxxy:3128
      - HTTPS_PROXY=http://roxxy:3128
    depends_on:
      - roxxy
```

### Benefits in Airgapped Environments

1. **Build Reproducibility**: Same artifacts served every time
2. **No External Dependencies**: All builds work offline
3. **Security**: No need to allow external internet access
4. **Speed**: Local cache is faster than any external source
5. **Predictability**: Build times are consistent and fast

## CI Platform Examples

### GitHub Actions

```yaml
name: Build with Roxxy Cache

on: [push, pull_request]

services:
  roxxy:
    image: roxxy:latest
    ports:
      - 3128:3128
    volumes:
      - roxxy-cache:/app/cache
    options: >-
      --health-cmd="curl -f http://localhost:3128/ui/ || exit 1"
      --health-interval=10s
      --health-timeout=5s
      --health-retries=3

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      
      - name: Configure Docker for roxxy
        run: |
          echo '{
            "proxies": {
              "http-proxy": "http://roxxy:3128",
              "https-proxy": "http://roxxy:3128",
              "no-proxy": "localhost,127.0.0.1"
            }
          }' | sudo tee /etc/docker/daemon.json
          sudo systemctl restart docker
      
      - name: Build Docker image
        run: docker build -t myapp:${{ github.sha }} .
      
      - name: Push to registry
        run: docker push myapp:${{ github.sha }}

volumes:
  roxxy-cache:
```

### GitLab CI

```yaml
services:
  - name: roxxy:latest
    alias: roxxy

variables:
  HTTP_PROXY: http://roxxy:3128
  HTTPS_PROXY: http://roxxy:3128
  NO_PROXY: localhost,127.0.0.1

build:
  image: docker:latest
  services:
    - docker:dind
  before_script:
    - echo '{"proxies":{"http-proxy":"http://roxxy:3128","https-proxy":"http://roxxy:3128"}}' > /etc/docker/daemon.json
    - service docker restart
  script:
    - docker build -t myapp:$CI_COMMIT_SHA .
    - docker push myapp:$CI_COMMIT_SHA
```

### Jenkins

```groovy
pipeline {
    agent any
    
    environment {
        HTTP_PROXY = 'http://roxxy:3128'
        HTTPS_PROXY = 'http://roxxy:3128'
    }
    
    stages {
        stage('Build') {
            steps {
                sh '''
                    docker build -t myapp:${BUILD_NUMBER} .
                '''
            }
        }
    }
}
```

### CircleCI

```yaml
version: 2.1

jobs:
  build:
    docker:
      - image: docker:latest
      - image: roxxy:latest
        environment:
          CONFIG_FILE: /app/config.yaml
    environment:
      HTTP_PROXY: http://localhost:3128
      HTTPS_PROXY: http://localhost:3128
    steps:
      - checkout
      - setup_remote_docker:
          version: 20.10.14
      - run:
          name: Build
          command: |
            docker build -t myapp:$CIRCLE_SHA1 .
```

## Package Manager Caching

Roxxy also caches package manager downloads, speeding up builds that install dependencies:

### APT (Debian/Ubuntu)

```dockerfile
FROM ubuntu:22.04
# Configure APT to use roxxy
RUN echo 'Acquire::http::Proxy "http://roxxy:3128";' > /etc/apt/apt.conf.d/proxy.conf
RUN apt-get update && apt-get install -y python3 python3-pip
```

### APK (Alpine)

```dockerfile
FROM alpine:3.18
# Configure APK to use roxxy
RUN echo "http://roxxy:3128" > /etc/apk/proxy
RUN apk add --no-cache python3 py3-pip
```

### NPM

```dockerfile
FROM node:18
# Configure NPM to use roxxy
RUN npm config set proxy http://roxxy:3128
RUN npm config set https-proxy http://roxxy:3128
RUN npm install
```

### PyPI

```dockerfile
FROM python:3.11
# Configure pip to use roxxy
ENV HTTP_PROXY=http://roxxy:3128
ENV HTTPS_PROXY=http://roxxy:3128
RUN pip install -r requirements.txt
```

### Go Modules

```dockerfile
FROM golang:1.21
# Configure Go to use roxxy
ENV HTTP_PROXY=http://roxxy:3128
ENV HTTPS_PROXY=http://roxxy:3128
ENV GOPROXY=https://proxy.golang.org,direct
RUN go mod download
```

## Performance Metrics

### Typical Speedup

| Scenario | Without Roxxy | With Roxxy | Speedup |
|----------|---------------|------------|---------|
| First Docker pull | 10-30s | 10-30s | 1x (cache miss) |
| Subsequent pulls | 10-30s | <1s | 10-30x |
| Package install (cached) | 5-15s | <1s | 5-15x |
| Parallel builds (shared cache) | N × 30s | 30s + (N-1) × <1s | N× speedup |

### Bandwidth Savings

- **Docker images**: 100% reduction after first pull
- **Package downloads**: 100% reduction after first download
- **Index files**: 100% reduction after first fetch

## Best Practices

### 1. Persistent Cache Volumes

Always use persistent volumes for roxxy cache in CI:

```yaml
volumes:
  roxxy-cache:
    driver: local
    driver_opts:
      type: none
      o: bind
      device: /path/to/persistent/cache
```

### 2. Cache Warming

Pre-populate cache before CI runs:

```bash
# Warm cache with common images
docker pull nginx:alpine
docker pull node:18
docker pull python:3.11
docker pull golang:1.21
```

### 3. Cache Size Management

Monitor cache size and prune when needed:

```bash
# Check cache size
roxxy cache ls --sort size

# Prune unreferenced Docker blobs
roxxy docker prune
```

### 4. Health Checks

Add health checks to ensure roxxy is ready:

```yaml
healthcheck:
  test: ["CMD", "curl", "-f", "http://localhost:3128/ui/"]
  interval: 10s
  timeout: 5s
  retries: 3
```

### 5. Logging and Monitoring

Monitor cache hit rates:

```bash
# View access statistics
cat ~/.cache/roxxy/access-stats.json

# Or use web UI
open http://localhost:3128/ui/
```

## Troubleshooting

### Docker Build Not Using Cache

**Problem**: Docker builds still downloading images

**Solution**:
1. Verify Docker is configured to use roxxy:
   ```bash
   cat /etc/docker/daemon.json
   ```
2. Check roxxy logs for requests:
   ```bash
   roxxy logs | grep docker
   ```
3. Verify cache is populated:
   ```bash
   roxxy docker ls
   ```

### Package Manager Not Using Cache

**Problem**: Package installs still downloading

**Solution**:
1. Verify proxy environment variables are set
2. Check roxxy is receiving requests:
   ```bash
   roxxy logs | grep -E "(npm|pip|apt|apk|go)"
   ```
3. Verify package cache:
   ```bash
   roxxy package ls
   ```

### 401 Authentication Errors

**Problem**: Getting 401 errors when pulling images

**Solution**: See [TROUBLESHOOTING.md](TROUBLESHOOTING.md) for authentication setup. Roxxy automatically handles Docker Hub authentication, but you may need to configure credentials for private registries.

## See Also

- [README.md](../README.md) - General roxxy documentation
- [AIRGAP.md](AIRGAP.md) - Airgap deployment guide
- [TROUBLESHOOTING.md](TROUBLESHOOTING.md) - Troubleshooting guide

