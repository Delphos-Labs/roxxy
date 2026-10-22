package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Delphos-Labs/roxxy/internal/cache"
)

func TestHandleCACertificate(t *testing.T) {
	// Create temporary directory for certificates
	tempDir := t.TempDir()
	certPath := filepath.Join(tempDir, "ca-cert.pem")
	certContent := "-----BEGIN CERTIFICATE-----\nTEST CERTIFICATE CONTENT\n-----END CERTIFICATE-----\n"

	// Write test certificate
	if err := os.WriteFile(certPath, []byte(certContent), 0644); err != nil {
		t.Fatalf("Failed to write test certificate: %v", err)
	}

	// Create API instance
	api := NewAPI("", &cache.StatsTracker{}, nil)
	api.SetCertsDir(tempDir)

	tests := []struct {
		name            string
		method          string
		wantStatus      int
		wantContentType string
		wantFilename    string
		wantBody        string
	}{
		{
			name:            "GET request returns certificate",
			method:          http.MethodGet,
			wantStatus:      http.StatusOK,
			wantContentType: "application/x-x509-ca-cert",
			wantFilename:    "roxxy-ca.crt",
			wantBody:        certContent,
		},
		{
			name:       "POST request returns method not allowed",
			method:     http.MethodPost,
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "PUT request returns method not allowed",
			method:     http.MethodPut,
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/api/v1/certs/ca", nil)
			w := httptest.NewRecorder()

			api.handleCACertificate(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("Status code = %d, want %d", w.Code, tt.wantStatus)
			}

			if tt.wantContentType != "" {
				contentType := w.Header().Get("Content-Type")
				if contentType != tt.wantContentType {
					t.Errorf("Content-Type = %q, want %q", contentType, tt.wantContentType)
				}
			}

			if tt.wantFilename != "" {
				contentDisposition := w.Header().Get("Content-Disposition")
				if !strings.Contains(contentDisposition, tt.wantFilename) {
					t.Errorf("Content-Disposition = %q, want to contain %q", contentDisposition, tt.wantFilename)
				}
			}

			if tt.wantBody != "" {
				body := w.Body.String()
				if body != tt.wantBody {
					t.Errorf("Body = %q, want %q", body, tt.wantBody)
				}
			}
		})
	}
}

func TestHandleCACertificate_NoCertsDir(t *testing.T) {
	api := NewAPI("", &cache.StatsTracker{}, nil)
	// Don't set certsDir

	req := httptest.NewRequest(http.MethodGet, "/api/v1/certs/ca", nil)
	w := httptest.NewRecorder()

	api.handleCACertificate(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("Status code = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

func TestHandleCACertificate_CertNotFound(t *testing.T) {
	tempDir := t.TempDir()

	api := NewAPI("", &cache.StatsTracker{}, nil)
	api.SetCertsDir(tempDir)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/certs/ca", nil)
	w := httptest.NewRecorder()

	api.handleCACertificate(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Status code = %d, want %d", w.Code, http.StatusNotFound)
	}
}
