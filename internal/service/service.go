package service

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// InstallService installs roxxy as a system service
func InstallService(binaryPath string) error {
	switch runtime.GOOS {
	case "darwin":
		return installLaunchAgent(binaryPath)
	case "windows":
		return installWindowsService(binaryPath)
	case "linux":
		return installSystemdService(binaryPath)
	default:
		return fmt.Errorf("service installation not supported on %s", runtime.GOOS)
	}
}

// UninstallService removes the service
func UninstallService() error {
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchAgent()
	case "windows":
		return uninstallWindowsService()
	case "linux":
		return uninstallSystemdService()
	default:
		return fmt.Errorf("service uninstallation not supported on %s", runtime.GOOS)
	}
}

// RestartService restarts the roxxy service
func RestartService() error {
	switch runtime.GOOS {
	case "darwin":
		return restartLaunchAgent()
	case "windows":
		return restartWindowsService()
	case "linux":
		return restartSystemdService()
	default:
		return fmt.Errorf("service restart not supported on %s", runtime.GOOS)
	}
}

// isInPath checks if a binary path is in the system PATH
func isInPath(binaryPath string) bool {
	// Check if it's an absolute path that's in a standard location
	absPath, err := filepath.Abs(binaryPath)
	if err != nil {
		return false
	}

	// Get PATH environment variable
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		return false
	}

	// Check each PATH entry
	for _, pathDir := range filepath.SplitList(pathEnv) {
		absPathDir, err := filepath.Abs(pathDir)
		if err != nil {
			continue
		}
		if strings.HasPrefix(absPath, absPathDir+string(filepath.Separator)) {
			return true
		}
	}

	return false
}

// macOS LaunchAgent installation
func installLaunchAgent(binaryPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	launchAgentsDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgentsDir, 0755); err != nil {
		return fmt.Errorf("failed to create LaunchAgents directory: %w", err)
	}

	plistPath := filepath.Join(launchAgentsDir, "com.roxxy.proxy.plist")

	// Get absolute path to binary
	absBinaryPath, err := filepath.Abs(binaryPath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}

	// Check if binary is in PATH
	if !isInPath(absBinaryPath) {
		log.Printf("⚠️  Warning: Binary is not in PATH: %s", absBinaryPath)
		log.Printf("   The service will use this exact path.")
		log.Printf("   Consider copying the binary to a location in PATH (e.g., /usr/local/bin/roxxy)")
		log.Printf("   and then reinstalling the service.")
		log.Printf("")
	}

	// Get log directory
	logDir := getDefaultLogDir()

	plistContent := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.roxxy.proxy</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.BinaryPath}}</string>
		<string>server</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>{{.LogDir}}/roxxy.log</string>
	<key>StandardErrorPath</key>
	<string>{{.LogDir}}/roxxy.error.log</string>
	<key>WorkingDirectory</key>
	<string>{{.HomeDir}}</string>
