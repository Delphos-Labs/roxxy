package tls

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"sync"
	"time"
)

// CertificateManager manages TLS certificate generation and caching
type CertificateManager struct {
	certCache   map[string]*tls.Certificate
	certCacheMu sync.RWMutex
	caCertPath  string
	caKeyPath   string
}

// NewCertificateManager creates a new certificate manager
func NewCertificateManager(caCertPath, caKeyPath string) *CertificateManager {
	return &CertificateManager{
		certCache:  make(map[string]*tls.Certificate),
		caCertPath: caCertPath,
		caKeyPath:  caKeyPath,
	}
}

// GenerateCertForHost generates or retrieves a cached certificate for the given hostname
func (cm *CertificateManager) GenerateCertForHost(hostname string) (*tls.Certificate, error) {
	// Check cache first
	cm.certCacheMu.RLock()
	if cert, ok := cm.certCache[hostname]; ok {
		cm.certCacheMu.RUnlock()
		return cert, nil
	}
	cm.certCacheMu.RUnlock()

	certPEM, keyPEM, err := cm.GenerateCertPEMForHost(hostname)
	if err != nil {
		return nil, err
	}

	// Load as tls.Certificate
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to load generated certificate: %w", err)
	}

	// Cache it
	cm.certCacheMu.Lock()
	cm.certCache[hostname] = &cert
	cm.certCacheMu.Unlock()

	return &cert, nil
}

// GenerateCertPEMForHost generates a CA-signed certificate for the hostname
// and returns it PEM-encoded (no caching), for callers that persist the
// certificate rather than serve from memory.
func (cm *CertificateManager) GenerateCertPEMForHost(hostname string) (certPEM, keyPEM []byte, err error) {
	// Load CA certificate and key
	caCertPEM, err := os.ReadFile(cm.caCertPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read CA cert: %w", err)
	}

	caKeyPEM, err := os.ReadFile(cm.caKeyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read CA key: %w", err)
	}

	// Parse CA certificate
	caCertBlock, _ := pem.Decode(caCertPEM)
	if caCertBlock == nil {
		return nil, nil, fmt.Errorf("failed to decode CA certificate")
	}
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	// Parse CA private key
	caKeyBlock, _ := pem.Decode(caKeyPEM)
	if caKeyBlock == nil {
		return nil, nil, fmt.Errorf("failed to decode CA key")
	}
	caKey, err := x509.ParsePKCS8PrivateKey(caKeyBlock.Bytes)
	if err != nil {
		// Try parsing as PKCS1 (RSA)
		caKey, err = x509.ParsePKCS1PrivateKey(caKeyBlock.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse CA key: %w", err)
		}
	}

	// Generate new private key for this certificate
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate private key: %w", err)
	}

	// Create certificate template
	serialNumber, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Registry Proxy"},
			CommonName:   hostname,
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{hostname},
	}

	// Sign certificate with CA
	certDER, err := x509.CreateCertificate(rand.Reader, &template, caCert, &privKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	// Encode certificate and key
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privKey)})

	return certPEM, keyPEM, nil
}

// GenerateCACertificate generates a self-signed CA certificate and key
// Returns the certificate and key as PEM-encoded bytes
func GenerateCACertificate() (certPEM []byte, keyPEM []byte, err error) {
	// Generate private key
	privKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate private key: %w", err)
	}

	// Create CA certificate template
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Roxxy"},
			CommonName:   "Roxxy CA",
			Country:      []string{"US"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(3650 * 24 * time.Hour), // 10 years
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}

	// Create self-signed certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privKey.PublicKey, privKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	// Encode certificate and key as PEM
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privKey)})

	return certPEM, keyPEM, nil
}
