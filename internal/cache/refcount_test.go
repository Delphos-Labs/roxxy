package cache

import (
	"os"
	"testing"
)

func TestIncrementRefcount(t *testing.T) {
	cacheDir := t.TempDir()
	contentHash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	// First increment
	if err := IncrementRefcount(cacheDir, contentHash); err != nil {
		t.Fatalf("IncrementRefcount() error = %v", err)
	}

	// Verify count is 1
	refcountPath := GetRefcountPath(cacheDir, contentHash)
	data, err := os.ReadFile(refcountPath)
	if err != nil {
		t.Fatalf("Failed to read refcount file: %v", err)
	}
	if string(data) != "1" {
		t.Errorf("Refcount = %q, want %q", string(data), "1")
	}

	// Second increment
	if err := IncrementRefcount(cacheDir, contentHash); err != nil {
		t.Fatalf("IncrementRefcount() error = %v", err)
	}

	// Verify count is 2
	data, err = os.ReadFile(refcountPath)
	if err != nil {
		t.Fatalf("Failed to read refcount file: %v", err)
	}
	if string(data) != "2" {
		t.Errorf("Refcount = %q, want %q", string(data), "2")
	}
}

func TestDecrementRefcount(t *testing.T) {
	cacheDir := t.TempDir()
	contentHash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	// Increment to 2
	if err := IncrementRefcount(cacheDir, contentHash); err != nil {
		t.Fatalf("IncrementRefcount() error = %v", err)
	}
	if err := IncrementRefcount(cacheDir, contentHash); err != nil {
		t.Fatalf("IncrementRefcount() error = %v", err)
	}

	// Decrement once
	shouldDelete, err := DecrementRefcount(cacheDir, contentHash)
	if err != nil {
		t.Fatalf("DecrementRefcount() error = %v", err)
	}
	if shouldDelete {
		t.Error("DecrementRefcount() shouldDelete = true, want false")
	}

	// Verify count is 1
	refcountPath := GetRefcountPath(cacheDir, contentHash)
	data, err := os.ReadFile(refcountPath)
	if err != nil {
		t.Fatalf("Failed to read refcount file: %v", err)
	}
	if string(data) != "1" {
		t.Errorf("Refcount = %q, want %q", string(data), "1")
	}

	// Decrement to 0
	shouldDelete, err = DecrementRefcount(cacheDir, contentHash)
	if err != nil {
		t.Fatalf("DecrementRefcount() error = %v", err)
	}
	if !shouldDelete {
		t.Error("DecrementRefcount() shouldDelete = false, want true")
	}

	// New semantics: refcount file is kept at "0" rather than deleted,
	// so a quick re-Increment can rebuild without re-fetching the blob.
	// Orphan reclamation is deferred to `docker prune-blobs` / audit.
	data, err = os.ReadFile(refcountPath)
	if err != nil {
		t.Fatalf("Refcount file should be kept at 0, but read failed: %v", err)
	}
	if string(data) != "0" {
		t.Errorf("Refcount at zero state = %q, want %q", string(data), "0")
	}
}

func TestDecrementRefcount_NotFound(t *testing.T) {
	cacheDir := t.TempDir()
	contentHash := "nonexistent1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	shouldDelete, err := DecrementRefcount(cacheDir, contentHash)
	if err == nil {
		t.Error("DecrementRefcount() error = nil, want error")
	}
	if shouldDelete {
		t.Error("DecrementRefcount() shouldDelete = true, want false")
	}
}
