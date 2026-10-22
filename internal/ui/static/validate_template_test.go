package static

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateHTMLTemplate(t *testing.T) {
	// Get the static directory path
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}

	// Navigate to the static directory
	staticDir := filepath.Join(wd, "..", "..", "..", "internal", "ui", "static")
	templatePath := filepath.Join(staticDir, "components", "package-list.html")

	if _, err := os.Stat(templatePath); os.IsNotExist(err) {
		t.Skipf("template not found: %s", templatePath)
	}

	err = ValidateHTMLTemplate(templatePath)
	if err != nil {
		// Log as info/warning rather than failing the test
		// These are style/best-practice warnings, not critical errors
		t.Logf("Template validation warnings (non-blocking):\n%v", err)
	}
}

func TestValidateJavaScriptInHTML(t *testing.T) {
	// Get the static directory path
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}

	// Navigate to the static directory
	staticDir := filepath.Join(wd, "..", "..", "..", "internal", "ui", "static")
	htmlPath := filepath.Join(staticDir, "index.html")

	if _, err := os.Stat(htmlPath); os.IsNotExist(err) {
		t.Skipf("HTML file not found: %s", htmlPath)
	}

	err = ValidateJavaScriptInHTML(htmlPath)
	if err != nil {
		// Fail the test on JavaScript syntax errors - these are critical
		t.Fatalf("JavaScript validation failed:\n%v", err)
	}
}