</dict>
</plist>`

	tmpl, err := template.New("plist").Parse(plistContent)
	if err != nil {
		return fmt.Errorf("failed to parse plist template: %w", err)
	}

	file, err := os.Create(plistPath)
	if err != nil {
		return fmt.Errorf("failed to create plist file: %w", err)
	}
	defer util.DeferClose(file)

	data := struct {
		BinaryPath string
		LogDir     string
		HomeDir    string
	}{
		BinaryPath: absBinaryPath,
		LogDir:     logDir,
		HomeDir:    home,
	}

	if err := tmpl.Execute(file, data); err != nil {
		return fmt.Errorf("failed to write plist file: %w", err)
	}

	// Load the service
	cmd := exec.Command("launchctl", "load", plistPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to load LaunchAgent: %w", err)
	}

	log.Printf("✓ Installed LaunchAgent: %s", plistPath)
	log.Printf("✓ Service started. Use 'launchctl unload %s' to stop it.", plistPath)
	return nil
}

func uninstallLaunchAgent() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.roxxy.proxy.plist")

	// Unload first
	cmd := exec.Command("launchctl", "unload", plistPath)
	util.Logerr(cmd.Run()) // Ignore error if not loaded

	// Remove plist
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove plist: %w", err)
	}

	log.Printf("✓ Uninstalled LaunchAgent")
	return nil
}

func restartLaunchAgent() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.roxxy.proxy.plist")

	// Check if plist exists
	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		return fmt.Errorf("service not installed (plist not found: %s)", plistPath)
	}

	// Try using kickstart first (macOS 10.10+), which is cleaner
	cmd := exec.Command("launchctl", "kickstart", "-k", "gui/"+os.Getenv("UID")+"/com.roxxy.proxy")
	if err := cmd.Run(); err == nil {
		// kickstart worked
		log.Printf("✓ Restarted LaunchAgent (using kickstart)")
		return nil
	}

	// Fall back to unload/load method
	// Stop (unload)
	cmd = exec.Command("launchctl", "unload", plistPath)
	if err := cmd.Run(); err != nil {
		// Log but don't fail - service might not be running
		log.Printf("Note: unload returned error (may not be running): %v", err)
	}

	// Small delay to ensure clean shutdown
	time.Sleep(500 * time.Millisecond)

	// Start (load)
	cmd = exec.Command("launchctl", "load", plistPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to restart LaunchAgent: %w", err)
	}

	log.Printf("✓ Restarted LaunchAgent")
	return nil
}

// isSystemdAvailable checks if systemd is available and working
func isSystemdAvailable() bool {
	// Check if systemctl exists
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}

	// Try a simple systemctl command to see if it works
	cmd := exec.Command("systemctl", "--version")
	if err := cmd.Run(); err != nil {
		return false
	}

	// Check if user systemd is available (for WSL compatibility)
	cmd = exec.Command("systemctl", "--user", "list-units", "--type=service", "--no-pager")
	output, err := cmd.CombinedOutput()
	// If we get "Failed to connect to bus" or similar, systemd isn't running
	if err != nil && strings.Contains(string(output), "connect to bus") {
		return false
	}

	return true
}

// Linux systemd service installation
func installSystemdService(binaryPath string) error {
	absBinaryPath, err := filepath.Abs(binaryPath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}

	// Check if systemd is available
	if !isSystemdAvailable() {
		// Check if we're in WSL
		if isWSL() {
			return fmt.Errorf(`systemd is not available in this WSL environment.

Option 1: Enable systemd in WSL
1. Update WSL: wsl --update
2. Enable systemd in /etc/wsl.conf:
   [boot]
   systemd=true
3. Restart WSL: wsl --shutdown

