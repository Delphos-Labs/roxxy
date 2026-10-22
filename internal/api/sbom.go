package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/sbom"
)

// SBOMManagerInterface is an interface for SBOM session management
type SBOMManagerInterface interface {
	StartSession(name string, ip string) (string, error)
	StopSession(name string, ip string) (*sbom.Session, error)
	GetSession(ip string) interface{}                            // Returns *sbom.Session or nil
	AddArtifact(ip string, artifact interface{})                 // Artifact should be sbom.Artifact
	ListSessions() map[string][]string                           // Returns name -> list of IPs
	RemoveSession(name string, ip string) (*sbom.Session, error) // Remove name or IP from name
}

// SetSBOMManager sets the SBOM session manager
func (a *API) SetSBOMManager(manager SBOMManagerInterface) {
	a.sbomManager = manager
}

// getClientIP extracts the client IP address from the request
// Handles X-Forwarded-For header and RemoteAddr
// Normalizes localhost IPs (127.0.0.1 and ::1) to "127.0.0.1" for consistency
func getClientIP(r *http.Request) string {
	var ip string

	// Check X-Forwarded-For header first (for proxies)
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded != "" {
		// X-Forwarded-For can contain multiple IPs, take the first one
		ips := strings.Split(forwarded, ",")
		if len(ips) > 0 {
			ip = strings.TrimSpace(ips[0])
			// Remove port if present
			if host, _, err := net.SplitHostPort(ip); err == nil {
				ip = host
			}
		}
	}

	// Fall back to RemoteAddr if no X-Forwarded-For
	if ip == "" {
		ip = r.RemoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}
	}

	// Normalize localhost IPs: both IPv4 (127.0.0.1) and IPv6 (::1) map to 127.0.0.1
	if ip == "::1" || ip == "[::1]" {
		return "127.0.0.1"
	}

	return ip
}

