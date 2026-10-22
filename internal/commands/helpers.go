package commands

import (
	"fmt"
	"os"
)

// ShowUsage displays usage information
func ShowUsage() {
	fmt.Println("roxxy - Docker registry and package manager proxy")
	fmt.Println()
	fmt.Println("Usage: roxxy [command] [arguments]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  server              Start the proxy server (default)")
	fmt.Println("  service <cmd>       Service management (install|uninstall|restart)")
	fmt.Println("  raw ls            List cached raw files")
	fmt.Println("  raw flush         Flush all cache")
	fmt.Println("  package ls          List cached packages (APK, APT, NPM, PyPI, Go)")
	fmt.Println("  package parse       Parse index files to extract package metadata")
	fmt.Println("  docker parse        Extract metadata (license, description) from Docker/OCI images")
	fmt.Println("  docker ls           List Docker images")
	fmt.Println("  docker rmi <image>  Remove Docker image from cache")
	fmt.Println("  docker prune        Remove unreferenced blobs")
	fmt.Println("  docker discover     Discover tags for manifests stored by digest only")
	fmt.Println("  hardlink            Create hardlinks for convenience commands")
	fmt.Println("  embed ls [path]     List embedded files")
	fmt.Println("  help                Show this help message")
	fmt.Println()
	fmt.Println("Flags (for server command):")
	fmt.Println("  --listen <addr>     Listen address (e.g., :3128 or :8080). Overrides LISTEN_ADDR env var.")
	fmt.Println("  --airgap-proxy      Cache all files with no size limits (for airgap preparation)")
	fmt.Println("  --airgapped         Airgapped mode: no external access, return 404 for missing artifacts")
}

// GetEnv gets an environment variable or returns a default value
func GetEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
