package sbom

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// Session represents an active SBOM capture session
type Session struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"` // Common name for the session
	IPs       []string   `json:"ips"`  // List of IPs contributing to this session
	StartedAt time.Time  `json:"started_at"`
	Artifacts []Artifact `json:"artifacts"`
}

// Artifact represents a tracked artifact (package or Docker image)
type Artifact struct {
	Type         string    `json:"type"`                   // "package" or "docker"
	PackageType  string    `json:"package_type,omitempty"` // "apk", "apt", "npm", "pypi", "go"
	Name         string    `json:"name"`
	Version      string    `json:"version,omitempty"`
	Digest       string    `json:"digest,omitempty"` // For Docker images
	Architecture string    `json:"architecture,omitempty"`
	License      string    `json:"license,omitempty"`
	Description  string    `json:"description,omitempty"`
	URL          string    `json:"url,omitempty"`
	PulledAt     time.Time `json:"pulled_at"`
}

// SessionManager manages active SBOM capture sessions
type SessionManager struct {
	// name -> Session (shared session for all IPs contributing to that name)
	sessions map[string]*Session
	// name -> IP -> count (tracks how many times each IP has started for each name)
	nameIPs map[string]map[string]int
	// IP -> name (maps IP to the name it's contributing to)
	ipToName map[string]string
	mutex    sync.RWMutex
}

// NewSessionManager creates a new session manager
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*Session),
		nameIPs:  make(map[string]map[string]int),
		ipToName: make(map[string]string),
	}
}

// normalizeIP normalizes localhost IPs (127.0.0.1 and ::1) to "127.0.0.1" for consistency
func normalizeIP(ip string) string {
	if ip == "::1" || ip == "[::1]" {
		return "127.0.0.1"
	}
	return ip
}

// StartSession starts a new SBOM capture session for the given name and IP
// If the name doesn't exist, creates a new session. If it exists, increments the count for this IP.
// Returns the session ID
func (sm *SessionManager) StartSession(name string, ip string) (string, error) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	// Normalize IP to ensure consistency (::1 -> 127.0.0.1)
	ip = normalizeIP(ip)

	// Check if IP is already registered to a different name
	if existingName, exists := sm.ipToName[ip]; exists && existingName != name {
		return "", fmt.Errorf("IP %s is already registered to name '%s'", ip, existingName)
	}

	// Check if name already exists
	session, exists := sm.sessions[name]
	if !exists {
		// Create new session for this name
		sessionID := generateSessionID()
		session = &Session{
			ID:        sessionID,
			Name:      name,
			IPs:       []string{ip},
			StartedAt: time.Now(),
			Artifacts: make([]Artifact, 0),
		}
		sm.sessions[name] = session
		sm.nameIPs[name] = make(map[string]int)
		sm.nameIPs[name][ip] = 1
		sm.ipToName[ip] = name
		return sessionID, nil
	}

	// Increment count for this IP (don't dedupe - allow multiple starts from same IP)
	sm.nameIPs[name][ip]++

	// Add IP to session IPs list if not already present
	ipInList := false
	for _, existingIP := range session.IPs {
		if existingIP == ip {
			ipInList = true
			break
		}
	}
	if !ipInList {
		session.IPs = append(session.IPs, ip)
		sm.ipToName[ip] = name
	}

	return session.ID, nil
}

// StopSession stops the session for the given name and IP
// Decrements the count for this IP. If this is the last stop (all IPs at 0), returns the session (for SBOM generation)
// Otherwise returns nil
func (sm *SessionManager) StopSession(name string, ip string) (*Session, error) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	// Normalize IP to ensure consistency (::1 -> 127.0.0.1)
	ip = normalizeIP(ip)

	session, exists := sm.sessions[name]
	if !exists {
		return nil, nil // No active session for this name
	}

	// Check if IP is registered to this name
	count, ipExists := sm.nameIPs[name][ip]
	if !ipExists || count <= 0 {
		return nil, fmt.Errorf("IP %s is not registered to name '%s' or count is already 0", ip, name)
	}

	// Decrement count for this IP
	sm.nameIPs[name][ip]--

	// If count reaches 0, remove IP from mapping (but keep in session.IPs list for reference)
	if sm.nameIPs[name][ip] == 0 {
		delete(sm.nameIPs[name], ip)
		delete(sm.ipToName, ip)
	}

	// Check if all IPs have been stopped (all counts are 0 or map is empty)
	// Don't delete the session here - keep it for output endpoint to retrieve
	if len(sm.nameIPs[name]) == 0 {
		// All IPs removed, but keep session for output endpoint
		// Clear nameIPs entry but keep session
		delete(sm.nameIPs, name)
		return session, nil
	}

	// Not the last IP, return nil
	return nil, nil
}

