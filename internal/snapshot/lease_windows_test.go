//go:build windows

package snapshot

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"path/filepath"
	"testing"

	"github.com/neuroforge-io/RKC/internal/privatepath"
)

func TestWindowsLeaseSurvivesRenameAndBlocksRecovery(t *testing.T) {
	root := t.TempDir()
	building := filepath.Join(root, "building")
	if err := os.Mkdir(building, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(building, "lease")
	lease, err := createTransactionLease(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, live, err := acquireAbandonedTransactionLease(path); err != nil || !live {
		t.Fatalf("active lease = live %t, %v", live, err)
	}
	published := filepath.Join(root, "published")
	if err := privatepath.Rename(building, published); err != nil {
		t.Fatalf("live lease prevented directory publication: %v", err)
	}
	path = filepath.Join(published, "lease")
	if err := lease.validate(path); err != nil {
		t.Fatalf("renamed lease lost identity: %v", err)
	}
	if _, live, err := acquireAbandonedTransactionLease(path); err != nil || !live {
		t.Fatalf("renamed active lease = live %t, %v", live, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, live, err := acquireAbandonedTransactionLease(path)
	if err != nil || live || recovered == nil {
		t.Fatalf("released lease = recovered %v, live %t, %v", recovered, live, err)
	}
	_ = recovered.Close()
}

func TestWindowsLeaseRejectsInvalidDescriptorAndPaths(t *testing.T) {
	event := &windowsLeaseEvent{handle: windows.Handle(1 << 30)}
	if event.valid() {
		t.Fatal("invalid event descriptor accepted")
	}
	if err := validateWindowsLeaseEventSecurity(event.handle, nil); err == nil {
		t.Fatal("invalid event security accepted")
	}
	if _, _, err := acquireWindowsLeaseEvent("invalid\x00event"); err == nil {
		t.Fatal("NUL event name accepted")
	}
	if _, err := openLeaseFile("bad\x00path", true); err == nil {
		t.Fatal("NUL lease path accepted")
	}
	if _, err := openLeaseFile(filepath.Join(t.TempDir(), "missing"), false); err == nil {
		t.Fatal("missing existing lease accepted")
	}
}

func TestWindowsLeaseRecordTamperingCannotBypassLiveness(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace_%t", replace), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lease")
			lease, err := createTransactionLease(path)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			record := lease.record
			if replace {
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
			} else {
				// Change exactly one token byte, retaining the original inode.
				at := len(windowsLeaseRecordPrefix)
				replacement := "0"
				if record[at] == '0' {
					replacement = "1"
				}
				record = record[:at] + replacement + record[at+1:]
			}
			if err := os.WriteFile(path, []byte(record), 0600); err != nil {
				t.Fatal(err)
			}
			if err := lease.validate(path); !errors.Is(err, ErrBuildingUnowned) {
				t.Fatalf("changed lease validation = %v", err)
			}
			recovered, live, err := acquireAbandonedTransactionLease(path)
			if recovered != nil {
				_ = recovered.Close()
			}
			if err != nil || !live || recovered != nil {
				t.Fatalf("changed lease bypassed live directory event: %v, %t, %v", recovered, live, err)
			}
		})
	}
}

func TestWindowsLeaseRejectsMalformedAndLegacyRecords(t *testing.T) {
	for _, record := range []string{"", "legacy", windowsLeaseRecordPrefix + strings.Repeat("A", 64) + "\n", windowsLeaseRecordPrefix + strings.Repeat("g", 64) + "\n", windowsLeaseRecordPrefix + strings.Repeat("0", 64) + "x", windowsLeaseRecordPrefix + strings.Repeat("0", 64) + "\nextra"} {
		path := filepath.Join(t.TempDir(), "lease")
		if err := os.WriteFile(path, []byte(record), 0600); err != nil {
			t.Fatal(err)
		}
		lease, live, err := acquireAbandonedTransactionLease(path)
		if lease != nil {
			_ = lease.Close()
		}
		if !errors.Is(err, ErrBuildingUnowned) || live || lease != nil {
			t.Fatalf("invalid lease admitted: length=%d live=%t err=%v", len(record), live, err)
		}
	}
}

