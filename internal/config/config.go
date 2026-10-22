package config

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// GlobalConfig holds top-level configuration including paths
type GlobalConfig struct {
	CacheDir          string   `yaml:"cache_dir"`          // Cache directory (default: platform-specific)
	CertsDir          string   `yaml:"certs_dir"`          // Certificates directory (default: platform-specific)
	LogDir            string   `yaml:"log_dir"`            // Log directory (default: platform-specific)
	ListenAddr        string   `yaml:"listen_addr"`        // Listen address (default: :3128) - deprecated, use listen_addrs
	ListenAddrs       []string `yaml:"listen_addrs"`       // Listen addresses (e.g., [":3128", "127.0.0.1:3128", "192.168.1.100:3128"])
	UpstreamProxy     string   `yaml:"upstream_proxy"`     // Proxy URL for upstream requests (e.g., "http://proxy:3128"). If empty, uses HTTP_PROXY env var. If set to "none", bypasses proxy.
	ExcludedPlatforms []string `yaml:"excluded_platforms"` // Docker platforms to exclude when pulling multi-arch images (default: empty - all platforms are pulled)
	LocalHostnames    []string `yaml:"local_hostnames"`    // Additional hostnames that should be treated as localhost (e.g., ["roxxy.argocd.svc", "roxxy.argocd.svc.cluster.local"])
	Debug             bool     `yaml:"debug"`              // Enable debug logging (verbose progress logs)
}

// RegistryConfig holds configuration for a registry proxy
type RegistryConfig struct {
	PathPrefix        string   `yaml:"path_prefix"`        // Path prefix to strip before forwarding (unused, kept for compatibility)
	RepositoryPath    string   `yaml:"repository_path"`    // Path prefix to prepend (unused, kept for compatibility)
	RewriteFunction   string   `yaml:"rewrite_function"`   // Name of path rewrite function to apply (unused, kept for compatibility)
	Description       string   `yaml:"description"`        // Optional description
	CacheType         string   `yaml:"cache_type"`         // "local" for content-addressable filesystem cache (default: "local")
	MinCacheSize      int64    `yaml:"min_cache_size"`     // Minimum file size to cache (bytes)
	ExcludeExtensions []string `yaml:"exclude_extensions"` // File extensions to exclude from caching
}

// PackageManagerConfig holds configuration for a package manager proxy
type PackageManagerConfig struct {
	Type        string   `yaml:"type"`         // Package manager type: apk, apt, npm, pypi, go
	UpstreamURL string   `yaml:"upstream_url"` // Upstream URL for the package manager
	Hostnames   []string `yaml:"hostnames"`    // List of hostnames to route to this proxy
}

// DockerRegistryConfig holds configuration for a Docker registry proxy
type DockerRegistryConfig struct {
	UpstreamURL    string   `yaml:"upstream_url"`     // Upstream URL (e.g., "https://registry-1.docker.io")
	Hostnames      []string `yaml:"hostnames"`        // List of hostnames to route to this proxy
	ProxyPushes    bool     `yaml:"proxy_pushes"`     // If true, proxy push operations to upstream; if false (default), store locally
	LocalOnlyPaths []string `yaml:"local_only_paths"` // Repository paths that should only check local storage (e.g., ["delphos/", "myorg/"])
}

// Config holds the application configuration
type Config struct {
	Global           GlobalConfig           `yaml:"global"`
	Registries       map[string]interface{} `yaml:"registries"`
	PackageManagers  []PackageManagerConfig `yaml:"package_managers"`
	DockerRegistries []DockerRegistryConfig `yaml:"docker_registries"`
	Archive          ArchiveConfig          `yaml:"archive"`
	Index            IndexConfig            `yaml:"index"`
}

