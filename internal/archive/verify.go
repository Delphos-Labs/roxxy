package archive

import (
	"bytes"
	"fmt"
	"os/exec"
)

// extractFingerprint parses an ASCII-armored PGP public-key block and
// returns the primary key's fingerprint. Uses the local gpg binary to
// avoid a heavy PGP parsing dependency for a tiny once-per-refresh
// operation.
func extractFingerprint(armored []byte) (string, error) {
	cmd := exec.Command("gpg", "--with-colons", "--show-keys")
	cmd.Stdin = bytes.NewReader(armored)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gpg --show-keys: %w", err)
	}
	for _, line := range bytes.Split(out, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("fpr:")) {
			continue
		}
		fields := bytes.Split(line, []byte(":"))
		if len(fields) > 9 && len(fields[9]) > 0 {
			return string(fields[9]), nil
		}
	}
	return "", fmt.Errorf("no fingerprint found in pubkey")
}
