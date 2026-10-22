package api

import (
	"net/http"
)

// GetBuildTime returns the build time (set from main package)
var GetBuildTime = func() string { return "unknown" }

// SetBuildTime sets the build time (called from main package)
func SetBuildTime(buildTime string) {
	GetBuildTime = func() string { return buildTime }
}

// handleBuildInfo handles the build info endpoint
func (a *API) handleBuildInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, map[string]interface{}{
		"build_time": GetBuildTime(),
	})
}