// ArchiveConfig holds the snapshot-archive ingestion settings (Stage 1).
// When URL is empty, archive ingestion is disabled and roxxy behaves
// exactly as it did before Stage 1.
type ArchiveConfig struct {
	URL                      string        `yaml:"url"`                        // snarchive base URL
	PubkeyFingerprint        string        `yaml:"pubkey_fingerprint"`         // required when URL is set
	RefreshInterval          time.Duration `yaml:"refresh_interval"`           // default 1h
	BlocklistRefreshInterval time.Duration `yaml:"blocklist_refresh_interval"` // default 5m
	TrackedPaths             []string      `yaml:"tracked_paths"`              // paths under /current/ to mirror
}

// IndexConfig holds quarantine-delay settings consulted by the serve
// path in Stage 2.
type IndexConfig struct {
	DelayDays map[string]int `yaml:"delay_days"` // per-format override, e.g. {"default":7,"apt_debian":4}
}

// GetRoxxyDir returns the platform-specific roxxy directory
func GetRoxxyDir() string {
	// If running in Docker container (use more reliable detection)
	if isDockerContainer() {
		return "/app"
	}

	// Warn if binary is in /app but we're not in Docker (user shouldn't install there)
	if execPath, err := os.Executable(); err == nil {
		if absPath, err := filepath.Abs(execPath); err == nil {
			if strings.HasPrefix(absPath, "/app/") {
				log.Printf("⚠️  Warning: Binary is located in /app/ (%s)", absPath)
				log.Printf("   This location is reserved for Docker containers.")
				log.Printf("   Please install roxxy to a location in your PATH (e.g., /usr/local/bin/roxxy)")
				log.Printf("   or ~/.local/bin/roxxy) to avoid conflicts with Docker container detection.")
				log.Printf("")
			}
		}
	}

	// getUserHomeDir handles SYSTEM user case on Windows
	home, err := getUserHomeDir()
	if err != nil {
		return "./"
	}

	switch runtime.GOOS {
	case "darwin": // macOS
		return filepath.Join(home, "Library", "Application Support", "roxxy")
	case "windows":
		// Default to LOCALAPPDATA (Local) for config directory
		// Note: FindConfigFile() will check both Roaming and Local and prefer the one with config.yaml
		// getUserLocalAppData handles SYSTEM user case
		if appData := getUserLocalAppData(); appData != "" {
			return filepath.Join(appData, "roxxy")
		}
		if appData := getUserAppData(); appData != "" {
			return filepath.Join(appData, "roxxy")
		}
		return filepath.Join(home, "AppData", "Local", "roxxy")
	default: // Linux and others
		return filepath.Join(home, ".roxxy")
	}
}

// FindConfigFile searches for config file in multiple locations
// Priority: 1. ./config.yaml, 2. ~/.roxxy.conf, 3. platform-specific config.yaml
// verbose controls whether to log which config file is being used.
func FindConfigFile(verbose bool) string {
	// Check ./config.yaml (current directory) - highest priority for development
	if _, err := os.Stat("./config.yaml"); err == nil {
		if verbose {
			log.Printf("Using config from ./config.yaml")
		}
		return "./config.yaml"
	}

	// Check ~/.roxxy.conf (cross-platform compatibility)
	// getUserHomeDir handles SYSTEM user case on Windows
	if home, err := getUserHomeDir(); err == nil {
		roxxyConf := filepath.Join(home, ".roxxy.conf")
		if _, err := os.Stat(roxxyConf); err == nil {
			if verbose {
				log.Printf("Using config from %s", roxxyConf)
			}
			return roxxyConf
		}
	}

	// Check platform-specific location
	// On Windows, check both Roaming (APPDATA) and Local (LOCALAPPDATA) directories
	// Prefer Roaming to match cache/log directories
	if runtime.GOOS == "windows" {
		// Check Roaming first (APPDATA)
		if appData := getUserAppData(); appData != "" {
			roamingConfig := filepath.Join(appData, "roxxy", "config.yaml")
			if _, err := os.Stat(roamingConfig); err == nil {
				if verbose {
					log.Printf("Using config from %s", roamingConfig)
				}
				return roamingConfig
			}
		}

		// Check Local (LOCALAPPDATA) as fallback
		if localAppData := getUserLocalAppData(); localAppData != "" {
			localConfig := filepath.Join(localAppData, "roxxy", "config.yaml")
			if _, err := os.Stat(localConfig); err == nil {
				if verbose {
					log.Printf("Using config from %s", localConfig)
				}
				return localConfig
			}
		}

		// Default to Roaming if neither exists (to match cache/log directories)
		if appData := getUserAppData(); appData != "" {
			roamingConfig := filepath.Join(appData, "roxxy", "config.yaml")
			if verbose {
				log.Printf("No config file found, will use defaults and create at %s", roamingConfig)
			}
			return roamingConfig
		}
	}

	// For non-Windows or fallback, use GetRoxxyDir()
	roxxyDir := GetRoxxyDir()
	configYaml := filepath.Join(roxxyDir, "config.yaml")
	if _, err := os.Stat(configYaml); err == nil {
		if verbose {
			log.Printf("Using config from %s", configYaml)
		}
		return configYaml
	}

	// Default to platform-specific location (will be created if needed)
	if verbose {
		log.Printf("No config file found, will use defaults and create at %s", configYaml)
	}
	return configYaml
}

