package archive

import "testing"

func TestBlocklistMatches(t *testing.T) {
	b := NewBlocklistStore()
	doc := []byte(`{"version":1,"updated_at":"2026-05-14T00:00:00Z","entries":[
		{"format":"npm","coordinate":"evil@1.0.0","sha256":"abc","reason":"r","added_at":"2026-05-14T00:00:00Z"}
	]}`)
	if err := b.Update(doc); err != nil {
		t.Fatal(err)
	}
	if !b.IsBlocked("npm", "evil@1.0.0", "") {
		t.Fatal("coord match failed")
	}
	if !b.IsBlocked("npm", "wrong", "abc") {
		t.Fatal("hash match failed (should win even with wrong coord)")
	}
	if b.IsBlocked("npm", "fine@1.0.0", "") {
		t.Fatal("false positive")
	}
}

func TestBlocklistRejectsBadJSON(t *testing.T) {
	b := NewBlocklistStore()
	if err := b.Update([]byte("not json")); err == nil {
		t.Fatal("expected error on bad JSON")
	}
}
