package ui

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/api"
	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/registry"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

//go:generate node static/build-js.js
//go:embed static/*
//go:embed static/components/*
//go:embed static/components/templates/*
//go:embed static/js/*
var staticFiles embed.FS

// packageListTemplate is the parsed Go template for the package list component
var packageListTemplate *template.Template

func init() {
	// Parse all package list template files
	// Template files must be parsed in dependency order
	templateFiles := []string{
		"static/components/templates/version-click-handler.tmpl",
		"static/components/templates/version-header.tmpl",
		"static/components/templates/version-metadata.tmpl",
		"static/components/templates/package-metadata.tmpl",
		"static/components/templates/package-versions.tmpl",
		"static/components/templates/package-subpackages.tmpl",
		"static/components/templates/package-component.tmpl",
		"static/components/templates/flat-group.tmpl",
		"static/components/templates/group-recursive.tmpl",
		"static/components/templates/package-list-main.tmpl",
	}

	// Create template with custom functions
	tmpl := template.New("package-list").Funcs(template.FuncMap{
		"add": func(a, b int) int { return a + b },
		"dict": func(values ...interface{}) (map[string]interface{}, error) {
			if len(values)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			dict := make(map[string]interface{})
			for i := 0; i < len(values); i += 2 {
				key, ok := values[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key must be string, got %T", values[i])
				}
				dict[key] = values[i+1]
			}
			return dict, nil
		},
	})

	// Parse all template files
	var parseErrors []string
	for _, file := range templateFiles {
		tmplData, err := staticFiles.ReadFile(file)
		if err != nil {
			parseErrors = append(parseErrors, fmt.Sprintf("read %s: %v", file, err))
			continue
		}
		_, err = tmpl.Parse(string(tmplData))
		if err != nil {
			parseErrors = append(parseErrors, fmt.Sprintf("parse %s: %v", file, err))
			continue
		}
	}

	if len(parseErrors) > 0 {
		log.Printf("Warning: Failed to parse some package-list template files:")
		for _, err := range parseErrors {
			log.Printf("  %s", err)
		}
		// Still set the template even if some files failed - it might be partially functional
	}

	packageListTemplate = tmpl
}

// GetStaticFiles returns the embedded static files filesystem
// This allows other packages (like cmd/lsembed) to access the embedded files
func GetStaticFiles() embed.FS {
	return staticFiles
}

// Server handles serving the web UI
type Server struct {
	api   *api.API
	apiV2 *APIV2
}

// NewServer creates a new UI server
func NewServer(cacheDir string, statsTracker *cache.StatsTracker, indexDB interface{}) *Server {
	s := &Server{
		api: api.NewAPI(cacheDir, statsTracker, indexDB),
	}

	// Try to create v2 API if indexDBV2 is available
	if indexDBV2, ok := indexDB.(IndexDBV2Interface); ok {
		s.apiV2 = NewAPIV2(cacheDir, statsTracker, indexDBV2)
	}

	return s
}

// SetSBOMManager sets the SBOM manager for the API
func (s *Server) SetSBOMManager(manager api.SBOMManagerInterface) {
	s.api.SetSBOMManager(manager)
}

// SetCertsDir sets the certificates directory for the API
func (s *Server) SetCertsDir(certsDir string) {
	s.api.SetCertsDir(certsDir)
}

// SetOverrideStore sets the image override store for the API
func (s *Server) SetOverrideStore(store *registry.OverrideStore) {
	s.api.SetOverrideStore(store)
}

