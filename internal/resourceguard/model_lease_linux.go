//go:build linux

package resourceguard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	maximumModelLeaseBytes     = 4096
	maximumModelLockTableBytes = 1024 * 1024
	maximumModelLockRows       = 8192
	maximumModelLockRowBytes   = 4096
	// Linux UAPI include/uapi/linux/nsfs.h fixes PID_NS_INIT_INO. Procfs
	// filters locks by the PID namespace associated with its mount; an ordinary
	// namespace inode, even one shared with PID 1, does not prove visibility.
	initialModelPIDNamespaceInode uint64 = 0xeffffffc
)

// checkERAISModelLease observes the canonical kernel lease without taking it,
// opening it for reading, or interpreting its mutable payload. A clear result
// describes only these observations; it never reserves host capacity or closes
// the race with a model starting later. Callers must keep monitoring while
// doing work, in addition to their independent resource admission.
func checkERAISModelLease() error {
	kernel, err := openModelLeaseKernelView("/proc", os.Getpid())
	if err != nil {
		return modelLeaseRefusal("kernel lease visibility is unproven")
	}
	defer kernel.close()
	return checkERAISModelLeaseUsing("/", uint32(os.Geteuid()), 0, kernel.readLocks)
}

func modelLeaseRefusal(reason string) error {
	return fmt.Errorf("%w: ERAIS model lease %s", ErrHigherPriorityActive, reason)
}

// The root and snapshot seams exist solely for isolated fixtures. Production
// always uses /, the effective UID, root-owned ancestors, and verified procfs.
func checkERAISModelLeaseUsing(root string, uid, ancestorUID uint32, readLocks func() ([]byte, error)) error {
	if readLocks == nil {
		return modelLeaseRefusal("inspector is unavailable")
	}
	tree, err := openModelLeaseTree(root, uid, ancestorUID)
	if err != nil {
		return modelLeaseRefusal("path safety is unproven")
	}
	defer tree.close()
	for pass := 0; pass < 2; pass++ {
		if err := tree.recheck(); err != nil {
			return modelLeaseRefusal("path changed during observation")
		}
		record, err := readLocks()
		if err != nil {
			return modelLeaseRefusal("kernel lease visibility is unproven")
		}
		busy, err := modelLeaseTableBusy(record, tree.leaf())
		if err != nil {
			return modelLeaseRefusal("kernel lock table is unproven")
		}
		if busy {
			return modelLeaseRefusal("has a kernel lock or waiter")
		}
		if err := tree.recheck(); err != nil {
			return modelLeaseRefusal("path changed during observation")
		}
	}
	return nil
}

type modelLeaseNode struct {
	fd       int
	name     string
	identity unix.Stat_t
}

type modelLeaseTree struct {
	root    string
	nodes   []modelLeaseNode
	missing string
}

func openModelLeaseTree(root string, uid, ancestorUID uint32) (_ *modelLeaseTree, resultErr error) {
	tree := &modelLeaseTree{root: root}
	defer func() {
		if resultErr != nil {
			tree.close()
		}
	}()
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	tree.nodes = append(tree.nodes, modelLeaseNode{fd: rootFD})
	if err := unix.Fstat(rootFD, &tree.nodes[0].identity); err != nil ||
		!modelLeaseDirectory(tree.nodes[0].identity, ancestorUID, false) {
		return nil, errors.New("unsafe lease ancestor")
	}
	parts := []string{"run", "user", strconv.FormatUint(uint64(uid), 10), "erais", "model-workload.lock"}
	for index, part := range parts {
		parent := tree.nodes[len(tree.nodes)-1].fd
		flags := unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if index != len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		fd, err := unix.Openat(parent, part, flags, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) && index >= 3 {
				tree.missing = part
				return tree, nil
			}
			return nil, err
		}
		node := modelLeaseNode{fd: fd, name: part}
		tree.nodes = append(tree.nodes, node)
		identity := &tree.nodes[len(tree.nodes)-1].identity
		if err := unix.Fstat(fd, identity); err != nil {
			return nil, err
		}
		if index == len(parts)-1 {
			if identity.Mode&unix.S_IFMT != unix.S_IFREG || identity.Uid != uid ||
				identity.Mode&0o077 != 0 || identity.Nlink != 1 ||
				identity.Size < 0 || identity.Size > maximumModelLeaseBytes {
				return nil, errors.New("unsafe lease leaf")
			}
		} else {
			expectedUID, private := ancestorUID, false
			if index >= 2 {
				expectedUID, private = uid, true
			}
			if !modelLeaseDirectory(*identity, expectedUID, private) {
				return nil, errors.New("unsafe lease directory")
			}
		}
	}
	return tree, nil
}

