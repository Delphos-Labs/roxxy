# Helm Values Storage Design

## Overview

Helm charts are stored as OCI artifacts in `docker-manifests/` and `docker-blobs/`, but Helm values files (YAML configuration) are separate and need their own storage mechanism. This document outlines the design for storing, indexing, and exporting Helm values files in roxxy.

## Design Principles

1. **Special Artifact Type**: Helm values are a first-class artifact type, not raw files
2. **Linked to Charts**: Values files are explicitly linked to their Helm charts
3. **Indexed**: Values are included in the tarball index for fast lookup
4. **Validated**: Chart existence is validated when storing values
5. **UI-Ready**: Structured metadata enables future UI features (viewing, editing, diffing)

## Storage Structure

```
helm-values/
  <registry>/              # e.g., oci://registry.example.com
    <repo>/                # e.g., charts/my-app
      <chart-name>/        # e.g., my-app
        <values-name>.yaml # e.g., values.yaml, values-prod.yaml
        <values-name>.yaml.meta  # Metadata file
```

### Example Paths

```
helm-values/
  oci.example.com/
    charts_my-app/
      my-app/
        values.yaml
        values.yaml.meta
        values-prod.yaml
        values-prod.yaml.meta
        values-dev.yaml
        values-dev.yaml.meta
```

## Metadata Format

Each values file has a corresponding `.meta` file containing structured metadata:

```json
{
  "chart_reference": "oci://registry.example.com/charts/my-app:1.2.3",
  "chart_digest": "sha256:abc123...",
  "values_name": "values-prod",
  "description": "Production values for my-app",
  "created_at": "2025-01-03T12:00:00Z",
  "updated_at": "2025-01-03T12:00:00Z",
  "created_by": "user@example.com"
}
```

### Metadata Fields

- **chart_reference** (required): Full OCI reference to the chart (e.g., `oci://registry/repo/chart:tag`)
- **chart_digest** (optional): Specific chart digest if values are tied to a specific chart version
- **values_name** (required): Name of the values file (without `.yaml` extension)
- **description** (optional): Human-readable description
- **created_at** (required): Timestamp when values were first created
- **updated_at** (required): Timestamp when values were last updated
- **created_by** (optional): User/entity that created the values

## Values File Format

Values files are stored as **raw YAML** (optionally gzipped for compression). The metadata file provides all context about which chart they belong to.

### Why Raw YAML?

- Direct compatibility with Helm commands (`helm install --values values.yaml`)
- Easy to view/edit in UI
- No wrapper format needed - metadata is separate
- Can be compressed transparently if needed

## Validation

When storing values files, roxxy should validate:

1. **Chart Existence**: The referenced chart must exist in `docker-manifests/`
   - Check by registry/repo/chart:tag
   - Or by digest if `chart_digest` is provided
   
2. **Chart Format**: Verify the chart is a valid OCI artifact (has manifest in docker-manifests)

3. **Values Format**: Basic YAML validation (optional, can be lenient)

### Validation Flow

```
Store Values Request
  ↓
Extract chart_reference from metadata
  ↓
Look up chart in docker-manifests/
  ↓
Chart exists? → Store values + metadata
  ↓
Chart missing? → Return error (chart not found)
```

## Export Behavior

### Docker Export

Helm values are **included** in docker exports because they're linked to Helm charts (OCI artifacts):

```go
case "docker":
    return relPath == "docker-blobs" ||
        strings.HasPrefix(relPath, "docker-blobs/") ||
        relPath == "docker-manifests" ||
        strings.HasPrefix(relPath, "docker-manifests/") ||
        relPath == "helm-values" ||           // NEW
        strings.HasPrefix(relPath, "helm-values/") ||  // NEW
        relPath == "refcount" ||
        strings.HasPrefix(relPath, "refcount/")
```

### Raw Export

Helm values are **NOT** included in raw exports - they're a special artifact type, not raw files.

### Full Export

Helm values are included in full exports (everything is included).

## Indexing

Helm values are indexed in the tarball index, just like other artifacts:

- Stored **before** the index (like manifests and blobs)
- Included in `.roxxy-index.json.gz` for fast lookup
- Path format: `helm-values/<registry>/<repo>/<chart>/<values-name>.yaml`