// handleSBOMStart starts a new SBOM capture session
// POST /api/v1/sbom/start
// Request body: {"name": "session-name"}
func (a *API) handleSBOMStart(w http.ResponseWriter, r *http.Request) {
	if !validateMethodPOST(w, r) {
		return
	}

	if a.sbomManager == nil {
		http.Error(w, "SBOM tracking not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Name string `json:"name"`
	}

	// Decode request body
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
			return
		}
	}

	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	clientIP := getClientIP(r)
	// Note: IP will be normalized in SessionManager, but log the normalized version for consistency
	normalizedIP := clientIP
	if normalizedIP == "::1" || normalizedIP == "[::1]" {
		normalizedIP = "127.0.0.1"
	}
	log.Printf("[sbom] Starting session for name '%s' from IP %s (RemoteAddr: %s, X-Forwarded-For: %s)", req.Name, normalizedIP, r.RemoteAddr, r.Header.Get("X-Forwarded-For"))
	sessionID, err := a.sbomManager.StartSession(req.Name, clientIP)
	if err != nil {
		log.Printf("[sbom] Failed to start session for name '%s' and IP %s: %v", req.Name, normalizedIP, err)
		http.Error(w, fmt.Sprintf("Failed to start SBOM session: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("[sbom] Started session %s for name '%s' and IP %s", sessionID, req.Name, normalizedIP)

	response := map[string]interface{}{
		"session_id": sessionID,
		"name":       req.Name,
		"ip":         clientIP,
		"status":     "started",
	}
	writeJSON(w, response)
}

// handleSBOMStop stops the SBOM capture session and returns the SBOM
// POST /api/v1/sbom/stop
// Request body: {"name": "session-name"}
// Returns SBOM JSON only when the last IP is removed
func (a *API) handleSBOMStop(w http.ResponseWriter, r *http.Request) {
	if !validateMethodPOST(w, r) {
		return
	}

	if a.sbomManager == nil {
		http.Error(w, "SBOM tracking not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Name string `json:"name"`
	}

	// Decode request body
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
			return
		}
	}

	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	clientIP := getClientIP(r)
	// Note: IP will be normalized in SessionManager, but log the normalized version for consistency
	normalizedIP := clientIP
	if normalizedIP == "::1" || normalizedIP == "[::1]" {
		normalizedIP = "127.0.0.1"
	}
	session, err := a.sbomManager.StopSession(req.Name, clientIP)
	if err != nil {
		log.Printf("[sbom] Failed to stop session for name '%s' and IP %s: %v", req.Name, normalizedIP, err)
		http.Error(w, fmt.Sprintf("Failed to stop SBOM session: %v", err), http.StatusInternalServerError)
		return
	}

	if session == nil {
		// Check if session exists to determine if it was already removed or just not the last IP
		if listSessions, ok := a.sbomManager.(interface{ ListSessions() map[string][]string }); ok {
			activeSessions := listSessions.ListSessions()
			if _, exists := activeSessions[req.Name]; exists {
				// Session still exists, this was not the last IP
				log.Printf("[sbom] Removed IP %s from name '%s' (session still active)", normalizedIP, req.Name)
			} else {
				// Session doesn't exist, it was already stopped/removed
				log.Printf("[sbom] Stop called for name '%s' and IP %s, but session was already removed", req.Name, normalizedIP)
			}
		} else {
			// Fallback: assume session still active if we can't check
			log.Printf("[sbom] Removed IP %s from name '%s' (session still active)", normalizedIP, req.Name)
		}
		response := map[string]interface{}{
			"name":   req.Name,
			"ip":     clientIP,
			"status": "removed",
		}
		writeJSON(w, response)
		return
	}

	// Last IP removed - but don't return SBOM here, just acknowledge removal
	// Use /api/v1/sbom/output to get the SBOM after all IPs are removed
	log.Printf("[sbom] Removed last IP %s from name '%s' (session ready for output, %d artifacts captured)", normalizedIP, req.Name, len(session.Artifacts))
	response := map[string]interface{}{
		"name":    req.Name,
		"ip":      clientIP,
		"status":  "removed",
		"message": "All IPs removed. Use /api/v1/sbom/output to retrieve the SBOM.",
	}
	writeJSON(w, response)
}

// handleSBOMList lists all in-progress SBOM sessions
// GET /api/v1/sbom/list
// Returns a map of name -> list of IPs
func (a *API) handleSBOMList(w http.ResponseWriter, r *http.Request) {
	if !validateMethodGET(w, r) {
		return
	}

	if a.sbomManager == nil {
		http.Error(w, "SBOM tracking not available", http.StatusServiceUnavailable)
		return
	}

	sessions := a.sbomManager.ListSessions()
	writeJSON(w, sessions)
}

// handleSBOMOutput outputs the SBOM for a session after validating all IPs are removed
// POST /api/v1/sbom/output
// Request body: {"name": "session-name"}
// Returns SBOM JSON and cleans up the session
func (a *API) handleSBOMOutput(w http.ResponseWriter, r *http.Request) {
	if !validateMethodPOST(w, r) {
		return
	}

	if a.sbomManager == nil {
		http.Error(w, "SBOM tracking not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Name string `json:"name"`
	}

	// Decode request body
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
			return
		}
	}

	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	// Check if session exists and all IPs are removed
	getSessionForOutput, ok := a.sbomManager.(interface {
		GetSessionForOutput(name string) (*sbom.Session, bool)
	})
	if !ok {
		http.Error(w, "SBOM manager does not support output validation", http.StatusInternalServerError)
		return
	}

	session, ready := getSessionForOutput.GetSessionForOutput(req.Name)
	if !ready {
		if session == nil {
			// Session doesn't exist
			http.Error(w, fmt.Sprintf("Session '%s' not found", req.Name), http.StatusNotFound)
			return
		}
		// Session exists but still has active IPs
		if listSessions, ok := a.sbomManager.(interface{ ListSessions() map[string][]string }); ok {
			activeSessions := listSessions.ListSessions()
			if ips, exists := activeSessions[req.Name]; exists {
				http.Error(w, fmt.Sprintf("Session '%s' still has active IPs: %v. Remove all IPs before output.", req.Name, ips), http.StatusBadRequest)
				return
			}
		}
		http.Error(w, fmt.Sprintf("Session '%s' still has active IPs. Remove all IPs before output.", req.Name), http.StatusBadRequest)
		return
	}

	// All IPs removed, generate and return SBOM
	log.Printf("[sbom] Outputting SBOM for session %s (name '%s'), captured %d artifacts", session.ID, req.Name, len(session.Artifacts))

	// Generate CycloneDX SBOM
	bom, err := sbom.GenerateCycloneDX(session)
	if err != nil {
		log.Printf("[sbom] Failed to generate SBOM for session %s: %v", session.ID, err)
		http.Error(w, fmt.Sprintf("Failed to generate SBOM: %v", err), http.StatusInternalServerError)
		return
	}

	// Convert to JSON
	jsonData, err := bom.ToJSON()
	if err != nil {
		log.Printf("[sbom] Failed to serialize SBOM for session %s: %v", session.ID, err)
		http.Error(w, fmt.Sprintf("Failed to serialize SBOM: %v", err), http.StatusInternalServerError)
		return
	}

	// Clean up the session after output
	if removeSession, ok := a.sbomManager.(interface {
		RemoveSession(name string, ip string) (*sbom.Session, error)
	}); ok {
		_, err := removeSession.RemoveSession(req.Name, "")
		if err != nil {
			log.Printf("[sbom] Warning: Failed to clean up session '%s' after output: %v", req.Name, err)
		} else {
			log.Printf("[sbom] Cleaned up session '%s' after output", req.Name)
		}
	}

	// Return SBOM as JSON
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(jsonData); err != nil {
		log.Printf("Error writing response: %v", err)
	}
}

