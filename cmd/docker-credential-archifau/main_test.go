package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAudience(t *testing.T) {
	cases := []struct {
		name     string
		server   string
		audEnv   string
		regEnv   string
		expected string
	}{
		{
			name:     "host from https url",
			server:   "https://registry.example.com/v2/foo",
			expected: "registry.example.com",
		},
		{
			name:     "host from http url",
			server:   "http://registry.local/path",
			expected: "registry.local",
		},
		{
			name:     "registry port remains part of audience",
			server:   "https://registry.example.com:9443/v2/",
			expected: "registry.example.com:9443",
		},
		{
			name:     "bare host no scheme",
			server:   "registry.example.com",
			expected: "registry.example.com",
		},
		{
			name:     "explicit audience override wins over server",
			server:   "https://registry.example.com",
			audEnv:   "custom-aud",
			expected: "custom-aud",
		},
		{
			name:     "empty server falls back to registry env",
			server:   "",
			regEnv:   "fallback.registry",
			expected: "fallback.registry",
		},
		{
			name:     "empty server and no registry yields empty",
			server:   "",
			expected: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ARCHIFAU_AUDIENCE", tc.audEnv)
			t.Setenv("ARCHIFAU_REGISTRY", tc.regEnv)
			if got := audience(tc.server); got != tc.expected {
				t.Errorf("audience(%q) = %q, want %q", tc.server, got, tc.expected)
			}
		})
	}
}

func TestFetchOIDCToken(t *testing.T) {
	var gotAuth, gotAudience string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAudience = r.URL.Query().Get("audience")
		_ = json.NewEncoder(w).Encode(map[string]string{"value": "the-id-token"})
	}))
	t.Cleanup(srv.Close)

	// GitHub's ACTIONS_ID_TOKEN_REQUEST_URL always carries a query string, so
	// the helper appends "&audience=" to an existing "?api-version=...".
	reqURL := srv.URL + "?api-version=2.0"
	token, err := fetchOIDCToken(reqURL, "req-token", "registry.example.com")
	if err != nil {
		t.Fatalf("fetchOIDCToken: %v", err)
	}
	if token != "the-id-token" {
		t.Errorf("token = %q, want the-id-token", token)
	}
	if gotAuth != "bearer req-token" {
		t.Errorf("Authorization = %q, want \"bearer req-token\"", gotAuth)
	}
	if gotAudience != "registry.example.com" {
		t.Errorf("audience param = %q, want registry.example.com", gotAudience)
	}
}

func TestFetchOIDCTokenEmptyValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"value": ""})
	}))
	t.Cleanup(srv.Close)

	if _, err := fetchOIDCToken(srv.URL+"?api-version=2.0", "req-token", "aud"); err == nil {
		t.Error("expected error when OIDC response value is empty")
	}
}
