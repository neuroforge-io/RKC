//go:build !linux && !darwin && !windows

package githubsource

import (
	"errors"
	"os"
)

func publishNoReplace(_ *os.Root, _, _ string) error {
	return errors.New("atomic GitHub source publication is unavailable on this platform")
}
