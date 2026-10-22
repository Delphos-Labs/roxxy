package util

import (
	"net/http"
	"testing"
)

func TestIsLocalhostRequest(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		remoteAddr string
		expected   bool
	}{
		{
			name:       "localhost in Host header",
			host:       "localhost:8080",
			remoteAddr: "192.168.1.1:12345",
			expected:   true,
		},
		{
			name:       "127.0.0.1 in Host header",
			host:       "127.0.0.1:8080",
			remoteAddr: "192.168.1.1:12345",
			expected:   true,
		},
		{
			name:       "127.0.0.1 in RemoteAddr",
			host:       "example.com:8080",
			remoteAddr: "127.0.0.1:12345",
			expected:   true,
		},
		{
			name:       "::1 in RemoteAddr",
			host:       "example.com:8080",
			remoteAddr: "[::1]:12345",
			expected:   true,
		},
		{
			name:       "localhost: prefix in RemoteAddr",
			host:       "example.com:8080",
			remoteAddr: "localhost:12345",
			expected:   true,
		},
		{
			name:       "non-localhost",
			host:       "example.com:8080",
			remoteAddr: "192.168.1.1:12345",
			expected:   false,
		},
		{
			name:       "empty host and non-localhost remote",
			host:       "",
			remoteAddr: "192.168.1.1:12345",
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &http.Request{
				Host:       tt.host,
				RemoteAddr: tt.remoteAddr,
			}
			result := IsLocalhostRequest(req)
			if result != tt.expected {
				t.Errorf("IsLocalhostRequest() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestReplacePort(t *testing.T) {
	tests := []struct {
		name     string
		hostport string
		newPort  int
		expected string
	}{
		{
			name:     "replace existing port",
			hostport: "localhost:3128",
			newPort:  443,
			expected: "localhost:443",
		},
		{
			name:     "add port to hostname without port",
			hostport: "localhost",
			newPort:  443,
			expected: "localhost:443",
		},
		{
			name:     "replace port in IPv4 address",
			hostport: "192.168.1.1:8080",
			newPort:  443,
			expected: "192.168.1.1:443",
		},
		{
			name:     "replace port in IPv6 address",
			hostport: "[::1]:8080",
			newPort:  443,
			expected: "[::1]:443",
		},
		{
			name:     "add port to IPv6 address without port",
			hostport: "[::1]",
			newPort:  443,
			expected: "[::1]:443",
		},
		{
			name:     "replace port with different port",
			hostport: "example.com:80",
			newPort:  8080,
			expected: "example.com:8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ReplacePort(tt.hostport, tt.newPort)
			if result != tt.expected {
				t.Errorf("ReplacePort(%q, %d) = %q, want %q", tt.hostport, tt.newPort, result, tt.expected)
			}
		})
	}
}

func TestBuildUpstreamURL(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		path     string
		query    string
		expected string
	}{
		{
			name:     "base URL with path and query",
			baseURL:  "https://example.com",
			path:     "/api/v1/test",
			query:    "param=value",
			expected: "https://example.com/api/v1/test?param=value",
		},
		{
			name:     "base URL with path, no query",
			baseURL:  "https://example.com",
			path:     "/api/v1/test",
			query:    "",
			expected: "https://example.com/api/v1/test",
		},
		{
			name:     "base URL with trailing slash",
			baseURL:  "https://example.com/",
			path:     "/api/v1/test",
			query:    "param=value",
			expected: "https://example.com/api/v1/test?param=value",
		},
		{
			name:     "base URL with existing path",
			baseURL:  "https://example.com/base",
			path:     "/api/v1/test",
			query:    "param=value",
			expected: "https://example.com/api/v1/test?param=value",
		},
		{
			name:     "empty path",
			baseURL:  "https://example.com",
			path:     "",
			query:    "param=value",
			expected: "https://example.com?param=value",
		},
		{
			name:     "empty path and query",
			baseURL:  "https://example.com",
			path:     "",
			query:    "",
			expected: "https://example.com",
		},
		{
			name:     "query with multiple parameters",
			baseURL:  "https://example.com",
			path:     "/api/v1/test",
			query:    "param1=value1&param2=value2",
			expected: "https://example.com/api/v1/test?param1=value1&param2=value2",
		},
		{
			name:     "invalid base URL (fallback to concatenation)",
			baseURL:  "://invalid",
			path:     "/api/v1/test",
			query:    "param=value",
			expected: "://invalid/api/v1/test?param=value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildUpstreamURL(tt.baseURL, tt.path, tt.query)
			if result != tt.expected {
				t.Errorf("BuildUpstreamURL(%q, %q, %q) = %q, want %q", tt.baseURL, tt.path, tt.query, result, tt.expected)
			}
		})
	}
}
