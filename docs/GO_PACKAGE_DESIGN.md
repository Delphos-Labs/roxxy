# Go Package Model Design

## Problem Statement
Go packages have a hierarchical structure where:
- `github.com/bytedance/sonic` is the main module
- `github.com/bytedance/sonic/loader` is a sub-package (directory) within the same module
- Both share the same version and are part of the same module

We want to:
1. Show only main modules in the package list (e.g., `sonic`)
2. Show sub-packages only in metadata as "Directories: loader"
3. Ensure versions show up correctly for main modules

## Current Issues
1. Sub-package detection logic is complex and error-prone
2. Filtering happens in multiple places (handlePackageGroups, getPackageGroups)
3. The logic for detecting sub-packages might be backwards or incomplete

## Proposed Solution

### Core Principle
**A sub-package is any package whose name starts with another package's name followed by "/"**

Example:
- Main module: `github.com/bytedance/sonic`
- Sub-package: `github.com/bytedance/sonic/loader` (starts with `github.com/bytedance/sonic/`)

### Implementation Strategy

#### 1. Helper Function: `isGoSubPackage(mainModule, candidate) bool`
```go
// Returns true if candidate is a sub-package of mainModule
func isGoSubPackage(mainModule, candidate string) bool {
    if mainModule == "" || candidate == "" {
        return false
    }
    // A sub-package must start with mainModule + "/"
    prefix := mainModule + "/"
    return strings.HasPrefix(candidate, prefix)
}
```

#### 2. Helper Function: `findMainModules(packages []Package) map[string]bool`
```go
// Returns a set of main module names (packages that are not sub-packages of any other)
func findMainModules(packages []Package) map[string]bool {
    mainModules := make(map[string]bool)
    
    // First pass: collect all Go package names
    goPackages := []string{}
    for _, pkg := range packages {
        if pkg.Type == TypeGo && pkg.Name != "" {
            goPackages = append(goPackages, pkg.Name)
        }
    }
    
    // Second pass: for each package, check if it's a sub-package of any other
    for _, pkgName := range goPackages {
        isSubPackage := false
        for _, otherPkgName := range goPackages {
            if pkgName != otherPkgName && isGoSubPackage(otherPkgName, pkgName) {
                isSubPackage = true
                break
            }
        }
        if !isSubPackage {
            mainModules[pkgName] = true
        }
    }
    
    return mainModules
}
```

#### 3. Filtering in `handlePackageGroups`
```go
// Before filtering by group, filter out Go sub-packages
mainModules := findMainModules(entries)
for _, entry := range entries {
    if entry.Type == string(packages.TypeGo) && entry.Name != "" {
        // Check if this is a sub-package
        isSubPackage := false
        for mainModule := range mainModules {
            if isGoSubPackage(mainModule, entry.Name) {
                isSubPackage = true
                break
            }
        }
        if isSubPackage {
            continue // Skip sub-packages
        }
    }
    // ... rest of filtering logic
}
```

#### 4. Filtering in `getPackageGroups`
Use the same helper functions to filter sub-packages before building hierarchy.

#### 5. Metadata Loading
Already correct - finds sub-packages by checking if package name starts with module path + "/"

## Benefits
1. **Single source of truth**: Helper functions define what a sub-package is
2. **Reusable**: Same logic used everywhere
3. **Clear and testable**: Easy to understand and verify
4. **Maintainable**: Changes to sub-package detection logic happen in one place

## Testing Cases
1. `github.com/bytedance/sonic` → main module ✓
2. `github.com/bytedance/sonic/loader` → sub-package of sonic ✓
3. `github.com/bytedance/sonic/loader/sub` → sub-package of sonic (not loader) ✓
4. `golang.org/x/net` → main module ✓
5. `golang.org/x/net/http2` → sub-package of net ✓

