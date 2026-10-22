package commands

import (
	"path/filepath"
	"testing"
)

func TestNormalizeRegistryURLForPath(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "docker.io registry",
			input: "https://registry-1.docker.io",
			want:  "docker.io",
		},
		{
			name:  "ghcr.io",
			input: "https://ghcr.io",
			want:  "ghcr.io",
		},
		{
			name:  "quay.io",
			input: "https://quay.io",
			want:  "quay.io",
		},
		{
			name:  "with port",
			input: "https://registry.example.com:5000",
			want:  "registry.example.com",
		},
		{
			name:  "http protocol",
			input: "http://registry.example.com",
			want:  "registry.example.com",
		},
		{
			name:  "empty string",
			input: "",
			want:  "docker.io",
		},
		{
			name:  "with path",
			input: "https://registry.example.com/v2",
			want:  "registry.example.com_v2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeRegistryURLForPath(tt.input)
			if got != tt.want {
				t.Errorf("NormalizeRegistryURLForPath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeRepoName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "library repo without prefix",
			input: "golang",
			want:  "library/golang",
		},
		{
			name:  "library repo with prefix",
			input: "library/golang",
			want:  "library/golang",
		},
		{
			name:  "user repo",
			input: "user/repo",
			want:  "user/repo",
		},
		{
			name:  "nested repo",
			input: "org/team/repo",
			want:  "org/team/repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeRepoName(tt.input)
			if got != tt.want {
				t.Errorf("NormalizeRepoName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSafeRepoName(t *testing.T) {
	tests := []struct{ name, input, want string }{
		{"library repo", "library/golang", "library_golang"},
		{"user repo", "user/repo", "user_repo"},
		{"nested repo", "org/team/repo", "org_team_repo"},
		{"no slashes", "golang", "golang"},
		{"underscore in name", "oliver006/redis_exporter", "oliver006_redis__exporter"},
		{"underscore at root", "node_exporter", "node__exporter"},
		{"adjacent special chars", "a/b_c/d", "a_b__c_d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SafeRepoName(tt.input); got != tt.want {
				t.Errorf("SafeRepoName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestUnsafeRepoName(t *testing.T) {
	tests := []struct{ name, input, want string }{
		{"library repo", "library_golang", "library/golang"},
		{"user repo", "user_repo", "user/repo"},
		{"nested repo", "org_team_repo", "org/team/repo"},
		{"no underscores", "golang", "golang"},
		{"escaped underscore", "oliver006_redis__exporter", "oliver006/redis_exporter"},
		{"escaped underscore at root", "node__exporter", "node_exporter"},
		{"adjacent special chars", "a_b__c_d", "a/b_c/d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UnsafeRepoName(tt.input); got != tt.want {
				t.Errorf("UnsafeRepoName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSafeRepoNameRoundTrip(t *testing.T) {
	cases := []string{
		"library/golang",
		"oliver006/redis_exporter",
		"prom/node_exporter",
		"a/b_c/d",
		"node_exporter",
		"underscore_only",
	}
	for _, c := range cases {
		if got := UnsafeRepoName(SafeRepoName(c)); got != c {
			t.Errorf("round-trip %q: got %q", c, got)
		}
	}
}

func TestDockerManifestPath(t *testing.T) {
	cacheDir := "/test/cache"
	registryID := "docker.io"
	repoName := "library/golang"
	reference := "1.20"

	got := DockerManifestPath(cacheDir, registryID, repoName, reference)
	want := filepath.Join(cacheDir, "docker-manifests", registryID, "library_golang", reference)

	if got != want {
		t.Errorf("DockerManifestPath() = %q, want %q", got, want)
	}
}

func TestDockerManifestDir(t *testing.T) {
	cacheDir := "/test/cache"
	registryID := "docker.io"
	repoName := "library/golang"

	got := DockerManifestDir(cacheDir, registryID, repoName)
	want := filepath.Join(cacheDir, "docker-manifests", registryID, "library_golang")

	if got != want {
		t.Errorf("DockerManifestDir() = %q, want %q", got, want)
	}
}

func TestIsValidPackageType(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid apk", "apk", true},
		{"valid apt", "apt", true},
		{"valid npm", "npm", true},
		{"valid pypi", "pypi", true},
		{"valid go", "go", true},
		{"invalid type", "invalid", false},
		{"empty string", "", false},
		{"case sensitive", "APK", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsValidPackageType(tt.input)
			if got != tt.want {
				t.Errorf("IsValidPackageType(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestGetPackageTypesToParse(t *testing.T) {
	tests := []struct {
		name    string
		pmType  string
		wantLen int
		wantErr bool
	}{
		{"empty returns all", "", 5, false},
		{"valid type returns one", "apk", 1, false},
		{"invalid type returns error", "invalid", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetPackageTypesToParse(tt.pmType)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetPackageTypesToParse(%q) error = %v, wantErr %v", tt.pmType, err, tt.wantErr)
				return
			}
			if len(got) != tt.wantLen {
				t.Errorf("GetPackageTypesToParse(%q) len = %d, want %d", tt.pmType, len(got), tt.wantLen)
			}
		})
	}
}
