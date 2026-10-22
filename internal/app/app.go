package app

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/commands"
	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/packages/index"
	v2index "github.com/Delphos-Labs/roxxy/internal/packages/index/v2"
	"github.com/Delphos-Labs/roxxy/internal/server"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// Application holds all application-level dependencies and state.
// This replaces global variables and enables dependency injection.
type Application struct {
	// Build metadata
	BuildTime string

	// Configuration
	ConfigFile   string
	GlobalConfig *config.GlobalConfig

	// Directories
	CacheDir string
	CertsDir string
	LogDir   string

	// Core services
	StatsTracker *cache.StatsTracker
	IndexDB      *index.IndexDB     // V1 index (kept for reference)
	IndexDBV2    *v2index.IndexDBV2 // V2 index (new system)

	// Server instance (initialized when starting server)
	Server *server.Server
}

// NewApplication creates and initializes a new Application instance.
// This performs all the initialization that was previously done with globals.
// verbose controls whether to log initialization messages (false for CLI commands, true for server).
func NewApplication(buildTime string, verbose bool) (*Application, error) {
	app := &Application{
		BuildTime: buildTime,
	}

	// Find and load config file
	app.ConfigFile = config.FindConfigFile(verbose)
	var err error
	app.GlobalConfig, err = config.LoadGlobalConfig(app.ConfigFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load global config: %w", err)
	}

	// Set paths from config
	app.CacheDir = app.GlobalConfig.CacheDir
	app.CertsDir = app.GlobalConfig.CertsDir
	app.LogDir = app.GlobalConfig.LogDir

	// Validate that CacheDir is set
	if app.CacheDir == "" {
		return nil, fmt.Errorf("CacheDir is empty - cannot initialize application")
	}

	// Set global config in config package so other packages can access it
	config.SetGlobalConfig(app.GlobalConfig)

	// Configure local hostnames for IsLocalhost checks
	if len(app.GlobalConfig.LocalHostnames) > 0 {
		util.SetLocalHostnames(app.GlobalConfig.LocalHostnames)
		log.Printf("Configured local hostnames: %v", app.GlobalConfig.LocalHostnames)
	}

	// Ensure directories exist
	// On Windows, MkdirAll may fail if parent directories don't exist, so ensure parent exists first
	if err := os.MkdirAll(app.CacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory %s: %w", app.CacheDir, err)
	}
	if err := os.MkdirAll(app.CertsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create certs directory %s: %w", app.CertsDir, err)
	}
	if err := os.MkdirAll(app.LogDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory %s: %w", app.LogDir, err)
	}

	if verbose {
		log.Printf("Using cache directory: %s", app.CacheDir)
		log.Printf("Using certs directory: %s", app.CertsDir)
		log.Printf("Using log directory: %s", app.LogDir)
	}

	// Bootstrap cache from tarball if cache is empty
	if err := app.bootstrapCacheFromTarball(verbose); err != nil {
		log.Printf("Warning: Failed to bootstrap cache from tarball: %v", err)
	}

	// Initialize stats tracker
	app.StatsTracker = cache.NewStatsTracker(filepath.Join(app.CacheDir, "access-stats.json"))
	if err := app.StatsTracker.Load(verbose); err != nil {
		log.Printf("Warning: Failed to load access stats: %v", err)
	}

	// Initialize indexDB (v1 - kept for reference)
	app.IndexDB = index.NewIndexDB(filepath.Join(app.CacheDir, "index"))

	// Initialize indexDBV2 (new system)
	indexDir := filepath.Join(app.CacheDir, "index")
	app.IndexDBV2 = v2index.NewIndexDBV2(indexDir)

	// Ensure v2 index exists (migrate from v1 if needed)
	if err := v2index.EnsureV2Index(indexDir, indexDir); err != nil {
		if verbose {
			log.Printf("Warning: Failed to ensure v2 index (migration may be needed): %v", err)
		}
	} else if verbose {
		log.Printf("V2 index ready (migrated from v1 if needed)")
	}

	return app, nil
}

