package server

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Delphos-Labs/roxxy/internal/cache"
	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/packages"
	"github.com/Delphos-Labs/roxxy/internal/packages/index"
	"github.com/Delphos-Labs/roxxy/internal/registry"
)

// SBOMManagerInterface is an interface for SBOM session management
// This matches the packages.SBOMManagerInterface
type SBOMManagerInterface = packages.SBOMManagerInterface

// Server holds all server dependencies and state
type Server struct {
	CacheDir              string
	CertsDir              string
	LogDir                string
	GlobalConfig          *config.GlobalConfig
	ConfigFile            string
	StatsTracker          *cache.StatsTracker
	IndexDB               *index.IndexDB
	SBOMManager           SBOMManagerInterface
	PackageEnricher       packages.PackageEnricherInterface
	KnownRegistries       config.RegistryMap
	PackageManagerProxies map[string]http.Handler
	DockerRegistryProxies map[string]http.Handler
	OverrideStore         *registry.OverrideStore
	Debug                 bool
}

// NewServer creates a new Server instance
func NewServer(cacheDir, certsDir, logDir string, globalConfig *config.GlobalConfig, configFile string, statsTracker *cache.StatsTracker) *Server {
	log.Printf("[NewServer] Creating server with cacheDir=%q, certsDir=%q, logDir=%q", cacheDir, certsDir, logDir)
	if globalConfig != nil {
		log.Printf("[NewServer] GlobalConfig.CacheDir=%q", globalConfig.CacheDir)
	}

	// Validate that cacheDir is set
	if cacheDir == "" {
		log.Printf("ERROR: CacheDir is empty when creating Server - this will cause package caching to fail")
		// Try to get it from globalConfig as fallback
		if globalConfig != nil && globalConfig.CacheDir != "" {
			cacheDir = globalConfig.CacheDir
			log.Printf("Using CacheDir from GlobalConfig: %s", cacheDir)
		}
	}

	srv := &Server{
		CacheDir:              cacheDir,
		CertsDir:              certsDir,
		LogDir:                logDir,
		GlobalConfig:          globalConfig,
		ConfigFile:            configFile,
		StatsTracker:          statsTracker,
		KnownRegistries:       make(config.RegistryMap),
		PackageManagerProxies: make(map[string]http.Handler),
		DockerRegistryProxies: make(map[string]http.Handler),
	}
	log.Printf("[NewServer] Server created with srv.CacheDir=%q", srv.CacheDir)
	return srv
}

// LoadConfig loads registry configuration and initializes package/Docker proxies
func (s *Server) LoadConfig(airgapProxy bool, airgapped bool) error {
	log.Printf("[LoadConfig] Starting LoadConfig with s.CacheDir=%q", s.CacheDir)
	// Log CacheDir to debug empty CacheDir issue
	if s.CacheDir == "" {
		log.Printf("ERROR: Server.CacheDir is empty in LoadConfig - this will cause package caching to fail")
		// Try to get from GlobalConfig as fallback
		if s.GlobalConfig != nil && s.GlobalConfig.CacheDir != "" {
			s.CacheDir = s.GlobalConfig.CacheDir
			log.Printf("Using CacheDir from GlobalConfig: %s", s.CacheDir)
		} else {
			log.Printf("ERROR: GlobalConfig is nil or GlobalConfig.CacheDir is empty - cannot recover")
		}
	} else {
		log.Printf("[LoadConfig] Server.CacheDir is set: %s", s.CacheDir)
	}

	registries, err := config.LoadConfig(s.ConfigFile)
	if err != nil {
		log.Printf("Warning: Failed to load config: %v", err)
		log.Printf("  Using default configuration")
		// Continue with defaults instead of failing
	} else {
		s.KnownRegistries = registries
	}

	// Initialize the shared image override store
	s.OverrideStore = registry.NewOverrideStore(s.CacheDir)

	// Initialize package manager proxies
	s.initializePackageManagerProxies(airgapProxy, airgapped)

	// Initialize Docker registry proxies
	s.initializeDockerRegistryProxies(airgapProxy, airgapped)

	return nil
}

