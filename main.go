package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"context"

	"github.com/Delphos-Labs/roxxy/internal/api"
	"github.com/Delphos-Labs/roxxy/internal/app"
	"github.com/Delphos-Labs/roxxy/internal/archive"
	"github.com/Delphos-Labs/roxxy/internal/commands"
	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/packages"
	"github.com/Delphos-Labs/roxxy/internal/proxy"
	"github.com/Delphos-Labs/roxxy/internal/registry"
	"github.com/Delphos-Labs/roxxy/internal/sbom"
	tlslib "github.com/Delphos-Labs/roxxy/internal/tls"
	"github.com/Delphos-Labs/roxxy/internal/ui"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// startArchive constructs and starts the snapshot-archive ingest loop if
// archive.url is set in the config. Returns nil (no error) when archive
// ingestion is disabled.
func startArchive(ctx context.Context, configPath, cacheDir string) (*archive.Archive, error) {
	archCfg, _, err := config.LoadArchiveAndIndex(configPath)
	if err != nil {
		return nil, err
	}
	if archCfg.URL == "" {
		return nil, nil
	}
	a, err := archive.New(archive.Config{
		URL:                      archCfg.URL,
		PubkeyFingerprint:        archCfg.PubkeyFingerprint,
		MirrorDir:                filepath.Join(cacheDir, "archive"),
		RefreshInterval:          archCfg.RefreshInterval,
		BlocklistRefreshInterval: archCfg.BlocklistRefreshInterval,
		TrackedPaths:             archCfg.TrackedPaths,
	})
	if err != nil {
		return nil, err
	}
	go func() {
		if err := a.Run(ctx); err != nil {
			log.Printf("archive: refresh loop exited: %v", err)
		}
	}()
	return a, nil
}

// BuildTime is set at build time using -ldflags
// Example: go build -ldflags "-X main.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
// If not set, will try to detect from git or file modification time
var BuildTime = "unknown"

func init() {
	// If BuildTime wasn't set via ldflags, try to detect it
	if BuildTime == "unknown" {
		// Try git first (most accurate)
		if gitTime := getGitCommitTime(); gitTime != "" {
			BuildTime = gitTime
			return
		}
		// Fall back to binary modification time
		if binTime := getBinaryModTime(); binTime != "" {
			BuildTime = binTime
			return
		}
	}
}

func getGitCommitTime() string {
	cmd := exec.Command("git", "log", "-1", "--format=%cI", "--", "main.go")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	timeStr := strings.TrimSpace(string(output))
	if timeStr != "" {
		// Convert to UTC and format
		if t, err := time.Parse(time.RFC3339, timeStr); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05Z")
		}
	}
	return ""
}

func getBinaryModTime() string {
	// Get the binary's own modification time
	binaryPath, err := os.Executable()
	if err != nil {
		return ""
	}
	info, err := os.Stat(binaryPath)
	if err != nil {
		return ""
	}
	return info.ModTime().UTC().Format("2006-01-02T15:04:05Z")
}

// ProxyServer is now in internal/proxy package
type ProxyServer = proxy.Server

