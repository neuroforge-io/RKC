//go:build windows

package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Canonical IDs contain colons and may exceed Windows component restrictions.
// Hash the exact case-sensitive ID into one bounded, safe filename; retain the
// complete public ID in CURRENT, ownership markers and committed records.
func snapshotDirectoryName(snapshotID string) string {
	digest := sha256.Sum256([]byte(snapshotID))
	return "snapshot-" + hex.EncodeToString(digest[:])
}

func validSnapshotDirectoryName(name string) bool {
	if len(name) != len("snapshot-")+64 || !strings.HasPrefix(name, "snapshot-") || name != strings.ToLower(name) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(name, "snapshot-"))
	return err == nil
}
