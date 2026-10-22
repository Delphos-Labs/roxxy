package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/archifau/audit"
	"github.com/Delphos-Labs/roxxy/internal/archifau/auth"
	"github.com/Delphos-Labs/roxxy/internal/archifau/s3cas"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// FileStore is the bare PUT/GET/DELETE file endpoint. Files live under
// /files/<namespace>/<path>, where the namespace (the leading org/repo or
// org segments) is what authorization is checked against — a CI job can only
// write under its own repository's prefix, mirroring the docker ACL.
type FileStore struct {
	root string // <data>/files
	meta string // <data>/files-meta
	tmp  string // <data>/tmp
	auth *auth.Authenticator
	s3   s3cas.Store   // nil => no second level
	repl *s3replicator // write-through queue, only used when s3 != nil

	// maxUploadBytes caps a PUT body; 0 means unlimited.
	maxUploadBytes int64
}

// NewFileStore creates the file store rooted under dataDir. s3 is the
// optional durable second level (nil disables it).
func NewFileStore(dataDir string, a *auth.Authenticator, s3 s3cas.Store) *FileStore {
	fs := &FileStore{
		root: filepath.Join(dataDir, "files"),
		meta: filepath.Join(dataDir, "files-meta"),
		tmp:  filepath.Join(dataDir, "tmp"),
		auth: a,
		s3:   s3,
		repl: &s3replicator{},
	}
	for _, dir := range []string{fs.root, fs.meta, fs.tmp} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			slog.Warn("files: create dir", "dir", dir, "err", err)
		}
	}
	return fs
}

// fileMeta records provenance for each upload — the identity claims are the
// audit trail, so they are kept alongside the content.
type fileMeta struct {
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type,omitempty"`
	UploadedAt  time.Time `json:"uploaded_at"`
	Uploader    string    `json:"uploader"`      // identity subject
	UploaderVia string    `json:"uploader_via"`  // "oidc" or "user"
	Ref         string    `json:"ref,omitempty"` // git ref for CI uploads
}