// handleSBOMRemove removes a name or a specific IP from a name
// POST /api/v1/sbom/rm
// Request body: {"name": "session-name"} or {"name": "session-name", "ip": "1.2.3.4"}
// If removing an IP and it's the last one, returns the SBOM JSON
func (a *API) handleSBOMRemove(w http.ResponseWriter, r *http.Request) {
	if !validateMethodPOST(w, r) {
		return
	}

	if a.sbomManager == nil {
		http.Error(w, "SBOM tracking not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Name string `json:"name"`
		IP   string `json:"ip"`
	}

	// Decode request body
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
			return
		}
	}

	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	session, err := a.sbomManager.RemoveSession(req.Name, req.IP)
	if err != nil {
		log.Printf("[sbom] Failed to remove session for name '%s' (IP: %s): %v", req.Name, req.IP, err)
		http.Error(w, fmt.Sprintf("Failed to remove SBOM session: %v", err), http.StatusInternalServerError)
		return
	}

	if session == nil {
		// Not the last IP or just removed an IP, return success
		if req.IP != "" {
			log.Printf("[sbom] Removed IP %s from name '%s' (session still active)", req.IP, req.Name)
		} else {
			log.Printf("[sbom] Removed name '%s'", req.Name)
		}
		response := map[string]interface{}{
			"name":   req.Name,
			"ip":     req.IP,
			"status": "removed",
		}
		writeJSON(w, response)
		return
	}

	// Last IP removed, generate and return SBOM
	log.Printf("[sbom] Removed last IP from name '%s', captured %d artifacts", req.Name, len(session.Artifacts))

	// Generate CycloneDX SBOM
	bom, err := sbom.GenerateCycloneDX(session)
	if err != nil {
		log.Printf("[sbom] Failed to generate SBOM for session %s: %v", session.ID, err)
		http.Error(w, fmt.Sprintf("Failed to generate SBOM: %v", err), http.StatusInternalServerError)
		return
	}

	// Convert to JSON
	jsonData, err := bom.ToJSON()
	if err != nil {
		log.Printf("[sbom] Failed to serialize SBOM for session %s: %v", session.ID, err)
		http.Error(w, fmt.Sprintf("Failed to serialize SBOM: %v", err), http.StatusInternalServerError)
		return
	}

	// Return SBOM as JSON
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(jsonData); err != nil {
		log.Printf("Error writing response: %v", err)
	}
}
