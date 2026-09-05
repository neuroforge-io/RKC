//go:build windows

package snapshot

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/neuroforge-io/RKC/internal/privatepath"
	"golang.org/x/sys/windows"
)

const windowsLeaseRecordPrefix = "rkc-windows-lease-v1\n"
const windowsLeaseRecordSize = len(windowsLeaseRecordPrefix) + 64 + 1

// Windows does not reliably allow a directory containing an open descendant to
// move, even when the descendant shares DELETE. Keep only a kernel event open.
// Its name binds the directory's volume/file ID and lease basename, independent
// of the mutable lease record. Replacing that record cannot bypass a live lease.
// CreateEvent atomically detects an existing object; the final handle close or
// process exit destroys it. No thread-affine mutex or PID reuse is involved.
type transactionLease struct {
	file              io.Closer
	identity          os.FileInfo
	directoryIdentity os.FileInfo
	eventName         string
	record            string
}

type windowsLeaseEvent struct {
	mu     sync.Mutex
	handle windows.Handle
}

func (event *windowsLeaseEvent) Close() error {
	event.mu.Lock()
	defer event.mu.Unlock()
	if event.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(event.handle)
	if err == nil {
		event.handle = 0
	}
	return err
}

func (event *windowsLeaseEvent) valid() bool {
	event.mu.Lock()
	defer event.mu.Unlock()
	if event.handle == 0 {
		return false
	}
	// The event is never signalled. This non-blocking query rejects a closed
	// descriptor without opening any path inside the transaction directory.
	state, err := windows.WaitForSingleObject(event.handle, 0)
	return err == nil && state == uint32(windows.WAIT_TIMEOUT)
}

func createTransactionLease(path string) (*transactionLease, error) {
	name, directory, err := windowsLeaseName(path)
	if err != nil {
		return nil, err
	}
	event, live, err := acquireWindowsLeaseEvent(name)
	if err != nil {
		return nil, err
	}
	if live {
		return nil, ErrTransactionLive
	}
	success := false
	defer func() {
		if !success {
			_ = event.Close()
		}
	}()
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	record := windowsLeaseRecordPrefix + hex.EncodeToString(token[:]) + "\n"
	file, err := openLeaseFile(path, true)
	if err != nil {
		return nil, err
	}
	identity, err := file.Stat()
	if err != nil || !identity.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("%w: transaction lease is not a regular file", ErrBuildingUnowned)
	}
	_, writeErr := io.WriteString(file, record)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = removeExactRegularFile(path, identity)
		return nil, errors.Join(writeErr, closeErr)
	}
	lease := &transactionLease{file: event, identity: identity, directoryIdentity: directory, eventName: name, record: record}
	if err := lease.validate(path); err != nil {
		_ = removeExactRegularFile(path, identity)
		return nil, err
	}
	success = true
	return lease, nil
}

// Missing, malformed, legacy file-lock leases and changed identities fail
// closed. They never establish that a transaction is abandoned. Observing an
// existing event temporarily holds another handle, which can only defer recovery
// conservatively if its owner exits during the observation.
func acquireAbandonedTransactionLease(path string) (*transactionLease, bool, error) {
	name, directory, err := windowsLeaseName(path)
	if err != nil {
		return nil, false, err
	}
	record, identity, err := readWindowsLeaseRecord(path)
	if err != nil {
		return nil, false, err
	}
	event, live, err := acquireWindowsLeaseEvent(name)
	if err != nil || live {
		return nil, live, err
	}
	lease := &transactionLease{file: event, identity: identity, directoryIdentity: directory, eventName: name, record: record}
	if err := lease.validate(path); err != nil {
		_ = event.Close()
		return nil, false, err
	}
	return lease, false, nil
}

func (lease *transactionLease) validate(path string) error {
	if lease == nil || lease.file == nil || lease.identity == nil || lease.directoryIdentity == nil {
		return fmt.Errorf("%w: missing transaction lease", ErrBuildingUnowned)
	}
	event, ok := lease.file.(*windowsLeaseEvent)
	if !ok || !event.valid() {
		return fmt.Errorf("%w: closed transaction lease", ErrBuildingUnowned)
	}
	name, directory, err := windowsLeaseName(path)
	if err != nil || name != lease.eventName || !os.SameFile(directory, lease.directoryIdentity) {
		return fmt.Errorf("%w: transaction lease directory changed", ErrBuildingUnowned)
	}
	record, identity, err := readWindowsLeaseRecord(path)
	if err != nil || record != lease.record || !os.SameFile(identity, lease.identity) {
		return fmt.Errorf("%w: transaction lease record or identity changed", ErrBuildingUnowned)
	}
	return nil
}

func (lease *transactionLease) Close() error {
	if lease == nil || lease.file == nil {
		return nil
	}
	err := lease.file.Close()
	lease.file = nil
	return err
}

