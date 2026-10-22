// Package server wires archifau's HTTP surface: an authenticated docker
// registry (reusing roxxy's local-storage registry implementation), a PyPI
// registry, and a bare file store — origin semantics only, no proxying.
package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/archifau/audit"
	"github.com/Delphos-Labs/roxxy/internal/archifau/auth"
	"github.com/Delphos-Labs/roxxy/internal/archifau/s3cas"
)

// Config configures the archifau HTTP handler tree.
type Config struct {
	// DataDir is the root of all stored content.
	DataDir string
	// Auth performs authentication and issues tokens.
	Auth *auth.Authenticator
	// Registry handles /v2/ requests (roxxy's DockerRegistryProxy in
	// airgapped, local-push mode). Authorization happens before it is called.
	Registry http.Handler
	// S3 is the durable second level for the PyPI and file surfaces.
	// nil disables it: pure local storage, exactly as before.
	S3 s3cas.Store
	// MaxUploadBytes caps a single PyPI or file upload body; 0 means
	// unlimited (preserving prior behavior). Oversized bodies get a 413.
	MaxUploadBytes int64
}

// Server is the archifau HTTP surface plus a handle on the background
// write-through replicators, so a graceful shutdown can drain in-flight
// replication before the process exits.
type Server struct {
	handler http.Handler
	pypi    *PyPI
	files   *FileStore
}

// repoAuthorizer is implemented by the reused docker registry; archifau wires
// its namespace ACL into it so catalog listings and cross-repo blob mounts are
// confined to the caller's readable repos (checks the dockerAuth scope can't
// make from the URL alone).
type repoAuthorizer interface {
	SetAuthorizer(fn func(ctx context.Context, repo string) bool)
}

// New builds the archifau server.
func New(cfg Config) *Server {
	mux := http.NewServeMux()

	// Enforce per-repo read authorization inside the registry (catalog filter,
	// mount source) using the identity dockerAuth places in the request context.
	// Fail closed if no identity is present.
	//
	// archifau's tenant isolation depends on this hook; refuse to start rather
	// than silently serving an unfiltered catalog / unconfined mounts if a
	// future refactor wraps cfg.Registry in a decorator that doesn't implement
	// repoAuthorizer.
	ra, ok := cfg.Registry.(repoAuthorizer)
	if !ok {
		panic("archifau: Registry must implement repoAuthorizer (per-repo authz is mandatory)")
	}
	ra.SetAuthorizer(func(ctx context.Context, repo string) bool {
		id := auth.IdentityFromContext(ctx)
		return id != nil && id.CanRead(repo)
	})

	mux.Handle("/v2/", dockerAuth(cfg.Auth, cfg.Registry))
	mux.Handle("/token", cfg.Auth.TokenHandler())

	pypi := NewPyPI(cfg.DataDir, cfg.Auth, cfg.S3)
	pypi.maxUploadBytes = cfg.MaxUploadBytes
	mux.Handle("/pypi/", pypi)

	files := NewFileStore(cfg.DataDir, cfg.Auth, cfg.S3)
	files.maxUploadBytes = cfg.MaxUploadBytes
	mux.Handle("/files/", files)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n")) // failure is visible in the access line
	})

	return &Server{handler: accessLog(mux), pypi: pypi, files: files}
}

// Handler returns the HTTP handler tree to serve.
func (s *Server) Handler() http.Handler { return s.handler }

// DrainContext blocks until all in-flight write-through replication jobs
// finish or ctx expires, so a graceful shutdown drains durability work but
// stays bounded by the shutdown budget. Call it after the HTTP server has
// stopped accepting requests. On ctx expiry it cancels in-flight jobs (so a
// wedged S3 can't hang shutdown until SIGKILL) and returns ctx.Err(); the
// caller should log the abandoned work at error level so it's alertable.
func (s *Server) DrainContext(ctx context.Context) error {
	if err := s.pypi.repl.waitContext(ctx); err != nil {
		_ = s.files.repl.waitContext(ctx) // already expired; cancel + unwind
		return err
	}
	return s.files.repl.waitContext(ctx)
}

// dockerAuth enforces authentication and namespace authorization on the
// docker registry API, speaking the registry v2 token-challenge protocol.
func dockerAuth(a *auth.Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo, action := dockerScope(r)

		id, err := a.RequestIdentity(r)
		if err != nil {
			audit.Anonymous(r.Context())
			scope := ""
			if repo != "" {
				scope = "repository:" + repo + ":" + action
			}
			a.ChallengeDocker(w, scope)
			return
		}
		audit.SetIdentity(r.Context(), id.Kind, id.Subject, id.Actor, id.Ref)
		audit.Object(r.Context(), repo)

		// /v2/ ping and /v2/_catalog need authentication only.
		if repo != "" {
			allowed := id.CanRead(repo)
			if action == "push,pull" {
				allowed = id.CanWrite(repo)
			}
			if !allowed {
				audit.Denied(r.Context(), "not authorized")
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}

		// Carry the identity into the registry handler so its per-repo
		// authorization hook (catalog filtering, cross-repo mount source) can
		// enforce the namespace ACL — checks the middleware's scope can't make
		// (the response body for _catalog, the from= param for mount).
		r = r.WithContext(auth.WithIdentity(r.Context(), id))
		next.ServeHTTP(w, r)
	})
}

// dockerScope derives the repository name and required action from a
// registry v2 API path. Returns repo == "" for paths that only need
// authentication (the /v2/ ping, /v2/_catalog).
func dockerScope(r *http.Request) (repo, action string) {
	path := strings.TrimPrefix(r.URL.Path, "/v2")
	path = strings.TrimPrefix(path, "/")
	if path == "" || path == "_catalog" {
		return "", "pull"
	}

	// Repository name is everything before the API suffix:
	//   <name>/manifests/<ref>, <name>/blobs/<digest>,
	//   <name>/blobs/uploads/[<uuid>], <name>/tags/list
	for _, marker := range []string{"/manifests/", "/blobs/", "/tags/"} {
		if i := strings.LastIndex(path, marker); i > 0 {
			repo = path[:i]
			break
		}
	}
	if repo == "" {
		// Unrecognized shape: treat the whole path as the repository so an
		// overly-creative path can't dodge authorization.
		repo = path
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return strings.ToLower(repo), "pull"
	default:
		return strings.ToLower(repo), "push,pull"
	}
}