// NewServer creates and initializes the server instance.
// This should be called when starting the proxy server.
// sbomManager can be nil if not yet initialized - it can be set later and LoadConfig called again
func (app *Application) NewServer(airgapProxy, airgapped, debug bool, sbomManager server.SBOMManagerInterface) (*server.Server, error) {
	// Validate CacheDir is set before creating server
	if app.CacheDir == "" {
		return nil, fmt.Errorf("CacheDir is empty - cannot create server. Check config file or default cache directory resolution")
	}

	srv := server.NewServer(
		app.CacheDir,
		app.CertsDir,
		app.LogDir,
		app.GlobalConfig,
		app.ConfigFile,
		app.StatsTracker,
	)
	srv.IndexDB = app.IndexDB
	srv.SBOMManager = sbomManager
	srv.Debug = debug

	if err := srv.LoadConfig(airgapProxy, airgapped); err != nil {
		return nil, fmt.Errorf("failed to load server config: %w", err)
	}

	app.Server = srv
	return srv, nil
}

// GetCommandConfig returns a commands.Config for CLI command handlers.
func (app *Application) GetCommandConfig() commands.Config {
	return commands.Config{
		CacheDir:     app.CacheDir,
		CertsDir:     app.CertsDir,
		LogDir:       app.LogDir,
		ConfigFile:   app.ConfigFile,
		StatsTracker: app.StatsTracker,
		IndexDB:      app.IndexDB,
		BuildTime:    app.BuildTime,
		GlobalConfig: app.GlobalConfig,
	}
}

// bootstrapCacheFromTarball initializes the cache directory from a tarball if it's empty.
// Similar to PostgreSQL's /docker-entrypoint-initdb.d/ functionality.
// It checks:
// 1. Environment variable ROXXY_CACHE_INIT_TARBALL for a specific tarball path
// 2. /docker-entrypoint-initcache.d/ directory for .tar.gz or .tar files
// Only runs if cache is empty (no marker file exists).
// verbose controls whether to log bootstrap messages.
func (app *Application) bootstrapCacheFromTarball(verbose bool) error {
	markerFile := filepath.Join(app.CacheDir, ".roxxy-bootstrapped")

	// Check if already bootstrapped
	if _, err := os.Stat(markerFile); err == nil {
		log.Printf("Cache already bootstrapped (marker file exists), skipping bootstrap")
		return nil
	}

	// Check if cache is empty (only marker file would be missing)
	entries, err := os.ReadDir(app.CacheDir)
	if err != nil {
		return fmt.Errorf("failed to read cache directory: %w", err)
	}

	// Filter out hidden files and the marker file itself
	hasContent := false
	for _, entry := range entries {
		name := entry.Name()
		if name != ".roxxy-bootstrapped" && !strings.HasPrefix(name, ".") {
			hasContent = true
			break
		}
	}

	if hasContent {
		if verbose {
			log.Printf("Cache directory has content, skipping bootstrap")
		}
		// Create marker file to prevent future bootstrap attempts
		if f, err := os.Create(markerFile); err == nil {
			util.DeferClose(f)
		}
		return nil
	}

	// Check environment variable first
	initTarball := os.Getenv("ROXXY_CACHE_INIT_TARBALL")
	if initTarball != "" {
		if verbose {
			log.Printf("Bootstrap: Found ROXXY_CACHE_INIT_TARBALL=%s", initTarball)
		}
		if err := app.extractTarball(initTarball, app.CacheDir); err != nil {
			return fmt.Errorf("failed to extract tarball from ROXXY_CACHE_INIT_TARBALL: %w", err)
		}
		// Create marker file
		if err := os.WriteFile(markerFile, []byte(time.Now().Format(time.RFC3339)), 0644); err != nil {
			log.Printf("Warning: Failed to create bootstrap marker file: %v", err)
		}
		if verbose {
			log.Printf("Cache bootstrapped successfully from %s", initTarball)
		}
		return nil
	}

	// Check /docker-entrypoint-initcache.d/ directory
	initDir := "/docker-entrypoint-initcache.d"
	if _, err := os.Stat(initDir); os.IsNotExist(err) {
		// Directory doesn't exist, nothing to bootstrap
		return nil
	}

	initEntries, err := os.ReadDir(initDir)
	if err != nil {
		return fmt.Errorf("failed to read init directory %s: %w", initDir, err)
	}

	// Find all .tar.gz and .tar files, sorted alphabetically
	var tarballs []string
	for _, entry := range initEntries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") || strings.HasSuffix(name, ".tar") {
			tarballs = append(tarballs, filepath.Join(initDir, name))
		}
	}

	if len(tarballs) == 0 {
		// No tarballs found, nothing to bootstrap
		return nil
	}

	sort.Strings(tarballs)
	log.Printf("Bootstrap: Found %d tarball(s) in %s", len(tarballs), initDir)

	// Extract all tarballs in order
	for _, tarball := range tarballs {
		log.Printf("Bootstrap: Extracting %s...", filepath.Base(tarball))
		if err := app.extractTarball(tarball, app.CacheDir); err != nil {
			return fmt.Errorf("failed to extract %s: %w", tarball, err)
		}
	}

	// Create marker file
	if err := os.WriteFile(markerFile, []byte(time.Now().Format(time.RFC3339)), 0644); err != nil {
		log.Printf("Warning: Failed to create bootstrap marker file: %v", err)
	}

	log.Printf("Cache bootstrapped successfully from %d tarball(s)", len(tarballs))
	return nil
}