// initializeDockerRegistryProxies creates Docker registry proxies from config
func (s *Server) initializeDockerRegistryProxies(airgapProxy bool, airgapped bool) {
	s.DockerRegistryProxies = make(map[string]http.Handler)

	// Load Docker registry configs from YAML
	data, err := os.ReadFile(s.ConfigFile)
	if err != nil {
		log.Printf("Warning: Failed to read config file: %v", err)
		log.Printf("  Using default Docker registry configuration")
		s.initializeDefaultDockerRegistries(airgapProxy, airgapped)
		return
	}

	var fullConfig config.Config
	if err := yaml.Unmarshal(data, &fullConfig); err != nil {
		log.Printf("Warning: Failed to parse config file: %v", err)
		log.Printf("  Using default Docker registry configuration")
		s.initializeDefaultDockerRegistries(airgapProxy, airgapped)
		return
	}

	// Process Docker registry configs
	if len(fullConfig.DockerRegistries) == 0 {
		log.Printf("No Docker registries configured, using defaults")
		s.initializeDefaultDockerRegistries(airgapProxy, airgapped)
		return
	}

	for _, dockerConfig := range fullConfig.DockerRegistries {
		// Allow empty upstream_url for local-only registries
		upstreamURL := dockerConfig.UpstreamURL
		if upstreamURL == "" || upstreamURL == "local-only" {
			upstreamURL = "" // Treat as local-only
		}
		localOnlyPaths := dockerConfig.LocalOnlyPaths
		if localOnlyPaths == nil {
			localOnlyPaths = []string{}
		}
		proxy := registry.NewDockerRegistryProxy(upstreamURL, s.StatsTracker, dockerConfig.Hostnames, airgapProxy, airgapped, dockerConfig.ProxyPushes, localOnlyPaths, s.CacheDir, s.SBOMManager, s.Debug)
		proxy.SetOverrideStore(s.OverrideStore)

		// Register proxy for all hostnames
		for _, hostname := range dockerConfig.Hostnames {
			s.DockerRegistryProxies[hostname] = proxy
			proxyPushesStatus := "local storage"
			if dockerConfig.ProxyPushes {
				proxyPushesStatus = "proxied to upstream"
			}
			registryType := "local-only"
			if upstreamURL != "" {
				registryType = upstreamURL
			}
			log.Printf("  Registered Docker registry proxy for %s -> %s (pushes: %s)", hostname, registryType, proxyPushesStatus)
		}
	}
}

// initializeDefaultDockerRegistries provides default Docker registry configuration
func (s *Server) initializeDefaultDockerRegistries(airgapProxy bool, airgapped bool) {
	defaultRegistries := map[string]string{
		"docker.io":            "https://registry-1.docker.io",
		"registry-1.docker.io": "https://registry-1.docker.io",
		"index.docker.io":      "https://registry-1.docker.io",
		"quay.io":              "https://quay.io",
		"ghcr.io":              "https://ghcr.io",
		"registry.k8s.io":      "https://registry.k8s.io",
		"nvcr.io":              "https://nvcr.io",
	}

	for hostname, upstreamURL := range defaultRegistries {
		// Default: store pushes locally (proxyPushes = false)
		proxy := registry.NewDockerRegistryProxy(upstreamURL, s.StatsTracker, []string{hostname}, airgapProxy, airgapped, false, []string{}, s.CacheDir, s.SBOMManager, s.Debug)
		proxy.SetOverrideStore(s.OverrideStore)
		s.DockerRegistryProxies[hostname] = proxy
		log.Printf("  Registered Docker registry proxy for %s -> %s (pushes: local storage)", hostname, upstreamURL)
	}

	// Create a default localhost proxy for direct pushes (helm charts, etc.)
	// This uses "localhost" as the upstream URL so manifests are stored under localhost/ namespace
	// localhost always stores locally (never proxies)
	// localhost aliases are configurable (config.local_hostnames) on top of
	// the built-in defaults, e.g. host.docker.internal for in-cluster access.
	localhostAliases := []string{"localhost", "127.0.0.1", "roxxy"}
	if s.GlobalConfig != nil {
		localhostAliases = append(localhostAliases, s.GlobalConfig.LocalHostnames...)
	}
	localhostProxy := registry.NewDockerRegistryProxy("localhost", s.StatsTracker, localhostAliases, airgapProxy, airgapped, false, []string{}, s.CacheDir, s.SBOMManager, s.Debug)
	localhostProxy.SetOverrideStore(s.OverrideStore)
	for _, alias := range localhostAliases {
		s.DockerRegistryProxies[alias] = localhostProxy
	}
	log.Printf("  Registered Docker registry proxy for %v -> localhost (for direct pushes)", localhostAliases)
}

