package util

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// localHostnames holds additional hostnames that should be treated as localhost.
// Set via SetLocalHostnames during startup from config.
var (
	localHostnamesMu sync.RWMutex
	localHostnames   []string
)

// SetLocalHostnames configures additional hostnames that IsLocalhost should treat as local.
func SetLocalHostnames(hostnames []string) {
	localHostnamesMu.Lock()
	defer localHostnamesMu.Unlock()
	localHostnames = hostnames
}

// IsLocalhost checks if a hostname string represents localhost.
// It recognizes built-in localhost addresses (localhost, 127.0.0.1, ::1, host.docker.internal)
// and any additional hostnames configured via SetLocalHostnames.
func IsLocalhost(hostname string) bool {
	switch hostname {
	case "localhost", "127.0.0.1", "::1", "[::1]", "host.docker.internal":
		return true
	}
	localHostnamesMu.RLock()
	defer localHostnamesMu.RUnlock()
	for _, h := range localHostnames {
		if hostname == h {
			return true
		}
	}
	return false
}

// IsLocalhostRequest checks if an HTTP request is from localhost
// It checks both the Host header and RemoteAddr to detect localhost connections.
// When Docker uses a proxy, the Host header is the registry hostname, but RemoteAddr is localhost.
func IsLocalhostRequest(r *http.Request) bool {
	// Check Host header - extract hostname without port
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if IsLocalhost(host) {
		return true
	}

	// Check RemoteAddr
	remoteHost := r.RemoteAddr
	if h, _, err := net.SplitHostPort(remoteHost); err == nil {
		remoteHost = h
	}
	return IsLocalhost(remoteHost)
}

// ReplacePort replaces the port in a host:port string with a new port
// Uses net.SplitHostPort and net.JoinHostPort for proper parsing, handling IPv6 addresses correctly.
// If the original string has no port, the new port is appended.
func ReplacePort(hostport string, newPort int) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		// No port in original
		// Check if it's an IPv6 address with brackets (e.g., "[::1]")
		if strings.HasPrefix(hostport, "[") && strings.HasSuffix(hostport, "]") {
			// Remove brackets, JoinHostPort will add them back
			ipv6 := strings.Trim(hostport, "[]")
			return net.JoinHostPort(ipv6, strconv.Itoa(newPort))
		}
		// Regular hostname or IPv4, just append port
		return net.JoinHostPort(hostport, strconv.Itoa(newPort))
	}
	return net.JoinHostPort(host, strconv.Itoa(newPort))
}

// BuildUpstreamURL builds a full URL from a base URL, path, and query string
// Uses url.Parse and url.URL for proper URL manipulation instead of string concatenation.
func BuildUpstreamURL(baseURL, path, query string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		// Fallback to string concatenation if parsing fails
		result := baseURL + path
		if query != "" {
			result += "?" + query
		}
		return result
	}
	u.Path = path
	u.RawQuery = query
	return u.String()
}

// ParseHTTPRangeHeader parses an HTTP Range header (e.g., "bytes=0-1023")
// Returns start and end offsets. If end is not specified, returns -1 for end.
// Returns error if the header format is invalid.
func ParseHTTPRangeHeader(rangeHeader string, contentLength int64) (start, end int64, err error) {
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, 0, fmt.Errorf("invalid range header format: %s", rangeHeader)
	}

	rangeSpec := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.Split(rangeSpec, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid range spec: %s", rangeSpec)
	}

	// Parse start
	if parts[0] != "" {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid start offset: %s", parts[0])
		}
	}

	// Parse end (optional)
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid end offset: %s", parts[1])
		}
	} else {
		// If end not specified, use content length - 1
		end = contentLength - 1
	}

	return start, end, nil
}
