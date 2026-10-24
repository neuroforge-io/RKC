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

type parentUnitBinding struct {
	unit         string
	userGroup    string
	userID       int
	lifetimeOnly bool
}

func (binding parentUnitBinding) valid() bool {
	if binding.unit == "" {
		return binding.userGroup == "" && !binding.lifetimeOnly
	}
	if binding.lifetimeOnly {
		return ordinaryUserService(binding.userGroup, binding.userID) == binding.unit
	}
	return validLowPriorityUnit(binding.unit)
}

func currentParentUnitBinding() (parentUnitBinding, error) {
	return currentParentUnitBindingUsing("/proc", os.Getpid(), os.Getuid(), RequireCurrentProcessLowPriority, currentUserUnitControlGroup)
}

// This compatibility helper returns only a name; the constructor's string-only
// injection seam accepts reserved units only. Ordinary services carry separate
// lifetime-only evidence through currentParentUnitBinding instead.
func currentParentRKCUnitUsing(procRoot string, pid int, proveEnvelope func() error, managerGroup func(string) (string, error)) (string, error) {
	binding, err := currentParentUnitBindingUsing(procRoot, pid, os.Getuid(), proveEnvelope, managerGroup)
	return binding.unit, err
}

// Reserved RKC parents require the full resource envelope. An ordinary user
// service can prove only its lifetime through matching kernel/user-manager
// membership. That proof never authorizes reusable resource admission; a new
// child retains its own limits and pre-launch admission. Environment names,
// system services, sessions, and namespace-hidden memberships grant no binding.
func currentParentUnitBindingUsing(procRoot string, pid, uid int, proveEnvelope func() error, managerGroup func(string) (string, error)) (parentUnitBinding, error) {
	fail := func(err error) (parentUnitBinding, error) { return parentUnitBinding{}, err }
	if pid <= 0 || proveEnvelope == nil || managerGroup == nil {
		return fail(errors.New("parent resource guard inspector is not configured"))
	}
	path := filepath.Join(procRoot, strconv.Itoa(pid), "cgroup")
	record, identity, err := readParentMembership(path)
	if err != nil {
		return fail(fmt.Errorf("read parent cgroup membership: %w", err))
	}
	relative, err := unifiedCgroupPathAllowRoot(record)
	if err != nil {
		return fail(fmt.Errorf("parse parent cgroup membership: %w", err))
	}
	unit := filepath.Base(filepath.FromSlash(relative))
	binding := parentUnitBinding{unit: unit}
	if !validLowPriorityUnit(unit) {
		if strings.HasPrefix(unit, "rkc-low-") {
			return fail(errors.New("parent cgroup claims an invalid reserved RKC unit"))
		}
		if ordinaryUserService(relative, uid) == "" {
			return parentUnitBinding{}, nil
		}
		binding.userGroup, binding.userID, binding.lifetimeOnly = relative, uid, true
	} else if err := proveEnvelope(); err != nil {
		return fail(fmt.Errorf("parent RKC unit envelope is unproven: %w", err))
	}
	group, err := managerGroup(unit)
	if err != nil {
		return fail(fmt.Errorf("observe parent user-systemd unit: %w", err))
	}
	if group != relative {
		return fail(errors.New("parent user-systemd control group differs from kernel membership"))
	}
	observed, observedIdentity, err := readParentMembership(path)
	if err != nil {
		return fail(fmt.Errorf("re-read parent cgroup membership: %w", err))
	}
	if observed != record || !os.SameFile(identity, observedIdentity) {
		return fail(errors.New("parent cgroup membership changed during lifetime proof"))
	}
	return binding, nil
}

func readParentMembership(path string) (string, os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	identity, err := file.Stat()
	if err != nil || !identity.Mode().IsRegular() {
		return "", nil, errors.New("parent cgroup record is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumControlRead+1))
	if err != nil || len(data) > maximumControlRead {
		return "", nil, errors.New("parent cgroup record is unreadable or exceeds its size limit")
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(identity, current) {
		return "", nil, errors.New("parent cgroup record was replaced or is indirect")
	}
	return strings.TrimSpace(string(data)), identity, nil
}

func ordinaryUserService(relative string, uid int) string {
	if uid <= 0 {
		return ""
	}
	prefix := "/user.slice/user-" + strconv.Itoa(uid) + ".slice/user@" + strconv.Itoa(uid) + ".service/"
	if !strings.HasPrefix(relative, prefix) {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(relative, prefix), "/")
	for index, part := range parts {
		suffix := ".slice"
		if index == len(parts)-1 {
			suffix = ".service"
		}
		if !validLifetimeUnitName(part, suffix) {
			return ""
		}
	}
	unit := parts[len(parts)-1]
	if strings.HasPrefix(unit, "rkc-low-") {
		return ""
	}
	return unit
}

func validLifetimeUnitName(unit, suffix string) bool {
	if len(unit) > 255 || !strings.HasSuffix(unit, suffix) || len(unit) <= len(suffix) {
		return false
	}
	for index, character := range unit {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' {
			continue
		}
		if index == 0 || character != '-' && character != '.' && character != '@' && character != ':' {
			return false
		}
	}
	return true
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
