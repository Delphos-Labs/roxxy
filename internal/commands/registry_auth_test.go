package commands

import (
	"encoding/base64"
	"testing"
)

func TestDecodeAuthEntry(t *testing.T) {
	tests := []struct {
		name     string
		entry    dockerAuthEntry
		wantUser string
		wantPass string
		wantErr  bool
	}{
		{
			name:     "base64 user:pass auth",
			entry:    dockerAuthEntry{Auth: base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))},
			wantUser: "alice",
			wantPass: "s3cret",
		},
		{
			name:     "password containing a colon keeps everything after the first",
			entry:    dockerAuthEntry{Auth: base64.StdEncoding.EncodeToString([]byte("alice:a:b:c"))},
			wantUser: "alice",
			wantPass: "a:b:c",
		},
		{
			name:    "malformed auth without a colon errors",
			entry:   dockerAuthEntry{Auth: base64.StdEncoding.EncodeToString([]byte("nocolon"))},
			wantErr: true,
		},
		{
			name:    "auth that is not valid base64 errors",
			entry:   dockerAuthEntry{Auth: "!!!not-base64!!!"},
			wantErr: true,
		},
		{
			name:     "explicit username/password fallback when no auth field",
			entry:    dockerAuthEntry{Username: "bob", Password: "hunter2"},
			wantUser: "bob",
			wantPass: "hunter2",
		},
		{
			name:  "empty entry yields empty creds, no error",
			entry: dockerAuthEntry{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			creds, err := decodeAuthEntry(tt.entry)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got creds %+v", creds)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if creds.Username != tt.wantUser || creds.Password != tt.wantPass {
				t.Errorf("got user=%q pass=%q, want user=%q pass=%q",
					creds.Username, creds.Password, tt.wantUser, tt.wantPass)
			}
		})
	}
}

func TestMatchHostDockerHubLegacyKey(t *testing.T) {
	// Docker Hub stores credentials under the legacy v1 key even though pushes
	// target registry-1.docker.io; the candidate fallback must find it.
	m := map[string]string{
		"https://index.docker.io/v1/": "desktop",
	}
	if _, ok := matchHost(m, "index.docker.io"); !ok {
		t.Errorf("expected legacy docker hub key to match host index.docker.io")
	}
}

func TestHostCandidatesIncludesLegacyV1Variant(t *testing.T) {
	cands := hostCandidates("index.docker.io")
	want := "https://index.docker.io/v1/"
	found := false
	for _, c := range cands {
		if c == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("hostCandidates(%q) = %v, missing legacy variant %q", "index.docker.io", cands, want)
	}
}
