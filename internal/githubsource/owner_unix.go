//go:build !windows

package githubsource

import (
	"os"
	"syscall"
)

func ownedByCurrentUser(_ string, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}
