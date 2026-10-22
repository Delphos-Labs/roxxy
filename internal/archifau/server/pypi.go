package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/archifau/audit"
	"github.com/Delphos-Labs/roxxy/internal/archifau/auth"
	"github.com/Delphos-Labs/roxxy/internal/archifau/s3cas"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// PyPI is an origin Python package index: twine uploads to /pypi/legacy/,
// pip installs from /pypi/simple/ (PEP 503). The package namespace is flat —
// like pypi.org itself — so uploads are gated on write-capable identities
// (CI on main, or org members) rather than per-name ownership.
type PyPI struct {
	root string // <data>/pypi/<normalized-name>/<filename>
	meta string // <data>/pypi-meta/<normalized-name>/<filename>.json
	auth *auth.Authenticator
	s3   s3cas.Store   // nil => no second level
	repl *s3replicator // write-through queue, only used when s3 != nil

	// maxUploadBytes caps an upload body; 0 means unlimited.
	maxUploadBytes int64
}

// NewPyPI creates the PyPI registry rooted under dataDir. s3 is the optional
// durable second level (nil disables it).
func NewPyPI(dataDir string, a *auth.Authenticator, s3 s3cas.Store) *PyPI {
	p := &PyPI{
		root: filepath.Join(dataDir, "pypi"),
		meta: filepath.Join(dataDir, "pypi-meta"),
		auth: a,
		s3:   s3,
		repl: &s3replicator{},
	}
	for _, dir := range []string{p.root, p.meta} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			slog.Warn("pypi: create dir", "dir", dir, "err", err)
		}
	}
	return p
}

// normalizePyPIName implements PEP 503 name normalization.
var pypiNameSeps = regexp.MustCompile(`[-_.]+`)

func normalizePyPIName(name string) string {
	return strings.ToLower(pypiNameSeps.ReplaceAllString(name, "-"))
}

// pypiNormalizedName is the PEP 503 normalized-name shape: lowercase
// alphanumerics separated by single hyphens. A normalized name must match this,
// so any name carrying separators, control bytes, backslashes, or stray
// characters that would pollute the S3 key namespace and on-disk dirs is
// rejected at the boundary rather than relying on filesystem/net-http behavior.
var pypiNormalizedName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func validPyPIName(name string) bool {
	return pypiNormalizedName.MatchString(name)
}

// pypiFilename allows the characters that legitimately appear in wheel and
// sdist filenames, and nothing path-shaped. The leading-alphanumeric anchor
// rejects the bare "." and ".." components (and any dot/dash-leading name) so
// the validator itself blocks traversal-shaped filenames rather than leaning on
// filepath/net-http to neutralize them downstream.
var pypiFilename = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

type pypiFileMeta struct {
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	Version     string    `json:"version,omitempty"`
	UploadedAt  time.Time `json:"uploaded_at"`
	Uploader    string    `json:"uploader"`
	UploaderVia string    `json:"uploader_via"`
	Ref         string    `json:"ref,omitempty"`
}

