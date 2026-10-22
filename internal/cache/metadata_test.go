package cache

import (
	"path/filepath"
	"testing"
)

func TestPathHash(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantLen  int
		wantSame bool // If true, same path should produce same hash
	}{
		{
			name:     "simple path",
			path:     "/api/v1/packages",
			wantLen:  64, // SHA256 hex = 64 chars
			wantSame: true,
		},
		{
			name:     "empty path",
			path:     "",
			wantLen:  64,
			wantSame: true,
		},
		{
			name:     "path with special chars",
			path:     "/path/with/special@chars#123",
			wantLen:  64,
			wantSame: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash1 := PathHash(tt.path)
			if len(hash1) != tt.wantLen {
				t.Errorf("PathHash(%q) length = %d, want %d", tt.path, len(hash1), tt.wantLen)
			}

			if tt.wantSame {
				hash2 := PathHash(tt.path)
				if hash1 != hash2 {
					t.Errorf("PathHash(%q) = %q, second call = %q, want same", tt.path, hash1, hash2)
				}
			}
		})
	}
}

func TestGetURLPath(t *testing.T) {
	cacheDir := "/test/cache"
	hostname := "example.com"
	pathHash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	got := GetURLPath(cacheDir, hostname, pathHash)
	want := filepath.Join(cacheDir, "url", hostname, "ab", "cd", pathHash)

	if got != want {
		t.Errorf("GetURLPath() = %q, want %q", got, want)
	}
}

func TestGetContentPath(t *testing.T) {
	cacheDir := "/test/cache"
	contentHash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	got := GetContentPath(cacheDir, contentHash)
	want := filepath.Join(cacheDir, "content", "ab", "cd", contentHash)

	if got != want {
		t.Errorf("GetContentPath() = %q, want %q", got, want)
	}
}

func TestGetRefcountPath(t *testing.T) {
	cacheDir := "/test/cache"
	contentHash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	got := GetRefcountPath(cacheDir, contentHash)
	want := filepath.Join(cacheDir, "refcount", "ab", "cd", contentHash)

	if got != want {
		t.Errorf("GetRefcountPath() = %q, want %q", got, want)
	}
}
