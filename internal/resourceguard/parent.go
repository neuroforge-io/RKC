package resourceguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func currentParentRKCUnit() (string, error) {
	return currentParentRKCUnitUsing("/proc", os.Getpid(), RequireCurrentProcessLowPriority, currentUserUnitControlGroup)
}

// A worker may bind only to a reserved outer unit proven through actual kernel
// membership and scheduling/resource controls. An environment unit name alone
// is never authority. Namespace-hidden or ordinary external units remain
// unbound; their caller must retain its explicit Run cancellation monitor.
func currentParentRKCUnitUsing(procRoot string, pid int, proveEnvelope func() error, managerGroup func(string) (string, error)) (string, error) {
	if pid <= 0 || proveEnvelope == nil || managerGroup == nil {
		return "", errors.New("parent resource guard inspector is not configured")
	}
	path := filepath.Join(procRoot, strconv.Itoa(pid), "cgroup")
	record, err := readSmallControl(path)
	if err != nil {
		return "", fmt.Errorf("read parent cgroup membership: %w", err)
	}
	relative, err := unifiedCgroupPathAllowRoot(record)
	if err != nil {
		return "", fmt.Errorf("parse parent cgroup membership: %w", err)
	}
	unit := filepath.Base(filepath.FromSlash(relative))
	if !validLowPriorityUnit(unit) {
		if strings.HasPrefix(unit, "rkc-low-") {
			return "", errors.New("parent cgroup claims an invalid reserved RKC unit")
		}
		return "", nil
	}
	if err := proveEnvelope(); err != nil {
		return "", fmt.Errorf("parent RKC unit envelope is unproven: %w", err)
	}
	group, err := managerGroup(unit)
	if err != nil {
		return "", fmt.Errorf("observe parent user-systemd unit: %w", err)
	}
	if group != relative {
		return "", errors.New("parent user-systemd control group differs from kernel membership")
	}
	observed, err := readSmallControl(path)
	if err != nil {
		return "", fmt.Errorf("re-read parent cgroup membership: %w", err)
	}
	if observed != record {
		return "", errors.New("parent cgroup membership changed during envelope proof")
	}
	return unit, nil
}

func currentUserUnitControlGroup(unit string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), unitStateQueryTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "systemctl", "--user", "show", "--property=ControlGroup", "--value", unit)
	command.Env = ResourceGuardEnvironment()
	command.WaitDelay = unitQuiescencePoll
	output := &parentControlOutput{cancel: cancel}
	command.Stdout = output
	command.Stderr = io.Discard
	err := command.Run()
	if output.exceeded {
		return "", errors.New("parent control group query exceeded its size limit")
	}
	if err != nil {
		return "", errors.New("parent control group query failed")
	}
	return strings.TrimSpace(string(output.data[:output.used])), nil
}

var errParentControlOutputTooLarge = errors.New("parent control group output exceeded its limit")

// Fixed storage bounds collection itself. Overflow cancels the query promptly
// and remains sticky, so neither stdout nor a noisy stderr can grow a receipt.
type parentControlOutput struct {
	data     [maximumControlRead]byte
	used     int
	exceeded bool
	cancel   func()
}

func (output *parentControlOutput) Write(data []byte) (int, error) {
	if output.exceeded {
		return 0, errParentControlOutputTooLarge
	}
	written := copy(output.data[output.used:], data)
	output.used += written
	if written != len(data) {
		output.exceeded = true
		if output.cancel != nil {
			output.cancel()
		}
		return written, errParentControlOutputTooLarge
	}
	return written, nil
}
