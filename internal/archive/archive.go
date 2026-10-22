// Package archive ingests signed package-index snapshots from a
// snapshot-archive service (snarchive) over HTTP. The data fetched here
// is what roxxy's serve path will draw from in Stage 2 to implement the
// quarantine delay; Stage 1 just gets the data flowing and verified.
//
// Trust model: roxxy pins the snarchive public-key fingerprint at config
// time. On every refresh, the client fetches /pubkey and refuses to
// proceed if the published key doesn't match the pin. Per-blob integrity
// rides on snarchive's strong ETags (the git blob SHA).
package archive

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// Archive is the running ingest loop. New() builds one from config;
// Run() drives periodic refreshes until ctx is cancelled.
type Archive struct {
	client    *Client
	mirror    *Mirror
	blocklist *BlocklistStore

	url      string
	pubkeyFP string

	refreshInterval          time.Duration
	blocklistRefreshInterval time.Duration

	mu        sync.Mutex
	lastError error
	lastOK    time.Time
}

// Config is the runtime subset of roxxy's archive YAML stanza.
type Config struct {
	URL                      string
	PubkeyFingerprint        string        // required when URL is set
	MirrorDir                string        // local mirror under <cache>/archive/
	RefreshInterval          time.Duration // default 1h
	BlocklistRefreshInterval time.Duration // default 5m
	TrackedPaths             []string      // logical paths under /current/ to mirror; empty = lazy mode
}

func New(cfg Config) (*Archive, error) {
	if cfg.URL == "" {
		return nil, errors.New("archive: URL is required")
	}
	if cfg.PubkeyFingerprint == "" {
		return nil, errors.New("archive: pubkey fingerprint pin is required")
	}
	if cfg.MirrorDir == "" {
		return nil, errors.New("archive: MirrorDir is required")
	}
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = time.Hour
	}
	if cfg.BlocklistRefreshInterval == 0 {
		cfg.BlocklistRefreshInterval = 5 * time.Minute
	}
	mirror, err := OpenMirror(cfg.MirrorDir)
	if err != nil {
		return nil, fmt.Errorf("open mirror: %w", err)
	}
	a := &Archive{
		client:                   NewClient(cfg.URL),
		mirror:                   mirror,
		blocklist:                NewBlocklistStore(),
		url:                      cfg.URL,
		pubkeyFP:                 cfg.PubkeyFingerprint,
		refreshInterval:          cfg.RefreshInterval,
		blocklistRefreshInterval: cfg.BlocklistRefreshInterval,
	}
	a.mirror.tracked = append(a.mirror.tracked, cfg.TrackedPaths...)
	return a, nil
}

// Mirror exposes the local mirror for serve-time reads (Stage 2).
func (a *Archive) Mirror() *Mirror { return a.mirror }

// Blocklist exposes the in-memory blocklist for serve-time checks.
func (a *Archive) Blocklist() *BlocklistStore { return a.blocklist }

// LastStatus returns when the most recent successful refresh completed,
// and the last error if any. Useful for /healthz-style introspection.
func (a *Archive) LastStatus() (time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastOK, a.lastError
}

// Run drives two refresh loops on independent intervals until ctx is
// cancelled. Both fire immediately on entry so a fresh roxxy bootstraps
// without waiting for the first tick.
func (a *Archive) Run(ctx context.Context) error {
	go a.loop(ctx, a.blocklistRefreshInterval, a.refreshBlocklist, "blocklist")
	a.loop(ctx, a.refreshInterval, a.refreshIndices, "indices")
	return nil
}

func (a *Archive) loop(ctx context.Context, interval time.Duration, fn func(context.Context) error, name string) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := fn(ctx); err != nil {
			log.Printf("archive: %s refresh failed: %v", name, err)
			a.recordError(err)
		} else {
			a.recordSuccess()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *Archive) recordSuccess() {
	a.mu.Lock()
	a.lastError = nil
	a.lastOK = time.Now()
	a.mu.Unlock()
}

func (a *Archive) recordError(err error) {
	a.mu.Lock()
	a.lastError = err
	a.mu.Unlock()
}

func (a *Archive) refreshIndices(ctx context.Context) error {
	// First: verify the pubkey hasn't drifted from our pin.
	if err := a.client.VerifyPubkey(ctx, a.pubkeyFP); err != nil {
		return fmt.Errorf("pubkey pin: %w", err)
	}
	tracked := a.mirror.TrackedPaths()
	if len(tracked) == 0 {
		// Lazy mode — nothing to do until something asks for a specific
		// path via mirror.Fetch (not implemented yet).
		return nil
	}
	for _, p := range tracked {
		entry := a.mirror.LookupEntry(p)
		body, etag, modTime, status, err := a.client.GetConditional(ctx, "/current/"+p, entry.ETag, entry.ModTime)
		if err != nil {
			return fmt.Errorf("get %s: %w", p, err)
		}
		switch status {
		case 304:
			// No change — nothing to do.
		case 200:
			if err := a.mirror.Write(p, body, etag, modTime); err != nil {
				return fmt.Errorf("write %s: %w", p, err)
			}
		default:
			return fmt.Errorf("get %s: unexpected status %d", p, status)
		}
	}
	return nil
}

func (a *Archive) refreshBlocklist(ctx context.Context) error {
	if err := a.client.VerifyPubkey(ctx, a.pubkeyFP); err != nil {
		return fmt.Errorf("pubkey pin: %w", err)
	}
	body, _, _, status, err := a.client.GetConditional(ctx, "/blocklist", a.blocklist.ETag(), time.Time{})
	if err != nil {
		return err
	}
	switch status {
	case 304:
		return nil
	case 200:
		return a.blocklist.Update(body)
	default:
		return fmt.Errorf("unexpected status %d", status)
	}
}
