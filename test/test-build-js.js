const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const root = path.resolve(__dirname, '..');
const sources = ['init', 'utils', 'app', 'fallback'];

function fixture(t) {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'roxxy-build-js-'));
    t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
    fs.copyFileSync(path.join(root, 'internal/ui/static/build-js.js'), path.join(dir, 'build-js.js'));
    fs.mkdirSync(path.join(dir, 'js'));
    for (const name of sources) {
        fs.writeFileSync(path.join(dir, 'js', `${name}.js`),
            '// This comment must not appear in the minified output.\nglobalThis.result = 2 + 3;\n');
    }
    return dir;
}

test('missing local esbuild fails without invoking npx', (t) => {
    const dir = fixture(t);
    const bin = path.join(dir, 'bin');
    const log = path.join(dir, 'npx.log');
    fs.mkdirSync(bin);
    fs.writeFileSync(path.join(bin, 'npx'),
        '#!/usr/bin/env node\nrequire("node:fs").writeFileSync(process.env.NPX_LOG, "invoked");\n',
        { mode: 0o755 });
    const result = spawnSync(process.execPath, [path.join(dir, 'build-js.js')], {
        cwd: os.tmpdir(),
        encoding: 'utf8',
        env: { ...process.env, NODE_PATH: '', PATH: `${bin}${path.delimiter}${process.env.PATH}`, NPX_LOG: log }
    });
    assert.equal(result.error, undefined);
    assert.notEqual(result.status, 0);
    assert.equal(fs.existsSync(log), false, 'build must not invoke npx when dependencies are missing');
    assert.match(result.stderr, /Cannot find module 'esbuild'/);
    for (const name of sources) {
        assert.equal(fs.existsSync(path.join(dir, 'js', `${name}.min.js`)), false);
    }
});

test('installed esbuild minifies all sources from an unrelated working directory', (t) => {
    const dir = fixture(t);
    fs.symlinkSync(path.join(root, 'node_modules'), path.join(dir, 'node_modules'), 'junction');
    const result = spawnSync(process.execPath, [path.join(dir, 'build-js.js')], {
        cwd: os.tmpdir(), encoding: 'utf8', env: { ...process.env, NODE_PATH: '' }
    });
    assert.equal(result.error, undefined);
    assert.equal(result.status, 0, result.stderr);
    for (const name of sources) {
        const output = fs.readFileSync(path.join(dir, 'js', `${name}.min.js`), 'utf8');
        assert.equal(output.includes('This comment'), false);
        const context = {};
        vm.runInNewContext(output, context);
        assert.equal(context.result, 5);
    }
});