// generateAndInstallCACert generates roxxy's CA certificate if it doesn't exist,
// and adds it to the system trust store (for container environments)
func generateAndInstallCACert(certsDir string) {
	// Convert to absolute path
	absCertsDir, err := filepath.Abs(certsDir)
	if err != nil {
		log.Fatalf("Error resolving absolute path for certs directory %s: %v", certsDir, err)
	}
	log.Printf("generateAndInstallCACert: certsDir = %s (absolute: %s)", certsDir, absCertsDir)

	certPath := filepath.Join(absCertsDir, "ca-cert.pem")
	keyPath := filepath.Join(absCertsDir, "ca-key.pem")
	log.Printf("generateAndInstallCACert: certPath = %s, keyPath = %s", certPath, keyPath)

	// Check if cert already exists
	if _, err := os.Stat(certPath); err == nil {
		log.Printf("CA certificate already exists: %s", certPath)
	} else {
		// Generate certificate
		log.Printf("Generating CA certificate in directory: %s", absCertsDir)

		// Ensure directory exists
		if err := os.MkdirAll(absCertsDir, 0755); err != nil {
			log.Fatalf("Error creating certs directory %s: %v", absCertsDir, err)
		}
		log.Printf("✓ Created certs directory: %s", absCertsDir)

		// Generate CA certificate and key using Go's crypto libraries
		certPEM, keyPEM, err := tlslib.GenerateCACertificate()
		if err != nil {
			log.Fatalf("Error generating CA certificate: %v", err)
		}

		// Write certificate file
		if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
			log.Fatalf("Error writing certificate file: %v", err)
		}
		log.Printf("✓ Generated certificate: %s", certPath)

		// Write key file
		if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
			log.Fatalf("Error writing key file: %v", err)
		}
		log.Printf("✓ Generated private key: %s", keyPath)

		// Verify the file was actually created
		if stat, err := os.Stat(certPath); err != nil {
			log.Fatalf("Error: Certificate file was not created at %s: %v", certPath, err)
		} else {
			log.Printf("✓ Verified certificate file exists: %s (size: %d bytes)", certPath, stat.Size())
		}
	}

	// Add to system trust store (for container environments)
	// This allows roxxy's own processes and other tools in the container to trust roxxy's MITM cert
	log.Printf("Adding CA certificate to system trust store...")

	// Try Alpine/Debian location first
	if _, err := os.Stat("/usr/local/share/ca-certificates"); err == nil {
		destPath := "/usr/local/share/ca-certificates/roxxy-ca.crt"
		if err := exec.Command("cp", certPath, destPath).Run(); err != nil {
			log.Printf("Warning: Failed to copy certificate to %s: %v", destPath, err)
		} else {
			// Update certificate store
			if err := exec.Command("update-ca-certificates").Run(); err != nil {
				log.Printf("Warning: Failed to update certificates: %v", err)
			}
			// Also append to bundle file (required for some tools like Python)
			bundlePath := "/etc/ssl/certs/ca-certificates.crt"
			if bundleFile, err := os.OpenFile(bundlePath, os.O_APPEND|os.O_WRONLY, 0644); err == nil {
				if certData, err := os.ReadFile(certPath); err == nil {
					if _, err := bundleFile.Write(certData); err != nil {
						log.Printf("Error writing to bundle: %v", err)
					}
					util.Logerr(bundleFile.Close())
					log.Printf("✓ Added to system trust store: %s", bundlePath)
				}
			}
		}
	} else if _, err := os.Stat("/etc/pki/ca-trust/source/anchors"); err == nil {
		// Try RHEL/CentOS/Fedora location
		destPath := "/etc/pki/ca-trust/source/anchors/roxxy-ca.crt"
		if err := exec.Command("cp", certPath, destPath).Run(); err != nil {
			log.Printf("Warning: Failed to copy certificate to %s: %v", destPath, err)
		} else {
			if err := exec.Command("update-ca-trust").Run(); err != nil {
				log.Printf("Warning: Failed to update trust: %v", err)
			} else {
				log.Printf("✓ Added to system trust store: %s", destPath)
			}
		}
	} else {
		// Fallback: just append to bundle file directly
		bundlePath := "/etc/ssl/certs/ca-certificates.crt"
		if bundleFile, err := os.OpenFile(bundlePath, os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			if certData, err := os.ReadFile(certPath); err == nil {
				if _, err := bundleFile.Write(certData); err != nil {
					log.Printf("Error writing to bundle: %v", err)
				}
				util.Logerr(bundleFile.Close())
				log.Printf("✓ Added to certificate bundle: %s", bundlePath)
			} else {
				log.Printf("Warning: Failed to read certificate: %v", err)
			}
		} else {
			log.Printf("Warning: Could not update certificate bundle: %v", err)
		}
	}
}