func TestWindowsLeaseVolumeRejectsNonlocalIdentities(t *testing.T) {
	const guid = "12345678-1234-5678-9ABC-123456789012"
	volume, err := windowsLeaseVolume(`\\?\Volume{` + guid + `}\folder`)
	if err != nil || volume != strings.ToLower(guid) {
		t.Fatalf("local volume = %q, %v", volume, err)
	}
	for _, path := range []string{`C:\folder`, `\\server\share\folder`, `\\?\UNC\server\share`, `\\?\Volume{x}\folder`, `\\?\Volume{12345678x1234-5678-9abc-123456789012}\folder`, `\\?\Volume{12345678-1234-5678-9abc-123456789012}x`} {
		if _, err := windowsLeaseVolume(path); !errors.Is(err, ErrBuildingUnowned) {
			t.Fatalf("nonlocal/invalid volume admitted %q: %v", path, err)
		}
	}
}

// The child really owns the event: abrupt termination, rather than a deferred
// Close in this process, must make its renamed transaction recoverable.
func TestWindowsLeaseCrossProcessCrashRecovery(t *testing.T) {
	root := t.TempDir()
	building := filepath.Join(root, "building")
	if err := os.Mkdir(building, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(building, "lease")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsLeaseChildProcess$")
	command.Env = append(os.Environ(), "RKC_WINDOWS_LEASE_CHILD=hold", "RKC_WINDOWS_LEASE_PATH="+path)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "ready\n" {
			_ = command.Process.Kill()
			_ = command.Wait()
			waited = true
			t.Fatalf("child readiness %q: %s", line, stderr.String())
		}
	case <-ctx.Done():
		t.Fatal("child lease readiness timed out")
	}
	if lease, live, err := acquireAbandonedTransactionLease(path); lease != nil || !live || err != nil {
		t.Fatalf("cross-process live proof = %v %t %v", lease, live, err)
	}
	moved := filepath.Join(root, "published")
	if err := privatepath.Rename(building, moved); err != nil {
		t.Fatalf("live child blocked directory rename: %v", err)
	}
	path = filepath.Join(moved, "lease")
	if lease, live, err := acquireAbandonedTransactionLease(path); lease != nil || !live || err != nil {
		t.Fatalf("renamed cross-process live proof = %v %t %v", lease, live, err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	waited = true
	lease, live, err := acquireAbandonedTransactionLease(path)
	if err != nil || live || lease == nil {
		t.Fatalf("crashed child recovery = %v %t %v", lease, live, err)
	}
	defer lease.Close()
	if err := lease.validate(path); err != nil {
		t.Fatal(err)
	}
	probe := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsLeaseChildProcess$")
	probe.Env = append(os.Environ(), "RKC_WINDOWS_LEASE_CHILD=probe", "RKC_WINDOWS_LEASE_PATH="+path)
	if output, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("recovery lease did not exclude child: %v %s", err, output)
	}
}

func TestWindowsLeaseChildProcess(t *testing.T) {
	mode := os.Getenv("RKC_WINDOWS_LEASE_CHILD")
	if mode == "" {
		return
	}
	path := os.Getenv("RKC_WINDOWS_LEASE_PATH")
	if mode == "probe" {
		lease, live, err := acquireAbandonedTransactionLease(path)
		if lease != nil {
			_ = lease.Close()
		}
		if err != nil || !live || lease != nil {
			t.Fatalf("parent recovery lease = %v %t %v", lease, live, err)
		}
		return
	}
	if mode != "hold" {
		t.Fatal("unknown lease child mode")
	}
	lease, err := createTransactionLease(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}