// initializePackageManagerProxies creates package manager proxies from config
func (s *Server) initializePackageManagerProxies(airgapProxy bool, airgapped bool) {
	log.Printf("[initializePackageManagerProxies] Starting with s.CacheDir=%q", s.CacheDir)
	s.PackageManagerProxies = make(map[string]http.Handler)

	// Validate CacheDir is set
	if s.CacheDir == "" {
		log.Printf("ERROR: CacheDir is empty - cannot initialize package manager proxies")
		return
	}

	// Create index database for parsing package index files (shared global)
	if s.IndexDB == nil {
		s.IndexDB = index.NewIndexDB(filepath.Join(s.CacheDir, "index"))
	}

	// Load package manager configs from YAML
	data, err := os.ReadFile(s.ConfigFile)
	if err != nil {
		log.Printf("Warning: Failed to read config file: %v", err)
		log.Printf("  Using default package manager configuration")
		s.initializeDefaultPackageManagers(airgapProxy, airgapped)
		return
	}

	var fullConfig config.Config
	if err := yaml.Unmarshal(data, &fullConfig); err != nil {
		log.Printf("Warning: Failed to parse config file: %v", err)
		log.Printf("  Using default package manager configuration")
		s.initializeDefaultPackageManagers(airgapProxy, airgapped)
		return
	}

	// Process package manager configs
	if len(fullConfig.PackageManagers) == 0 {
		log.Printf("No package managers configured, using defaults")
		s.initializeDefaultPackageManagers(airgapProxy, airgapped)
		return
	}

	log.Printf("Loaded %d package manager(s) from config", len(fullConfig.PackageManagers))
	for _, pmConfig := range fullConfig.PackageManagers {
		pmType := packages.Type(pmConfig.Type)
		minCacheSize := int64(1024) // Default: cache files >= 1KB
		if airgapProxy {
			minCacheSize = 0 // Cache all files with no size limits
		}
		packageCacheDir := filepath.Join(s.CacheDir, "packages")
		if packageCacheDir == "packages" || s.CacheDir == "" {
			log.Printf("ERROR: CacheDir is empty for %s proxy (s.CacheDir=%q, packageCacheDir=%q) - using relative path which may fail", pmType, s.CacheDir, packageCacheDir)
		}
		log.Printf("Creating %s proxy with CacheDir: %s (from s.CacheDir: %s)", pmType, packageCacheDir, s.CacheDir)
		pmCfg := packages.Config{
			Type:         pmType,
			UpstreamURL:  pmConfig.UpstreamURL,
			CacheDir:     packageCacheDir,
			IndexTTL:     1 * time.Hour, // Cache indices for 1 hour
			MinCacheSize: minCacheSize,
			Airgapped:    airgapped, // Pass airgapped flag
		}

		// Wrap IndexDB with adapter to implement packages.IndexDBInterface
		indexDBAdapter := index.NewIndexDBInterfaceAdapter(s.IndexDB)

		var handler http.Handler
		switch pmType {
		case packages.TypeAPK:
			handler = packages.NewAPKHandler(pmCfg, s.StatsTracker, indexDBAdapter, s.SBOMManager, s.PackageEnricher)
		case packages.TypeAPT:
			handler = packages.NewAPTHandler(pmCfg, s.StatsTracker, indexDBAdapter, s.SBOMManager, s.PackageEnricher)
		case packages.TypeNPM:
			handler = packages.NewNPMHandler(pmCfg, s.StatsTracker, indexDBAdapter, s.SBOMManager, s.PackageEnricher)
		case packages.TypePyPI:
			handler = packages.NewPyPIHandler(pmCfg, s.StatsTracker, indexDBAdapter, s.SBOMManager, s.PackageEnricher)
		case packages.TypeGo:
			handler = packages.NewGoHandler(pmCfg, s.StatsTracker, indexDBAdapter, s.SBOMManager, s.PackageEnricher)
		default:
			log.Printf("  Unknown package manager type: %s", pmType)
			continue
		}

		// Register proxy for all hostnames
		for _, hostname := range pmConfig.Hostnames {
			s.PackageManagerProxies[hostname] = handler
			log.Printf("  Registered %s proxy for %s -> %s", pmType, hostname, pmConfig.UpstreamURL)
		}
	}
}