func main() {
	// Check for --buildtime flag first (before initializing anything)
	if len(os.Args) > 1 && os.Args[1] == "--buildtime" {
		fmt.Printf("Build time: %s\n", BuildTime)
		os.Exit(0)
	}

	// Handle service commands early (before any initialization)
	// We need minimal initialization for service commands
	if len(os.Args) > 1 {
		cmd := os.Args[1]
		args := os.Args[2:]
		if cmd == "service" || cmd == "install-service" {
			// Minimal config for service commands
			cmdConfig := commands.Config{
				CacheDir:     "",  // Not needed for service commands
				CertsDir:     "",  // Not needed for service commands
				LogDir:       "",  // Not needed for service commands
				ConfigFile:   "",  // Not needed for service commands
				StatsTracker: nil, // Not needed for service commands
				IndexDB:      nil, // Not needed for service commands
				BuildTime:    BuildTime,
				GlobalConfig: nil, // Not needed for service commands
			}
			commands.HandleService(cmdConfig, args)
			os.Exit(0) // Explicit exit after service command
		}
	}

	// Unset HTTP_PROXY and HTTPS_PROXY for roxxy itself - roxxy should never use a proxy
	// for its own upstream fetches (it IS the proxy). This prevents proxy loops.
	// Other processes that want to use roxxy should set HTTP_PROXY themselves.
	util.Logerr(os.Unsetenv("HTTP_PROXY"))
	util.Logerr(os.Unsetenv("HTTPS_PROXY"))
	util.Logerr(os.Unsetenv("http_proxy"))
	util.Logerr(os.Unsetenv("https_proxy"))

	// Detect if we're running a CLI command (not server)
	// CLI commands should suppress verbose initialization logs
	isCLICommand := false
	if len(os.Args) > 1 {
		cmd := os.Args[1]
		cliCommands := []string{"raw", "package", "docker", "embed", "import", "push", "deploy", "hardlink", "link", "logs", "certs", "certificates", "config", "info", "cache"}
		for _, cliCmd := range cliCommands {
			if cmd == cliCmd {
				isCLICommand = true
				break
			}
		}
		// Also check binary name for hardlink commands
		binaryName := filepath.Base(os.Args[0])
		if binaryName == "docker" || binaryName == "cache" || binaryName == "raw" || binaryName == "package" || binaryName == "embed" {
			isCLICommand = true
		}
	}

	// Create and initialize application (replaces all global variable initialization)
	// Pass verbose=false for CLI commands to suppress initialization logs
	// Log to stderr first so NSSM can capture it
	if !isCLICommand {
		fmt.Fprintf(os.Stderr, "[roxxy] Initializing application...\n")
	}
	appInstance, err := app.NewApplication(BuildTime, !isCLICommand)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[roxxy] ERROR: Failed to initialize application: %v\n", err)
		log.Fatalf("Failed to initialize application: %v", err)
	}
	if !isCLICommand {
		fmt.Fprintf(os.Stderr, "[roxxy] Application initialized successfully\n")
		fmt.Fprintf(os.Stderr, "[roxxy] Config file: %s\n", appInstance.ConfigFile)
		fmt.Fprintf(os.Stderr, "[roxxy] Log directory: %s\n", appInstance.LogDir)
	}

	// Get command config from application
	cmdConfig := appInstance.GetCommandConfig()

	// Check if binary was called as 'docker' or 'cache' (via hardlink)
	// This allows hardlinks in scratch containers to work as separate commands
	binaryName := filepath.Base(os.Args[0])

	// Server flag declarations live here so we can parse them before the
	// subcommand switch — this lets `--airgap-proxy server` work — and re-parse
	// after `server` is consumed, so `server --airgap-proxy` works too.
	var airgapProxy bool
	var airgapped bool
	var listenAddr string
	var generateCerts bool
	var debug bool
	flag.BoolVar(&airgapProxy, "airgap-proxy", false, "Cache all files with no size limits (for airgap preparation)")
	flag.BoolVar(&airgapped, "airgapped", false, "Airgapped mode: no external access, return 404 for missing artifacts")
	flag.StringVar(&listenAddr, "listen", "", "Listen address (e.g., :3128 or :8080). Overrides LISTEN_ADDR environment variable.")
	flag.BoolVar(&generateCerts, "generate", false, "Generate CA certificate if missing and add to system trust store, then start server")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging (verbose progress logs). Overrides config file setting.")

	// First pass: consume any leading global flags. flag.Parse stops at the
	// first positional, which is the subcommand (or empty for an implicit server).
	// flag.ExitOnError mode never returns a non-nil error.
	_ = flag.CommandLine.Parse(os.Args[1:])
	positionals := flag.Args()

	// Command routing
	if len(positionals) > 0 {
		cmd := positionals[0]
		args := positionals[1:]

		switch cmd {
		case "raw":
			commands.HandleCache(cmdConfig, args)
			return
		case "package":
			commands.HandlePackage(cmdConfig, args)
			return
		case "docker":
			commands.HandleDocker(cmdConfig, args)
			return
		case "embed":
			commands.HandleEmbed(cmdConfig, args)
			return
		case "import":
			commands.HandleImport(cmdConfig, args)
			return
		case "push":
			commands.HandlePush(cmdConfig, args)
			return
		case "deploy":
			commands.HandleDeploy(cmdConfig, args)
			return
		case "hardlink", "link":
			commands.HandleHardlink(cmdConfig)
			return
		case "logs":
			commands.HandleLogs(cmdConfig, args)
			return
		case "certs", "certificates":
			commands.HandleCerts(cmdConfig, args)
			return
		case "config":
			commands.HandleConfig(cmdConfig, args)
			return
		case "info":
			commands.HandleInfo(cmdConfig)
			return
		case "server":
			// Second pass: parse any flags that appeared after "server".
			// flag.CommandLine retains values from the first pass, so leading
			// and trailing flag positions both work.
			// flag.ExitOnError mode never returns a non-nil error.
			_ = flag.CommandLine.Parse(args)
			// Fall through to start server
		case "help", "--help", "-h":
			commands.ShowUsage()
			os.Exit(0)
		default:
			// Unknown command - exit with error
			fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
			os.Exit(1)
		}
	} else {
		// No arguments - check binary name for hardlink behavior
		switch binaryName {
		case "docker":
			// Called as 'docker' with no args - show docker help
			fmt.Println("Usage: docker <command>")
			fmt.Println("Commands:")
			fmt.Println("  ls              List Docker images")
			fmt.Println("  rmi <image>     Remove Docker image from cache")
			fmt.Println("  prune           Remove unreferenced blobs")
			os.Exit(1)
		case "cache", "raw":
			// Called as 'cache' or 'raw' with no args - show cache help
			fmt.Println("Usage: cache <command>")
			fmt.Println("Commands:")
			fmt.Println("  ls              List cached content")
			fmt.Println("  flush           Flush all cache")
			os.Exit(1)
		case "package":
			// Called as 'package' with no args - show package help
			fmt.Println("Usage: package <command>")
			fmt.Println("Commands:")
			fmt.Println("  ls              List cached packages")
			fmt.Println("  parse [--type]  Parse index files for specified type (or all types)")
			os.Exit(1)
		case "embed":
			// Called as 'embed' with no args - show embed help
			fmt.Println("Usage: embed ls [path]")
			os.Exit(1)
		}
	}

	// If binary was called as 'docker', 'cache', or 'raw', route directly to that handler
	// This allows: docker ls → HandleDocker(["ls"])
	if binaryName == "docker" && len(os.Args) > 1 {
		commands.HandleDocker(cmdConfig, os.Args[1:])
		return
	}
	if binaryName == "cache" && len(os.Args) > 1 {
		commands.HandleCache(cmdConfig, os.Args[1:])
		return
	}
	if binaryName == "raw" && len(os.Args) > 1 {
		commands.HandleCache(cmdConfig, os.Args[1:])
		return
	}
	if binaryName == "package" && len(os.Args) > 1 {
		commands.HandlePackage(cmdConfig, os.Args[1:])
		return
	}
	if binaryName == "embed" && len(os.Args) > 1 {
		commands.HandleEmbed(cmdConfig, os.Args[1:])
		return
	}

	// No command or "server" - start the proxy server

	// Log startup immediately to stderr (NSSM will capture this)
	// This helps diagnose if the service is starting at all
	fmt.Fprintf(os.Stderr, "[roxxy] Starting server...\n")
	fmt.Fprintf(os.Stderr, "[roxxy] Build time: %s\n", BuildTime)

	// Server flags were already declared and parsed before subcommand routing.
	// Merge debug flag with config: --debug flag overrides config file, or use config file value
	debugEnabled := debug || appInstance.GlobalConfig.Debug

	// Generate CA cert if missing (or if --generate flag is set to force regeneration)
	certPath := filepath.Join(appInstance.CertsDir, "ca-cert.pem")
	if generateCerts {
		// Force generation: remove existing cert if it exists
		if _, err := os.Stat(certPath); err == nil {
			log.Printf("--generate flag set, removing existing certificate...")
			util.Logerr(os.Remove(certPath))
			util.Logerr(os.Remove(filepath.Join(appInstance.CertsDir, "ca-key.pem")))
		}
		generateAndInstallCACert(appInstance.CertsDir)
	} else if _, err := os.Stat(certPath); os.IsNotExist(err) {
		// Auto-generate if missing
		log.Printf("CA certificate not found, generating...")
		generateAndInstallCACert(appInstance.CertsDir)
	}

	// Create SBOM manager BEFORE creating server (so it can be passed to handlers)
	sbomManager := sbom.NewSessionManager()

	// Create server instance and load configuration
	srv, err := appInstance.NewServer(airgapProxy, airgapped, debugEnabled, sbomManager)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	// Set up log file (only when running as server, not in Docker)
	if _, err := os.Stat("/app"); os.IsNotExist(err) {
		// Not in Docker - use log file
		// Ensure log directory exists (should already exist from NewApplication, but double-check)
		if err := os.MkdirAll(appInstance.LogDir, 0755); err != nil {
			log.Printf("Warning: Failed to create log directory %s: %v (using stderr only)", appInstance.LogDir, err)
		} else {
			logFile := filepath.Join(appInstance.LogDir, "roxxy.log")
			// Use O_RDWR instead of O_WRONLY to ensure file sharing on Windows
			// This allows concurrent reads while the service writes to the log file
			logFileHandle, err := os.OpenFile(logFile, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
			if err != nil {
				log.Printf("Warning: Failed to open log file %s: %v (using stderr only)", logFile, err)
			} else {
				// Write to both log file and stderr so service logs are visible
				multiWriter := io.MultiWriter(logFileHandle, os.Stderr)
				log.SetOutput(multiWriter)
				log.Printf("Logging to: %s", logFile)
			}
		}
	}

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Stage 1 archive ingestion: optional snapshot-archive client + refresh
	// loop. No serve-path integration yet — Stage 2 wires it in.
	archiveCtx, archiveCancel := context.WithCancel(context.Background())
	defer archiveCancel()
	if arch, err := startArchive(archiveCtx, appInstance.ConfigFile, appInstance.CacheDir); err != nil {
		log.Printf("Warning: archive ingestion disabled: %v", err)
	} else if arch != nil {
		log.Printf("Archive ingestion enabled (mirror under %s)", filepath.Join(appInstance.CacheDir, "archive"))
		_ = arch
	}

	// Determine listen addresses
	var listenAddrs []string
	if listenAddr != "" {
		// --listen flag overrides config
		listenAddrs = []string{listenAddr}
	} else if len(appInstance.GlobalConfig.ListenAddrs) > 0 {
		// Use configured listen addresses
		listenAddrs = appInstance.GlobalConfig.ListenAddrs
	} else {
		// Fallback to LISTEN_ADDR env var or default
		envAddr := commands.GetEnv("LISTEN_ADDR", ":3128")
		listenAddrs = []string{envAddr}
	}

	// Initialize registry proxy
	certManager := tlslib.NewCertificateManager(filepath.Join(appInstance.CertsDir, "ca-cert.pem"), filepath.Join(appInstance.CertsDir, "ca-key.pem"))
	registryProxy := registry.NewProxy(certManager, appInstance.StatsTracker, appInstance.CacheDir, srv.KnownRegistries)

	// SBOM manager was already created and set on server during NewServer

	// Create package enricher v2 for background license/package info extraction
	// CPU threshold: only process when CPU usage is below 50%
	enricherV2 := packages.NewPackageEnricherV2(appInstance.CacheDir, appInstance.IndexDBV2, 0.5)
	enricherV2.Start()
	srv.PackageEnricher = enricherV2 // PackageEnricherInterface is compatible

	// Note: v2 enricher doesn't have ScanAndEnqueue yet, packages are enqueued on cache

	// Create UI server (pass IndexDBV2 for v2 API)
	uiServer := ui.NewServer(appInstance.CacheDir, appInstance.StatsTracker, appInstance.IndexDBV2)
	uiServer.SetCertsDir(appInstance.CertsDir)
	uiServer.SetSBOMManager(sbomManager)
	uiServer.SetOverrideStore(srv.OverrideStore)
	// Set build time in API so it can be exposed via endpoint
	api.SetBuildTime(appInstance.BuildTime)
	uiMux := http.NewServeMux()
	uiServer.RegisterRoutes(uiMux)

	// Create proxy server
	nexusURL, _ := url.Parse("http://localhost:5002")
	proxyServer := proxy.NewServer(proxy.Config{
		ListenAddrs:     listenAddrs,
		NexusURL:        nexusURL,
		CertManager:     certManager,
		RegistryProxy:   registryProxy,
		PackageProxies:  srv.PackageManagerProxies,
		DockerProxies:   srv.DockerRegistryProxies,
		KnownRegistries: srv.KnownRegistries,
		UIServer:        uiMux,
	})

	// Start proxy in a goroutine
	go func() {
		if err := proxyServer.Start(); err != nil {
			log.Fatalf("Proxy failed: %v", err)
		}
	}()

	// Log that server is ready
	log.Printf("Roxxy proxy server started and ready")
	fmt.Fprintf(os.Stderr, "[roxxy] Server started successfully, listening on %v\n", listenAddrs)

	// Note: We no longer periodically save stats to disk while server is running.
	// CLI commands will fetch stats from the API instead of reading from disk.
	// Stats are only saved to disk on server shutdown.

	// Wait for shutdown signal
	sig := <-sigChan
	log.Printf("Received signal %v, shutting down gracefully...", sig)

	// Stop package enricher
	enricherV2.Stop()

	// Save access statistics one final time (only save to disk on exit)
	if appInstance.StatsTracker != nil {
		if err := appInstance.StatsTracker.Save(); err != nil {
			log.Printf("Error saving access stats: %v", err)
		}
	}

	log.Printf("Shutdown complete")
	os.Exit(0)
}
