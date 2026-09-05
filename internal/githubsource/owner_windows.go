package githubsource

import (
	"os"

	"github.com/neuroforge-io/RKC/internal/privatepath"
)

// Windows privacy requires an identity-bound, protected current-user DACL;
// ownership or Unix mode bits alone cannot establish it.
func ownedByCurrentUser(path string, info os.FileInfo) bool {
	return privatepath.CheckDir(path, info) == nil
}
