//go:build linux

package resourceguard

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

type modelLeaseFixture struct {
	root string
	uid  uint32
	path string
}

func newModelLeaseFixture(t *testing.T) modelLeaseFixture {
	t.Helper()
	root := t.TempDir()
	// testing.TempDir's numbered child is created with 0777 before umask;
	// establish this newly owned fixture's permissions explicitly instead of
	// letting a group-writable developer umask invalidate every negative case.
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Geteuid())
	directory := filepath.Join(root, "run", "user", strconv.FormatUint(uint64(uid), 10), "erais")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "model-workload.lock")
	// Deliberately not JSON: admission must never interpret mutable lease text.
	if err := os.WriteFile(path, []byte("private fixture text, not an authority\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := modelLeaseFixture{root: root, uid: uid, path: path}
	views := 0
	if err := fixture.inspect(func() ([]byte, error) {
		views++
		return nil, nil
	}); err != nil || views != 2 {
		// Before any unsafe-path mutation, prove the whole fake chain is valid.
		// This avoids negative tests passing solely because their common setup
		// was already rejected. Only this synthetic fixture's metadata is logged.
		for _, component := range []string{root, filepath.Join(root, "run"), filepath.Join(root, "run", "user"), filepath.Dir(directory), directory, path} {
			var identity unix.Stat_t
			if statErr := unix.Lstat(component, &identity); statErr != nil {
				t.Logf("fixture component metadata unavailable: %v", statErr)
			} else {
				t.Logf("fixture component mode=%#o uid=%d links=%d size=%d", identity.Mode, identity.Uid, identity.Nlink, identity.Size)
			}
		}
		t.Fatalf("invalid observer fixture: clear check=%v kernel views=%d; want two", err, views)
	}
	return fixture
}

func (fixture modelLeaseFixture) inspect(read func() ([]byte, error)) error {
	return checkERAISModelLeaseUsing(fixture.root, fixture.uid, fixture.uid, read)
}

func (fixture modelLeaseFixture) identity(t *testing.T) unix.Stat_t {
	t.Helper()
	var identity unix.Stat_t
	if err := unix.Lstat(fixture.path, &identity); err != nil {
		t.Fatal(err)
	}
	return identity
}

func modelLeaseTestRow(identity unix.Stat_t, prefix string) []byte {
	return []byte(fmt.Sprintf("1: %s %02x:%02x:%d 0 EOF\n", prefix,
		unix.Major(uint64(identity.Dev)), unix.Minor(uint64(identity.Dev)), identity.Ino))
}

func assertModelLeaseRefusal(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrHigherPriorityActive) {
		t.Fatalf("observer = %v; want priority refusal", err)
	}
	for _, forbidden := range []string{"/", "private fixture", "pid=", "inode=", "123456"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("observer disclosed private detail: %v", err)
		}
	}
}

