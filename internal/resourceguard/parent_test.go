package resourceguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParentUnitAuthorityUsesKernelMembershipAndEnvelopeProof(t *testing.T) {
	for _, test := range []struct {
		name, record, want                          string
		proofFailure, move, remove, invalidControls bool
		managerFailure, managerMismatch             bool
		wantError                                   bool
	}{
		{name: "scope", record: "0::/user.slice/rkc-low-42.scope\n", want: "rkc-low-42.scope"},
		{name: "service", record: "0::/user.slice/rkc-low-42-123.service\n", want: "rkc-low-42-123.service"},
		{name: "supervised scope", record: "0::/user.slice/rkc-low-42-aB12cD34eF56gH78.scope\n", want: "rkc-low-42-aB12cD34eF56gH78.scope"},
		{name: "supervised service", record: "0::/user.slice/rkc-low-42-aB12cD34eF56gH78.service\n", want: "rkc-low-42-aB12cD34eF56gH78.service"},
		{name: "external", record: "0::/user.slice/workbench.service\n"},
		{name: "namespace hidden", record: "0::/\n"},
		{name: "malformed", record: "not-a-cgroup-record\n", wantError: true},
		{name: "reserved malformed", record: "0::/user.slice/rkc-low-secret.service\n", wantError: true},
		{name: "reserved short nonce", record: "0::/user.slice/rkc-low-42-abcdefghijklmno.scope\n", wantError: true},
		{name: "unproven", record: "0::/user.slice/rkc-low-42.scope\n", proofFailure: true, wantError: true},
		{name: "changed", record: "0::/user.slice/rkc-low-42.scope\n", move: true, wantError: true},
		{name: "removed", record: "0::/user.slice/rkc-low-42.scope\n", remove: true, wantError: true},
		{name: "unsafe controls", record: "0::/user.slice/rkc-low-42.scope\n", invalidControls: true, wantError: true},
		{name: "manager query failed", record: "0::/user.slice/rkc-low-42.scope\n", managerFailure: true, wantError: true},
		{name: "other manager unit", record: "0::/user.slice/rkc-low-42.scope\n", managerMismatch: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEnvelopeFixture(t)
			t.Setenv("RKC_RESOURCE_GUARD_UNIT", "rkc-low-999.service")
			fixture.writeProc("cgroup", test.record)
			if test.want != "" && test.want != filepath.Base(fixture.unit) {
				parent := filepath.Join(fixture.cgroup, "user.slice", test.want)
				if err := os.Rename(fixture.unit, parent); err != nil {
					t.Fatal(err)
				}
				fixture.unit = parent
			}
			if test.invalidControls {
				fixture.writeControl("cpu.max", "max 100000\n")
			}
			proofCalls := 0
			prove := func() error {
				proofCalls++
				if test.proofFailure {
					return ErrLowPriorityEnvelope
				}
				if err := requireProcessLowPriority(fixture.proc, fixture.cgroup, fixture.pid, func(int) (schedulingEnvelope, error) {
					return schedulingEnvelope{nice: rkcNice, ioClass: rkcIOClassIdle}, nil
				}); err != nil {
					return err
				}
				if test.move {
					fixture.writeProc("cgroup", "0::/user.slice/unrelated.scope\n")
				}
				if test.remove {
					return os.Remove(filepath.Join(fixture.process, "cgroup"))
				}
				return nil
			}
			manager := func(unit string) (string, error) {
				if test.managerFailure {
					return "", errors.New("manager unavailable")
				}
				if test.managerMismatch {
					return "/different.slice/" + unit, nil
				}
				return strings.TrimSuffix(strings.TrimPrefix(test.record, "0::"), "\n"), nil
			}
			unit, err := currentParentRKCUnitUsing(fixture.proc, fixture.pid, prove, manager)
			if (err != nil) != test.wantError || unit != test.want {
				t.Fatalf("parent authority = %q, %v; want %q, error=%v", unit, err, test.want, test.wantError)
			}
			if test.want != "" && proofCalls != 1 {
				t.Fatalf("parent envelope was proved %d times, want 1", proofCalls)
			}
			if test.name == "external" || test.name == "namespace hidden" {
				if proofCalls != 0 {
					t.Fatal("external parent acquired authority through an environment unit name")
				}
			}
		})
	}
	manager := func(string) (string, error) { return "", nil }
	if _, err := currentParentRKCUnitUsing(t.TempDir(), 42, func() error { return nil }, manager); err == nil {
		t.Fatal("missing kernel membership was accepted")
	}
	if _, err := currentParentRKCUnitUsing("unused", 0, func() error { return nil }, manager); err == nil {
		t.Fatal("invalid parent process identity was accepted")
	}
	if _, err := currentParentRKCUnitUsing("unused", 42, nil, manager); err == nil {
		t.Fatal("missing parent envelope inspector was accepted")
	}
	if _, err := currentParentRKCUnitUsing("unused", 42, func() error { return nil }, nil); err == nil {
		t.Fatal("missing parent manager inspector was accepted")
	}
}

