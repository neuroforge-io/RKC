//go:build windows

package safeoutput

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func persistentPathIdentityToken(path string, identity os.FileInfo) (string, error) {
	// Force Go's lazy Windows FileInfo identity to be captured before opening or
	// moving this path. FileInfo.Sys exposes attributes, not a stable file ID.
	if identity == nil || !identity.IsDir() || !os.SameFile(identity, identity) {
		return "", errors.New("filesystem identity is unavailable")
	}
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(encoded, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(identity, opened) {
		return "", errors.New("directory identity changed before capturing persistent identity")
	}
	// FILE_ID_INFO uses the full 128-bit file identifier, including on ReFS.
	// Unsupported filesystems fail closed rather than substituting timestamps.
	var id struct {
		Volume uint64
		FileID [16]byte
	}
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&id)), uint32(unsafe.Sizeof(id))); err != nil {
		return "", fmt.Errorf("read durable Windows filesystem identity: %w", err)
	}
	if id.FileID == [16]byte{} {
		return "", errors.New("Windows filesystem returned an empty file identity")
	}
	current, err := os.Lstat(path)
	if err != nil || !current.IsDir() || !os.SameFile(identity, current) {
		return "", errors.New("directory identity changed while capturing persistent identity")
	}
	return fmt.Sprintf("win:%016x:%x", id.Volume, id.FileID), nil
}