func TestModelLeaseClearRequiresTwoBoundedKernelViews(t *testing.T) {
	for _, missing := range []string{"", "leaf", "directory"} {
		t.Run(missing, func(t *testing.T) {
			fixture := newModelLeaseFixture(t)
			if missing != "" {
				if err := os.Remove(fixture.path); err != nil {
					t.Fatal(err)
				}
			}
			if missing == "directory" {
				if err := os.Remove(filepath.Dir(fixture.path)); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			err := fixture.inspect(func() ([]byte, error) {
				calls++
				return []byte("9: POSIX ADVISORY READ -1 00:00:999999 0 EOF\n"), nil
			})
			if err != nil || calls != 2 {
				t.Fatalf("clear observer = %v, calls=%d; want two views", err, calls)
			}
		})
	}
}

func TestModelLeaseKernelIdentityOverridesPayloadOwnerAndKind(t *testing.T) {
	for _, prefix := range []string{
		"FLOCK ADVISORY WRITE 123456",
		"POSIX ADVISORY READ 0",
		"OFDLCK ADVISORY WRITE -1",
		"-> FLOCK ADVISORY WRITE 123456",
		"FUTURE-KIND UNCERTAIN UNKNOWN -1",
		"-> FUTURE-KIND UNCERTAIN UNKNOWN -1",
	} {
		t.Run(prefix, func(t *testing.T) {
			fixture := newModelLeaseFixture(t)
			row := modelLeaseTestRow(fixture.identity(t), prefix)
			assertModelLeaseRefusal(t, fixture.inspect(func() ([]byte, error) { return row, nil }))
		})
	}
}

func TestModelLeaseSecondKernelViewCanBecomeBusy(t *testing.T) {
	fixture := newModelLeaseFixture(t)
	identity := fixture.identity(t)
	calls := 0
	err := fixture.inspect(func() ([]byte, error) {
		calls++
		if calls == 2 {
			return modelLeaseTestRow(identity, "FLOCK ADVISORY WRITE -1"), nil
		}
		return nil, nil
	})
	assertModelLeaseRefusal(t, err)
	if calls != 2 {
		t.Fatalf("views = %d; want two", calls)
	}
}

func TestModelLeaseExactDeviceAndInodeAreRequired(t *testing.T) {
	fixture := newModelLeaseFixture(t)
	identity := fixture.identity(t)
	for _, change := range []func(*unix.Stat_t){
		func(value *unix.Stat_t) { value.Ino++ },
		func(value *unix.Stat_t) {
			value.Dev = unix.Mkdev(unix.Major(uint64(value.Dev))+1, unix.Minor(uint64(value.Dev)))
		},
		func(value *unix.Stat_t) {
			value.Dev = unix.Mkdev(unix.Major(uint64(value.Dev)), unix.Minor(uint64(value.Dev))+1)
		},
	} {
		other := identity
		change(&other)
		row := modelLeaseTestRow(other, "FLOCK ADVISORY WRITE -1")
		if err := fixture.inspect(func() ([]byte, error) { return row, nil }); err != nil {
			t.Fatalf("unrelated inode blocked: %v", err)
		}
	}
}

func TestModelLeaseRejectsUnsafePathWithoutKernelRead(t *testing.T) {
	for _, kind := range []string{
		"leaf symlink", "directory symlink", "ancestor symlink", "hard link", "FIFO",
		"directory leaf", "oversized leaf", "public leaf", "public runtime", "public erais",
		"writable ancestor", "wrong owner", "missing runtime",
	} {
		t.Run(kind, func(t *testing.T) {
			fixture := newModelLeaseFixture(t)
			directory := filepath.Dir(fixture.path)
			runtimeRoot := filepath.Dir(directory)
			var err error
			switch kind {
			case "leaf symlink", "FIFO", "directory leaf":
				err = os.Remove(fixture.path)
				if err == nil {
					switch kind {
					case "leaf symlink":
						err = os.Symlink("missing-private-target", fixture.path)
					case "FIFO":
						err = unix.Mkfifo(fixture.path, 0o600)
					case "directory leaf":
						err = os.Mkdir(fixture.path, 0o700)
					}
				}
			case "directory symlink":
				err = os.Rename(directory, directory+"-saved")
				if err == nil {
					err = os.Symlink(directory+"-saved", directory)
				}
			case "ancestor symlink":
				run := filepath.Join(fixture.root, "run")
				err = os.Rename(run, run+"-saved")
				if err == nil {
					err = os.Symlink(run+"-saved", run)
				}
			case "hard link":
				err = os.Link(fixture.path, fixture.path+"-alias")
			case "oversized leaf":
				err = os.WriteFile(fixture.path, bytes.Repeat([]byte{'x'}, maximumModelLeaseBytes+1), 0o600)
			case "public leaf":
				err = os.Chmod(fixture.path, 0o644)
			case "public runtime":
				err = os.Chmod(runtimeRoot, 0o750)
			case "public erais":
				err = os.Chmod(directory, 0o710)
			case "writable ancestor":
				err = os.Chmod(filepath.Join(fixture.root, "run"), 0o770)
			case "wrong owner":
				fixture.uid++
			case "missing runtime":
				err = os.Rename(runtimeRoot, runtimeRoot+"-saved")
			}
			if err != nil {
				t.Fatal(err)
			}
			reads := 0
			assertModelLeaseRefusal(t, fixture.inspect(func() ([]byte, error) { reads++; return nil, nil }))
			if reads != 0 {
				t.Fatalf("unsafe lease triggered %d lock table reads", reads)
			}
		})
	}
}

func TestModelLeaseDetectsPathAndDescriptorDrift(t *testing.T) {
	for _, kind := range []string{"replace leaf", "rewrite leaf", "chmod leaf", "link leaf", "replace erais", "replace run", "create missing leaf", "create missing erais"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newModelLeaseFixture(t)
			if strings.HasPrefix(kind, "create missing") {
				if err := os.Remove(fixture.path); err != nil {
					t.Fatal(err)
				}
				if kind == "create missing erais" {
					if err := os.Remove(filepath.Dir(fixture.path)); err != nil {
						t.Fatal(err)
					}
				}
			}
			mutated := false
			assertModelLeaseRefusal(t, fixture.inspect(func() ([]byte, error) {
				if mutated {
					return nil, nil
				}
				mutated = true
				var err error
				switch kind {
				case "replace leaf":
					err = os.Rename(fixture.path, fixture.path+"-saved")
					if err == nil {
						err = os.WriteFile(fixture.path, []byte("private fixture text, not an authority\n"), 0o600)
					}
				case "rewrite leaf":
					err = os.WriteFile(fixture.path, []byte("different private fixture text\n"), 0o600)
				case "chmod leaf":
					err = os.Chmod(fixture.path, 0o400)
				case "link leaf":
					err = os.Link(fixture.path, fixture.path+"-alias")
				case "replace erais", "replace run":
					directory := filepath.Dir(fixture.path)
					if kind == "replace run" {
						directory = filepath.Join(fixture.root, "run")
					}
					err = os.Rename(directory, directory+"-saved")
					if err == nil {
						err = os.Mkdir(directory, 0o700)
					}
				case "create missing leaf":
					err = os.WriteFile(fixture.path, nil, 0o600)
				case "create missing erais":
					err = os.Mkdir(filepath.Dir(fixture.path), 0o700)
				}
				if err != nil {
					t.Fatal(err)
				}
				return nil, nil
			}))
		})
	}
}

