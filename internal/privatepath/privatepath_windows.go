//go:build windows

package privatepath

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Lstat captures no-follow metadata and file identity from one live handle.
// Go's Windows os.Lstat defers file-ID acquisition until os.SameFile is first
// called; after a pathname swap that can bind an old FileInfo to the replacement.
// Closing this handle preserves the eager identity without blocking renames.
func Lstat(path string) (os.FileInfo, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(encoded, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	return file.Stat()
}

// MkdirTemp creates a directory with a protected current-user DACL at creation;
// there is no interval in which inherited permissions expose its contents.
func MkdirTemp(parent, pattern string) (string, error) {
	path, file, err := createTemp(parent, pattern, true)
	if file != nil {
		_ = file.Close()
	}
	return path, err
}

// CreateTemp creates a file with a protected current-user DACL at creation.
func CreateTemp(parent, pattern string) (*os.File, error) {
	_, file, err := createTemp(parent, pattern, false)
	return file, err
}

func createTemp(parent, pattern string, directory bool) (string, *os.File, error) {
	if strings.ContainsAny(pattern, `/\`) {
		return "", nil, errors.New("private temporary pattern contains a path separator")
	}
	if parent == "" {
		parent = os.TempDir()
	}
	user, err := currentUser()
	if err != nil {
		return "", nil, err
	}
	flags := ""
	if directory {
		flags = "OICI"
	}
	descriptor, err := windows.SecurityDescriptorFromString("O:" + user.String() + "D:P(A;" + flags + ";FA;;;" + user.String() + ")")
	if err != nil {
		return "", nil, fmt.Errorf("construct private security descriptor: %w", err)
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	prefix, suffix := pattern, ""
	if star := strings.LastIndexByte(pattern, '*'); star >= 0 {
		prefix, suffix = pattern[:star], pattern[star+1:]
	}
	for attempt := 0; attempt < 100; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		path := filepath.Join(parent, prefix+hex.EncodeToString(random[:])+suffix)
		encoded, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return "", nil, err
		}
		var file *os.File
		if directory {
			err = windows.CreateDirectory(encoded, &attributes)
		} else {
			var handle windows.Handle
			handle, err = windows.CreateFile(encoded, windows.GENERIC_READ|windows.GENERIC_WRITE,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
				&attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
			if err == nil {
				file = os.NewFile(uintptr(handle), path)
			}
		}
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			continue
		}
		if err != nil {
			return "", nil, &os.PathError{Op: "create private temporary path", Path: path, Err: err}
		}
		// Filesystems may ignore SECURITY_ATTRIBUTES when they lack persistent
		// ACLs. Verify privacy before returning a handle on which a caller could
		// write secrets; never treat successful creation alone as permission proof.
		identity, statErr := Lstat(path)
		if file != nil {
			identity, statErr = file.Stat()
		}
		if statErr == nil {
			statErr = checkPrivate(path, identity, directory)
		}
		if statErr != nil {
			if file != nil {
				_ = file.Close()
			}
			if current, err := Lstat(path); err == nil && identity != nil && os.SameFile(identity, current) {
				_ = os.Remove(path)
			}
			return "", nil, fmt.Errorf("new temporary path is not private: %w", statErr)
		}
		return path, file, nil
	}
	return "", nil, errors.New("private temporary name collision limit exceeded")
}

func currentUser() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, errors.New("current-user SID is unavailable")
	}
	return user.User.Sid.Copy()
}

func checkPrivate(path string, identity os.FileInfo, directory bool) error {
	if err := checkIdentity(path, identity, directory); err != nil {
		return err
	}
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(encoded, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(identity, opened) {
		return errors.New("private path identity changed before checking its ACL")
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	user, err := currentUser()
	if err != nil {
		return err
	}
	if descriptor == nil || !descriptor.IsValid() {
		return errors.New("private path has no valid security descriptor")
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		return errors.New("private path is not owned by the current user")
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PRESENT == 0 || control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("private path DACL is not present and protected")
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil || dacl == nil || defaulted || dacl.AceCount != 1 {
		return errors.New("private path DACL must have one explicit current-user entry")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return err
	}
	flags := byte(0)
	if directory {
		flags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != flags {
		return errors.New("private path DACL entry has unexpected type or inheritance")
	}
	const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff
	if ace.Mask&windows.GENERIC_ALL == 0 && ace.Mask&fileAllAccess != fileAllAccess {
		return errors.New("private path DACL does not grant the current user full control")
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !sid.IsValid() || !sid.Equals(user) {
		return errors.New("private path DACL grants another principal access")
	}
	return checkIdentity(path, identity, directory)
}

// A read-only directory handle cannot be flushed with FlushFileBuffers. Do not
// classify ERROR_ACCESS_DENIED as success: avoid the unsupported operation and
// retain all identity/open errors in SyncDirectoryStable instead. Data is flushed
// by file writers; Rename requests Windows' documented write-through move.
// https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-flushfilebuffers
func syncOpenedDirectory(_ *os.File) error { return nil }

// Rename has os.Rename replacement semantics with a write-through request. It
// does not permit cross-volume copy/delete and does not imply POSIX directory
// fsync guarantees. Output publication requiring no replacement uses its own
// MoveFileEx call without MOVEFILE_REPLACE_EXISTING.
func Rename(oldPath, newPath string) error {
	from, err := windows.UTF16PtrFromString(oldPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(newPath)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: err}
	}
	return nil
}
