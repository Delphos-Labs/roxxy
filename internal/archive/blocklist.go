package archive

import (
	"encoding/json"
	"sync"
	"time"
)

// BlocklistEntry mirrors snapshot-archive's blocklist schema.
type BlocklistEntry struct {
	Format     string    `json:"format"`
	Coordinate string    `json:"coordinate"`
	SHA256     string    `json:"sha256,omitempty"`
	Reason     string    `json:"reason"`
	AddedAt    time.Time `json:"added_at"`
}

type blocklistDoc struct {
	Version   int              `json:"version"`
	UpdatedAt time.Time        `json:"updated_at"`
	Entries   []BlocklistEntry `json:"entries"`
}

// BlocklistStore holds the latest blocklist with lookup indices for the
// serve-time hot path. Safe for concurrent reads + occasional updates.
type BlocklistStore struct {
	mu      sync.RWMutex
	doc     blocklistDoc
	etag    string
	byCoord map[string]bool // "format:coordinate" → blocked
	byHash  map[string]bool // sha256 → blocked
}

func NewBlocklistStore() *BlocklistStore {
	return &BlocklistStore{
		byCoord: map[string]bool{},
		byHash:  map[string]bool{},
	}
}

// ETag returns the last-known blocklist ETag, used for conditional GET.
func (b *BlocklistStore) ETag() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.etag
}

// Update parses a fresh blocklist body and replaces the in-memory state.
func (b *BlocklistStore) Update(body []byte) error {
	var doc blocklistDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return err
	}
	byCoord := map[string]bool{}
	byHash := map[string]bool{}
	for _, e := range doc.Entries {
		byCoord[e.Format+":"+e.Coordinate] = true
		if e.SHA256 != "" {
			byHash[e.SHA256] = true
		}
	}
	b.mu.Lock()
	b.doc = doc
	b.byCoord = byCoord
	b.byHash = byHash
	b.mu.Unlock()
	return nil
}

// SetETag records the ETag returned by snarchive alongside the last
// successful update. Called by the refresh loop on 200.
func (b *BlocklistStore) SetETag(etag string) {
	b.mu.Lock()
	b.etag = etag
	b.mu.Unlock()
}

// IsBlocked returns true if a (format, coordinate) pair is blocked, or
// if a known sha256 matches.
func (b *BlocklistStore) IsBlocked(format, coordinate, sha256 string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if sha256 != "" && b.byHash[sha256] {
		return true
	}
	return b.byCoord[format+":"+coordinate]
}

// Entries returns a snapshot of the current blocklist entries (for
// /healthz-style debugging).
func (b *BlocklistStore) Entries() []BlocklistEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]BlocklistEntry, len(b.doc.Entries))
	copy(out, b.doc.Entries)
	return out
}