// RegisterRoutes registers UI routes with the given mux
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	// Register v2 API routes first (replaces existing package endpoints)
	if s.apiV2 != nil {
		s.apiV2.RegisterRoutes(mux)
		// Register other API routes but skip package endpoints that v2 handles
		s.api.RegisterRoutesExceptPackages(mux)
	} else {
		// Fallback to old API if v2 not available
		s.api.RegisterRoutes(mux)
	}

	// Serve static files
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Printf("Warning: Failed to load static files: %v", err)
		return
	}

	// Serve package-list component as a Go template (if available)
	// This must be registered BEFORE the static file handler to take precedence
	if packageListTemplate != nil {
		mux.HandleFunc("/ui/static/components/package-list.html", s.handlePackageListComponent)
		log.Printf("Package list template handler registered at /ui/static/components/package-list.html")
	} else {
		log.Printf("Warning: Package list template not available, will serve static file")
	}

	// Serve UI at /ui with gzip support
	// staticFS is already rooted at "static", so files are at the root
	// When request is /ui/static/app.min.js, StripPrefix("/ui/") gives /static/app.min.js
	// But we need to strip /ui/static/ to get app.min.js
	mux.Handle("/ui/static/", http.StripPrefix("/ui/static/", gzipFileServer(http.FS(staticFS))))
	mux.Handle("/ui/", http.StripPrefix("/ui/", gzipFileServer(http.FS(staticFS))))
	mux.HandleFunc("/ui", func(w http.ResponseWriter, r *http.Request) {
		// Redirect to /ui/ for proper path handling
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	})

	// Also serve at root for convenience
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Only serve UI if path is exactly "/"
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/ui/", http.StatusTemporaryRedirect)
			return
		}
		// Otherwise, let other handlers deal with it
		http.NotFound(w, r)
	})
}

// gzipFileServer wraps http.FileServer to support serving pre-compressed .gz files
// when the client accepts gzip encoding. If a .gz version exists and the client
// accepts gzip, it serves that with Content-Encoding: gzip header.
// Otherwise, it falls back to the regular file.
func gzipFileServer(fs http.FileSystem) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if client accepts gzip
		acceptsGzip := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")

		// Try to open the requested file
		path := r.URL.Path
		if path == "" || path == "/" {
			path = "/index.html"
		}

		file, err := fs.Open(path)
		if err != nil {
			// If regular file doesn't exist, try .gz version
			if acceptsGzip {
				gzPath := path + ".gz"
				gzFile, gzErr := fs.Open(gzPath)
				if gzErr == nil {
					defer util.DeferClose(gzFile)
					// Serve the pre-compressed file
					info, _ := gzFile.Stat()
					w.Header().Set("Content-Encoding", "gzip")
					w.Header().Set("Content-Type", getContentType(path))
					w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
					w.Header().Set("Vary", "Accept-Encoding")
					util.LogerrT(io.Copy(w, gzFile))
					return
				}
			}
			// File not found, let FileServer handle 404
			http.NotFound(w, r)
			return
		}
		defer util.DeferClose(file)

		info, err := file.Stat()
		if err != nil {
			http.NotFound(w, r)
			return
		}

		// If it's a directory, let FileServer handle it
		if info.IsDir() {
			http.FileServer(fs).ServeHTTP(w, r)
			return
		}

		// Check if .gz version exists and client accepts gzip
		if acceptsGzip {
			gzPath := path + ".gz"
			gzFile, gzErr := fs.Open(gzPath)
			if gzErr == nil {
				defer util.DeferClose(gzFile)
				gzInfo, _ := gzFile.Stat()
				// Only serve .gz if it's smaller (or similar size)
				// This avoids serving larger compressed files
				if gzInfo.Size() < info.Size() {
					w.Header().Set("Content-Encoding", "gzip")
					w.Header().Set("Content-Type", getContentType(path))
					w.Header().Set("Content-Length", fmt.Sprintf("%d", gzInfo.Size()))
					w.Header().Set("Vary", "Accept-Encoding")
					util.LogerrT(io.Copy(w, gzFile))
					return
				}
			}
		}

		// Serve regular file
		http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	})
}

// handlePackageListComponent renders the package list component using Go templates
func (s *Server) handlePackageListComponent(w http.ResponseWriter, r *http.Request) {
	if packageListTemplate == nil {
		// Fallback to static file if template not available
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Template data
	data := map[string]interface{}{
		"Types": []string{"apk", "apt", "npm", "pypi", "go"},
	}

	// Execute template to a buffer first to check for errors
	var buf strings.Builder
	if err := packageListTemplate.Execute(&buf, data); err != nil {
		log.Printf("Error rendering package-list template: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Write the buffered output
	if _, err := w.Write([]byte(buf.String())); err != nil {
		log.Printf("Error writing response: %v", err)
	}
}

// getContentType returns the Content-Type based on file extension
func getContentType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	case ".woff", ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".gz":
		// For .gz files, return the content type of the original file
		basePath := strings.TrimSuffix(path, ".gz")
		return getContentType(basePath)
	default:
		return "application/octet-stream"
	}
}
