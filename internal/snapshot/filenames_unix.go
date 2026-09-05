//go:build !windows

package snapshot

// Preserve the existing Unix store layout. Public snapshot IDs and CURRENT
// never depend on the host's physical directory naming convention.
func snapshotDirectoryName(snapshotID string) string { return snapshotID }

func validSnapshotDirectoryName(name string) bool { return validSnapshotID(name) }