func modelLeaseDirectory(identity unix.Stat_t, uid uint32, private bool) bool {
	forbidden := uint32(0o022)
	if private {
		forbidden = 0o077
	}
	return identity.Mode&unix.S_IFMT == unix.S_IFDIR && identity.Uid == uid &&
		identity.Mode&forbidden == 0 && identity.Mode&0o100 != 0
}

func (tree *modelLeaseTree) close() {
	for _, node := range tree.nodes {
		_ = unix.Close(node.fd)
	}
	tree.nodes = nil
}

func (tree *modelLeaseTree) leaf() *unix.Stat_t {
	if tree.missing != "" || len(tree.nodes) != 6 {
		return nil
	}
	return &tree.nodes[len(tree.nodes)-1].identity
}

func sameModelLeaseNode(before, after unix.Stat_t) bool {
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Mode != after.Mode ||
		before.Uid != after.Uid || before.Gid != after.Gid {
		return false
	}
	// A lease publisher can rewrite an already open inode. Do not read payloads
	// or mistake that metadata drift for a stable observation. Directory times
	// need not stay fixed when unrelated runtime files change.
	return before.Mode&unix.S_IFMT != unix.S_IFREG ||
		before.Nlink == after.Nlink && before.Size == after.Size &&
			before.Mtim == after.Mtim && before.Ctim == after.Ctim
}

func (tree *modelLeaseTree) recheck() error {
	if len(tree.nodes) == 0 {
		return errors.New("lease chain is closed")
	}
	for index, node := range tree.nodes {
		var descriptor, path unix.Stat_t
		if err := unix.Fstat(node.fd, &descriptor); err != nil || !sameModelLeaseNode(node.identity, descriptor) {
			return errors.New("lease descriptor changed")
		}
		var err error
		if index == 0 {
			err = unix.Lstat(tree.root, &path)
		} else {
			err = unix.Fstatat(tree.nodes[index-1].fd, node.name, &path, unix.AT_SYMLINK_NOFOLLOW)
		}
		if err != nil || !sameModelLeaseNode(node.identity, path) {
			return errors.New("lease path changed")
		}
	}
	if tree.missing != "" {
		var current unix.Stat_t
		if err := unix.Fstatat(tree.nodes[len(tree.nodes)-1].fd, tree.missing, &current, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
			return errors.New("absent lease path changed")
		}
	}
	return nil
}

