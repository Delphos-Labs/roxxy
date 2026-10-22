package util

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/config"
)

// GetProxyPort extracts the port number from the configured listen address
// Returns the port as an integer, defaulting to 3128 if not configured
// Uses the first listen address if multiple are configured
func GetProxyPort() int {
	listenAddr := ":3128" // Default
	if globalConfig := config.GetGlobalConfig(); globalConfig != nil {
		if len(globalConfig.ListenAddrs) > 0 {
			listenAddr = globalConfig.ListenAddrs[0] // Use first listen address
		} else if globalConfig.ListenAddr != "" {
			listenAddr = globalConfig.ListenAddr // Fallback to old listen_addr
		}
	}

	// Extract port from listen address (format: ":3128" or "localhost:3128")
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		// No port in address, try to parse as just port number
		if strings.HasPrefix(listenAddr, ":") {
			port = strings.TrimPrefix(listenAddr, ":")
		} else {
			return 3128 // Default
		}
	}

	portInt, err := strconv.Atoi(port)
	if err != nil {
		return 3128 // Default on parse error
	}
	return portInt
}

// GetProxyHostPort returns the host:port string for the proxy
// Uses the request's Host header if available, otherwise constructs from config
// Falls back to "localhost:3128" if no request context
func GetProxyHostPort(r *http.Request) string {
	// If we have a request, use its Host header (includes port if present)
	if r != nil && r.Host != "" {
		return r.Host
	}

	// No request context - use configured listen address
	listenAddr := ":3128" // Default
	if globalConfig := config.GetGlobalConfig(); globalConfig != nil {
		if len(globalConfig.ListenAddrs) > 0 {
			listenAddr = globalConfig.ListenAddrs[0] // Use first listen address
		} else if globalConfig.ListenAddr != "" {
			listenAddr = globalConfig.ListenAddr // Fallback to old listen_addr
		}
	} else if envAddr := os.Getenv("LISTEN_ADDR"); envAddr != "" {
		listenAddr = envAddr
	}

	// Convert listen address to host:port format
	if strings.HasPrefix(listenAddr, ":") {
		// Just port, use localhost
		return "localhost" + listenAddr
	}
	return listenAddr
}

// GetRegistryURL constructs a registry API URL
// Uses the request's host/port when available, falling back to configured port
// In Docker containers, uses HTTPS on port 443 for registry API
func GetRegistryURL(r *http.Request, useHTTPS bool) string {
	scheme := "http"
	if useHTTPS {
		scheme = "https"
	}

	// If we have a request, use its host/port
	if r != nil && r.Host != "" {
		host := r.Host
		// In Docker, if accessing via proxy port (3128), use 443 for registry API
		proxyPort := GetProxyPort()
		if strings.HasSuffix(host, ":"+strconv.Itoa(proxyPort)) {
			host = ReplacePort(host, 443)
			scheme = "https"
		}
		return scheme + "://" + host
	}

	// No request context - check if in Docker
	if _, err := os.Stat("/app"); err == nil {
		// Running in Docker - use localhost:443 for registry API
		return "https://localhost:443"
	}

	// Not in Docker, no request - use configured proxy address
	hostPort := GetProxyHostPort(nil)
	return scheme + "://" + hostPort
}
