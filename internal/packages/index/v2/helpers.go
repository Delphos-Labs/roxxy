package v2

import (
	"strings"
)

// DecodeGoModulePath decodes Go module proxy path encoding (exported)
// The Go module proxy encodes uppercase letters as ! followed by lowercase
// e.g., !proton!mail -> ProtonMail, !microsoft -> Microsoft
func DecodeGoModulePath(path string) string {
	return decodeGoModulePath(path)
}

// decodeGoModulePath decodes Go module proxy path encoding (internal)
func decodeGoModulePath(path string) string {
	var result strings.Builder
	i := 0
	for i < len(path) {
		if path[i] == '!' && i+1 < len(path) {
			// Check if next character is lowercase
			next := path[i+1]
			if next >= 'a' && next <= 'z' {
				// Convert to uppercase
				result.WriteByte(next - ('a' - 'A'))
				i += 2
				continue
			}
		}
		result.WriteByte(path[i])
		i++
	}
	return result.String()
}

// getGoBaseModulePath strips /vN suffix from Go module path for grouping
// e.g., "github.com/Microsoft/go-winio/v5" -> "github.com/Microsoft/go-winio"
// e.g., "github.com/Microsoft/go-winio" -> "github.com/Microsoft/go-winio"
func getGoBaseModulePath(modulePath string) string {
	parts := strings.Split(modulePath, "/")
	if len(parts) == 0 {
		return modulePath
	}

	lastPart := parts[len(parts)-1]
	// Check if last part matches /vN pattern (where N is a digit)
	if strings.HasPrefix(lastPart, "v") && len(lastPart) > 1 {
		isVersionSuffix := true
		for i := 1; i < len(lastPart); i++ {
			if lastPart[i] < '0' || lastPart[i] > '9' {
				isVersionSuffix = false
				break
			}
		}
		if isVersionSuffix {
			// Remove the /vN part
			return strings.Join(parts[:len(parts)-1], "/")
		}
	}
	return modulePath
}