func TestModelLeaseRejectsUnprovenKernelSnapshots(t *testing.T) {
	for _, record := range [][]byte{
		[]byte("private malformed kernel snapshot\n"),
		[]byte("1: FLOCK ADVISORY WRITE -1 00:00:123456 0 EOF\x00\n"),
		[]byte("1: FLOCK ADVISORY WRITE nope 00:00:123456 0 EOF\n"),
		[]byte("1: FLOCK ADVISORY WRITE -1 00:00:nope 0 EOF\n"),
		[]byte("1: FLOCK ADVISORY WRITE -1 00:00:123456 nope EOF\n"),
		[]byte("1: FLOCK ADVISORY WRITE -1 00:00:123456 0 nope\n"),
		[]byte("bad: FLOCK ADVISORY WRITE -1 00:00:123456 0 EOF\n"),
		[]byte("1: FLOCK ADVISORY WRITE -1 00:00:123456 0 EOF extra\n"),
		[]byte("\n\n"),
		bytes.Repeat([]byte{'x'}, maximumModelLockRowBytes+1),
		bytes.Repeat([]byte{'x'}, maximumModelLockTableBytes+1),
		bytes.Repeat([]byte("1: FLOCK ADVISORY WRITE -1 00:00:123456 0 EOF\n"), maximumModelLockRows+1),
	} {
		fixture := newModelLeaseFixture(t)
		assertModelLeaseRefusal(t, fixture.inspect(func() ([]byte, error) { return record, nil }))
	}
	fixture := newModelLeaseFixture(t)
	assertModelLeaseRefusal(t, fixture.inspect(func() ([]byte, error) {
		return nil, errors.New("private kernel path /secret/123456")
	}))
	assertModelLeaseRefusal(t, fixture.inspect(nil))
}

func TestModelLeaseEmptyOpaqueLeaseCanBeUnheld(t *testing.T) {
	fixture := newModelLeaseFixture(t)
	if err := os.WriteFile(fixture.path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.inspect(func() ([]byte, error) { return nil, nil }); err != nil {
		t.Fatalf("unheld empty lease = %v", err)
	}
}

func TestModelLeaseKernelViewRejectsFakeProc(t *testing.T) {
	if view, err := openModelLeaseKernelView(t.TempDir(), os.Getpid()); err == nil {
		view.close()
		t.Fatal("ordinary directory accepted as procfs")
	}
	if view, err := openModelLeaseKernelView("/proc", 0); err == nil {
		view.close()
		t.Fatal("invalid observer PID accepted")
	}
}

func TestModelLeasePIDNamespaceMustBeInitial(t *testing.T) {
	initial := unix.Stat_t{Dev: 7, Ino: initialModelPIDNamespaceInode}
	if !modelLeaseInitialPIDNamespace(initial) {
		t.Fatal("initial namespace rejected")
	}
	nested := initial
	nested.Ino++
	if modelLeaseInitialPIDNamespace(nested) || modelLeaseInitialPIDNamespace(unix.Stat_t{}) {
		t.Fatal("nested or missing namespace accepted")
	}
}

func TestModelLeaseSelfLinkMustNameCaller(t *testing.T) {
	const pid = 123456
	for _, target := range []string{
		"123456", "123457", "0123456", "+123456", "../123456", "missing", "directory",
		strings.Repeat("1", 64),
	} {
		t.Run(target, func(t *testing.T) {
			root := t.TempDir()
			if target == "directory" {
				if err := os.Mkdir(filepath.Join(root, "self"), 0o700); err != nil {
					t.Fatal(err)
				}
			} else if target != "missing" {
				if err := os.Symlink(target, filepath.Join(root, "self")); err != nil {
					t.Fatal(err)
				}
			}
			fd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			err = modelLeaseSelfLink(fd, pid)
			if (err == nil) != (target == "123456") {
				t.Fatalf("self link %q: %v", target, err)
			}
			if err := modelLeaseSelfLink(fd, 0); err == nil {
				t.Fatal("invalid caller accepted")
			}
		})
	}
}

func TestModelLeaseObservesActualTemporaryFlockWithoutChangingIt(t *testing.T) {
	// The fixture is an unrelated private temporary inode, never the actual
	// canonical lease. Nested namespace CI must refuse observation, not infer
	// host-wide visibility from a filtered /proc/locks table.
	kernel, err := openModelLeaseKernelView("/proc", os.Getpid())
	if err != nil {
		t.Skip("initial PID namespace visibility unavailable")
	}
	defer kernel.close()
	fixture := newModelLeaseFixture(t)
	before := fixture.identity(t)
	fd, err := unix.Open(fixture.path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	assertModelLeaseRefusal(t, fixture.inspect(kernel.readLocks))
	if !sameModelLeaseNode(before, fixture.identity(t)) {
		t.Fatal("observer changed fixture metadata")
	}
	if err := unix.Flock(fd, unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := fixture.inspect(kernel.readLocks); err != nil {
		t.Fatalf("released temporary fixture = %v", err)
	}
}
