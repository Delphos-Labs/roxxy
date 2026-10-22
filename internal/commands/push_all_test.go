package commands

import "testing"

func TestParseManifestPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantRepo string
		wantTag  string
		wantOK   bool
	}{
		{
			name:     "valid three-segment docker-manifests path",
			path:     "docker-manifests/docker.io/library_redis/7",
			wantRepo: "library/redis",
			wantTag:  "7",
			wantOK:   true,
		},
		{
			name:   "wrong segment count is rejected",
			path:   "docker-manifests/docker.io/library_redis",
			wantOK: false,
		},
		{
			name:   "too many segments is rejected",
			path:   "docker-manifests/docker.io/library_redis/7/extra",
			wantOK: false,
		},
		{
			name:   "non docker-manifests prefix is rejected",
			path:   "helm-charts/docker.io/library_redis/7",
			wantOK: false,
		},
		{
			name:     "sha256 tag is parsed (caller filters it later)",
			path:     "docker-manifests/ghcr.io/owner_app/sha256:abc123",
			wantRepo: "owner/app",
			wantTag:  "sha256:abc123",
			wantOK:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, tag, ok := parseManifestPath(tt.path)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if repo != tt.wantRepo || tag != tt.wantTag {
				t.Errorf("got repo=%q tag=%q, want repo=%q tag=%q", repo, tag, tt.wantRepo, tt.wantTag)
			}
		})
	}
}

// TestPushAllExitCode pins the `roar push --all` exit-code contract: 2 if any ref
// failed to push or any manifest path was unparseable (registry INCOMPLETE), else
// 0 — benign tag conflicts alone are NOT fatal.
func TestPushAllExitCode(t *testing.T) {
	ref := &pushRef{repoName: "library/x", tag: "1"}
	tests := []struct {
		name        string
		outcome     pushOutcome
		unparseable []string
		want        int
	}{
		{"clean", pushOutcome{}, nil, 0},
		{"conflicts only (benign)", pushOutcome{conflicts: []*pushRef{ref}}, nil, 0},
		{"genuine failure", pushOutcome{failures: []*pushRef{ref}}, nil, 2},
		{"unparseable", pushOutcome{}, []string{"helm-charts/x"}, 2},
		{"failure + conflict", pushOutcome{conflicts: []*pushRef{ref}, failures: []*pushRef{ref}}, nil, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pushAllExitCode(tt.outcome, tt.unparseable); got != tt.want {
				t.Errorf("pushAllExitCode = %d, want %d", got, tt.want)
			}
		})
	}
}