func modelLeaseTableBusy(record []byte, leaf *unix.Stat_t) (bool, error) {
	if len(record) > maximumModelLockTableBytes || bytes.IndexByte(record, 0) >= 0 {
		return false, errors.New("lock table exceeds its bounds")
	}
	// SplitN bounds the row-index allocation even for a table consisting only
	// of newlines; the byte bound alone would otherwise permit a large slice.
	rows := bytes.SplitN(record, []byte{'\n'}, maximumModelLockRows+2)
	if len(rows) > maximumModelLockRows+1 {
		return false, errors.New("lock table has too many rows")
	}
	for index, row := range rows {
		if len(row) == 0 && index == len(rows)-1 {
			continue
		}
		if index >= maximumModelLockRows {
			return false, errors.New("lock table has too many rows")
		}
		if len(row) == 0 || len(row) > maximumModelLockRowBytes {
			return false, errors.New("lock row exceeds its bounds")
		}
		fields := strings.Fields(string(row))
		// Identify a matching inode before interpreting lock kind or owner.
		// OFD owners can be -1; process death, CPU idleness, unknown kinds, and
		// queued rows never turn a positively observed lock into free capacity.
		for _, field := range fields {
			major, minor, inode, ok := modelLeaseDeviceInode(field)
			if ok && leaf != nil && major == unix.Major(uint64(leaf.Dev)) &&
				minor == unix.Minor(uint64(leaf.Dev)) && inode == leaf.Ino {
				return true, nil
			}
		}
		if len(fields) < 1 || !strings.HasSuffix(fields[0], ":") {
			return false, errors.New("lock row has no identifier")
		}
		if _, err := strconv.ParseUint(strings.TrimSuffix(fields[0], ":"), 10, 64); err != nil {
			return false, errors.New("lock row identifier is invalid")
		}
		if len(fields) > 1 && fields[1] == "->" {
			fields = append(fields[:1], fields[2:]...)
		}
		if len(fields) != 8 {
			return false, errors.New("lock row shape is unproven")
		}
		if _, _, _, ok := modelLeaseDeviceInode(fields[5]); !ok {
			return false, errors.New("lock row inode is invalid")
		}
		if _, err := strconv.ParseInt(fields[4], 10, 64); err != nil {
			return false, errors.New("lock row owner is invalid")
		}
		if _, err := strconv.ParseUint(fields[6], 10, 64); err != nil {
			return false, errors.New("lock row range is invalid")
		}
		if fields[7] != "EOF" {
			if _, err := strconv.ParseUint(fields[7], 10, 64); err != nil {
				return false, errors.New("lock row end is invalid")
			}
		}
	}
	return false, nil
}

func modelLeaseDeviceInode(field string) (uint32, uint32, uint64, bool) {
	parts := strings.Split(field, ":")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	major, majorErr := strconv.ParseUint(parts[0], 16, 32)
	minor, minorErr := strconv.ParseUint(parts[1], 16, 32)
	inode, inodeErr := strconv.ParseUint(parts[2], 10, 64)
	return uint32(major), uint32(minor), inode, majorErr == nil && minorErr == nil && inodeErr == nil
}

type modelLeaseKernelView struct {
	procRoot string
	procFD   int
	pid      int
	identity unix.Stat_t
}

