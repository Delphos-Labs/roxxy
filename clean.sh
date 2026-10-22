#!/bin/bash
# Clean script to remove build artifacts and generated files

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "=== Cleaning Roxxy Build Artifacts ==="
echo ""

# Count items before cleaning
items_removed=0

# Remove binary files
echo "Removing binary files..."
for binary in roxxy roxxy-test main lscache lsembed; do
    if [ -f "$binary" ]; then
        rm -f "$binary"
        echo "  ✓ Removed $binary"
        ((items_removed++))
    fi
done

# Remove minified JavaScript files
echo "Removing minified JavaScript files..."
for js_file in internal/ui/static/app.min.js internal/ui/static/app.min.js.gz; do
    if [ -f "$js_file" ]; then
        rm -f "$js_file"
        echo "  ✓ Removed $js_file"
        ((items_removed++))
    fi
done

# Remove temporary JavaScript files
echo "Removing temporary JavaScript files..."
if [ -f "internal/ui/static/app.js.tmp" ]; then
    rm -f "internal/ui/static/app.js.tmp"
    echo "  ✓ Removed internal/ui/static/app.js.tmp"
    ((items_removed++))
fi

# Remove backup files
echo "Removing backup files..."
while IFS= read -r file; do
    if [ -f "$file" ]; then
        rm -f "$file"
        echo "  ✓ Removed $file"
        ((items_removed++))
    fi
done < <(find . -type f \( -name "*.bak" -o -name "*.bak2" -o -name "*.orig" -o -name "*~" \) ! -path "./.git/*" 2>/dev/null)

# Remove test/export tarballs
echo "Removing test/export tarballs..."
for tarball in *.tar *.tar.gz *.tgz docker*.tar*; do
    if [ -f "$tarball" ]; then
        rm -f "$tarball"
        echo "  ✓ Removed $tarball"
        ((items_removed++))
    fi
done

# Remove old build artifacts directory
echo "Removing old build artifacts..."
if [ -d "registry-proxy" ]; then
    rm -rf "registry-proxy"
    echo "  ✓ Removed registry-proxy/ directory"
    ((items_removed++))
fi

# Remove Go test cache (optional, but can be large)
if [ -d ".go" ]; then
    echo "Removing Go test cache..."
    rm -rf ".go"
    echo "  ✓ Removed .go/ directory"
    ((items_removed++))
fi

# Remove Go module cache (optional - only if you want to clean it)
# Uncomment if you want to clean the module cache too
# if [ -n "$GOMODCACHE" ] && [ -d "$GOMODCACHE" ]; then
#     echo "Removing Go module cache..."
#     rm -rf "$GOMODCACHE"
#     echo "  ✓ Removed Go module cache"
# fi

echo ""
if [ $items_removed -eq 0 ]; then
    echo "✓ No build artifacts found - already clean!"
else
    echo "✓ Cleaned $items_removed build artifact(s)"
fi
echo ""