// isDockerContainer checks if we're running in a Docker container
func isDockerContainer() bool {
	// Check for /.dockerenv (Docker creates this file) - most reliable
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	// Check for /app/cache directory (more specific than just /app)
	if _, err := os.Stat("/app/cache"); err == nil {
		return true
	}
	// Check if the binary is at /app/roxxy (common Docker container location)
	if execPath, err := os.Executable(); err == nil {
		if absPath, err := filepath.Abs(execPath); err == nil {
			if absPath == "/app/roxxy" || absPath == "/app/roxxy.exe" {
				return true
			}
		}
	}
	return false
}

// getUserAppData returns the user's APPDATA directory, even when running as SYSTEM
// On Windows, when running as a service (SYSTEM user), APPDATA points to SYSTEM's profile.
// This function checks if we're in SYSTEM's profile and uses USERPROFILE (set by NSSM) instead.
func getUserAppData() string {
	if runtime.GOOS != "windows" {
		return os.Getenv("APPDATA")
	}

	appData := os.Getenv("APPDATA")
	// Check if APPDATA points to SYSTEM profile (service running as SYSTEM)
	if strings.Contains(strings.ToLower(appData), "systemprofile") {
		// Use USERPROFILE (set by NSSM) to construct user's APPDATA
		if userProfile := os.Getenv("USERPROFILE"); userProfile != "" {
			return filepath.Join(userProfile, "AppData", "Roaming")
		}
		// Fallback: try LOCALAPPDATA (also set by NSSM)
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			// LOCALAPPDATA is Local, but we want Roaming, so go up one level
			if parent := filepath.Dir(localAppData); parent != "" {
				return filepath.Join(parent, "Roaming")
			}
		}
	}
	return appData
}

// getUserLocalAppData returns the user's LOCALAPPDATA directory, even when running as SYSTEM
func getUserLocalAppData() string {
	if runtime.GOOS != "windows" {
		return os.Getenv("LOCALAPPDATA")
	}

	localAppData := os.Getenv("LOCALAPPDATA")
	// Check if LOCALAPPDATA points to SYSTEM profile
	if strings.Contains(strings.ToLower(localAppData), "systemprofile") {
		// Use USERPROFILE (set by NSSM) to construct user's LOCALAPPDATA
		if userProfile := os.Getenv("USERPROFILE"); userProfile != "" {
			return filepath.Join(userProfile, "AppData", "Local")
		}
	}
	return localAppData
}

