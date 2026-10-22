package api

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

// handleCACertificate serves the CA certificate for download
func (a *API) handleCACertificate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if a.certsDir == "" {
		log.Printf("API: CA cert endpoint called but certsDir is empty")
		http.Error(w, "Certificates directory not configured", http.StatusInternalServerError)
		return
	}

	certPath := filepath.Join(a.certsDir, "ca-cert.pem")
	log.Printf("API: CA cert endpoint called, checking cert at: %s", certPath)

	// Check if certificate exists
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		log.Printf("API: CA certificate not found at %s (error: %v)", certPath, err)
		http.Error(w, "CA certificate not found", http.StatusNotFound)
		return
	}

	// Read certificate file
	certData, err := os.ReadFile(certPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read certificate: %v", err), http.StatusInternalServerError)
		return
	}

	// Set appropriate headers for download
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="roxxy-ca.crt"`)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(certData)))

	// Write certificate data
	if _, err := w.Write(certData); err != nil {
		log.Printf("Error writing certificate: %v", err)
	}
}

// handleCertTest returns information about the TLS connection for testing HTTPS
// GET /api/v1/certs/test
func (a *API) handleCertTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response := map[string]interface{}{
		"https":       r.TLS != nil,
		"method":      r.Method,
		"url":         r.URL.String(),
		"host":        r.Host,
		"remote_addr": r.RemoteAddr,
		"proto":       r.Proto,
		"scheme":      r.URL.Scheme,
	}

	if r.TLS != nil {
		// Add TLS connection details
		tlsInfo := map[string]interface{}{
			"version":                 tlsVersionString(r.TLS.Version),
			"negotiated_protocol":     r.TLS.NegotiatedProtocol,
			"server_name":             r.TLS.ServerName,
			"cipher_suite":            tls.CipherSuiteName(r.TLS.CipherSuite),
			"peer_certificates_count": len(r.TLS.PeerCertificates),
			"verified_chains_count":   len(r.TLS.VerifiedChains),
			"handshake_complete":      r.TLS.HandshakeComplete,
		}

		// Add peer certificate information if available
		if len(r.TLS.PeerCertificates) > 0 {
			cert := r.TLS.PeerCertificates[0]
			tlsInfo["peer_certificate"] = map[string]interface{}{
				"subject":    cert.Subject.String(),
				"issuer":     cert.Issuer.String(),
				"not_before": cert.NotBefore.Format("2006-01-02 15:04:05"),
				"not_after":  cert.NotAfter.Format("2006-01-02 15:04:05"),
				"dns_names":  cert.DNSNames,
			}
		}

		response["tls"] = tlsInfo
		response["message"] = "HTTPS connection successful! TLS is working correctly."
	} else {
		response["message"] = "This is an HTTP connection. Use HTTPS to test certificate functionality."
		response["tls"] = nil
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Error encoding cert test response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// tlsVersionString converts a TLS version number to a string
func tlsVersionString(version uint16) string {
	switch version {
	case tls.VersionTLS10:
		return "TLS 1.0"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	default:
		return fmt.Sprintf("Unknown (0x%04x)", version)
	}
}
