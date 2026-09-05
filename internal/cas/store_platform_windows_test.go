//go:build windows

package cas

import (
	"errors"

	"golang.org/x/sys/windows"
)

func isOpenFileRenameDenied(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
