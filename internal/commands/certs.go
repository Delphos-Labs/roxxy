package commands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/Delphos-Labs/roxxy/internal/config"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// HandleCerts handles certificate generation and installation
func HandleCerts(cfg Config, args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: roxxy certs <generate|install>")
		fmt.Println("")
		fmt.Println("Commands:")
		fmt.Println("  generate  Generate a new CA certificate and key for MITM TLS proxying")
		fmt.Println("             Use --force to overwrite existing certificates without prompting")
		fmt.Println("  install   Install the CA certificate in the system trust store")
		fmt.Println("")
		fmt.Println("Files will be created in the certs directory (platform-specific).")
		os.Exit(1)
	}

	command := args[0]
	// Check for --force flag
	force := false
	for _, arg := range args {
		if arg == "--force" || arg == "-f" {
			force = true
		}
	}

	switch command {
	case "generate":
		HandleCertsGenerate(cfg, force)
	case "install":
		HandleCertsInstall(cfg)
	default:
		fmt.Printf("Unknown command: %s\n", command)
		fmt.Println("Usage: roxxy certs <generate|install>")
		os.Exit(1)
	}
}

// HandleCertsGenerate generates a new CA certificate
// force: if true, overwrite existing certificates without prompting (useful for containers)
func HandleCertsGenerate(cfg Config, force bool) {
	// Get certs directory - use from config, fallback to default
	certsDir := cfg.CertsDir
	if certsDir == "" {
		certsDir = config.GetDefaultCertsDir()
	}

	// Ensure directory exists
	if err := os.MkdirAll(certsDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating certs directory: %v\n", err)
		os.Exit(1)
	}

	certPath := filepath.Join(certsDir, "ca-cert.pem")
	keyPath := filepath.Join(certsDir, "ca-key.pem")

	// Check if certs already exist
	if _, err := os.Stat(certPath); err == nil {
		if force {
			fmt.Printf("Certificate already exists: %s\n", certPath)
			fmt.Println("Overwriting (--force specified)...")
		} else {
			fmt.Printf("Certificate already exists: %s\n", certPath)
			fmt.Print("Overwrite? (y/N): ")
			var response string
			util.LogerrT(fmt.Scanln(&response))
			if response != "y" && response != "Y" {
				fmt.Println("Cancelled.")
				os.Exit(0)
			}
		}
	}

	fmt.Printf("Generating CA certificate and key...\n")
	fmt.Printf("Certificate: %s\n", certPath)
	fmt.Printf("Key: %s\n", keyPath)

	// Generate certificate using openssl
	// First generate private key
	keyCmd := exec.Command("openssl", "genrsa", "-out", keyPath, "4096")
	if err := keyCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error generating private key: %v\n", err)
		fmt.Fprintf(os.Stderr, "Make sure openssl is installed\n")
		os.Exit(1)
	}

	// Generate self-signed certificate
	certCmd := exec.Command("openssl", "req", "-new", "-x509", "-days", "3650",
		"-key", keyPath, "-out", certPath,
		"-subj", "/CN=Roxxy CA/O=Roxxy/C=US")
	if err := certCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error generating certificate: %v\n", err)
		os.Exit(1)
	}

	// Set permissions
	util.Logerr(os.Chmod(keyPath, 0600))
	util.Logerr(os.Chmod(certPath, 0644))

	fmt.Println("✓ Certificate and key generated successfully")
	fmt.Println("")
	fmt.Println("To install this certificate in the system trust store:")
	fmt.Println("  roxxy certs install")
}

