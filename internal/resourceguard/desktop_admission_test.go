package resourceguard

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestDesktopAdmissionRequiresPhysicalHardAllowancePlusReserve(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	for _, test := range []struct {
		name, content string
		pass          bool
	}{
		{"exact sum", "MemTotal: 18874368 kB\nMemAvailable: 18874368 kB\n", true},
		{"larger host", "MemTotal: 33554432 kB\nMemAvailable: 25165824 kB\n", true},
		{"reserve alone", "MemTotal: 33554432 kB\nMemAvailable: 8388608 kB\n", false},
		{"one KiB below sum", "MemTotal: 33554432 kB\nMemAvailable: 18874367 kB\n", false},
		{"total too small", "MemTotal: 8388608 kB\nMemAvailable: 8388608 kB\n", false},
		{"inconsistent", "MemTotal: 8388608 kB\nMemAvailable: 25165824 kB\n", false},
		{"missing total", "MemAvailable: 25165824 kB\n", false},
		{"missing available", "MemTotal: 33554432 kB\n", false},
		{"duplicate total", "MemTotal: 33554432 kB\nMemTotal: 33554432 kB\nMemAvailable: 25165824 kB\n", false},
		{"wrong unit", "MemTotal: 33554432 MB\nMemAvailable: 25165824 kB\n", false},
		{"negative", "MemTotal: -33554432 kB\nMemAvailable: 25165824 kB\n", false},
		{"signed", "MemTotal: +33554432 kB\nMemAvailable: 25165824 kB\n", false},
		{"leading zero", "MemTotal: 033554432 kB\nMemAvailable: 25165824 kB\n", false},
		{"private malformed", "MemTotal: PRIVATE_ADMISSION_MEMORY kB\nMemAvailable: 25165824 kB\n", false},
		{"oversized number", "MemTotal: 9999999999999999 kB\nMemAvailable: 25165824 kB\n", false},
		{"oversized file", strings.Repeat("x", int(maximumMeminfoBytes)+1), false},
		{"oversized line", strings.Repeat("x", maximumMeminfoLineBytes+1) + "\nMemTotal: 33554432 kB\nMemAvailable: 25165824 kB\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "meminfo")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			err := checkDesktopHostMemoryAdmission(path, 10*gib, 8*gib)
			if test.pass && err != nil || !test.pass && !errors.Is(err, ErrHostMemoryReserve) {
				t.Fatalf("desktop admission = %v, want pass=%t", err, test.pass)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE_ADMISSION_MEMORY") {
				t.Fatal("desktop admission disclosed malformed counter content")
			}
		})
	}
	for _, test := range []struct{ maximum, reserve int64 }{
		{0, 8 * gib}, {10 * gib, 4096*profileMiB - 1}, {math.MaxInt64, 8 * gib},
	} {
		if err := checkDesktopHostMemoryAdmission("unused", test.maximum, test.reserve); !errors.Is(err, ErrHostMemoryReserve) {
			t.Fatalf("invalid desktop admission arithmetic was accepted: %v", err)
		}
	}
	if err := checkDesktopHostMemoryAdmission(filepath.Join(t.TempDir(), "missing"), 10*gib, 8*gib); !errors.Is(err, ErrHostMemoryReserve) {
		t.Fatalf("unreadable desktop physical counters were accepted: %v", err)
	}
	if err := (resourceProfile{name: "default"}).hostMemoryAdmissionCheckForPlatform("darwin", "unused", 0); err != nil {
		t.Fatalf("default profile gained desktop admission: %v", err)
	}
	if err := (resourceProfile{name: "desktop"}).hostMemoryAdmissionCheckForPlatform("darwin", "unused", 10*gib); !errors.Is(err, ErrHostMemoryReserve) {
		t.Fatalf("desktop admission without Linux physical proof succeeded: %v", err)
	}
}

