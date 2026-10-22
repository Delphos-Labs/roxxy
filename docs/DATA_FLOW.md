# Roxxy Data Flow Diagram

This diagram shows how data flows into and out of roxxy, including Docker client configuration via HTTP_PROXY, docker build integration, and cache storage.

## High-Level Overview

```mermaid
flowchart LR
    subgraph Clients["Clients"]
        Docker["Docker Client<br/>HTTP_PROXY:3128"]
        PM["Package Managers<br/>HTTP_PROXY:3128"]
    end

    subgraph Proxy["Roxxy Proxy"]
        Entry["Entry Point<br/>Port 3128<br/>(CONNECT Proxy)"]
        Optional["Optional Ports<br/>443 (SNI), 80 (HTTP)<br/>(Docker only)"]
        Route["Request Router"]
        Handler["Handlers"]
    end

    subgraph Storage["Cache Storage"]
        Cache["Filesystem Cache"]
    end

    subgraph External["External"]
        Upstream["Upstream Sources"]
        ChainProxy["Upstream Proxy<br/>(if chaining)"]
    end

    Docker --> Entry
    PM --> Entry
    Entry --> Optional
    Optional --> Route
    Entry --> Route
    Route --> Handler
    Handler --> Cache
    Handler --> ChainProxy
    Handler --> Upstream
    ChainProxy --> Upstream
    Upstream --> Cache
    Cache --> Handler
    Handler --> Clients

    style Docker fill:#e1f5ff
    style PM fill:#e1f5ff
    style Handler fill:#fff4e1
    style Cache fill:#e8f5e9
    style ChainProxy fill:#fff9c4
```

