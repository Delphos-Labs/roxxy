package inspect

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// NPMInspector parses an npm tarball, extracts package.json, and
// inspects the install-time hook scripts (preinstall/install/postinstall)
// for credential-takeover-worm patterns.
//
// What we flag, by severity:
//
//	warn  — any install hook is present at all (most legitimate
//	        packages have none; presence alone deserves a look)
//	warn  — hook references external HTTP(S) URLs (could be a CDN
//	        download or a payload fetch — context required)
//	block — hook contains obfuscation patterns: base64-decode +
//	        eval/exec, unicode-escape sequences with eval, shell
//	        piping (curl|sh, wget|sh), node -e with base64
//	block — hook content > 4 KB (overwhelmingly correlated with
//	        smuggled payloads; legit hooks are tiny)
type NPMInspector struct{}

func (NPMInspector) Format() string { return "npm" }

// npmPackageJSON is the slice of package.json we care about. We don't
// json.Unmarshal the whole thing because legitimate manifests can carry
// non-standard fields with custom types we don't want to validate.
type npmPackageJSON struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	Scripts map[string]string `json:"scripts"`
}

// The install-hook script names npm runs on install, in the order npm
// runs them. https://docs.npmjs.com/cli/v10/using-npm/scripts
var npmInstallHooks = []string{"preinstall", "install", "postinstall"}

// Obfuscation patterns: each is a regex whose match in a hook script
// raises a SeverityBlock finding.
var obfuscationPatterns = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`(?i)\bbase64[_-]?decode\b|atob\s*\(|Buffer\.from\s*\([^)]*['"]base64['"]\)`), "base64 decode"},
	{regexp.MustCompile(`(?i)\beval\s*\(`), "eval() call"},
	{regexp.MustCompile(`(?i)\bexec\s*\(|child_process|execSync|spawnSync`), "child_process / exec"},
	// `node -e` at install time runs arbitrary JS on the dev's host, full
	// stop. Some legit packages use it for tiny version probes or banner
	// printing, but the policy layer (allowlists, per-tenant rules) is
	// the right place to suppress those — not detection. Inspector job
	// is to flag risk; policy job is to weigh it.
	{regexp.MustCompile(`(?i)\bnode\s+-e\b`), "node -e inline script"},
	// ANSI escape banners typically have 1–3 \xNN sequences; obfuscated
	// payloads have dozens to hundreds. Threshold suppresses ANSI without
	// suppressing genuine hex-encoded blobs.
	{regexp.MustCompile(`(\\x[0-9a-fA-F]{2}[^\\]*){8,}`), "many hex-escaped chars (>=8)"},
	{regexp.MustCompile(`(\\u[0-9a-fA-F]{4}[^\\]*){8,}`), "many unicode-escaped chars (>=8)"},
	{regexp.MustCompile(`\|\s*(sh|bash|zsh|node|python3?)\b`), "shell pipe to interpreter"},
	{regexp.MustCompile(`(?i)\bcurl\b.*\|`), "curl piped to interpreter"},
	{regexp.MustCompile(`(?i)\bwget\b.*\|`), "wget piped to interpreter"},
	{regexp.MustCompile(`\$\{[^}]*:[^}]*:[^}]*\}`), "bash substring obfuscation"},
}

// localDispatchPatterns: hook delegates to code inside the package.
// These don't *prove* maliciousness — many legit native-binding builds
// shell out to node-gyp this way — but the shape is exactly how worms
// arrange to run their payload. Flag and let policy decide.
var localDispatchPatterns = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`(?i)\bnode\s+\.{0,2}/`), "node runs local script (node ./...)"},
	{regexp.MustCompile(`(?i)\brequire\s*\(\s*['"]\.{1,2}/`), "require('./...') in hook"},
	{regexp.MustCompile(`(?i)\b(python3?|ruby|perl)\s+\.{0,2}/`), "interpreter runs local script"},
	{regexp.MustCompile(`(?i)(^|\s|;|&|\|)(\.|source)\s+\.{0,2}/`), "shell-sources local file"},
}

// URL pattern for network references in hooks.
var urlRE = regexp.MustCompile(`https?://[^\s'"` + "`" + `)]+`)