func (f *FileStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rel, ok := cleanRelPath(strings.TrimPrefix(r.URL.Path, "/files/"))
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	id, err := f.auth.RequestIdentity(r)
	if err != nil {
		audit.Anonymous(r.Context())
		f.auth.ChallengeBasic(w)
		return
	}
	audit.SetIdentity(r.Context(), id.Kind, id.Subject, id.Actor, id.Ref)
	audit.Object(r.Context(), rel)

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if !id.CanRead(rel) {
			audit.Denied(r.Context(), "not authorized")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		f.serve(w, r, rel)
	case http.MethodPut:
		if !id.CanWrite(rel) {
			audit.Denied(r.Context(), "not authorized")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		f.put(w, r, rel, id)
	case http.MethodDelete:
		if !id.CanWrite(rel) {
			audit.Denied(r.Context(), "not authorized")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		f.delete(w, r, rel)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (f *FileStore) serve(w http.ResponseWriter, r *http.Request, rel string) {
	full := filepath.Join(f.root, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	if err != nil {
		if f.s3 != nil && f.serveFromS3(w, r, rel, full) {
			return
		}
		http.NotFound(w, r)
		return
	}
	if info.IsDir() {
		f.list(w, rel, full)
		return
	}
	audit.Event(r.Context(), "download", rel)
	if m := f.readMeta(rel); m != nil {
		if m.ContentType != "" {
			w.Header().Set("Content-Type", m.ContentType)
		}
		audit.Content(r.Context(), m.SHA256, m.Size)
	}
	http.ServeFile(w, r, full)
}

// list renders a directory as JSON — enough for scripted discovery without
// a UI.
func (f *FileStore) list(w http.ResponseWriter, rel, full string) {
	entries, err := os.ReadDir(full)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"path": rel, "entries": names}); err != nil {
		slog.Warn("files: writing listing", "path", rel, "err", err)
	}
}

func (f *FileStore) put(w http.ResponseWriter, r *http.Request, rel string, id *auth.Identity) {
	if r.Body == nil {
		http.Error(w, "PUT requires a body", http.StatusBadRequest)
		return
	}
	defer util.DeferClose(r.Body)

	body := r.Body
	if f.maxUploadBytes > 0 {
		body = http.MaxBytesReader(w, body, f.maxUploadBytes)
	}

	tmp, err := os.CreateTemp(f.tmp, "put-*.tmp")
	if err != nil {
		slog.Error("files: creating temp file", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(hasher, tmp), body)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
			return
		}
		slog.Error("files: writing upload", "path", rel, "err", err)
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	sum := hex.EncodeToString(hasher.Sum(nil))

	full := filepath.Join(f.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		slog.Error("files: creating dir", "path", rel, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmpName, full); err != nil {
		slog.Error("files: storing", "path", rel, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := f.writeMeta(rel, &fileMeta{
		SHA256:      sum,
		Size:        size,
		ContentType: r.Header.Get("Content-Type"),
		UploadedAt:  time.Now().UTC(),
		Uploader:    id.Subject,
		UploaderVia: id.Kind,
		Ref:         id.Ref,
	}); err != nil {
		// The sidecar drives listing and S3 replication; without it the upload
		// is not durably complete. Keep the content (a same-content retry is
		// idempotent) but report failure and do not enqueue replication — a
		// missing sidecar would otherwise make replicate DELETE the S3 meta.
		slog.Error("files: writing meta", "path", rel, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	f.replicate(rel)

	audit.Event(r.Context(), "upload", rel)
	audit.Content(r.Context(), sum, size)
	w.Header().Set("X-Content-SHA256", sum)
	w.WriteHeader(http.StatusCreated)
	_, _ = fmt.Fprintf(w, "%s\n", sum)
}

func (f *FileStore) delete(w http.ResponseWriter, r *http.Request, rel string) {
	full := filepath.Join(f.root, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := os.Remove(full); err != nil {
		slog.Error("files: deleting", "path", rel, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := os.Remove(f.metaPath(rel)); err != nil && !os.IsNotExist(err) {
		slog.Warn("files: deleting meta", "path", rel, "err", err)
	}
	f.replicate(rel)
	audit.Event(r.Context(), "delete", rel)
	w.WriteHeader(http.StatusNoContent)
}

func (f *FileStore) metaPath(rel string) string {
	return filepath.Join(f.meta, filepath.FromSlash(rel)+".json")
}

// writeMeta persists the sidecar. It is load-bearing — the sidecar drives
// directory listing and S3 replication — so its failure is returned, not just
// logged, and the caller fails the upload.
func (f *FileStore) writeMeta(rel string, m *fileMeta) error {
	p := f.metaPath(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return fmt.Errorf("creating meta dir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding meta: %w", err)
	}
	if err := os.WriteFile(p, data, 0644); err != nil {
		return fmt.Errorf("writing meta: %w", err)
	}
	return nil
}

func (f *FileStore) readMeta(rel string) *fileMeta {
	data, err := os.ReadFile(f.metaPath(rel))
	if err != nil {
		return nil
	}
	var m fileMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return &m
}

// serveFromS3 attempts a read-through from the second level on a local miss.
// Returns true when it wrote a response (success or error); false means the
// object is genuinely absent and the caller should 404.
func (f *FileStore) serveFromS3(w http.ResponseWriter, r *http.Request, rel, full string) bool {
	raw, err := f.s3.GetMeta(r.Context(), s3cas.FilesMetaKey(rel))
	if errors.Is(err, s3cas.ErrNotFound) {
		return false
	}
	if err != nil {
		slog.Error("files: s3 meta lookup", "path", rel, "err", err)
		http.Error(w, "upstream storage error", http.StatusBadGateway)
		return true
	}
	var m fileMeta
	if err := json.Unmarshal(raw, &m); err != nil || m.SHA256 == "" {
		slog.Error("files: s3 meta malformed", "path", rel, "err", err)
		http.Error(w, "upstream storage error", http.StatusBadGateway)
		return true
	}
	audit.Event(r.Context(), "download", rel)
	audit.Content(r.Context(), m.SHA256, m.Size)
	return streamThroughCAS(w, r, f.s3,
		casPointer{digest: m.SHA256, size: m.Size, contentType: m.ContentType},
		full, f.tmp,
		func() error { // byte-identical sidecar mirror
			p := f.metaPath(rel)
			if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
				return err
			}
			return os.WriteFile(p, raw, 0644)
		})
}

// replicate converges the S3 second level on the current local state of rel:
// present locally => CAS blob + meta pushed; absent => the S3 meta object is
// deleted (CAS blobs are immutable and possibly shared, never deleted). Jobs
// re-read the sidecar at send time and serialize per key, so concurrent
// writes to one name converge on the last local commit regardless of
// goroutine scheduling.
func (f *FileStore) replicate(rel string) {
	if f.s3 == nil {
		return
	}
	key := s3cas.FilesMetaKey(rel)
	sidecar := f.metaPath(rel)
	content := filepath.Join(f.root, filepath.FromSlash(rel))
	f.repl.enqueue(key, func(ctx context.Context) {
		replicateThrough(ctx, f.s3, key, sidecar, content, "files")
	})
}

// cleanRelPath normalizes a request path into a safe relative slash-path.
// Rejects traversal, absolute paths, and empty paths.
func cleanRelPath(p string) (string, bool) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", false
	}
	cleaned := path.Clean(p)
	if cleaned != p || strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "/../") {
		return "", false
	}
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return cleaned, true
}
