# Package List Templates - Component Architecture

## Structure: Separate Template Files

The package list component is now split into **10 focused template files** instead of one large 446-line file.

## Template Files

1. **package-list-main.tmpl** - Main entry point, type headers
2. **group-recursive.tmpl** - Recursive group component (the key recursion!)
3. **package-component.tmpl** - Reusable package rendering
4. **package-metadata.tmpl** - Package-level metadata
5. **package-versions.tmpl** - Version list rendering
6. **version-header.tmpl** - Version header with badges
7. **version-click-handler.tmpl** - Type-specific click handlers
8. **version-metadata.tmpl** - Version metadata (basic/full/type-specific)
9. **package-subpackages.tmpl** - Sub-packages (Go only)
10. **flat-group.tmpl** - Flat group rendering

## Benefits of Separate Templates

### ✅ **Better Organization**
- Each component is in its own file
- Easy to find and modify specific parts
- Clear separation of concerns

### ✅ **Easier Maintenance**
- Change one component without affecting others
- Smaller files are easier to understand
- Less scrolling and searching

### ✅ **Reusability**
- Components can be reused across different templates
- Template definitions are isolated and testable

### ✅ **Better Collaboration**
- Multiple developers can work on different components
- Fewer merge conflicts
- Clear ownership of components

### ✅ **Improved Readability**
- Average file size: ~44 lines (vs 446 lines)
- Each file has a single, clear purpose
- Easier to understand the overall structure

## Comparison

| Approach | Lines | Files | Maintainability | Reusability |
|----------|-------|-------|-----------------|-------------|
| **One Template** | 446 | 1 | ⚠️ Hard to navigate | ❌ None |
| **Separate Templates** | 442 | 10 | ✅ Easy to find/edit | ✅ High |

## Template Parsing

All templates are parsed together in dependency order:

```go
templateFiles := []string{
    "version-click-handler.tmpl",    // No dependencies
    "version-header.tmpl",            // No dependencies
    "version-metadata.tmpl",         // Uses version-header
    "package-metadata.tmpl",          // No dependencies
    "package-versions.tmpl",          // Uses version-header, version-metadata
    "package-subpackages.tmpl",      // Uses version-click-handler
    "package-component.tmpl",        // Uses package-metadata, package-versions
    "flat-group.tmpl",                // Uses package-component
    "group-recursive.tmpl",           // Uses package-component (recursive!)
    "package-list-main.tmpl",         // Uses group-recursive, flat-group
}
```

## Adding a New Component

1. Create a new `.tmpl` file in `templates/`
2. Define your template: `{{define "my-component"}}...{{end}}`
3. Add the file to `templateFiles` in `server.go`
4. Use it: `{{template "my-component" .}}`

## Best Practices

1. **Keep templates focused** - One responsibility per file
2. **Name clearly** - File name should match template purpose
3. **Document dependencies** - Comment which templates a file uses
4. **Order matters** - Parse dependencies before dependents
5. **Test incrementally** - Test each component as you build

## When to Split vs Combine

**Split when:**
- Component is >50 lines
- Component has a distinct purpose
- Component might be reused
- Component has complex logic

**Combine when:**
- Components are tightly coupled
- Total size is <100 lines
- Components are never used separately