// extractTarball extracts a tarball (tar.gz, tgz, or tar) to the target directory
func (app *Application) extractTarball(tarballPath, targetDir string) error {
	file, err := os.Open(tarballPath)
	if err != nil {
		return fmt.Errorf("failed to open tarball: %w", err)
	}
	defer util.DeferClose(file)

	var tarReader *tar.Reader

	// Check if it's gzipped
	if strings.HasSuffix(tarballPath, ".gz") || strings.HasSuffix(tarballPath, ".tgz") {
		gzReader, err := gzip.NewReader(file)
		if err != nil {
			return fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer util.DeferClose(gzReader)
		tarReader = tar.NewReader(gzReader)
	} else {
		tarReader = tar.NewReader(file)
	}

	// Extract all files
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Strip cache/ prefix if present (for backwards compatibility with old tarballs)
		// New tarballs don't have cache/ prefix, but old ones might
		entryName := strings.TrimPrefix(header.Name, "cache/")

		// Sanitize path to prevent directory traversal
		targetPath := filepath.Join(targetDir, entryName)
		if !strings.HasPrefix(targetPath, filepath.Clean(targetDir)+string(os.PathSeparator)) {
			return fmt.Errorf("invalid path in tarball: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			// Create directory
			if err := os.MkdirAll(targetPath, os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", targetPath, err)
			}
		case tar.TypeReg:
			// Create file
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return fmt.Errorf("failed to create parent directory for %s: %w", targetPath, err)
			}
			outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return fmt.Errorf("failed to create file %s: %w", targetPath, err)
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				util.Logerr(outFile.Close())
				return fmt.Errorf("failed to write file %s: %w", targetPath, err)
			}
			util.Logerr(outFile.Close())
		case tar.TypeSymlink:
			// Validate symlink target to prevent directory traversal
			// Resolve the symlink target relative to the symlink's directory
			symlinkDir := filepath.Dir(targetPath)
			resolvedTarget := filepath.Join(symlinkDir, header.Linkname)
			// Clean the path to resolve any ../ components
			cleanedTarget := filepath.Clean(resolvedTarget)

			// Ensure the resolved target stays within the target directory
			if !strings.HasPrefix(cleanedTarget, filepath.Clean(targetDir)+string(os.PathSeparator)) {
				log.Printf("Warning: Skipping symlink %s -> %s (target outside extraction directory)", targetPath, header.Linkname)
				continue
			}

			// Create symlink
			if err := os.Symlink(header.Linkname, targetPath); err != nil {
				// Log but don't fail on symlink errors (they might not work in all environments)
				log.Printf("Warning: Failed to create symlink %s -> %s: %v", targetPath, header.Linkname, err)
			}
		}
	}

	return nil
}
