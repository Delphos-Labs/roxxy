package archive

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestE2E_LiveSnarchive runs against a real snarchive at SNARCHIVE_URL,
// pinning the fingerprint via SNARCHIVE_FPR. Skipped when env is unset.
//
// Example:
//
//	SNARCHIVE_URL=http://localhost:8080 \
//	SNARCHIVE_FPR=B5327C3876D816A48E20DABB4037AF80B8E337D0 \
//	  go test -run TestE2E_LiveSnarchive ./internal/archive/...
func TestE2E_LiveSnarchive(t *testing.T) {
	url := os.Getenv("SNARCHIVE_URL")
	fpr := os.Getenv("SNARCHIVE_FPR")
	if url == "" || fpr == "" {
		t.Skip("SNARCHIVE_URL or SNARCHIVE_FPR not set")
	}
	a, err := New(Config{
		URL:               url,
		PubkeyFingerprint: fpr,
		MirrorDir:         t.TempDir(),
		TrackedPaths: []string{
			"pypi/pypi.org/simple/requests/index.html",
			"go/proxy.golang.org/github.com/gin-gonic/gin/@latest",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := a.client.VerifyPubkey(ctx, fpr); err != nil {
		t.Fatalf("verify pubkey: %v", err)
	}
	if err := a.refreshIndices(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// Second refresh should be all-304s — verify nothing breaks.
	if err := a.refreshIndices(ctx); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	body, err := a.mirror.Read("pypi/pypi.org/simple/requests/index.html")
	if err != nil || len(body) == 0 {
		t.Fatalf("read mirrored pypi/requests: %v (%d bytes)", err, len(body))
	}
	if err := a.refreshBlocklist(ctx); err != nil {
		t.Fatalf("blocklist: %v", err)
	}
	t.Logf("mirror has %d bytes for pypi requests; blocklist has %d entries",
		len(body), len(a.blocklist.Entries()))
}
