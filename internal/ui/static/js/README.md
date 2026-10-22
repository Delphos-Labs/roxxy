# JavaScript Source Files

This directory contains the source JavaScript files for the Roxxy UI.

## Files

- **init.js** - Initialization script (Tailwind config, activeTab setup)
- **app.js** - Main Alpine.js application data and logic
- **fallback.js** - Fallback and browser compatibility checks

## Build Process

During the build, these files are:
1. Minified (if esbuild is available)
2. Embedded into `index.html` at the appropriate placeholders

The build script (`build-js.js`) replaces these placeholders in `index.html`:
- `<!-- INIT_JS -->` → minified init.js
- `<!-- APP_JS -->` → minified app.js  
- `<!-- FALLBACK_JS -->` → minified fallback.js

## Editing

Edit these files directly with proper indentation. The build process will handle minification and embedding.

**Note:** After editing, run `node build-js.js` or `./build.sh` to rebuild the HTML file.