func (p *PyPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := p.auth.RequestIdentity(r)
	if err != nil {
		audit.Anonymous(r.Context())
		p.auth.ChallengeBasic(w)
		return
	}
	audit.SetIdentity(r.Context(), id.Kind, id.Subject, id.Actor, id.Ref)

	rest := strings.TrimPrefix(r.URL.Path, "/pypi")
	switch {
	case rest == "/legacy/" || rest == "/legacy":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !id.CanWriteFlat() {
			audit.Denied(r.Context(), "not authorized")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		p.upload(w, r, id)
	case rest == "/simple/" || rest == "/simple":
		p.rootIndex(w)
	case strings.HasPrefix(rest, "/simple/"):
		p.projectIndex(w, r, strings.TrimPrefix(rest, "/simple/"))
	case strings.HasPrefix(rest, "/packages/"):
		p.serveFile(w, r, strings.TrimPrefix(rest, "/packages/"))
	default:
		http.NotFound(w, r)
	}
}

// upload implements the (de facto) twine/legacy upload API: a multipart POST
// with metadata fields and the distribution in the "content" field.
func (p *PyPI) upload(w http.ResponseWriter, r *http.Request, id *auth.Identity) {
	if p.maxUploadBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, p.maxUploadBytes)
	}
	// Spool large bodies to disk rather than RAM.
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "malformed multipart form", http.StatusBadRequest)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			if err := r.MultipartForm.RemoveAll(); err != nil {
				slog.Warn("pypi: cleaning multipart temp files", "err", err)
			}
		}
	}()

	if action := r.FormValue(":action"); action != "" && action != "file_upload" {
		http.Error(w, fmt.Sprintf("unsupported :action %q", action), http.StatusBadRequest)
		return
	}

	name := normalizePyPIName(r.FormValue("name"))
	if name == "" {
		http.Error(w, "missing package name", http.StatusBadRequest)
		return
	}
	if !validPyPIName(name) {
		// PEP 503 normalized names are lowercase alnum joined by single hyphens.
		// Reject anything else so an authenticated uploader can't inject path /
		// S3-key separators, backslashes, spaces, or control bytes into the
		// (intentionally flat) pypi namespace.
		http.Error(w, "invalid package name", http.StatusBadRequest)
		return
	}

	file, hdr, err := r.FormFile("content")
	if err != nil {
		http.Error(w, "missing content file", http.StatusBadRequest)
		return
	}
	defer util.DeferClose(file)

	filename := filepath.Base(hdr.Filename)
	if !pypiFilename.MatchString(filename) {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	hasher := sha256.New()
	size, err := io.Copy(hasher, file)
	if err != nil {
		http.Error(w, "reading upload", http.StatusInternalServerError)
		return
	}
	sum := hex.EncodeToString(hasher.Sum(nil))

	if claimed := strings.ToLower(r.FormValue("sha256_digest")); claimed != "" && claimed != sum {
		http.Error(w, "sha256_digest does not match uploaded content", http.StatusBadRequest)
		return
	}

	dest := filepath.Join(p.root, name, filename)
	existing := p.readMeta(name, filename)
	if existing == nil {
		// A fresh local disk may still hold the file in the second level:
		// the immutability check must hold across nodes. If S3 is
		// unreachable, proceed local-only (availability over a rare
		// cross-node race) — the conflict would surface at replication.
		existing = p.readS3Meta(r.Context(), name, filename)
	}
	if existing != nil {
		if existing.SHA256 == sum {
			// Idempotent re-upload of identical content.
			w.WriteHeader(http.StatusOK)
			return
		}
		// PyPI semantics: a filename is immutable once published.
		http.Error(w, "file already exists with different content", http.StatusConflict)
		return
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		slog.Error("pypi: creating dir", "dest", dest, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out, err := os.CreateTemp(filepath.Dir(dest), ".upload-*")
	if err != nil {
		slog.Error("pypi: creating temp file", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tmpName := out.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed
	if _, err := io.Copy(out, file); err != nil {
		_ = out.Close()
		slog.Error("pypi: writing", "dest", dest, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := out.Close(); err != nil {
		slog.Error("pypi: closing", "dest", dest, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmpName, dest); err != nil {
		slog.Error("pypi: storing", "dest", dest, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := p.writeMeta(name, filename, &pypiFileMeta{
		SHA256:      sum,
		Size:        size,
		Version:     r.FormValue("version"),
		UploadedAt:  time.Now().UTC(),
		Uploader:    id.Subject,
		UploaderVia: id.Kind,
		Ref:         id.Ref,
	}); err != nil {
		// The sidecar drives the index and S3 replication; without it the
		// upload is not durably complete. Keep the content (a same-content
		// retry is idempotent) but report failure and skip replication — a
		// missing sidecar would otherwise make replicate DELETE the S3 meta.
		slog.Error("pypi: writing meta", "name", name, "file", filename, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	p.replicate(name, filename)

	audit.Event(r.Context(), "upload", name+"/"+filename)
	audit.Content(r.Context(), sum, size)
	w.WriteHeader(http.StatusOK)
}

// rootIndex renders the PEP 503 root index: one anchor per project.
func (p *PyPI) rootIndex(w http.ResponseWriter) {
	entries, err := os.ReadDir(p.root)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><head><title>Simple index</title></head><body>\n")
	for _, n := range names {
		fmt.Fprintf(&b, "<a href=\"/pypi/simple/%s/\">%s</a><br/>\n", html.EscapeString(n), html.EscapeString(n))
	}
	b.WriteString("</body></html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := io.WriteString(w, b.String()); err != nil {
		slog.Warn("pypi: writing root index", "err", err)
	}
}

// projectIndex renders the PEP 503 per-project page with sha256 fragments.
func (p *PyPI) projectIndex(w http.ResponseWriter, r *http.Request, rest string) {
	name := normalizePyPIName(strings.Trim(rest, "/"))
	if !validPyPIName(name) {
		http.Error(w, "invalid project name", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(p.root, name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<!DOCTYPE html><html><head><title>Links for %s</title></head><body>\n<h1>Links for %s</h1>\n",
		html.EscapeString(name), html.EscapeString(name))
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, f := range files {
		href := fmt.Sprintf("/pypi/packages/%s/%s", name, f)
		if m := p.readMeta(name, f); m != nil && m.SHA256 != "" {
			href += "#sha256=" + m.SHA256
		}
		fmt.Fprintf(&b, "<a href=%q>%s</a><br/>\n", href, html.EscapeString(f))
	}
	b.WriteString("</body></html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := io.WriteString(w, b.String()); err != nil {
		slog.Warn("pypi: writing project index", "err", err)
	}
}

func (p *PyPI) serveFile(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	name := normalizePyPIName(parts[0])
	filename := parts[1]
	if !validPyPIName(name) || !pypiFilename.MatchString(filename) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	audit.Event(r.Context(), "download", name+"/"+filename)
	local := filepath.Join(p.root, name, filename)
	if _, err := os.Stat(local); err != nil {
		if p.s3 != nil && p.serveFromS3(w, r, name, filename, local) {
			return
		}
		http.NotFound(w, r)
		return
	}
	if m := p.readMeta(name, filename); m != nil {
		audit.Content(r.Context(), m.SHA256, m.Size)
	}
	http.ServeFile(w, r, local)
}

// serveFromS3 attempts a read-through from the second level on a local miss.
// Returns true when it wrote a response; false means genuinely absent.
func (p *PyPI) serveFromS3(w http.ResponseWriter, r *http.Request, name, filename, local string) bool {
	raw, err := p.s3.GetMeta(r.Context(), s3cas.PyPIMetaKey(name, filename))
	if errors.Is(err, s3cas.ErrNotFound) {
		return false
	}
	if err != nil {
		slog.Error("pypi: s3 meta lookup", "name", name, "file", filename, "err", err)
		http.Error(w, "upstream storage error", http.StatusBadGateway)
		return true
	}
	var m pypiFileMeta
	if err := json.Unmarshal(raw, &m); err != nil || m.SHA256 == "" {
		slog.Error("pypi: s3 meta malformed", "name", name, "file", filename, "err", err)
		http.Error(w, "upstream storage error", http.StatusBadGateway)
		return true
	}
	audit.Content(r.Context(), m.SHA256, m.Size)
	return streamThroughCAS(w, r, p.s3,
		casPointer{digest: m.SHA256, size: m.Size},
		local, filepath.Dir(local),
		func() error { // byte-identical sidecar mirror
			mp := p.metaPath(name, filename)
			if err := os.MkdirAll(filepath.Dir(mp), 0755); err != nil {
				return err
			}
			return os.WriteFile(mp, raw, 0644)
		})
}

// readS3Meta fetches and parses the second-level meta for a distribution
// file, or nil when absent, disabled, or unreadable.
func (p *PyPI) readS3Meta(ctx context.Context, name, filename string) *pypiFileMeta {
	if p.s3 == nil {
		return nil
	}
	raw, err := p.s3.GetMeta(ctx, s3cas.PyPIMetaKey(name, filename))
	if err != nil {
		if !errors.Is(err, s3cas.ErrNotFound) {
			slog.Warn("pypi: s3 conflict check unavailable", "name", name, "file", filename, "err", err)
		}
		return nil
	}
	var m pypiFileMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return &m
}

// replicate converges the S3 second level on the current local state of one
// distribution file (see FileStore.replicate for the convergence semantics).
func (p *PyPI) replicate(name, filename string) {
	if p.s3 == nil {
		return
	}
	key := s3cas.PyPIMetaKey(name, filename)
	sidecar := p.metaPath(name, filename)
	content := filepath.Join(p.root, name, filename)
	p.repl.enqueue(key, func(ctx context.Context) {
		replicateThrough(ctx, p.s3, key, sidecar, content, "pypi")
	})
}

func (p *PyPI) metaPath(name, filename string) string {
	return filepath.Join(p.meta, name, filename+".json")
}

// writeMeta persists the sidecar. It is load-bearing — the sidecar drives the
// per-project index and S3 replication — so its failure is returned, not just
// logged, and the caller fails the upload.
func (p *PyPI) writeMeta(name, filename string, m *pypiFileMeta) error {
	mp := p.metaPath(name, filename)
	if err := os.MkdirAll(filepath.Dir(mp), 0755); err != nil {
		return fmt.Errorf("creating meta dir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding meta: %w", err)
	}
	if err := os.WriteFile(mp, data, 0644); err != nil {
		return fmt.Errorf("writing meta: %w", err)
	}
	return nil
}

func (p *PyPI) readMeta(name, filename string) *pypiFileMeta {
	data, err := os.ReadFile(p.metaPath(name, filename))
	if err != nil {
		return nil
	}
	var m pypiFileMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return &m
}