// AddArtifact adds an artifact to the session for the given IP
// Uses IP->name mapping to find the correct session
// Implements packages.SBOMManagerInterface
func (sm *SessionManager) AddArtifact(ip string, artifact interface{}) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	// Normalize IP to ensure consistency (::1 -> 127.0.0.1)
	ip = normalizeIP(ip)

	// Find the name for this IP
	name, exists := sm.ipToName[ip]
	if !exists {
		log.Printf("[sbom] AddArtifact: IP %s not found in ipToName mapping (available IPs: %v)", ip, sm.getAllIPs())
		return // No active session for this IP, ignore
	}

	session, exists := sm.sessions[name]
	if !exists {
		log.Printf("[sbom] AddArtifact: Session for name '%s' not found (IP: %s)", name, ip)
		return // Session doesn't exist, ignore
	}

	// Cast artifact to Artifact type
	art, ok := artifact.(Artifact)
	if !ok {
		log.Printf("[sbom] AddArtifact: Invalid artifact type for IP %s", ip)
		return // Invalid artifact type, ignore
	}

	art.PulledAt = time.Now()
	session.Artifacts = append(session.Artifacts, art)
	log.Printf("[sbom] AddArtifact: Added artifact %s@%s to session '%s' (IP: %s, total artifacts: %d)", art.Name, art.Version, name, ip, len(session.Artifacts))
}

// getAllIPs returns all IPs currently registered (for debugging)
func (sm *SessionManager) getAllIPs() []string {
	ips := make([]string, 0, len(sm.ipToName))
	for ip := range sm.ipToName {
		ips = append(ips, ip)
	}
	return ips
}

// GetSession returns the active session for the given IP, or nil if none exists
// Uses IP->name mapping to find the correct session
// Implements packages.SBOMManagerInterface
func (sm *SessionManager) GetSession(ip string) interface{} {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	// Normalize IP to ensure consistency (::1 -> 127.0.0.1)
	ip = normalizeIP(ip)

	name, exists := sm.ipToName[ip]
	if !exists {
		return nil
	}

	return sm.sessions[name]
}

// GetSessionForOutput returns the session for a given name if all IPs have been removed
// Returns the session and true if ready for output, or nil and false if not ready
func (sm *SessionManager) GetSessionForOutput(name string) (*Session, bool) {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	session, exists := sm.sessions[name]
	if !exists {
		return nil, false
	}

	// Check if all IPs have been removed
	// If nameIPs[name] doesn't exist or is empty, all IPs have been removed
	if ipCounts, exists := sm.nameIPs[name]; exists && len(ipCounts) > 0 {
		// Still has active IPs
		return nil, false
	}

	// All IPs removed, ready for output
	return session, true
}

// ListSessions returns a map of name -> list of IPs with their counts for all active sessions
func (sm *SessionManager) ListSessions() map[string][]string {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	result := make(map[string][]string)
	for name, ipCounts := range sm.nameIPs {
		ips := make([]string, 0, len(ipCounts))
		for ip, count := range ipCounts {
			if count > 0 {
				if count > 1 {
					ips = append(ips, fmt.Sprintf("%s (count: %d)", ip, count))
				} else {
					ips = append(ips, ip)
				}
			}
		}
		result[name] = ips
	}
	return result
}

// RemoveSession removes a name or a specific IP from a name
// If removing an IP and it's the last one, returns the session for SBOM generation
func (sm *SessionManager) RemoveSession(name string, ip string) (*Session, error) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	// Normalize IP to ensure consistency (::1 -> 127.0.0.1)
	if ip != "" {
		ip = normalizeIP(ip)
	}

	if ip == "" {
		// Remove entire name
		session, exists := sm.sessions[name]
		if !exists {
			return nil, fmt.Errorf("name '%s' not found", name)
		}

		// Remove all IPs from ipToName
		for ip := range sm.nameIPs[name] {
			delete(sm.ipToName, ip)
		}

		delete(sm.sessions, name)
		delete(sm.nameIPs, name)
		return session, nil
	}

	// Remove specific IP from name (same logic as StopSession but we already have the lock)
	session, exists := sm.sessions[name]
	if !exists {
		return nil, fmt.Errorf("name '%s' not found", name)
	}

	// Check if IP is registered to this name
	count, ipExists := sm.nameIPs[name][ip]
	if !ipExists || count <= 0 {
		return nil, fmt.Errorf("IP %s is not registered to name '%s' or count is already 0", ip, name)
	}

	// Remove IP completely (set count to 0 and remove from mapping)
	delete(sm.nameIPs[name], ip)
	delete(sm.ipToName, ip)

	// Update session IPs list
	newIPs := make([]string, 0, len(session.IPs))
	for _, existingIP := range session.IPs {
		if existingIP != ip {
			newIPs = append(newIPs, existingIP)
		}
	}
	session.IPs = newIPs

	// If this was the last IP, return the session and clean up
	if len(sm.nameIPs[name]) == 0 {
		delete(sm.sessions, name)
		delete(sm.nameIPs, name)
		return session, nil
	}

	// Not the last IP, return nil
	return nil, nil
}

// generateSessionID generates a unique session ID
func generateSessionID() string {
	b := make([]byte, 16)
	util.LogerrT(rand.Read(b))
	return hex.EncodeToString(b)
}