Option 2: Use native Windows service (recommended for WSL)
1. Install Chocolatey (Windows package manager):
   Run PowerShell as Administrator:
   Set-ExecutionPolicy Bypass -Scope Process -Force; [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor 3072; iex ((New-Object System.Net.WebClient).DownloadString('https://community.chocolatey.org/install.ps1'))

2. Install NSSM via Chocolatey:
   choco install nssm -y

3. Install native Windows binary:
   Download roxxy Windows binary and install it on Windows (not in WSL)
   Then run: roxxy service install (from Windows PowerShell/CMD)

Option 3: Run manually in WSL
You can run roxxy manually or use a process manager:
- screen: screen -dmS roxxy roxxy server
- tmux: tmux new-session -d -s roxxy 'roxxy server'
- nohup: nohup roxxy server > /dev/null 2>&1 &`)
		}
		return fmt.Errorf("systemd is not available on this system. Please install systemd or use an alternative process manager")
	}

	// Check if binary is in PATH
	if !isInPath(absBinaryPath) {
		log.Printf("⚠️  Warning: Binary is not in PATH: %s", absBinaryPath)
		log.Printf("   The service will use this exact path.")
		log.Printf("   Consider copying the binary to a location in PATH (e.g., /usr/local/bin/roxxy)")
		log.Printf("   and then reinstalling the service.")
		log.Printf("")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	logDir := getDefaultLogDir()

	// Ensure log directory exists
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Printf("Warning: Failed to create log directory %s: %v", logDir, err)
	}

	// Use user systemd service (no root required)
	// User services are stored in ~/.config/systemd/user/
	configDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create systemd user directory: %w", err)
	}

	serviceContent := fmt.Sprintf(`[Unit]
Description=Roxxy Registry Proxy
After=network.target

[Service]
Type=simple
ExecStart=%s server
Restart=always
RestartSec=10
StandardOutput=append:%s/roxxy.log
StandardError=append:%s/roxxy.error.log
WorkingDirectory=%s

[Install]
WantedBy=default.target
`, absBinaryPath, logDir, logDir, home)

	servicePath := filepath.Join(configDir, "roxxy.service")
	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("failed to write service file: %w", err)
	}

	// Reload user systemd
	cmd := exec.Command("systemctl", "--user", "daemon-reload")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to reload systemd: %w\nOutput: %s\n\nIf you're in WSL, systemd may not be enabled See error message above for instructions", err, string(output))
	}

	// Enable and start service (user mode)
	cmd = exec.Command("systemctl", "--user", "enable", "roxxy.service")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to enable service: %w\nOutput: %s", err, string(output))
	}

	cmd = exec.Command("systemctl", "--user", "start", "roxxy.service")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to start service: %w\nOutput: %s", err, string(output))
	}

	log.Printf("✓ Installed user systemd service: %s", servicePath)
	log.Printf("✓ Service started. Use 'systemctl --user status roxxy' to check status.")
	log.Printf("  Note: User services run only when the user is logged in.")
	log.Printf("  To enable lingering (run when user is not logged in), run:")
	log.Printf("  systemctl --user enable roxxy.service")
	log.Printf("  loginctl enable-linger $USER")
	return nil
}

// isWSL checks if we're running in WSL (Windows Subsystem for Linux)
func isWSL() bool {
	// Check for WSL-specific files
	if _, err := os.Stat("/proc/version"); err == nil {
		data, err := os.ReadFile("/proc/version")
		if err == nil {
			version := string(data)
			if strings.Contains(strings.ToLower(version), "microsoft") ||
				strings.Contains(strings.ToLower(version), "wsl") {
				return true
			}
		}
	}
	// Check for WSL environment variable
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" {
		return true
	}
	return false
}

func uninstallSystemdService() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	// Try user service first (no root required)
	configDir := filepath.Join(home, ".config", "systemd", "user")
	servicePath := filepath.Join(configDir, "roxxy.service")

	// Check if user service exists
	if _, err := os.Stat(servicePath); err == nil {
		// Stop and disable user service
		util.Logerr(exec.Command("systemctl", "--user", "stop", "roxxy.service").Run())
		util.Logerr(exec.Command("systemctl", "--user", "disable", "roxxy.service").Run())

		// Remove service file
		if err := os.Remove(servicePath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove service file: %w", err)
		}

		// Reload user systemd
		util.Logerr(exec.Command("systemctl", "--user", "daemon-reload").Run())

		log.Printf("✓ Uninstalled user systemd service")
		return nil
	}

	// Fall back to system service (requires root) for backwards compatibility
	if os.Geteuid() == 0 {
		systemServicePath := "/etc/systemd/system/roxxy.service"
		if _, err := os.Stat(systemServicePath); err == nil {
			// Stop and disable
			util.Logerr(exec.Command("systemctl", "stop", "roxxy.service").Run())
			util.Logerr(exec.Command("systemctl", "disable", "roxxy.service").Run())

			// Remove service file
			if err := os.Remove(systemServicePath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove service file: %w", err)
			}

			// Reload systemd
			util.Logerr(exec.Command("systemctl", "daemon-reload").Run())

			log.Printf("✓ Uninstalled system systemd service")
			return nil
		}
	}

	return fmt.Errorf("service not found (checked user and system locations)")
}

func restartSystemdService() error {
	// Try user service first (no root required)
	cmd := exec.Command("systemctl", "--user", "status", "roxxy.service")
	if err := cmd.Run(); err == nil {
		// User service exists, restart it
		cmd = exec.Command("systemctl", "--user", "restart", "roxxy.service")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to restart user service: %w", err)
		}
		log.Printf("✓ Restarted user systemd service")
		return nil
	}

	// Fall back to system service (requires root) for backwards compatibility
	if os.Geteuid() == 0 {
		cmd = exec.Command("systemctl", "status", "roxxy.service")
		if err := cmd.Run(); err == nil {
			// System service exists, restart it
			cmd = exec.Command("systemctl", "restart", "roxxy.service")
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("failed to restart system service: %w", err)
			}
			log.Printf("✓ Restarted system systemd service")
			return nil
		}
	}

	return fmt.Errorf("service not installed or not found (checked user and system locations)")
}

// Windows service installation (requires NSSM or similar)
func installWindowsService(binaryPath string) error {
	absBinaryPath, err := filepath.Abs(binaryPath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}

	// Check if binary is in PATH
	if !isInPath(absBinaryPath) {
		log.Printf("⚠️  Warning: Binary is not in PATH: %s", absBinaryPath)
		log.Printf("   The service will use this exact path.")
		log.Printf("   Consider copying the binary to a location in PATH")
		log.Printf("   and then reinstalling the service.")
		log.Printf("")
	}

	// Check if running as administrator
	if !isAdmin() {
		return fmt.Errorf("windows service installation requires administrator privileges")
	}

	// Try to use NSSM (Non-Sucking Service Manager) if available
	if nssmPath := findNSSM(); nssmPath != "" {
		return installWithNSSM(nssmPath, absBinaryPath)
	}

	// Fall back to sc.exe (requires service wrapper)
	return fmt.Errorf("NSSM not found. Please install NSSM from https://nssm.cc/ or use a service wrapper")
}

func installWithNSSM(nssmPath, binaryPath string) error {
	logDir := getDefaultLogDir()

	// Ensure log directory exists
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Printf("Warning: Failed to create log directory %s: %v", logDir, err)
	}

	// Check if service already exists
	checkCmd := exec.Command(nssmPath, "status", "Roxxy")
	if err := checkCmd.Run(); err == nil {
		// Service exists - try to remove it first
		log.Printf("Service 'Roxxy' already exists. Removing old service...")
		util.Logerr(exec.Command(nssmPath, "stop", "Roxxy").Run())
		util.Logerr(exec.Command(nssmPath, "remove", "Roxxy", "confirm").Run())
		// Give it a moment to clean up
		time.Sleep(500 * time.Millisecond)
	}

	// Install service
	cmd := exec.Command(nssmPath, "install", "Roxxy", binaryPath, "server")
	output, err := cmd.CombinedOutput()
	if err != nil {
		// NSSM exit codes:
		// 1 = general error
		// 2 = invalid parameter
		// 3 = service already exists
		// 4 = service not found
		// 5 = service already exists (alternative)
		// 6 = access denied
		if strings.Contains(string(output), "already exists") || strings.Contains(string(output), "already installed") {
			return fmt.Errorf("service 'Roxxy' already exists. Use 'roxxy service uninstall' first, or manually remove it with 'nssm remove Roxxy confirm'")
		}
		if strings.Contains(string(output), "access denied") || strings.Contains(string(output), "permission") {
			return fmt.Errorf("access denied - ensure you're running as administrator")
		}
		return fmt.Errorf("failed to install service: %w\nNSSM output: %s", err, string(output))
	}

	// Get current username for running service as current user (not SYSTEM)
	username := os.Getenv("USERNAME")
	if username == "" {
		username = os.Getenv("USER")
	}

	// Get the directory where the binary is located (for AppDirectory)
	binaryDir := filepath.Dir(binaryPath)

	// Get user's environment variables to set in NSSM
	// This ensures the service uses the user's directories even when running as SYSTEM
	userAppData := os.Getenv("APPDATA")
	userLocalAppData := os.Getenv("LOCALAPPDATA")
	userProfile := os.Getenv("USERPROFILE")
	if userProfile == "" {
		if home, err := os.UserHomeDir(); err == nil {
			userProfile = home
		}
	}

	// Configure service (basic settings first)
	configs := []struct {
		key   string
		value string
	}{
		{"AppDirectory", binaryDir}, // Set working directory to binary location
		{"AppStdout", filepath.Join(logDir, "roxxy.log")},
		{"AppStderr", filepath.Join(logDir, "roxxy.error.log")},
		{"AppRotateFiles", "1"},
		{"AppRotateBytes", "10485760"}, // 10MB
		{"Start", "SERVICE_AUTO_START"},
	}

	for _, cfg := range configs {
		cmd := exec.Command(nssmPath, "set", "Roxxy", cfg.key, cfg.value)
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Printf("Warning: Failed to set %s: %v\nNSSM output: %s", cfg.key, err, string(output))
		}
	}

	// Set environment variables so service uses user's directories even when running as SYSTEM
	// NSSM AppEnvironmentExtra can be set multiple times - each call adds to the list
	envVars := []string{}
	if userAppData != "" {
		envVars = append(envVars, fmt.Sprintf("APPDATA=%s", userAppData))
	}
	if userLocalAppData != "" {
		envVars = append(envVars, fmt.Sprintf("LOCALAPPDATA=%s", userLocalAppData))
	}
	if userProfile != "" {
		envVars = append(envVars, fmt.Sprintf("USERPROFILE=%s", userProfile))
	}

	// Set each environment variable (NSSM allows multiple AppEnvironmentExtra entries)
	for _, envVar := range envVars {
		cmd := exec.Command(nssmPath, "set", "Roxxy", "AppEnvironmentExtra", envVar)
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Printf("Warning: Failed to set environment variable %s: %v\nNSSM output: %s", envVar, err, string(output))
		} else {
			log.Printf("✓ Set environment variable: %s", envVar)
		}
	}

	// Try to set ObjectName to run as current user (optional - continue if it fails)
	// NSSM requires the format "DOMAIN\USERNAME" or just "USERNAME" for local accounts
	// If this fails, the service will run as SYSTEM (which is fine, just less ideal)
	if username != "" {
		// Try setting ObjectName - this may fail if user doesn't have permission
		// or if the format is incorrect, but that's okay, service will run as SYSTEM
		cmd := exec.Command(nssmPath, "set", "Roxxy", "ObjectName", username)
		if err := cmd.Run(); err != nil {
			log.Printf("Warning: Failed to set ObjectName to %s: %v", username, err)
			log.Printf("  Service will run as SYSTEM (this is acceptable)")
			log.Printf("  To run as your user, you can manually set it later with:")
			log.Printf("  nssm set Roxxy ObjectName %s", username)
		} else {
			log.Printf("✓ Service configured to run as user: %s", username)
		}
	}

	// Start service
	startCmd := exec.Command(nssmPath, "start", "Roxxy")
	if output, err := startCmd.CombinedOutput(); err != nil {
		// Check if service is already running
		statusCmd := exec.Command(nssmPath, "status", "Roxxy")
		if statusErr := statusCmd.Run(); statusErr == nil {
			log.Printf("✓ Service is already running")
			return nil
		}
		// Get more details about why it failed
		statusOutput, _ := exec.Command(nssmPath, "status", "Roxxy").CombinedOutput()
		return fmt.Errorf("failed to start service: %w\nNSSM start output: %s\nNSSM status output: %s\n\nTroubleshooting:\n1. Check the error log: %s\n2. Verify the binary path: %s\n3. Check service configuration: nssm edit Roxxy", err, string(output), string(statusOutput), filepath.Join(logDir, "roxxy.error.log"), binaryPath)
	}

	log.Printf("✓ Installed Windows service using NSSM")
	log.Printf("✓ Service started. Use 'nssm status Roxxy' to check status.")
	log.Printf("  Use 'nssm edit Roxxy' to configure the service further.")
	log.Printf("")
	log.Printf("Log file locations:")
	log.Printf("  Application log: %s", filepath.Join(logDir, "roxxy.log"))
	log.Printf("  Error log: %s", filepath.Join(logDir, "roxxy.error.log"))
	log.Printf("  NSSM stdout: %s (configured in service)", filepath.Join(logDir, "roxxy.log"))
	log.Printf("  NSSM stderr: %s (configured in service)", filepath.Join(logDir, "roxxy.error.log"))
	log.Printf("")
	log.Printf("If logs are empty, check:")
	log.Printf("  1. Service status: nssm status Roxxy")
	log.Printf("  2. Service output: nssm get Roxxy AppStdout")
	log.Printf("  3. Error output: nssm get Roxxy AppStderr")
	log.Printf("  4. Windows Event Viewer for service errors")
	return nil
}

func uninstallWindowsService() error {
	if !isAdmin() {
		return fmt.Errorf("windows service uninstallation requires administrator privileges")
	}

	if nssmPath := findNSSM(); nssmPath != "" {
		// Stop and remove
		util.Logerr(exec.Command(nssmPath, "stop", "Roxxy").Run())
		util.Logerr(exec.Command(nssmPath, "remove", "Roxxy", "confirm").Run())
		log.Printf("✓ Uninstalled Windows service")
		return nil
	}

	return fmt.Errorf("NSSM not found")
}

func restartWindowsService() error {
	if !isAdmin() {
		return fmt.Errorf("windows service restart requires administrator privileges")
	}

	nssmPath := findNSSM()
	if nssmPath == "" {
		return fmt.Errorf("NSSM not found")
	}

	// Check if service exists
	cmd := exec.Command(nssmPath, "status", "Roxxy")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("service not installed or not found")
	}

	// Stop service first (ensures clean restart)
	log.Printf("Stopping service...")
	stopCmd := exec.Command(nssmPath, "stop", "Roxxy")
	if output, err := stopCmd.CombinedOutput(); err != nil {
		// Check if service is already stopped
		statusCmd := exec.Command(nssmPath, "status", "Roxxy")
		if statusErr := statusCmd.Run(); statusErr != nil {
			// Service is stopped (status returns non-zero for stopped)
			log.Printf("Service is already stopped")
		} else {
			log.Printf("Warning: Stop command returned error: %v\nNSSM output: %s", err, string(output))
		}
	}

	// Wait for service to fully stop by polling status
	// NSSM status exit codes: 0=running, 1=stopped, 2=start_pending, 3=stop_pending
	log.Printf("Waiting for service to stop...")
	maxWait := 30 // Maximum 30 seconds
	for i := 0; i < maxWait; i++ {
		statusCmd := exec.Command(nssmPath, "status", "Roxxy")
		if err := statusCmd.Run(); err != nil {
			// Exit code != 0 means service is stopped (or doesn't exist, but we checked earlier)
			log.Printf("✓ Service stopped")
			break
		}
		if i == 0 {
			// Only log once
			log.Printf("  Waiting for service to stop...")
		}
		time.Sleep(1 * time.Second)
		if i == maxWait-1 {
			return fmt.Errorf("service did not stop within %d seconds", maxWait)
		}
	}

	// Start service (this will reload config on startup)
	log.Printf("Starting service...")
	startCmd := exec.Command(nssmPath, "start", "Roxxy")
	if output, err := startCmd.CombinedOutput(); err != nil {
		// Check if service is already running
		statusCmd := exec.Command(nssmPath, "status", "Roxxy")
		if statusErr := statusCmd.Run(); statusErr == nil {
			log.Printf("✓ Service is already running")
		} else {
			return fmt.Errorf("failed to start service: %w\nNSSM output: %s", err, string(output))
		}
	} else {
		log.Printf("✓ Service started")
	}

	log.Printf("✓ Restarted Windows service")
	log.Printf("  Configuration will be reloaded on service start")
	log.Printf("  Check logs at: %s", getDefaultLogDir())
	return nil
}

func findNSSM() string {
	// Check common locations
	paths := []string{
		"C:\\nssm\\nssm.exe",
		"C:\\Program Files\\nssm\\nssm.exe",
		"C:\\Program Files (x86)\\nssm\\nssm.exe",
	}

	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	// Check PATH
	if path, err := exec.LookPath("nssm.exe"); err == nil {
		return path
	}

	return ""
}

func isAdmin() bool {
	// Simple check - on Windows, try to open a system file
	if runtime.GOOS == "windows" {
		// Try to write to a system directory
		testFile := "C:\\Windows\\Temp\\roxxy-admin-test"
		file, err := os.Create(testFile)
		if err != nil {
			return false
		}
		util.Logerr(file.Close())
		util.Logerr(os.Remove(testFile))
		return true
	}
	return os.Geteuid() == 0
}

func getDefaultLogDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "./logs"
	}

	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Logs", "roxxy")
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "roxxy", "Logs")
		}
		return filepath.Join(home, "AppData", "Roaming", "roxxy", "Logs")
	default:
		if xdgState := os.Getenv("XDG_STATE_HOME"); xdgState != "" {
			return filepath.Join(xdgState, "roxxy", "logs")
		}
		return filepath.Join(home, ".local", "state", "roxxy", "logs")
	}
}