// initializeDefaultPackageManagers provides default package manager configuration
// Used as fallback if config file can't be loaded
func (s *Server) initializeDefaultPackageManagers(airgapProxy bool, airgapped bool) {
	// Validate CacheDir is set
	if s.CacheDir == "" {
		log.Printf("ERROR: CacheDir is empty - cannot initialize default package managers")
		return
	}

	// Create index database for parsing package index files (shared global)
	if s.IndexDB == nil {
		s.IndexDB = index.NewIndexDB(filepath.Join(s.CacheDir, "index"))
	}

	pmMappings := map[string]struct {
		pmType      packages.Type
		upstreamURL string
		hostnames   []string
	}{
		"apk": {
			pmType:      packages.TypeAPK,
			upstreamURL: "https://dl-cdn.alpinelinux.org",
			hostnames:   []string{"dl-cdn.alpinelinux.org", "dl-2.alpinelinux.org", "dl-3.alpinelinux.org", "dl-4.alpinelinux.org", "dl-5.alpinelinux.org", "alpinelinux.org"},
		},
		"apt-ubuntu": {
			pmType:      packages.TypeAPT,
			upstreamURL: "http://archive.ubuntu.com",
			hostnames:   []string{"archive.ubuntu.com", "security.ubuntu.com", "old-releases.ubuntu.com"},
		},
		"apt-ubuntu-ports": {
			pmType:      packages.TypeAPT,
			upstreamURL: "http://ports.ubuntu.com",
			hostnames:   []string{"ports.ubuntu.com"},
		},
		"apt-debian": {
			pmType:      packages.TypeAPT,
			upstreamURL: "http://deb.debian.org",
			hostnames:   []string{"deb.debian.org", "ftp.debian.org"},
		},
		"apt-debian-security": {
			pmType:      packages.TypeAPT,
			upstreamURL: "http://security.debian.org",
			hostnames:   []string{"security.debian.org"},
		},
		"npm": {
			pmType:      packages.TypeNPM,
			upstreamURL: "https://registry.npmjs.org",
			hostnames:   []string{"registry.npmjs.org"},
		},
		"pypi": {
			pmType:      packages.TypePyPI,
			upstreamURL: "https://pypi.org",
			hostnames:   []string{"pypi.org", "files.pythonhosted.org"},
		},
		"go": {
			pmType:      packages.TypeGo,
			upstreamURL: "https://proxy.golang.org",
			hostnames:   []string{"proxy.golang.org", "proxy.golang.com"},
		},
	}

	// Create proxies for each package manager
	for _, mapping := range pmMappings {
		minCacheSize := int64(1024) // Default: cache files >= 1KB
		if airgapProxy {
			minCacheSize = 0 // Cache all files with no size limits
		}
		packageCacheDir := filepath.Join(s.CacheDir, "packages")
		if packageCacheDir == "packages" || s.CacheDir == "" {
			log.Printf("ERROR: CacheDir is empty for %s proxy (s.CacheDir=%q, packageCacheDir=%q) - using relative path which may fail", mapping.pmType, s.CacheDir, packageCacheDir)
		}
		log.Printf("Creating %s proxy with CacheDir: %s (from s.CacheDir: %s)", mapping.pmType, packageCacheDir, s.CacheDir)
		pmCfg := packages.Config{
			Type:         mapping.pmType,
			UpstreamURL:  mapping.upstreamURL,
			CacheDir:     packageCacheDir,
			IndexTTL:     1 * time.Hour, // Cache indices for 1 hour
			MinCacheSize: minCacheSize,
			Airgapped:    airgapped, // Pass airgapped flag
		}

		// Wrap IndexDB with adapter to implement packages.IndexDBInterface
		indexDBAdapter := index.NewIndexDBInterfaceAdapter(s.IndexDB)

		var handler http.Handler
		switch mapping.pmType {
		case packages.TypeAPK:
			handler = packages.NewAPKHandler(pmCfg, s.StatsTracker, indexDBAdapter, s.SBOMManager, s.PackageEnricher)
		case packages.TypeAPT:
			handler = packages.NewAPTHandler(pmCfg, s.StatsTracker, indexDBAdapter, s.SBOMManager, s.PackageEnricher)
		case packages.TypeNPM:
			handler = packages.NewNPMHandler(pmCfg, s.StatsTracker, indexDBAdapter, s.SBOMManager, s.PackageEnricher)
		case packages.TypePyPI:
			handler = packages.NewPyPIHandler(pmCfg, s.StatsTracker, indexDBAdapter, s.SBOMManager, s.PackageEnricher)
		case packages.TypeGo:
			handler = packages.NewGoHandler(pmCfg, s.StatsTracker, indexDBAdapter, s.SBOMManager, s.PackageEnricher)
		default:
			log.Printf("  Unknown package manager type: %s", mapping.pmType)
			continue
		}

		// Register proxy for all hostnames
		for _, hostname := range mapping.hostnames {
			s.PackageManagerProxies[hostname] = handler
			log.Printf("  Registered %s proxy for %s -> %s", mapping.pmType, hostname, mapping.upstreamURL)
		}
	}
}