func openModelLeaseKernelView(procRoot string, pid int) (_ *modelLeaseKernelView, resultErr error) {
	if pid <= 0 {
		return nil, errors.New("invalid inspector identity")
	}
	fd, err := unix.Open(procRoot, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	view := &modelLeaseKernelView{procRoot: procRoot, procFD: fd, pid: pid}
	defer func() {
		if resultErr != nil {
			view.close()
		}
	}()
	if err := unix.Fstat(fd, &view.identity); err != nil {
		return nil, err
	}
	if err := view.proveVisibility(); err != nil {
		return nil, err
	}
	return view, nil
}

func (view *modelLeaseKernelView) close() { _ = unix.Close(view.procFD) }

func (view *modelLeaseKernelView) proveVisibility() error {
	var filesystem unix.Statfs_t
	var descriptor, path unix.Stat_t
	if unix.Fstatfs(view.procFD, &filesystem) != nil || int64(filesystem.Type) != unix.PROC_SUPER_MAGIC ||
		unix.Fstat(view.procFD, &descriptor) != nil || unix.Lstat(view.procRoot, &path) != nil ||
		!sameModelLeaseNode(view.identity, descriptor) || !sameModelLeaseNode(view.identity, path) {
		return errors.New("procfs identity is unproven")
	}
	if err := modelLeaseSelfLink(view.procFD, view.pid); err != nil {
		return err
	}
	self, err := view.pidNamespace(view.pid)
	if err != nil {
		return err
	}
	if !modelLeaseInitialPIDNamespace(self) {
		return errors.New("initial PID namespace is unproven")
	}
	return modelLeaseSelfLink(view.procFD, view.pid)
}

func modelLeaseInitialPIDNamespace(self unix.Stat_t) bool {
	return self.Ino == initialModelPIDNamespaceInode
}

func modelLeaseSelfLink(procFD, pid int) error {
	// Kernel fs/proc/self.c resolves this magic link to the calling process's
	// TGID in the proc mount's PID namespace, or ENOENT when it is invisible.
	// Requiring that target to equal getpid ties the numeric directory opened
	// below to this caller. Combined with that caller's proven initial NSFS
	// identity, this excludes a descendant/filtered proc mount without needing
	// privileged access to PID 1's namespace files.
	var target [32]byte
	length, err := unix.Readlinkat(procFD, "self", target[:])
	expected := strconv.Itoa(pid)
	if err != nil || pid <= 0 || length != len(expected) || string(target[:length]) != expected {
		return errors.New("proc caller identity is unproven")
	}
	return nil
}

func (view *modelLeaseKernelView) pidNamespace(pid int) (unix.Stat_t, error) {
	var identity unix.Stat_t
	processFD, err := unix.Openat(view.procFD, strconv.Itoa(pid), unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return identity, err
	}
	defer unix.Close(processFD)
	nsFD, err := unix.Openat(processFD, "ns", unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return identity, err
	}
	defer unix.Close(nsFD)
	// This is the sole intentional magic-link follow: the containing directory
	// is anchored in verified procfs, and the opened target must prove NSFS and
	// the PID namespace type. Lease path components never follow links.
	fd, err := unix.Openat(nsFD, "pid", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return identity, err
	}
	defer unix.Close(fd)
	var filesystem unix.Statfs_t
	if err := unix.Fstatfs(fd, &filesystem); err != nil || int64(filesystem.Type) != unix.NSFS_MAGIC {
		return identity, errors.New("namespace filesystem is unproven")
	}
	kind, err := unix.IoctlRetInt(fd, unix.NS_GET_NSTYPE)
	if err != nil || kind != unix.CLONE_NEWPID {
		return identity, errors.New("namespace type is unproven")
	}
	if err := unix.Fstat(fd, &identity); err != nil {
		return identity, err
	}
	var current unix.Stat_t
	if err := unix.Fstatat(nsFD, "pid", &current, 0); err != nil ||
		identity.Dev != current.Dev || identity.Ino != current.Ino {
		return identity, errors.New("namespace identity changed")
	}
	return identity, nil
}

func (view *modelLeaseKernelView) readLocks() ([]byte, error) {
	if err := view.proveVisibility(); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(view.procFD, "locks", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "kernel lock table")
	defer file.Close()
	var before, after unix.Stat_t
	var filesystem unix.Statfs_t
	if unix.Fstat(fd, &before) != nil || before.Mode&unix.S_IFMT != unix.S_IFREG ||
		unix.Fstatfs(fd, &filesystem) != nil || int64(filesystem.Type) != unix.PROC_SUPER_MAGIC {
		return nil, errors.New("kernel lock file is unproven")
	}
	record, err := io.ReadAll(io.LimitReader(file, maximumModelLockTableBytes+1))
	if err != nil || len(record) > maximumModelLockTableBytes {
		return nil, errors.New("kernel lock read is unproven")
	}
	if unix.Fstatat(view.procFD, "locks", &after, unix.AT_SYMLINK_NOFOLLOW) != nil ||
		!sameModelLeaseNode(before, after) {
		return nil, errors.New("kernel lock file changed")
	}
	if err := view.proveVisibility(); err != nil {
		return nil, err
	}
	return record, nil
}
