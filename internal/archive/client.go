package archive

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// Client speaks to snarchive's HTTP API.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 2 * time.Minute},
	}
}

// GetConditional issues a GET with optional If-None-Match and
// If-Modified-Since. Returns the body (empty on 304), the response ETag
// and Last-Modified, the status code, and an error.
//
// Status codes returned: 200, 304, or an error for anything else.
func (c *Client) GetConditional(ctx context.Context, path, etag string, modTime time.Time) (body []byte, newETag string, newMod time.Time, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, "", time.Time{}, 0, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if !modTime.IsZero() {
		req.Header.Set("If-Modified-Since", modTime.UTC().Format(http.TimeFormat))
	}
	req.Header.Set("User-Agent", "roxxy-archive/0.1")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", time.Time{}, 0, err
	}
	defer util.DeferClose(resp.Body)
	status = resp.StatusCode
	newETag = resp.Header.Get("ETag")
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			newMod = t
		}
	}
	switch status {
	case http.StatusNotModified:
		return nil, newETag, newMod, status, nil
	case http.StatusOK:
		body, err = io.ReadAll(resp.Body)
		return body, newETag, newMod, status, err
	default:
		// Drain so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, newETag, newMod, status, fmt.Errorf("status %d for %s", status, path)
	}
}

// VerifyPubkey fetches /pubkey and confirms the published key's
// fingerprint matches the pin. The fingerprint comparison is
// case-insensitive and ignores spaces (which gpg sometimes embeds in
// fingerprints).
func (c *Client) VerifyPubkey(ctx context.Context, pinFingerprint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/pubkey", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer util.DeferClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	fp, err := extractFingerprint(body)
	if err != nil {
		return err
	}
	if normaliseFingerprint(fp) != normaliseFingerprint(pinFingerprint) {
		return fmt.Errorf("pubkey fingerprint mismatch: published %s, pinned %s", fp, pinFingerprint)
	}
	return nil
}

func normaliseFingerprint(s string) string {
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ToLower(s)
	return s
}
