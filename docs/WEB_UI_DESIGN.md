# Web UI Design

## Overview

A modern single-page web UI for registry-proxy that provides detailed views of cached artifacts, management capabilities, and filtering. The UI will be integrated into the existing HTTP server and complement the CLI tools.

## Architecture

### Integration Points

1. **HTTP Server Integration**
   - Add UI routes to existing `proxy.Server`
   - Serve UI on dedicated path (e.g., `/ui` or `/dashboard`)
   - API endpoints under `/api/v1/`
   - Static assets embedded in binary (or served from filesystem)

2. **Data Sources**
   - Reuse existing CLI data structures (`packages.CacheEntry`, `cli.CacheEntry`)
   - Leverage existing listing functions (`packages.ListCache`, `cli.listCacheEntries`)
   - Access stats from `cache.StatsTracker`
   - Docker registry via filesystem storage

3. **Package Structure**
   ```
   internal/
     ui/
       server.go      # HTTP handlers for UI and API
       api.go         # API endpoint handlers
       assets.go      # Embedded static files (or serve from filesystem)
   ```

## Features

### Dashboard View
- **Overview Cards**
  - Total cache size
  - Package count by type
  - Docker image count
  - Total time saved
  - Cache hit rate

### Package Manager Cache
- **List View**
  - Filterable by type (APK, APT, NPM, PyPI, Go)
  - Searchable by name/version
  - Sortable columns (name, version, size, cached_at, access_count)
  - Pagination for large datasets
  - Group by type with expand/collapse

- **Package Details**
  - Name, version, architecture
  - Size, cached date, fetch duration
  - Access count and last accessed
  - Cache path
  - Delete action

### Docker Registry
- **Repository List**
  - All repositories with tags
  - Image sizes
  - Last pushed date
  - Layer count

- **Image Details**
  - Tags and digests
  - Layer breakdown
  - Manifest details
  - Delete action

### URL Cache
- **List View**
  - Filterable by hostname
  - Searchable by path
  - Sortable columns
  - Content type, size, access stats

### Management Actions
- **Bulk Operations**
  - Flush entire cache
  - Flush by type
  - Flush by hostname
  - Delete specific artifacts

- **Statistics**
  - Access patterns
  - Time saved visualization
  - Cache growth over time
  - Most accessed packages

## API Endpoints

```
GET  /api/v1/stats/summary          # Dashboard overview
GET  /api/v1/packages               # List packages (query: ?type=, ?search=, ?sort=, ?page=)
GET  /api/v1/packages/{id}          # Package details
DELETE /api/v1/packages/{id}       # Delete package
GET  /api/v1/docker/repositories    # List Docker repositories
GET  /api/v1/docker/repositories/{name}/tags  # List tags for repository
GET  /api/v1/docker/images/{name}:{tag}      # Image details
DELETE /api/v1/docker/images/{name}:{tag}     # Delete image
GET  /api/v1/cache/url              # List URL cache entries
DELETE /api/v1/cache/url/{id}      # Delete URL cache entry
POST /api/v1/cache/flush            # Flush cache (body: {type?, hostname?})
GET  /api/v1/stats/access           # Access statistics
```

## Technology Choices

### Frontend
- **Vanilla JavaScript** or **Preact** (lightweight React alternative)
- **Tailwind CSS** (via CDN or embedded)
- **HTMX** (optional, for progressive enhancement)
- No build step required - serve directly

### Server
- **Go templates** for initial HTML (optional)
- **JSON API** for all data
- Reuse existing data structures
- Minimal new dependencies

## Implementation Plan

1. **Phase 1: API Endpoints**
   - Create `internal/ui/api.go` with REST handlers
   - Reuse existing CLI data collection logic
   - Add JSON response formatting

2. **Phase 2: UI Server**
   - Create `internal/ui/server.go` with HTTP handlers
   - Serve static HTML/JS/CSS
   - Integrate with existing `proxy.Server`

3. **Phase 3: Frontend**
   - Build SPA with modern UI
   - Implement filtering, sorting, pagination
   - Add management actions

4. **Phase 4: Polish**
   - Add real-time updates (optional)
   - Performance optimizations
   - Responsive design

## Design Principles

1. **Complement, don't replace** - CLI tools remain primary for automation
2. **Lightweight** - Minimal dependencies, fast load times
3. **Integrated** - Uses existing code, not bolted on
4. **Modern UX** - Clean, intuitive interface
5. **Performance** - Efficient data loading, pagination for large datasets