**See detailed flows below:**
- [Docker Client Flow](#docker-client-flow) → [Request Routing](#request-routing) → [Cache Storage](#cache-storage)
- [Package Manager Flow](#package-manager-flow) → [Request Routing](#request-routing) → [Cache Storage](#cache-storage)

## Docker Client Flow

Docker clients (including `docker build`, `docker pull`, etc.) connect via HTTP_PROXY on port 3128.

```mermaid
flowchart TB
    Docker["Docker Client<br/>HTTP_PROXY=localhost:3128"]
    CONN["CONNECT Handler<br/>Port 3128"]
    Extract["Extract Hostname<br/>from CONNECT request"]
    TLS["TLS Termination<br/>Generate cert for hostname"]
    DRP["DockerRegistryProxy<br/>Handler"]
    CacheCheck{"Check Cache"}
    CacheHit["Return from Cache"]
    CacheMiss["Fetch from Upstream"]
    Store["Store in Cache"]
    Response["Return to Client"]

    Docker -->|"CONNECT registry:443"| CONN
    CONN --> Extract
    Extract --> TLS
    TLS -->|"Route to handler"| DRP
    DRP --> CacheCheck
    
    CacheCheck -->|"Hit"| CacheHit
    CacheCheck -->|"Miss"| CacheMiss
    
    CacheHit --> Response
    CacheMiss -->|"Fetch"| Store
    Store --> Response
    Response --> Docker

    style Docker fill:#e1f5ff
    style DRP fill:#fff4e1
    style CacheHit fill:#e8f5e9
    style Store fill:#e8f5e9
```

**Connects to:** [Request Routing](#request-routing) | [Cache Storage](#cache-storage)

```mermaid
sequenceDiagram
    participant Docker as Docker Client
    participant CONN as CONNECT Handler (3128)
    participant DRP as DockerRegistryProxy
    participant Cache as Cache Storage
    participant Upstream as Upstream Registry

    Docker->>CONN: CONNECT registry-1.docker.io:443
    CONN->>CONN: Extract hostname from CONNECT
    CONN->>CONN: Generate TLS cert for hostname
    CONN->>CONN: Establish TLS tunnel
    CONN->>DRP: Route to DockerRegistryProxy
    
    Docker->>DRP: HTTPS: /v2/<repo>/manifests/<ref>
    DRP->>DRP: Parse path: /v2/<repo>/manifests/<ref>
    DRP->>Cache: Check docker-manifests/<registry>/<repo>/<ref>
    
    alt Cache Hit
        Cache->>DRP: Return manifest + metadata
        DRP->>Docker: Return cached manifest
    else Cache Miss
        DRP->>Upstream: GET /v2/<repo>/manifests/<ref>
        Upstream->>DRP: Return manifest
        DRP->>Cache: Store manifest + .meta
        DRP->>Docker: Return manifest
    end
    
    Note over Docker,Upstream: Blob requests follow similar flow
```

**Note:** Ports 443 (SNI router) and 80 (HTTP router) are optional and only available when running in Docker containers. When running on the host, only port 3128 is used.

## Package Manager Flow

Package managers (APT, APK, NPM, PyPI, Go) connect via HTTP_PROXY on port 3128.

```mermaid
flowchart TB
    PM["Package Manager<br/>HTTP_PROXY=localhost:3128"]
    CONN["CONNECT Handler<br/>Port 3128"]
    Extract["Extract Hostname<br/>from CONNECT request"]
    TLS["TLS Termination<br/>Generate cert"]
    Route{"Route by<br/>Hostname"}
    PackageHandler["Package Manager Proxy<br/>APK, APT, NPM, PyPI, Go"]
    Cache["Cache Lookup"]
    Upstream["Upstream Fetch"]
    Response["Return Response"]

    PM -->|"CONNECT hostname:443"| CONN
    CONN --> Extract
    Extract --> TLS
    TLS --> Route
    
    Route -->|"dl-cdn.alpinelinux.org,<br/>registry.npmjs.org,<br/>pypi.org, etc."| PackageHandler
    
    PackageHandler --> Cache
    
    Cache -->|"Miss"| Upstream
    Upstream --> Cache
    Cache --> Response
    Response --> PM

    style PM fill:#e1f5ff
    style PackageHandler fill:#fff4e1
    style Cache fill:#e8f5e9
```

**Connects to:** [Request Routing](#request-routing) | [Cache Storage](#cache-storage)

## Request Routing

```mermaid
flowchart TB
    Entry["Entry Points"]
    Port3128["Port 3128<br/>CONNECT Proxy<br/>(Always Available)"]
    Port443["Port 443<br/>SNI Router<br/>(Docker Only)"]
    Port80["Port 80<br/>HTTP Router<br/>(Docker Only)"]
    
    Hostname{"Extract<br/>Hostname"}
    
    CheckDocker{"Docker<br/>Registry?"}
    CheckPackage{"Package<br/>Manager?"}
    CheckKnown{"Known<br/>Registry?"}
    
    DockerProxy["DockerRegistryProxy"]
    PackageProxy["Package Manager Proxy"]
    LocalCache["Local Cache Handler"]
    PassThrough["Pass-Through"]

    Entry --> Port3128
    Entry --> Port443
    Entry --> Port80
    
    Port3128 --> Hostname
    Port443 --> Hostname
    Port80 --> Hostname
    
    Hostname --> CheckDocker
    Hostname --> CheckPackage
    Hostname --> CheckKnown
    
    CheckDocker -->|"Yes"| DockerProxy
    CheckPackage -->|"Yes"| PackageProxy
    CheckKnown -->|"Yes"| LocalCache
    CheckKnown -->|"No"| PassThrough

    style DockerProxy fill:#fff4e1
    style PackageProxy fill:#fff4e1
    style LocalCache fill:#fff4e1
```

**Connects from:** [Docker Client Flow](#docker-client-flow) | [Package Manager Flow](#package-manager-flow)  
**Connects to:** [Cache Storage](#cache-storage)

## Cache Storage

```mermaid
flowchart TB
    Handler["Request Handler"]
    CheckCache{"Check Cache"}
    
    subgraph DockerCache["Docker Cache"]
        Manifests["docker-manifests/<br/>&lt;registry&gt;/&lt;repo&gt;/&lt;ref&gt;<br/>+ .meta files"]
        Blobs["docker-blobs/<br/>&lt;digest&gt;"]
    end
    
    subgraph PackageCache["Package Cache"]
        Packages["packages/&lt;type&gt;/<br/>&lt;path&gt;"]
        Index["index/&lt;type&gt;/<br/>Parsed metadata"]
    end
    
    subgraph Metadata["Metadata"]
        Stats["access-stats.json"]
        Refcount["refcount/<br/>&lt;hash&gt;"]
    end
    
    Upstream["Upstream Source"]
    Store["Store in Cache"]

    Handler --> CheckCache
    CheckCache -->|"Docker"| Manifests
    CheckCache -->|"Docker"| Blobs
    CheckCache -->|"Package"| Packages
    CheckCache -->|"Package"| Index
    
    CheckCache -->|"Miss"| Upstream
    Upstream --> Store
    Store --> Manifests
    Store --> Blobs
    Store --> Packages
    Store --> Index
    
    Manifests --> Stats
    Blobs --> Refcount
    Packages --> Stats
    Packages --> Refcount

    style Manifests fill:#e8f5e9
    style Blobs fill:#e8f5e9
    style Packages fill:#e8f5e9
    style Index fill:#e8f5e9
    style Stats fill:#e8f5e9
    style Refcount fill:#e8f5e9
```

**Connects from:** [Request Routing](#request-routing)  
**See:** [Cache Storage Structure](#cache-storage-structure) | [What Gets Stored](#what-gets-stored)

## Cache Storage Structure

### Docker Registry Cache

```
/app/cache/
├── docker-manifests/
│   └── <registry>/          # e.g., docker.io, quay.io
│       └── <repo>/           # e.g., library/golang → library_golang
│           ├── <tag>         # e.g., 1.25-alpine
│           ├── <tag>.meta      # Metadata (content_type, stored_at, upstream_url)
│           ├── sha256:<digest> # Manifest by digest
│           └── sha256:<digest>.meta
├── docker-blobs/
│   └── sha256:<digest>       # Blob content (layer data)
└── refcount/
    └── <hash>/               # Content hash (first 2 chars)
        └── <hash>/           # Next 2 chars
            └── <hash>         # Full hash (refcount file)
```

### Package Manager Cache

```
/app/cache/
├── packages/
│   ├── apk/                  # Alpine APK packages
│   ├── apt/                  # Debian/Ubuntu APT packages
│   ├── npm/                  # NPM packages
│   ├── pypi/                 # PyPI packages
│   └── go/                   # Go modules
├── index/                    # Parsed index database
│   └── <type>/               # Index files parsed by type
└── access-stats.json         # Access statistics
```

## Detailed Request Sequence

### Docker Build Request Flow

```mermaid
sequenceDiagram
    participant Docker as Docker Build
    participant CONN as CONNECT (3128)
    participant DRP as DockerRegistryProxy
    participant Cache as Cache Storage
    participant Upstream as Upstream Registry

    Docker->>CONN: CONNECT registry-1.docker.io:443
    CONN->>CONN: Extract hostname
    CONN->>CONN: Generate TLS cert
    CONN->>CONN: Establish tunnel
    CONN->>DRP: Route to DockerRegistryProxy
    
    Docker->>DRP: HTTPS: /v2/library/nginx/manifests/alpine
    DRP->>Cache: Check docker-manifests/docker.io/library/nginx/alpine
    
    alt Cache Hit
        Cache->>DRP: Return manifest + .meta
        DRP->>Docker: 200 OK (cached)
    else Cache Miss
        DRP->>Upstream: GET /v2/library/nginx/manifests/alpine
        Upstream->>DRP: 200 OK (manifest)
        DRP->>Cache: Store manifest + .meta
        DRP->>Docker: 200 OK (fetched)
    end
    
    Note over Docker,Upstream: Blob pulls follow same flow
```

### Package Manager Request Flow

```mermaid
sequenceDiagram
    participant PM as Package Manager
    participant CONN as CONNECT (3128)
    participant Router as Router
    participant Handler as Package Handler
    participant Cache as Cache
    participant Upstream as Upstream

    PM->>CONN: CONNECT registry.npmjs.org:443
    CONN->>CONN: Extract hostname
    CONN->>CONN: Generate TLS cert
    CONN->>CONN: Establish tunnel
    CONN->>Router: Route by hostname
    
    Router->>Handler: Package Manager Proxy
    
    PM->>Handler: HTTPS: /express/-/express-4.18.2.tgz
    Handler->>Cache: Check packages/npm/express/4.18.2
    
    alt Cache Hit
        Cache->>Handler: Return cached
        Handler->>PM: 200 OK (cached)
    else Cache Miss
        Handler->>Upstream: Fetch package
        Upstream->>Handler: Return package
        Handler->>Cache: Store package
        Handler->>Handler: Parse index if needed
        Handler->>PM: 200 OK (fetched)
    end
```

## What Gets Stored

### Docker Registry

1. **Manifests** (`docker-manifests/<registry>/<repo>/<reference>`)
   - Tag-based manifests (e.g., `library/golang:1.25-alpine`)
   - Digest-based manifests (e.g., `sha256:abc123...`)
   - Multi-platform indexes
   - Metadata files (`.meta`) containing:
     - Content type
     - Storage timestamp
     - Upstream URL

2. **Blobs** (`docker-blobs/<digest>`)
   - Layer data (tar.gz files)
   - Config blobs
   - Content-addressable storage
   - Refcount tracking for deduplication

### Package Managers

1. **Package Files** (`packages/<type>/<path>`)
   - APK: `.apk` files and `APKINDEX.tar.gz`
   - APT: `.deb` files and index files (`Packages.gz`, `Release`, etc.)
   - NPM: `.tgz` tarballs and registry JSON
   - PyPI: `.whl`, `.tar.gz` files and simple HTML pages
   - Go: `.zip` files, `.mod` files, and `.info` files

2. **Index Database** (`index/<type>/`)
   - Parsed metadata from index files
   - Package listings and dependencies
   - Used for package discovery and listing

3. **Access Statistics** (`access-stats.json`)
   - Access counts per cached item
   - Last access timestamps
   - Used for cache management and reporting

## Configuration

### Docker Client Configuration

**Docker Desktop (macOS/Windows):**
- Settings → Resources → Proxies
- HTTP Proxy: `http://host.docker.internal:3128`
- HTTPS Proxy: `http://host.docker.internal:3128`

**Docker Daemon (Linux):**
- `/etc/docker/daemon.json`:
  ```json
  {
    "proxies": {
      "http-proxy": "http://localhost:3128",
      "https-proxy": "http://localhost:3128"
    }
  }
  ```

### Roxxy Configuration

- **Config file**: `~/.roxxy/config.yaml` (or `./config.yaml`)
- **Cache directory**: `~/.cache/roxxy/` (Linux) or `~/Library/Application Support/roxxy/Cache/` (macOS)
- **Certificates**: `~/.roxxy/certs/`

### Proxy Chaining

Roxxy supports upstream proxy chaining:
```yaml
global:
  upstream_proxy: "http://upstream-proxy:3128"
```

This allows chaining: `airgap-roxxy → system-roxxy → internet`

## Key Features

1. **Transparent Proxying**: Clients don't need to know about cache structure
2. **Content-Addressable Storage**: Blobs stored by digest for deduplication
3. **Registry Isolation**: Manifests organized by registry to prevent collisions
4. **Index Parsing**: Package index files parsed for better cache management
5. **Access Tracking**: Statistics tracked for cache optimization
6. **Refcount Management**: Blobs tracked with reference counts for safe deletion

