package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// handlePackageDetail handles the package detail endpoint
func (a *API) handlePackageDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		a.handleDeletePackage(w, r)
		return
	}

	if !validateMethodGET(w, r) {
		return
	}

	// Extract package ID from path
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/packages/")
	if path == "" {
		http.Error(w, "Package ID required", http.StatusBadRequest)
		return
	}

	// For now, package ID is the cache path
	// In the future, we could use a hash or UUID
	entries, err := a.listPackages("", "", true) // Include uncached for detail view
	if err != nil {
		http.Error(w, fmt.Sprintf("Error listing packages: %v", err), http.StatusInternalServerError)
		return
	}

	for _, entry := range entries {
		if entry.ID == path {
			writeJSON(w, entry)
			return
		}
	}

	http.Error(w, "Package not found", http.StatusNotFound)
}

// handleDeletePackage handles package deletion
func (a *API) handleDeletePackage(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/packages/")
	if path == "" {
		http.Error(w, "Package ID required", http.StatusBadRequest)
		return
	}

	// Delete the package file and metadata
	// For now, path is the cache path
	if err := deletePackage(path); err != nil {
		http.Error(w, fmt.Sprintf("Error deleting package: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// deletePackage deletes a package file and its metadata
func deletePackage(path string) error {
	// Delete the package file
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}

	// Delete metadata file
	metaPath := path + ".meta"
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

// handleBulkDeletePackages handles bulk package deletion
func (a *API) handleBulkDeletePackages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer util.DeferClose(r.Body)

	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if len(req.IDs) == 0 {
		http.Error(w, "No package IDs provided", http.StatusBadRequest)
		return
	}

	// Delete each package
	var deleted []string
	var failed []string
	for _, id := range req.IDs {
		// ID is the cache path
		if err := deletePackage(id); err != nil {
			failed = append(failed, id)
		} else {
			deleted = append(deleted, id)
		}
	}

	writeJSON(w, map[string]interface{}{
		"deleted": deleted,
		"failed":  failed,
	})
}