func readWindowsLeaseRecord(path string) (string, os.FileInfo, error) {
	before, err := privatepath.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, fmt.Errorf("%w: transaction lease is missing", ErrBuildingUnowned)
		}
		return "", nil, err
	}
	if !before.Mode().IsRegular() || before.Size() != int64(windowsLeaseRecordSize) {
		return "", nil, fmt.Errorf("%w: invalid transaction lease record", ErrBuildingUnowned)
	}
	file, err := openLeaseFile(path, false)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return "", nil, fmt.Errorf("%w: transaction lease identity changed", ErrBuildingUnowned)
	}
	payload, err := io.ReadAll(io.LimitReader(file, int64(windowsLeaseRecordSize+1)))
	if err != nil {
		return "", nil, err
	}
	record := string(payload)
	if len(record) != windowsLeaseRecordSize || !strings.HasPrefix(record, windowsLeaseRecordPrefix) || record[len(record)-1] != '\n' {
		return "", nil, fmt.Errorf("%w: malformed transaction lease record", ErrBuildingUnowned)
	}
	token := record[len(windowsLeaseRecordPrefix) : len(record)-1]
	if _, err := hex.DecodeString(token); err != nil || strings.ToLower(token) != token {
		return "", nil, fmt.Errorf("%w: malformed transaction lease token", ErrBuildingUnowned)
	}
	current, err := privatepath.Lstat(path)
	if err != nil || !os.SameFile(opened, current) {
		return "", nil, fmt.Errorf("%w: transaction lease changed while reading", ErrBuildingUnowned)
	}
	return record, opened, nil
}

func windowsLeaseName(path string) (string, os.FileInfo, error) {
	directoryPath := filepath.Dir(path)
	encoded, err := windows.UTF16PtrFromString(directoryPath)
	if err != nil {
		return "", nil, err
	}
	handle, err := windows.CreateFile(encoded, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return "", nil, err
	}
	file := os.NewFile(uintptr(handle), directoryPath)
	defer file.Close()
	identity, err := file.Stat()
	if err != nil || !identity.IsDir() || identity.Mode()&os.ModeSymlink != 0 {
		return "", nil, fmt.Errorf("%w: lease parent is not a regular directory", ErrBuildingUnowned)
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return "", nil, err
	}
	// Host-local events cannot coordinate writers on other machines. Resolve a
	// local volume GUID from this very handle; network shares have no volume GUID
	// and fail closed, including mapped drives or paths reached through junctions.
	// https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-getfinalpathnamebyhandlew
	var finalPath [32768]uint16
	length, err := windows.GetFinalPathNameByHandle(handle, &finalPath[0], uint32(len(finalPath)), 1) // VOLUME_NAME_GUID
	if err != nil || length == 0 || length >= uint32(len(finalPath)) {
		return "", nil, fmt.Errorf("%w: Windows snapshot leases require a local volume GUID", ErrBuildingUnowned)
	}
	volume, err := windowsLeaseVolume(windows.UTF16ToString(finalPath[:length]))
	if err != nil {
		return "", nil, err
	}
	var binding [12]byte
	binary.BigEndian.PutUint32(binding[:4], info.VolumeSerialNumber)
	binary.BigEndian.PutUint32(binding[4:8], info.FileIndexHigh)
	binary.BigEndian.PutUint32(binding[8:], info.FileIndexLow)
	hash := sha256.New()
	_, _ = hash.Write([]byte(volume))
	_, _ = hash.Write(binding[:])
	_, _ = hash.Write([]byte(strings.ToLower(filepath.Base(path))))
	return `Global\NeuroForgeIO.RKC.Snapshot.` + hex.EncodeToString(hash.Sum(nil)), identity, nil
}

func windowsLeaseVolume(path string) (string, error) {
	const prefix = `\\?\Volume{`
	rest, ok := strings.CutPrefix(path, prefix)
	if !ok || len(rest) < 38 || rest[36:38] != `}\` {
		return "", fmt.Errorf("%w: Windows snapshot lease is not on a local volume", ErrBuildingUnowned)
	}
	guid := rest[:36]
	for i, character := range guid {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if character == '-' {
				continue
			}
		} else if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F') {
			continue
		}
		return "", fmt.Errorf("%w: invalid local volume identity", ErrBuildingUnowned)
	}
	return strings.ToLower(guid), nil
}

func acquireWindowsLeaseEvent(name string) (*windowsLeaseEvent, bool, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, false, errors.New("transaction lease current-user identity is unavailable")
	}
	sid := user.User.Sid
	descriptor, err := windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;;GA;;;" + sid.String() + ")")
	if err != nil {
		return nil, false, err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	encoded, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, false, err
	}
	handle, err := windows.CreateEvent(&attributes, 0, 0, encoded)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(handle)
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("create transaction liveness event: %w", err)
	}
	if err := validateWindowsLeaseEventSecurity(handle, sid); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, false, err
	}
	return &windowsLeaseEvent{handle: handle}, false, nil
}

func validateWindowsLeaseEventSecurity(handle windows.Handle, sid *windows.SID) error {
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || descriptor == nil || !descriptor.IsValid() {
		return errors.New("transaction lease has no valid security descriptor")
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(sid) {
		return errors.New("transaction lease is not owned by the current user")
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PRESENT == 0 || control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("transaction lease DACL is not protected")
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil || dacl == nil || defaulted || dacl.AceCount != 1 {
		return errors.New("transaction lease DACL is not current-user only")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return err
	}
	if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 ||
		(ace.Mask&windows.GENERIC_ALL == 0 && ace.Mask&windows.EVENT_ALL_ACCESS != windows.EVENT_ALL_ACCESS) {
		return errors.New("transaction lease DACL has unexpected access")
	}
	grantee := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !grantee.IsValid() || !grantee.Equals(sid) {
		return errors.New("transaction lease DACL grants another principal access")
	}
	return nil
}

func openLeaseFile(path string, create bool) (*os.File, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	disposition := uint32(windows.OPEN_EXISTING)
	if create {
		disposition = windows.CREATE_NEW
	}
	handle, err := windows.CreateFile(encoded, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, disposition, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open snapshot lease", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
