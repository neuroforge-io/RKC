//go:build !windows

package safeoutput

import "os"

func persistentPathIdentityToken(_ string, identity os.FileInfo) (string, error) {
	return persistentIdentityToken(identity)
}
