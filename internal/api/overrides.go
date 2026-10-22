package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/registry"
)

// SetOverrideStore sets the override store for the API
func (a *API) SetOverrideStore(store *registry.OverrideStore) {
	a.overrideStore = store
}

// handleOverrides handles CRUD operations on /api/v1/overrides
func (a *API) handleOverrides(w http.ResponseWriter, r *http.Request) {
	if a.overrideStore == nil {
		http.Error(w, "Override store not initialized", http.StatusInternalServerError)
		return
	}

	// /api/v1/overrides/{source...} — source may contain slashes
	subpath := strings.TrimPrefix(r.URL.Path, "/api/v1/overrides")
	subpath = strings.TrimPrefix(subpath, "/")

	switch r.Method {
	case http.MethodGet:
		if subpath == "" {
			// List all overrides
			overrides := a.overrideStore.List()
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(overrides); err != nil {
				log.Printf("[overrides-api] Error encoding response: %v", err)
			}
			return
		}
		// Get a specific override
		target, ok := a.overrideStore.Lookup(subpath)
		if !ok {
			http.Error(w, "Override not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(registry.Override{Source: subpath, Target: target}); err != nil {
			log.Printf("[overrides-api] Error encoding response: %v", err)
		}

	case http.MethodPost:
		// Create an override
		var req registry.Override
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.Source == "" || req.Target == "" {
			http.Error(w, "Both 'source' and 'target' are required", http.StatusBadRequest)
			return
		}
		if err := a.overrideStore.Set(req.Source, req.Target); err != nil {
			log.Printf("[overrides-api] Error creating override: %v", err)
			http.Error(w, "Failed to create override", http.StatusInternalServerError)
			return
		}
		log.Printf("[overrides-api] Created override: %s -> %s", req.Source, req.Target)
		w.WriteHeader(http.StatusCreated)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(req); err != nil {
			log.Printf("[overrides-api] Error encoding response: %v", err)
		}

	case http.MethodPut:
		// Update an override — source comes from URL path
		if subpath == "" {
			http.Error(w, "Source must be specified in URL path", http.StatusBadRequest)
			return
		}
		var req struct {
			Target string `json:"target"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.Target == "" {
			http.Error(w, "'target' is required", http.StatusBadRequest)
			return
		}
		if err := a.overrideStore.Set(subpath, req.Target); err != nil {
			log.Printf("[overrides-api] Error updating override: %v", err)
			http.Error(w, "Failed to update override", http.StatusInternalServerError)
			return
		}
		log.Printf("[overrides-api] Updated override: %s -> %s", subpath, req.Target)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(registry.Override{Source: subpath, Target: req.Target}); err != nil {
			log.Printf("[overrides-api] Error encoding response: %v", err)
		}

	case http.MethodDelete:
		if subpath == "" {
			http.Error(w, "Source must be specified in URL path", http.StatusBadRequest)
			return
		}
		deleted, err := a.overrideStore.Delete(subpath)
		if err != nil {
			log.Printf("[overrides-api] Error deleting override: %v", err)
			http.Error(w, "Failed to delete override", http.StatusInternalServerError)
			return
		}
		if !deleted {
			http.Error(w, "Override not found", http.StatusNotFound)
			return
		}
		log.Printf("[overrides-api] Deleted override: %s", subpath)
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