func desktopAdmissionFixtureCommand(t *testing.T, path string, budget int64, args ...string) (*Command, resourceProfile) {
	t.Helper()
	clearResourceProfile(t)
	t.Setenv(ResourceProfileEnvironment, "desktop")
	profile, err := resourceProfileFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	command, err := newCommand(context.Background(), Config{Executable: args[0], Arguments: args[1:],
		MaximumRSSBytes: budget, UnsafeDisableCgroup: true}, func() error { return nil })
	if err != nil || command.admissionCheck == nil {
		t.Fatalf("desktop constructor omitted launch admission: %v", err)
	}
	// Redirect both read-only observations into the same tiny fixture. No real
	// host state, cgroup or service is inspected by these runtime contracts.
	command.priorityCheck = func() error { return profile.hostMemoryCheckForPlatform("linux", path) }
	command.admissionCheck = func() error {
		return profile.hostMemoryAdmissionCheckForPlatform("linux", path, command.maximumRSSBytes)
	}
	return command, profile
}

func TestDesktopRunRefusesBeforeSpawnAndAdmitsNarrowedBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	for _, budget := range []int64{0, 128 * profileMiB} {
		t.Run(strconv.FormatInt(budget, 10), func(t *testing.T) {
			root := t.TempDir()
			path, marker := filepath.Join(root, "meminfo"), filepath.Join(root, "spawned")
			command, profile := desktopAdmissionFixtureCommand(t, path, budget, "/bin/sh", "-c", `printf spawned > "$1"`, "fixture", marker)
			// Headroom satisfies the continuous reserve but not the new budget.
			if err := os.WriteFile(path, []byte("MemTotal: 33554432 kB\nMemAvailable: 8388608 kB\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := command.Run(context.Background(), nil, nil); !errors.Is(err, ErrHostMemoryReserve) || command.cmd.Process != nil {
				t.Fatalf("desktop command spawned without full headroom: %v", err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected desktop launch wrote its marker: %v", err)
			}
			requiredKiB := (command.maximumRSSBytes + profile.hostMemoryMinimum) / 1024
			content := "MemTotal: 33554432 kB\nMemAvailable: " + strconv.FormatInt(requiredKiB, 10) + " kB\n"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := command.Run(context.Background(), nil, nil); err != nil {
				t.Fatalf("sufficient effective command headroom refused: %v", err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("admitted desktop launch omitted its marker: %v", err)
			}
		})
	}
}

func TestDesktopRunRechecksCancellationAfterAdmission(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	command, _ := desktopAdmissionFixtureCommand(t, "unused", 0, "/bin/true")
	command.priorityCheck = func() error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command.admissionCheck = func() error { cancel(); return nil }
	if _, err := command.Run(ctx, nil, nil); !errors.Is(err, context.Canceled) || command.cmd.Process != nil {
		t.Fatalf("desktop admission cancellation still spawned: %v", err)
	}
}

func TestDesktopRunningMonitorRetainsReserveWithoutChargingHardAllowanceAgain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	path := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(path, []byte("MemTotal: 33554432 kB\nMemAvailable: 25165824 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command, profile := desktopAdmissionFixtureCommand(t, path, 0, "/bin/sleep", "10")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checks, admissions := 0, 0
	command.priorityCheck = func() error {
		checks++
		err := profile.hostMemoryCheckForPlatform("linux", path)
		if checks >= 2 {
			cancel()
		}
		return err
	}
	admit := command.admissionCheck
	command.admissionCheck = func() error {
		admissions++
		if err := admit(); err != nil {
			return err
		}
		// Model the job consuming its admitted allowance after startup. The
		// remaining physical headroom exactly meets its continuous reserve.
		return os.WriteFile(path, []byte("MemTotal: 33554432 kB\nMemAvailable: 8388608 kB\n"), 0o600)
	}
	if _, err := command.Run(ctx, nil, nil); !errors.Is(err, context.Canceled) || errors.Is(err, ErrHostMemoryReserve) {
		t.Fatalf("running desktop command charged its allowance twice: %v", err)
	}
	if admissions != 1 || checks < 2 {
		t.Fatalf("admission/monitor checks = %d/%d", admissions, checks)
	}
}