// getUserHomeDir returns the user's home directory, even when running as SYSTEM
// On Windows, when running as a service (SYSTEM user), os.UserHomeDir() returns SYSTEM's profile.
// This function checks if we're in SYSTEM's profile and uses USERPROFILE (set by NSSM) instead.
func getUserHomeDir() (string, error) {
	if runtime.GOOS != "windows" {
		return os.UserHomeDir()
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return home, err
	}

	// Check if home points to SYSTEM profile (service running as SYSTEM)
	if strings.Contains(strings.ToLower(home), "systemprofile") {
		// Use USERPROFILE (set by NSSM) instead
		if userProfile := os.Getenv("USERPROFILE"); userProfile != "" {
			return userProfile, nil
		}
		// Fallback: try to construct from APPDATA or LOCALAPPDATA
		if appData := getUserAppData(); appData != "" {
			// APPDATA is AppData\Roaming, so go up two levels to get home
			if parent := filepath.Dir(filepath.Dir(appData)); parent != "" {
				return parent, nil
			}
		}
	}
	return home, nil
}

// GetDefaultCacheDir returns the default cache directory based on platform
// This can be overridden in config.yaml with cache_dir setting
func GetDefaultCacheDir() string {
	// If running in Docker container
	if isDockerContainer() {
		return "/app/cache"
	}

	// Use platform-specific directories
	// getUserHomeDir handles SYSTEM user case on Windows
	home, err := getUserHomeDir()
	if err != nil {
		return "./cache"
	}

	switch runtime.GOOS {
	case "darwin": // macOS
		// ~/Library/Application Support/roxxy/Cache
		return filepath.Join(home, "Library", "Application Support", "roxxy", "Cache")
	case "windows":
		// Use APPDATA (Roaming) by default, LOCALAPPDATA as fallback
		// getUserAppData handles SYSTEM user case
		if appData := getUserAppData(); appData != "" {
			return filepath.Join(appData, "roxxy", "Cache")
		}
		if appData := getUserLocalAppData(); appData != "" {
			return filepath.Join(appData, "roxxy", "Cache")
		}
		return filepath.Join(home, "AppData", "Roaming", "roxxy", "Cache")
	default: // Linux and others
		// Check XDG_CACHE_HOME first (follows XDG Base Directory spec)
		if xdgCache := os.Getenv("XDG_CACHE_HOME"); xdgCache != "" {
			return filepath.Join(xdgCache, "roxxy")
		}
		// Fall back to ~/.cache/roxxy (standard Linux cache location)
		return filepath.Join(home, ".cache", "roxxy")
	}
}

// GetDefaultCertsDir returns the default certificates directory based on platform
func GetDefaultCertsDir() string {
	// If running in Docker container
	if isDockerContainer() {
		return "/app/certs"
	}

	// Use platform-specific roxxy directory
	roxxyDir := GetRoxxyDir()
	return filepath.Join(roxxyDir, "certs")
}

// GetDefaultLogDir returns the default log directory based on platform
func GetDefaultLogDir() string {
	// If running in Docker container
	if isDockerContainer() {
		return "/app/logs"
	}

	// getUserHomeDir handles SYSTEM user case on Windows
	home, err := getUserHomeDir()
	if err != nil {
		return "./logs"
	}

	switch runtime.GOOS {
	case "darwin": // macOS
		// ~/Library/Logs/roxxy - standard macOS log location
		return filepath.Join(home, "Library", "Logs", "roxxy")
	case "windows":
		// Use APPDATA (Roaming) by default, LOCALAPPDATA as fallback
		// getUserAppData handles SYSTEM user case
		if appData := getUserAppData(); appData != "" {
			return filepath.Join(appData, "roxxy", "Logs")
		}
		if appData := getUserLocalAppData(); appData != "" {
			return filepath.Join(appData, "roxxy", "Logs")
		}
		return filepath.Join(home, "AppData", "Roaming", "roxxy", "Logs")
	default: // Linux and others
		// Check XDG_STATE_HOME first (follows XDG Base Directory spec)
		if xdgState := os.Getenv("XDG_STATE_HOME"); xdgState != "" {
			return filepath.Join(xdgState, "roxxy", "logs")
		}
		// Fall back to ~/.local/state/roxxy/logs (standard Linux log location)
		return filepath.Join(home, ".local", "state", "roxxy", "logs")
	}
}