// HandleCertsInstall installs the CA certificate in the system trust store
func HandleCertsInstall(cfg Config) {
	// Get certs directory - use from config, fallback to default
	certsDir := cfg.CertsDir
	if certsDir == "" {
		certsDir = config.GetDefaultCertsDir()
	}

	certPath := filepath.Join(certsDir, "ca-cert.pem")

	// Check if certificate exists
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: CA certificate not found at %s\n", certPath)
		fmt.Fprintf(os.Stderr, "Please run 'roxxy certs generate' first\n")
		os.Exit(1)
	}

	fmt.Println("=== Installing Roxxy CA Certificate ===")
	fmt.Printf("Certificate: %s\n", certPath)
	fmt.Println("")

	// Detect OS
	switch runtime.GOOS {
	case "darwin":
		installCACertMacOS(certPath)
	case "linux":
		installCACertLinux(certPath)
	case "windows":
		fmt.Fprintf(os.Stderr, "Windows certificate installation is not yet supported\n")
		fmt.Fprintf(os.Stderr, "Please manually install the certificate:\n")
		fmt.Printf("  %s\n", certPath)
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "Certificate installation not supported on %s\n", runtime.GOOS)
		os.Exit(1)
	}

	// Ask about Docker daemon installation
	fmt.Println("")
	fmt.Print("Install certificate for Docker daemon? (y/N): ")
	var dockerChoice string
	util.LogerrT(fmt.Scanln(&dockerChoice))
	if dockerChoice == "y" || dockerChoice == "Y" {
		installCACertDocker(certPath)
	}

	fmt.Println("")
	fmt.Println("=========================================")
	fmt.Println("✓ CA Certificate Installation Complete!")
	fmt.Println("=========================================")
}

// installCACertMacOS installs the CA certificate on macOS
func installCACertMacOS(certPath string) {
	fmt.Println("Installing CA certificate in macOS Keychain...")
	cmd := exec.Command("sudo", "security", "add-trusted-cert", "-d", "-r", "trustRoot",
		"-k", "/Library/Keychains/System.keychain", certPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error installing certificate: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("  ✓ Installed in System Keychain")
}

// installCACertLinux installs the CA certificate on Linux
func installCACertLinux(certPath string) {
	fmt.Println("Installing CA certificate in system trust store...")

	// Try Ubuntu/Debian location first
	if _, err := os.Stat("/usr/local/share/ca-certificates"); err == nil {
		destPath := "/usr/local/share/ca-certificates/roxxy-ca.crt"
		cmd := exec.Command("sudo", "cp", certPath, destPath)
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error copying certificate: %v\n", err)
			os.Exit(1)
		}
		cmd = exec.Command("sudo", "update-ca-certificates")
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error updating certificates: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("  ✓ Installed in /usr/local/share/ca-certificates")
		return
	}

	// Try RHEL/CentOS/Fedora location
	if _, err := os.Stat("/etc/pki/ca-trust/source/anchors"); err == nil {
		destPath := "/etc/pki/ca-trust/source/anchors/roxxy-ca.crt"
		cmd := exec.Command("sudo", "cp", certPath, destPath)
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error copying certificate: %v\n", err)
			os.Exit(1)
		}
		cmd = exec.Command("sudo", "update-ca-trust")
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error updating trust: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("  ✓ Installed in /etc/pki/ca-trust/source/anchors")
		return
	}

	fmt.Fprintf(os.Stderr, "  ⚠ Could not detect certificate directory\n")
	fmt.Fprintf(os.Stderr, "  Please manually install the certificate\n")
	os.Exit(1)
}

// installCACertDocker installs the CA certificate for Docker daemon
func installCACertDocker(certPath string) {
	fmt.Println("")
	fmt.Println("Installing CA certificate for Docker daemon...")

	dockerCertDir := "/etc/docker/certs.d"
	registries := []string{"registry-1.docker.io", "docker.io", "quay.io", "ghcr.io", "registry.k8s.io"}

	for _, registry := range registries {
		registryCertDir := filepath.Join(dockerCertDir, registry)
		destPath := filepath.Join(registryCertDir, "ca.crt")
		cmd := exec.Command("sudo", "mkdir", "-p", registryCertDir)
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating directory: %v\n", err)
			continue
		}
		cmd = exec.Command("sudo", "cp", certPath, destPath)
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error copying certificate for %s: %v\n", registry, err)
			continue
		}
		fmt.Printf("  ✓ Installed for %s\n", registry)
	}

	fmt.Println("")
	fmt.Println("Restarting Docker daemon...")
	if _, err := exec.LookPath("systemctl"); err == nil {
		cmd := exec.Command("sudo", "systemctl", "restart", "docker")
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠ Failed to restart Docker: %v\n", err)
			fmt.Fprintf(os.Stderr, "  Please manually restart Docker daemon\n")
		} else {
			fmt.Println("  ✓ Docker daemon restarted")
		}
	} else {
		fmt.Println("  ⚠ Please manually restart Docker daemon")
	}
}