## API Design (Future)

### Endpoints

#### Store Values
```
POST /api/v1/helm/values
Content-Type: multipart/form-data

Body:
  - chart_reference: "oci://registry/repo/chart:tag"
  - values_name: "values-prod"
  - description: "Production values"
  - values_file: <YAML file>
```

**Response**: 201 Created with location header

**Validation**: Returns 404 if chart doesn't exist

#### List Values for Chart
```
GET /api/v1/helm/charts/{registry}/{repo}/{chart}/values
```

**Response**:
```json
{
  "chart": "oci://registry/repo/chart:tag",
  "values": [
    {
      "name": "values-prod",
      "description": "Production values",
      "created_at": "2025-01-03T12:00:00Z",
      "updated_at": "2025-01-03T12:00:00Z"
    }
  ]
}
```

#### Get Values File
```
GET /api/v1/helm/values/{registry}/{repo}/{chart}/{name}
```

**Response**: YAML file with `Content-Type: application/x-yaml`

#### Update Values
```
PUT /api/v1/helm/values/{registry}/{repo}/{chart}/{name}
Content-Type: multipart/form-data

Body:
  - values_file: <YAML file>
  - description: "Updated description" (optional)
```

#### Delete Values
```
DELETE /api/v1/helm/values/{registry}/{repo}/{chart}/{name}
```

## Implementation Details

### Path Helpers

Similar to `DockerManifestPath`, add:

```go
// HelmValuesPath constructs the filesystem path for a Helm values file
func HelmValuesPath(cacheDir, registry, repo, chartName, valuesName string) string {
    safeRepo := SafeRepoName(repo)  // Replace / with _
    safeRegistry := SafeRegistryName(registry)  // Handle oci:// prefix
    return filepath.Join(cacheDir, "helm-values", safeRegistry, safeRepo, chartName, valuesName+".yaml")
}

// HelmValuesMetadataPath returns the .meta file path for a values file
func HelmValuesMetadataPath(valuesPath string) string {
    return valuesPath + ".meta"
}
```

### Metadata Structure

```go
type HelmValuesMetadata struct {
    ChartReference string    `json:"chart_reference"` // oci://registry/repo/chart:tag
    ChartDigest    string    `json:"chart_digest,omitempty"` // Optional: specific digest
    ValuesName     string    `json:"values_name"`
    Description    string    `json:"description,omitempty"`
    CreatedAt      time.Time `json:"created_at"`
    UpdatedAt      time.Time `json:"updated_at"`
    CreatedBy      string    `json:"created_by,omitempty"`
}
```

### Chart Validation

```go
func ValidateChartExists(cacheDir, chartReference string) error {
    // Parse chart reference: oci://registry/repo/chart:tag
    // Look up in docker-manifests/<registry>/<repo>/<tag>
    // Return error if not found
}
```

## Questions & Decisions

### Q: Should every chart have a values file?
**A**: No. Values files are optional. A chart can have zero, one, or many values files.

### Q: Should we validate chart exists?
**A**: Yes. When storing values, validate the chart exists in docker-manifests. Return 404 if chart not found.

### Q: What's the format - just YAML or nested in context?
**A**: Just raw YAML. The metadata file provides all context (which chart, description, etc.). This makes values files directly usable with Helm commands.

### Q: Is this like an ArgoCD manifest?
**A**: Similar concept (structured metadata + content), but simpler. ArgoCD manifests have more complex structure. Helm values are just YAML with metadata.

### Q: Should values be in docker export?
**A**: Yes, because they're specifically linked to Helm charts (OCI artifacts stored in docker-manifests/docker-blobs).

### Q: Should values be in raw cache?
**A**: No. They're a special artifact type, not raw files. This enables:
- Introspection (list values per chart)
- Validation (ensure chart exists)
- UI editing (structured metadata)
- Future features (versioning, diffing)

## Future Enhancements

1. **UI Support**: View/edit values files in the web UI
2. **Versioning**: Track changes to values files over time
3. **Diffing**: Compare values files or versions
4. **Templates**: Value file templates with variable substitution
5. **Validation**: Schema validation against chart's values.schema.json
6. **Import/Export**: Bulk import/export of values files

#### 
