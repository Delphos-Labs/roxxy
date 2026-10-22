package cache

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStatsTracker_IncrementAccessCount(t *testing.T) {
	tracker := NewStatsTracker("")

	contentHash := "test1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	// First access
	tracker.IncrementAccessCount(contentHash)
	count, lastAccessed := tracker.GetAccessCount(contentHash)
	if count != 1 {
		t.Errorf("GetAccessCount() count = %d, want 1", count)
	}
	if lastAccessed.IsZero() {
		t.Error("GetAccessCount() lastAccessed is zero, want non-zero")
	}

	// Second access
	time.Sleep(10 * time.Millisecond) // Ensure different timestamp
	tracker.IncrementAccessCount(contentHash)
	count, newLastAccessed := tracker.GetAccessCount(contentHash)
	if count != 2 {
		t.Errorf("GetAccessCount() count = %d, want 2", count)
	}
	if !newLastAccessed.After(lastAccessed) {
		t.Error("GetAccessCount() lastAccessed should be updated")
	}
}

func TestStatsTracker_GetAccessCount_NotFound(t *testing.T) {
	tracker := NewStatsTracker("")
	contentHash := "nonexistent1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	count, lastAccessed := tracker.GetAccessCount(contentHash)
	if count != 0 {
		t.Errorf("GetAccessCount() count = %d, want 0", count)
	}
	if !lastAccessed.IsZero() {
		t.Error("GetAccessCount() lastAccessed should be zero for non-existent hash")
	}
}

func TestStatsTracker_ConcurrentAccess(t *testing.T) {
	tracker := NewStatsTracker("")
	contentHash := "test1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	// Concurrent increments
	var wg sync.WaitGroup
	iterations := 100
	wg.Add(iterations)

	for i := 0; i < iterations; i++ {
		go func() {
			defer wg.Done()
			tracker.IncrementAccessCount(contentHash)
		}()
	}

	wg.Wait()

	count, _ := tracker.GetAccessCount(contentHash)
	if count != iterations {
		t.Errorf("GetAccessCount() count = %d, want %d", count, iterations)
	}
}

func TestStatsTracker_SaveAndLoad(t *testing.T) {
	tempDir := t.TempDir()
	statsFile := filepath.Join(tempDir, "access-stats.json")

	tracker := NewStatsTracker(statsFile)
	contentHash := "test1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	// Increment and save
	tracker.IncrementAccessCount(contentHash)
	if err := tracker.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(statsFile); os.IsNotExist(err) {
		t.Fatal("Stats file was not created")
	}

	// Create new tracker and load
	tracker2 := NewStatsTracker(statsFile)
	if err := tracker2.Load(true); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	count, _ := tracker2.GetAccessCount(contentHash)
	if count != 1 {
		t.Errorf("After Load(), GetAccessCount() count = %d, want 1", count)
	}
}

func TestStatsTracker_Save_NoChanges(t *testing.T) {
	tempDir := t.TempDir()
	statsFile := filepath.Join(tempDir, "access-stats.json")

	tracker := NewStatsTracker(statsFile)

	// Save empty stats
	if err := tracker.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Get file modification time
	info1, err := os.Stat(statsFile)
	if err != nil {
		t.Fatalf("Failed to stat file: %v", err)
	}
	modTime1 := info1.ModTime()

	// Save again (no changes)
	time.Sleep(10 * time.Millisecond)
	if err := tracker.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// File should not be modified (Save should skip if no changes)
	info2, err := os.Stat(statsFile)
	if err != nil {
		t.Fatalf("Failed to stat file: %v", err)
	}
	modTime2 := info2.ModTime()

	// Mod times should be the same (or very close) since Save skips when no changes
	if modTime2.After(modTime1.Add(100 * time.Millisecond)) {
		t.Error("Save() modified file when there were no changes")
	}
}

func TestStatsTracker_GetAllStats(t *testing.T) {
	tracker := NewStatsTracker("")

	hash1 := "hash1_1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	hash2 := "hash2_1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	tracker.IncrementAccessCount(hash1)
	tracker.IncrementAccessCount(hash1)
	tracker.IncrementAccessCount(hash2)

	allStats := tracker.GetAllStats()
	if len(allStats) != 2 {
		t.Errorf("GetAllStats() len = %d, want 2", len(allStats))
	}

	if allStats[hash1].AccessCount != 2 {
		t.Errorf("GetAllStats()[hash1].AccessCount = %d, want 2", allStats[hash1].AccessCount)
	}

	if allStats[hash2].AccessCount != 1 {
		t.Errorf("GetAllStats()[hash2].AccessCount = %d, want 1", allStats[hash2].AccessCount)
	}

	// Verify it's a copy (modifying returned map shouldn't affect tracker)
	allStats[hash1].AccessCount = 999
	count, _ := tracker.GetAccessCount(hash1)
	if count != 2 {
		t.Errorf("Modifying returned map affected tracker: count = %d, want 2", count)
	}
}

func TestStatsTracker_Load_InvalidJSON(t *testing.T) {
	tempDir := t.TempDir()
	statsFile := filepath.Join(tempDir, "access-stats.json")

	// Write invalid JSON
	if err := os.WriteFile(statsFile, []byte("invalid json"), 0644); err != nil {
		t.Fatalf("Failed to write invalid JSON: %v", err)
	}

	tracker := NewStatsTracker(statsFile)
	if err := tracker.Load(true); err == nil {
		t.Error("Load() error = nil, want error for invalid JSON")
	}
}

func TestStatsTracker_Load_FileNotExists(t *testing.T) {
	tempDir := t.TempDir()
	statsFile := filepath.Join(tempDir, "nonexistent.json")

	tracker := NewStatsTracker(statsFile)
	if err := tracker.Load(true); err != nil {
		t.Errorf("Load() error = %v, want nil for non-existent file", err)
	}
}