// expandTilde expands ~ to the user's home directory
func expandTilde(path string) string {
	if len(path) == 0 || path[0] != '~' {
		return path
	}
	// getUserHomeDir handles SYSTEM user case on Windows
	if home, err := getUserHomeDir(); err == nil {
		return filepath.Join(home, path[1:])
	}
	return path
}

// LoadGlobalConfig loads the global configuration (cache_dir, certs_dir, etc.)
// LoadArchiveAndIndex returns the Archive and Index sections of the
// config (Stage 1+ wiring). Empty struct values when the file or sections
// are absent.
func LoadArchiveAndIndex(configPath string) (ArchiveConfig, IndexConfig, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ArchiveConfig{}, IndexConfig{}, nil
		}
		return ArchiveConfig{}, IndexConfig{}, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return ArchiveConfig{}, IndexConfig{}, fmt.Errorf("parse %s: %w", configPath, err)
	}
	return c.Archive, c.Index, nil
}

func LoadGlobalConfig(configPath string) (*GlobalConfig, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		// Return defaults if file doesn't exist
		return &GlobalConfig{
			CacheDir:          GetDefaultCacheDir(),
			CertsDir:          GetDefaultCertsDir(),
			LogDir:            GetDefaultLogDir(),
			ListenAddr:        ":3128",
			ExcludedPlatforms: []string{}, // Empty by default - all platforms are pulled
		}, nil
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Apply defaults for empty values
	if config.Global.CacheDir == "" {
		config.Global.CacheDir = GetDefaultCacheDir()
	} else {
		config.Global.CacheDir = expandTilde(config.Global.CacheDir)
	}
	if config.Global.CertsDir == "" {
		config.Global.CertsDir = GetDefaultCertsDir()
	} else {
		config.Global.CertsDir = expandTilde(config.Global.CertsDir)
	}
	if config.Global.LogDir == "" {
		config.Global.LogDir = GetDefaultLogDir()
	} else {
		config.Global.LogDir = expandTilde(config.Global.LogDir)
	}
	if config.Global.ListenAddr == "" {
		config.Global.ListenAddr = ":3128"
	}

	// If upstream_proxy is not set in config, check HTTP_PROXY environment variable
	// If it's set to "none", explicitly bypass proxy
	if config.Global.UpstreamProxy == "" {
		if envProxy := os.Getenv("HTTP_PROXY"); envProxy != "" {
			config.Global.UpstreamProxy = envProxy
		} else if envProxy := os.Getenv("http_proxy"); envProxy != "" {
			config.Global.UpstreamProxy = envProxy
		}
	}

	return &config.Global, nil
}

// RegistryMap maps hostnames to their registry configurations
type RegistryMap map[string][]RegistryConfig

// LoadConfig loads registry configuration from config.yaml and returns a RegistryMap
func LoadConfig(configPath string) (RegistryMap, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Convert interface{} values to []RegistryConfig
	knownRegistries := make(RegistryMap)
	for hostname, value := range config.Registries {
		switch v := value.(type) {
		case map[string]interface{}:
			// Single config - convert to slice
			var rc RegistryConfig
			yamlBytes, _ := yaml.Marshal(v)
			if err := yaml.Unmarshal(yamlBytes, &rc); err != nil {
				return nil, fmt.Errorf("failed to parse config for %s: %w", hostname, err)
			}
			knownRegistries[hostname] = []RegistryConfig{rc}
		case []interface{}:
			// List of configs
			configs := make([]RegistryConfig, 0, len(v))
			for _, item := range v {
				var rc RegistryConfig
				yamlBytes, _ := yaml.Marshal(item)
				if err := yaml.Unmarshal(yamlBytes, &rc); err != nil {
					return nil, fmt.Errorf("failed to parse config for %s: %w", hostname, err)
				}
				configs = append(configs, rc)
			}
			knownRegistries[hostname] = configs
		}
	}

	totalConfigs := 0
	for _, configs := range knownRegistries {
		totalConfigs += len(configs)
	}
	log.Printf("Loaded %d registries from config", totalConfigs)

	return knownRegistries, nil
}

