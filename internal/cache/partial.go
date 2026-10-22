package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// ByteRange represents a range of bytes in a file
type ByteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"` // inclusive
}

// PartialCacheMetadata tracks which ranges we have for a partially downloaded file
type PartialCacheMetadata struct {
	URL          string      `json:"url"`
	TotalSize    int64       `json:"total_size"` // -1 if unknown
	ContentType  string      `json:"content_type"`
	Ranges       []ByteRange `json:"ranges"`        // Sorted, non-overlapping ranges we have
	ExpectedHash string      `json:"expected_hash"` // Expected content hash (e.g., from Docker digest)
	mu           sync.RWMutex
}

// GetPartialCachePath returns the directory for partial cache of a URL
func GetPartialCachePath(cacheDir string, urlHash string) string {
	return filepath.Join(cacheDir, "partial", urlHash[:2], urlHash[2:4], urlHash)
}

// GetPartialMetadataPath returns the path to the metadata file
func GetPartialMetadataPath(cacheDir string, urlHash string) string {
	return filepath.Join(GetPartialCachePath(cacheDir, urlHash), "metadata.json")
}

// GetPartialChunkPath returns the path for a specific chunk
func GetPartialChunkPath(cacheDir string, urlHash string, start, end int64) string {
	return filepath.Join(GetPartialCachePath(cacheDir, urlHash), fmt.Sprintf("bytes-%d-%d", start, end))
}

// LoadPartialMetadata loads metadata for a partially cached file
func LoadPartialMetadata(cacheDir string, urlHash string) (*PartialCacheMetadata, error) {
	metadataPath := GetPartialMetadataPath(cacheDir, urlHash)
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, err
	}

	var metadata PartialCacheMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}

	return &metadata, nil
}

// SavePartialMetadata saves metadata for a partially cached file
func (m *PartialCacheMetadata) Save(cacheDir string, urlHash string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	metadataPath := GetPartialMetadataPath(cacheDir, urlHash)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0755); err != nil {
		return err
	}

	// Atomic write using temp file + rename
	tempFile, err := os.CreateTemp(filepath.Dir(metadataPath), ".metadata-*.tmp")
	if err != nil {
		return err
	}
	defer util.DeferRemove(tempFile.Name())

	if _, err := tempFile.Write(data); err != nil {
		util.Logerr(tempFile.Close())
		return err
	}

	util.Logerr(tempFile.Close())
	return os.Rename(tempFile.Name(), metadataPath)
}

// AddRange adds a completed range to the metadata
func (m *PartialCacheMetadata) AddRange(start, end int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	newRange := ByteRange{Start: start, End: end}
	m.Ranges = append(m.Ranges, newRange)

	// Merge overlapping/adjacent ranges and sort
	m.mergeRanges()
}

// mergeRanges merges overlapping or adjacent ranges (must hold lock)
func (m *PartialCacheMetadata) mergeRanges() {
	if len(m.Ranges) <= 1 {
		return
	}

	// Sort by start position
	sort.Slice(m.Ranges, func(i, j int) bool {
		return m.Ranges[i].Start < m.Ranges[j].Start
	})

	// Merge overlapping/adjacent ranges
	merged := []ByteRange{m.Ranges[0]}
	for i := 1; i < len(m.Ranges); i++ {
		current := &merged[len(merged)-1]
		next := m.Ranges[i]

		// Check if ranges overlap or are adjacent
		if next.Start <= current.End+1 {
			// Merge by extending current range
			if next.End > current.End {
				current.End = next.End
			}
		} else {
			// No overlap, add as new range
			merged = append(merged, next)
		}
	}

	m.Ranges = merged
}

// HasRange checks if we have a specific range
func (m *PartialCacheMetadata) HasRange(start, end int64) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, r := range m.Ranges {
		if r.Start <= start && r.End >= end {
			return true
		}
	}
	return false
}

// GetMissingRanges returns ranges we don't have yet
func (m *PartialCacheMetadata) GetMissingRanges() []ByteRange {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.TotalSize <= 0 {
		return nil // Unknown size, can't determine missing ranges
	}

	var missing []ByteRange
	pos := int64(0)

	for _, r := range m.Ranges {
		if pos < r.Start {
			// Gap before this range
			missing = append(missing, ByteRange{Start: pos, End: r.Start - 1})
		}
		pos = r.End + 1
	}

	// Check if there's a gap at the end
	if pos < m.TotalSize {
		missing = append(missing, ByteRange{Start: pos, End: m.TotalSize - 1})
	}

	return missing
}

// IsComplete checks if we have all ranges
func (m *PartialCacheMetadata) IsComplete() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.TotalSize <= 0 {
		return false // Unknown size
	}

	if len(m.Ranges) != 1 {
		return false // Not merged into single range
	}

	return m.Ranges[0].Start == 0 && m.Ranges[0].End == m.TotalSize-1
}

// GetCompletionPercentage returns download completion percentage
func (m *PartialCacheMetadata) GetCompletionPercentage() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.TotalSize <= 0 {
		return 0
	}

	var downloaded int64
	for _, r := range m.Ranges {
		downloaded += (r.End - r.Start + 1)
	}

	return float64(downloaded) / float64(m.TotalSize) * 100
}
