// Package inspect provides per-format real-time inspection of package
// artifacts as roxxy serves them. The goal is to catch credential-
// takeover worms (Shai-Hulud class) that ship their payload via the
// package manager's install-time hook (npm's postinstall, PyPI's
// setup.py cmdclass, .deb postinst, .apk install hooks) — code that
// runs on the developer's host the moment they install the package.
//
// Discipline: inspectors never write to disk and never execute code
// from the artifact. They parse the metadata bytes in memory only,
// with bounded extraction (size + entry count caps) so a malicious
// archive can't OOM or zip-bomb us.
package inspect

import "fmt"

// Severity ranks a Finding for downstream handling. "warn" surfaces but
// doesn't block; "block" means roxxy should refuse to serve.
type Severity string

const (
	SeverityWarn  Severity = "warn"
	SeverityBlock Severity = "block"
)

// Finding is one suspicious thing detected in an artifact.
type Finding struct {
	// Category identifies the rule. Stable string for log/index use.
	// Examples: "install-hook-present", "install-hook-obfuscated",
	// "install-hook-network", "install-hook-shell-pipe".
	Category string `json:"category"`
	// Severity controls action (warn vs block).
	Severity Severity `json:"severity"`
	// Detail is human-readable context. May include the offending
	// snippet (truncated) for log/audit.
	Detail string `json:"detail"`
	// Location is where in the artifact the finding lives (e.g.,
	// "package.json#scripts.postinstall").
	Location string `json:"location,omitempty"`
}

func (f Finding) String() string {
	loc := ""
	if f.Location != "" {
		loc = " at " + f.Location
	}
	return fmt.Sprintf("[%s] %s%s: %s", f.Severity, f.Category, loc, f.Detail)
}

// Inspector parses an artifact's bytes and returns findings. The body
// must be the full archive (tarball / wheel / .deb / .apk). Name and
// version are for logging only — the inspector doesn't trust them to
// derive any path.
type Inspector interface {
	// Format returns the registry format ("npm", "pypi", "apt", "apk").
	Format() string
	// Inspect parses body and returns findings. Returns nil findings
	// (not error) for clean artifacts. Returns error for malformed
	// archives — caller decides whether malformed = block.
	Inspect(body []byte, name, version string) ([]Finding, error)
}

// MaxArtifactBytes caps the artifact size inspectors will look at.
// 100 MB is generous for a metadata-only inspection; bigger artifacts
// get inspected up to this cap (we read the first MaxArtifactBytes of
// the stream).
const MaxArtifactBytes = 100 * 1024 * 1024

// MaxExtractedBytes caps the total decompressed bytes an inspector
// will pull from inside an archive. Defense against zip bombs.
const MaxExtractedBytes = 16 * 1024 * 1024

// MaxArchiveEntries caps the number of entries an inspector will
// iterate. Defense against tar-bomb style "millions of empty files."
const MaxArchiveEntries = 50000
