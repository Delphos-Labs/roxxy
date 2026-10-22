# Index File Parsing Design

## Overview

Parse package manager index files to extract metadata about all available packages, not just cached ones. This provides a complete view of what's available through the proxy.

## Index File Formats

### APT (Debian/Ubuntu)
- **Format**: `Packages.gz` - gzipped text file with key-value pairs
- **Structure**: Each package entry separated by blank line
- **Fields**: Package, Version, Architecture, Filename, Size, Description, Depends, etc.
- **Location**: `/dists/{suite}/{component}/binary-{arch}/Packages.gz`

### PyPI (Python)
- **Format**: HTML pages in `/simple/` directory
- **Structure**: HTML with `<a>` tags linking to package files
- **Fields**: Package name, version (from filename), file type (.whl, .tar.gz, etc.)
- **Location**: `/simple/{package}/` or `/simple/` (index of all packages)

### NPM (Node.js)
- **Format**: JSON API responses
- **Structure**: JSON object with package metadata
- **Fields**: name, version, dist.tarball, dependencies, description, etc.
- **Location**: `/{package}` or `/-/v1/search?text={query}`

### APK (Alpine)
- **Format**: `APKINDEX.tar.gz` - tar archive containing `APKINDEX` file
- **Structure**: Binary format (can be parsed with `apk` tools or custom parser)
- **Fields**: Package name, version, architecture, size, dependencies
- **Location**: `/alpine/{version}/main/{arch}/APKINDEX.tar.gz`

### Go Modules
- **Format**: `.info` files (JSON) and `/list` files (newline-separated versions)
- **Structure**: JSON for version info, plain text for version lists
- **Fields**: Version, Time, GoVersion
- **Location**: `/{module}/@v/list` and `/{module}/@v/{version}.info`

## Implementation Plan

### Phase 1: Index Parser Interface
1. Create `internal/packages/index` package
2. Define `IndexParser` interface
3. Implement parsers for each package manager type

### Phase 2: Index Storage
1. Create `internal/packages/indexdb` package
2. Store parsed package metadata in JSON files or SQLite
3. Key structure: `{type}/{package}/{version}` → metadata

### Phase 3: Index Processing
1. When index files are cached, trigger parsing
2. Extract package metadata and store in index database
3. Update index database when index files are refreshed

### Phase 4: API Integration
1. Update `/api/v1/packages` to query index database
2. Show both "available" (from indexes) and "cached" (from downloads) packages
3. Add filtering by availability status

## Data Model

```go
type IndexPackage struct {
    Type         string    `json:"type"`
    Name         string    `json:"name"`
    Version      string    `json:"version"`
    Architecture string    `json:"architecture,omitempty"`
    Filename     string    `json:"filename"`
    Size         int64     `json:"size"`
    Description  string    `json:"description,omitempty"`
    Dependencies []string  `json:"dependencies,omitempty"`
    URL          string    `json:"url"`
    IndexedAt    time.Time `json:"indexed_at"`
    Cached       bool      `json:"cached"` // True if actual file is cached
}
```

## Parser Examples

### APT Packages.gz Parser
```go
// Parse Packages.gz format:
// Package: nginx
// Version: 1.18.0-0ubuntu1
// Architecture: amd64
// Filename: pool/main/n/nginx/nginx_1.18.0-0ubuntu1_amd64.deb
// Size: 123456
// Description: small, powerful, scalable web/proxy server
// Depends: libc6, libssl1.1
```

### PyPI Simple Index Parser
```go
// Parse HTML like:
// <a href="../../packages/.../requests-2.31.0-py3-none-any.whl">requests-2.31.0-py3-none-any.whl</a>
```

### NPM Registry Parser
```go
// Parse JSON like:
// {
//   "name": "express",
//   "version": "4.18.2",
//   "dist": {"tarball": "https://registry.npmjs.org/express/-/express-4.18.2.tgz"}
// }
```