func TestParentManagerObservationIsBoundedAndFailClosed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux systemctl query fixture")
	}
	clearResourceProfile(t)
	directory := t.TempDir()
	controller := filepath.Join(directory, "systemctl")
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, test := range []struct {
		name, body, want string
		wantError        bool
	}{
		{name: "exact group", body: "printf '/user.slice/rkc-low-42.scope\\n'\n", want: "/user.slice/rkc-low-42.scope"},
		{name: "failed", body: "exit 1\n", wantError: true},
		{name: "oversized", body: "printf '" + strings.Repeat("a", maximumControlRead+1) + "'\n", wantError: true},
		{name: "private stderr", body: "printf 'PRIVATE_QUERY_STDERR_SENTINEL' >&2\nexit 1\n", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := "#!/bin/sh\n[ \"$1\" = --user ] && [ \"$2\" = show ] && " +
				"[ \"$3\" = --property=ControlGroup ] && [ \"$4\" = --value ] && " +
				"[ \"$5\" = rkc-low-42.scope ] || exit 98\n" + test.body
			if err := os.WriteFile(controller, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			group, err := currentUserUnitControlGroup("rkc-low-42.scope")
			if (err != nil) != test.wantError || group != test.want {
				t.Fatalf("manager group = %q, %v; want %q, error=%v", group, err, test.want, test.wantError)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE_QUERY_STDERR_SENTINEL") {
				t.Fatal("manager query exposed private stderr")
			}
		})
	}
}

func TestParentControlOutputCapsCollectionAndCancelsOnOverflow(t *testing.T) {
	cancels := 0
	output := &parentControlOutput{cancel: func() { cancels++ }}
	data := []byte(strings.Repeat("a", maximumControlRead))
	if written, err := output.Write(data); err != nil || written != len(data) || output.exceeded {
		t.Fatalf("exact-size control output = %d, %v", written, err)
	}
	if written, err := output.Write(nil); err != nil || written != 0 {
		t.Fatalf("empty control output = %d, %v", written, err)
	}
	if written, err := output.Write([]byte("private overflow")); !errors.Is(err, errParentControlOutputTooLarge) || written != 0 {
		t.Fatalf("overflow = %d, %v", written, err)
	}
	if written, err := output.Write([]byte("more overflow")); !errors.Is(err, errParentControlOutputTooLarge) || written != 0 {
		t.Fatalf("sticky overflow = %d, %v", written, err)
	}
	if output.used != maximumControlRead || cancels != 1 || string(output.data[:]) != string(data) {
		t.Fatalf("control capture grew or cancellation repeated: used=%d cancels=%d", output.used, cancels)
	}
	partial := &parentControlOutput{}
	if written, err := partial.Write(append(data, 'b')); !errors.Is(err, errParentControlOutputTooLarge) || written != maximumControlRead {
		t.Fatalf("partial bounded write = %d, %v", written, err)
	}
}

func TestWorkerServiceBindsOnlyToProvenParentUnit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux transient-service argument contract")
	}
	installFakeResourceGuardCommands(t, "exit 0\n")
	for _, parent := range []string{
		"rkc-low-42.scope", "rkc-low-42-123.service",
		"rkc-low-42-aB12cD34eF56gH78.scope", "rkc-low-42-aB12cD34eF56gH78.service", "",
	} {
		command, err := newCommandWithParentUnit(context.Background(), Config{Executable: "/opt/model"},
			func() error { return nil }, func() (string, error) { return parent, nil })
		if err != nil {
			t.Fatal(err)
		}
		arguments := strings.Join(command.cmd.Args, "\n")
		if parent != "" {
			for _, property := range []string{"BindsTo=" + parent, "After=" + parent, "KillMode=control-group"} {
				if !strings.Contains(arguments, property) {
					t.Errorf("worker omitted lifetime property %q: %s", property, arguments)
				}
			}
		} else if strings.Contains(arguments, "BindsTo=") || strings.Contains(arguments, "After=") {
			t.Fatal("unproven parent acquired a systemd lifetime dependency")
		}
	}
	for _, resolver := range []func() (string, error){
		nil,
		func() (string, error) { return "", errors.New("parent observation failed") },
		func() (string, error) { return "untrusted.service", nil },
	} {
		if _, err := newCommandWithParentUnit(context.Background(), Config{Executable: "/opt/model"},
			func() error { return nil }, resolver); err == nil {
			t.Fatal("invalid parent authority constructed a worker")
		}
	}
}