// Global config instance (set by main package)
var globalConfigInstance *GlobalConfig

// SetGlobalConfig sets the global configuration instance (called from main package)
func SetGlobalConfig(cfg *GlobalConfig) {
	globalConfigInstance = cfg
}

// GetGlobalConfig returns the global configuration instance
func GetGlobalConfig() *GlobalConfig {
	return globalConfigInstance
}

// GetUpstreamProxyFunc returns a proxy function for HTTP transports.
// It respects the upstream_proxy config setting or HTTP_PROXY environment variable,
// but prevents proxy loops by checking if the proxy URL points to roxxy's own listen address.
func GetUpstreamProxyFunc() func(*http.Request) (*url.URL, error) {
	// Get proxy URL from config or environment
	proxyURL := ""
	if globalConfigInstance != nil && globalConfigInstance.UpstreamProxy != "" {
		if globalConfigInstance.UpstreamProxy == "none" {
			// Explicitly bypass proxy
			return nil
		}
		proxyURL = globalConfigInstance.UpstreamProxy
	} else {
		// Fall back to environment variable
		if envProxy := os.Getenv("HTTP_PROXY"); envProxy != "" {
			proxyURL = envProxy
		} else if envProxy := os.Getenv("http_proxy"); envProxy != "" {
			proxyURL = envProxy
		}
	}

	if proxyURL == "" {
		return nil
	}

	// Parse proxy URL
	parsedURL, err := url.Parse(proxyURL)
	if err != nil {
		log.Printf("Warning: Invalid upstream proxy URL %s: %v, bypassing proxy", proxyURL, err)
		return nil
	}

	// Get roxxy's listen addresses to prevent loops
	var listenAddrs []string
	if globalConfigInstance != nil && len(globalConfigInstance.ListenAddrs) > 0 {
		listenAddrs = globalConfigInstance.ListenAddrs
	} else if globalConfigInstance != nil && globalConfigInstance.ListenAddr != "" {
		// Fallback to old listen_addr for backward compatibility
		listenAddrs = []string{globalConfigInstance.ListenAddr}
	} else {
		listenAddrs = []string{":3128"}
	}

	// Extract host:port from proxy URL
	proxyHost := parsedURL.Hostname()
	proxyPort := parsedURL.Port()
	if proxyPort == "" {
		// Default ports
		switch parsedURL.Scheme {
		case "http":
			proxyPort = "80"
		case "https":
			proxyPort = "443"
		default:
			// Other schemes don't have default ports
		}
	}

	// Normalize hostnames (localhost, 127.0.0.1, etc.)
	normalizeHost := func(host string) string {
		host = strings.ToLower(host)
		if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "" {
			return "localhost"
		}
		return host
	}

	// Check if proxy URL matches any of our listen addresses
	for _, listenAddr := range listenAddrs {
		// Extract host:port from listen address
		listenHost := "localhost"
		listenPort := "3128"
		if strings.HasPrefix(listenAddr, ":") {
			listenPort = strings.TrimPrefix(listenAddr, ":")
		} else {
			parts := strings.Split(listenAddr, ":")
			if len(parts) == 2 {
				listenHost = parts[0]
				listenPort = parts[1]
			}
		}

		// Check if proxy URL points to roxxy's own listen address (prevent loop)
		if normalizeHost(proxyHost) == normalizeHost(listenHost) && proxyPort == listenPort {
			log.Printf("Warning: Upstream proxy %s points to roxxy's own listen address %s, bypassing to prevent loop", proxyURL, listenAddr)
			return nil
		}
	}

	// Return proxy function
	log.Printf("Using upstream proxy: %s", proxyURL)
	return http.ProxyURL(parsedURL)
}
