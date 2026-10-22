#!/usr/bin/env node
// Build script: minify JavaScript source files into .min.js outputs.
//
// Source files:  js/app.js, js/utils.js, js/init.js, js/fallback.js
// Output files:  js/app.min.js, js/utils.min.js, js/init.min.js, js/fallback.min.js
//
// The .min.js files are gitignored. index.html references .min.js.
// NEVER overwrite source .js files — edit them directly, then run this script.
//
// Usage: node build-js.js

const fs = require('fs');
const esbuild = require('esbuild');
const path = require('path');

const jsDir = path.join(__dirname, 'js');

// All JavaScript source files to minify
const jsFiles = [
    'init.js',
    'utils.js',
    'app.js',
    'fallback.js'
];

function minifyJS(inputFile, outputFile) {
    const srcName = path.basename(inputFile);
    const outName = path.basename(outputFile);

    esbuild.buildSync({
        entryPoints: [inputFile],
        minify: true,
        format: 'iife',
        outfile: outputFile
    });

    const srcSize = (fs.statSync(inputFile).size / 1024).toFixed(1);
    const outSize = (fs.statSync(outputFile).size / 1024).toFixed(1);
    console.log(`  ${srcName} (${srcSize}kb) -> ${outName} (${outSize}kb)`);
}

function build() {
    console.log('Building JavaScript...');

    for (const jsFile of jsFiles) {
        const inputPath = path.join(jsDir, jsFile);
        const minifiedPath = path.join(jsDir, jsFile.replace('.js', '.min.js'));

        if (!fs.existsSync(inputPath)) {
            console.error(`  Error: ${jsFile} not found`);
            process.exit(1);
        }

        minifyJS(inputPath, minifiedPath);
    }

    console.log('Done');
}

build();