// Inspect parses the tarball in memory and returns findings.
func (n NPMInspector) Inspect(body []byte, name, version string) ([]Finding, error) {
	if len(body) == 0 {
		return nil, errors.New("empty body")
	}
	if len(body) > MaxArtifactBytes {
		body = body[:MaxArtifactBytes]
	}
	pkgJSON, err := readPackageJSON(body)
	if err != nil {
		return nil, fmt.Errorf("read package.json: %w", err)
	}
	if pkgJSON == nil {
		return nil, errors.New("no package.json in tarball")
	}
	var pkg npmPackageJSON
	if err := json.Unmarshal(pkgJSON, &pkg); err != nil {
		return nil, fmt.Errorf("parse package.json: %w", err)
	}
	if len(pkg.Scripts) == 0 {
		return nil, nil
	}
	var findings []Finding
	for _, hook := range npmInstallHooks {
		script, ok := pkg.Scripts[hook]
		if !ok {
			continue
		}
		findings = append(findings, inspectNPMHook(hook, script)...)
	}
	return findings, nil
}

// readPackageJSON streams the gzipped tarball and returns the first
// `package/package.json` entry it finds. Bounded by MaxArchiveEntries
// and MaxExtractedBytes.
func readPackageJSON(body []byte) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer util.DeferClose(gr)
	tr := tar.NewReader(gr)
	entries := 0
	totalBytes := int64(0)
	for {
		entries++
		if entries > MaxArchiveEntries {
			return nil, fmt.Errorf("too many tar entries (>%d)", MaxArchiveEntries)
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// npm tarballs always wrap contents in a "package/" prefix.
		// Match exactly to avoid loading some nested copy.
		if !isNPMPackageJSON(hdr.Name) {
			continue
		}
		// Bound the read size for this entry too.
		limited := io.LimitReader(tr, MaxExtractedBytes-totalBytes)
		buf, err := io.ReadAll(limited)
		if err != nil {
			return nil, fmt.Errorf("read entry %s: %w", hdr.Name, err)
		}
		return buf, nil
	}
}

func isNPMPackageJSON(name string) bool {
	// npm publishes wrap files under "package/", but some publishers use
	// a different prefix. Accept any single-level prefix.
	parts := strings.Split(name, "/")
	return len(parts) == 2 && parts[1] == "package.json"
}

// inspectNPMHook returns findings for one hook script.
func inspectNPMHook(hook, script string) []Finding {
	script = strings.TrimSpace(script)
	if script == "" {
		return nil
	}
	var findings []Finding

	// Baseline: hook exists at all.
	findings = append(findings, Finding{
		Category: "install-hook-present",
		Severity: SeverityWarn,
		Detail:   truncate(script, 200),
		Location: "package.json#scripts." + hook,
	})

	// Size signal: legit hooks are tiny (a few hundred bytes max).
	if len(script) > 4096 {
		findings = append(findings, Finding{
			Category: "install-hook-large",
			Severity: SeverityBlock,
			Detail:   fmt.Sprintf("hook is %d bytes (>4096 threshold); legit install hooks are tiny", len(script)),
			Location: "package.json#scripts." + hook,
		})
	}

	// Obfuscation patterns.
	for _, p := range obfuscationPatterns {
		if loc := p.re.FindStringIndex(script); loc != nil {
			findings = append(findings, Finding{
				Category: "install-hook-obfuscated",
				Severity: SeverityBlock,
				Detail:   fmt.Sprintf("%s — %q", p.reason, truncate(script[loc[0]:loc[1]], 100)),
				Location: "package.json#scripts." + hook,
			})
		}
	}

	// Hook delegates to a script file inside the package — the actual
	// common worm shape. The string `node ./postinstall.js` looks
	// innocuous, but ./postinstall.js can be the entire payload. Same
	// for `require('./X')`, sourced shell files, and `python X.py`.
	// Severity is block: the inspector flags the risky shape, and policy
	// decides whether to allow it for known-good publishers.
	for _, p := range localDispatchPatterns {
		if loc := p.re.FindStringIndex(script); loc != nil {
			findings = append(findings, Finding{
				Category: "install-hook-runs-local",
				Severity: SeverityBlock,
				Detail:   fmt.Sprintf("%s — %q", p.reason, truncate(script[loc[0]:loc[1]], 100)),
				Location: "package.json#scripts." + hook,
			})
		}
	}

	// Network references.
	if urls := urlRE.FindAllString(script, -1); len(urls) > 0 {
		uniq := uniqStrings(urls)
		findings = append(findings, Finding{
			Category: "install-hook-network",
			Severity: SeverityWarn,
			Detail:   fmt.Sprintf("references %d URL(s): %s", len(uniq), strings.Join(uniq, ", ")),
			Location: "package.json#scripts." + hook,
		})
	}

	return findings
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func uniqStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
