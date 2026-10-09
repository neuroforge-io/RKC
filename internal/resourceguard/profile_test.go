package resourceguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func clearResourceProfile(t *testing.T) {
	t.Helper()
	for _, name := range profileEnvironmentNames {
		t.Setenv(name, "")
	}
	t.Setenv("GOMEMLIMIT", "")
}

func selectBusyResourceProfile(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		cpuQuotaPercentEnvironment: "25", memoryHighMiBEnvironment: "512",
		memoryMaxMiBEnvironment: "640", memorySwapMiBEnvironment: "0",
		goMemoryLimitEnvironment: "384", HostAvailableMemoryMinimumEnvironment: "512",
	} {
		t.Setenv(name, value)
	}
}

func TestResourceProfileDefaultsAndBusyLimits(t *testing.T) {
	clearResourceProfile(t)
	profile, err := resourceProfileFromEnvironment()
	if err != nil || profile.selected || profile.cpuPercent != 100 ||
		profile.memoryHighMiB != 4096 || profile.memoryMaxMiB != 4608 ||
		profile.swapMaxMiB != 256 || profile.goMemoryMiB != 4096 || profile.hostMemoryMinimum != 0 {
		t.Fatalf("default profile = %+v, %v", profile, err)
	}
	selectBusyResourceProfile(t)
	profile, err = resourceProfileFromEnvironment()
	if err != nil || !profile.selected || profile.cpuPercent != 25 ||
		profile.memoryHighMiB != 512 || profile.memoryMaxMiB != 640 ||
		profile.swapMaxMiB != 0 || profile.goMemoryMiB != 384 || profile.hostMemoryMinimum != 512*profileMiB {
		t.Fatalf("busy profile = %+v, %v", profile, err)
	}
	t.Setenv(goMemoryLimitEnvironment, "")
	profile, err = resourceProfileFromEnvironment()
	if err != nil || profile.goMemoryMiB != 512 {
		t.Fatalf("inherited Go limit = %+v, %v", profile, err)
	}
	for name, value := range map[string]string{
		cpuQuotaPercentEnvironment: "1", memoryHighMiBEnvironment: "64",
		memoryMaxMiBEnvironment: "64", memorySwapMiBEnvironment: "0",
		HostAvailableMemoryMinimumEnvironment: "65536",
	} {
		t.Setenv(name, value)
	}
	profile, err = resourceProfileFromEnvironment()
	if err != nil || profile.cpuPercent != 1 || profile.memoryHighMiB != 64 ||
		profile.goMemoryMiB != 64 || profile.hostMemoryMinimum != 65536*profileMiB {
		t.Fatalf("minimum profile = %+v, %v", profile, err)
	}
}

func TestResourceProfileRejectsInvalidValuesBeforeConstructingCommand(t *testing.T) {
	for _, test := range []struct{ name, value string }{
		{cpuQuotaPercentEnvironment, "0"}, {cpuQuotaPercentEnvironment, "101"},
		{cpuQuotaPercentEnvironment, "025"}, {cpuQuotaPercentEnvironment, "+25"},
		{cpuQuotaPercentEnvironment, "25.5"}, {cpuQuotaPercentEnvironment, " 25 "},
		{memoryHighMiBEnvironment, "63"}, {memoryHighMiBEnvironment, "4097"},
		{memoryMaxMiBEnvironment, "511"}, {memoryMaxMiBEnvironment, "4609"},
		{memorySwapMiBEnvironment, "-1"}, {memorySwapMiBEnvironment, "257"},
		{goMemoryLimitEnvironment, "63"}, {goMemoryLimitEnvironment, "513"},
		{HostAvailableMemoryMinimumEnvironment, "65537"},
		{HostAvailableMemoryMinimumEnvironment, "0512"},
	} {
		t.Run(test.name+"_"+test.value, func(t *testing.T) {
			clearResourceProfile(t)
			selectBusyResourceProfile(t)
			t.Setenv(test.name, test.value)
			if _, err := resourceProfileFromEnvironment(); err == nil || !strings.Contains(err.Error(), test.name) {
				t.Fatalf("invalid profile error = %v", err)
			}
			if _, err := newCommand(context.Background(), Config{
				Executable: "/bin/true", UnsafeDisableCgroup: true,
			}, func() error { return nil }); err == nil {
				t.Fatal("invalid ambient profile constructed a command")
			}
		})
	}
	for _, name := range profileEnvironmentNames {
		t.Run(name+"_private", func(t *testing.T) {
			clearResourceProfile(t)
			const sentinel = "PRIVATE_PROFILE_VALUE_SENTINEL"
			t.Setenv(name, sentinel)
			if _, err := resourceProfileFromEnvironment(); err == nil || strings.Contains(err.Error(), sentinel) {
				t.Fatalf("private profile error = %v", err)
			}
		})
	}
}

func TestCommandMemoryBudgetsNeverEnlargeSelectedEnvelope(t *testing.T) {
	clearResourceProfile(t)
	defaults, err := resourceProfileFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if high, maximum := defaults.commandMemory(0); high != 4096*profileMiB || maximum != 4608*profileMiB {
		t.Fatalf("default memory = %d, %d", high, maximum)
	}
	if high, maximum := defaults.commandMemory(8192 * profileMiB); high != 8192*profileMiB*8/9 || maximum != 8192*profileMiB {
		t.Fatalf("historical explicit budget = %d, %d", high, maximum)
	}
	selectBusyResourceProfile(t)
	profile, err := resourceProfileFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name                            string
		budget, highMiB, maximum, goMiB int64
	}{
		{"default", 0, 512, 640 * profileMiB, 384},
		{"larger", 2048 * profileMiB, 512, 640 * profileMiB, 384},
		{"smaller", 128 * profileMiB, 113, 128 * profileMiB, 113},
		{"minimum", 64 * profileMiB, 64, 64 * profileMiB, 64},
		{"byte budget", 128*profileMiB + 1, 113, 128 * profileMiB, 113},
		{"minimum byte budget", 64*profileMiB + 17, 64, 64 * profileMiB, 64},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEnvelopeFixture(t)
			high, maximum := profile.commandMemory(test.budget)
			if high != test.highMiB*profileMiB || maximum != test.maximum {
				t.Fatalf("effective memory = %d, %d", high, maximum)
			}
			child := profile.childEnvironment([]string{
				"PATH=/safe/path", "RKC_CPU_QUOTA_PERCENT=100", "RKC_MEMORY_MAX_MIB=4608", "GOMEMLIMIT=8GiB",
			}, high, maximum)
			expectEnvironmentEntry(t, child, "GOMEMLIMIT", strconv.FormatInt(test.goMiB, 10)+"MiB")
			expectEnvironmentEntry(t, child, cpuQuotaPercentEnvironment, "25")
			expectEnvironmentEntry(t, child, memoryHighMiBEnvironment, strconv.FormatInt(test.highMiB, 10))
			expectEnvironmentEntry(t, child, memoryMaxMiBEnvironment, strconv.FormatInt(maximum/profileMiB, 10))
			expectEnvironmentEntry(t, child, memorySwapMiBEnvironment, "0")
			expectEnvironmentEntry(t, child, goMemoryLimitEnvironment, strconv.FormatInt(test.goMiB, 10))
			expectEnvironmentEntry(t, child, HostAvailableMemoryMinimumEnvironment, "512")
			for _, entry := range child {
				name, value, _ := strings.Cut(entry, "=")
				t.Setenv(name, value)
			}
			if descendant, err := resourceProfileFromEnvironment(); err != nil || !descendant.selected {
				t.Fatalf("normalized descendant profile = %+v, %v", descendant, err)
			}
			fixture.writeControl("cpu.max", "25000 100000\n")
			fixture.writeControl("memory.high", strconv.FormatInt(high, 10)+"\n")
			fixture.writeControl("memory.max", strconv.FormatInt(maximum, 10)+"\n")
			fixture.writeControl("memory.swap.max", "0\n")
			if err := requireProcessLowPriority(fixture.proc, fixture.cgroup, fixture.pid, func(int) (schedulingEnvelope, error) {
				return schedulingEnvelope{nice: rkcNice, ioClass: rkcIOClassIdle}, nil
			}); err != nil {
				t.Fatalf("emitted child profile rejected its actual unit: %v", err)
			}
		})
	}
}

func TestGuardedLauncherUsesBusyProfileAndNarrowEnvironment(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux transient-service argument contract")
	}
	installFakeResourceGuardCommands(t, "exit 0\n")
	clearResourceProfile(t)
	selectBusyResourceProfile(t)
	t.Setenv("RKC_PRIVATE_API_KEY", "SECRET_NOT_TO_INHERIT")
	for _, budget := range []int64{0, 128 * profileMiB, 128*profileMiB + 1, 64 * profileMiB, 2048 * profileMiB} {
		command, err := newCommandWithParentUnit(context.Background(), Config{
			Executable: "/opt/rkc", Arguments: []string{"scan", "literal-$SOURCE"},
			Environment:     []string{"PATH=/safe/path", "GOMEMLIMIT=8GiB"},
			MaximumRSSBytes: budget, UnitPrefix: "rkc-low",
		}, func() error { return nil }, func() (string, error) { return "", nil })
		if err != nil {
			t.Fatal(err)
		}
		profile, err := resourceProfileFromEnvironment()
		if err != nil {
			t.Fatal(err)
		}
		high, maximum := profile.commandMemory(budget)
		arguments := strings.Join(command.cmd.Args, "\n")
		for _, expected := range []string{
			"CPUQuota=25%", "MemoryHigh=" + strconv.FormatInt(high, 10),
			"MemoryMax=" + strconv.FormatInt(maximum, 10), "MemorySwapMax=0",
			"RKC_CPU_QUOTA_PERCENT=25", "RKC_HOST_AVAILABLE_MEMORY_MIN_MIB=512",
			"--expand-environment=no", "literal-$SOURCE", "env\n-i",
		} {
			if !strings.Contains(arguments, expected) {
				t.Errorf("launcher omitted %q: %s", expected, arguments)
			}
		}
		if command.maximumRSSBytes != maximum || strings.Contains(arguments, "SECRET_NOT_TO_INHERIT") ||
			strings.Contains(strings.Join(command.cmd.Env, "\n"), "RKC_PRIVATE_API_KEY") {
			t.Fatalf("launcher budget or environment drifted: %+v", command)
		}
	}
	for _, budget := range []int64{63 * profileMiB, 64*1024*profileMiB + 1} {
		if _, err := NewCommand(context.Background(), Config{
			Executable: "/opt/rkc", MaximumRSSBytes: budget,
		}); err == nil {
			t.Fatalf("invalid explicit RSS budget was accepted: %d", budget)
		}
	}
	for _, environment := range [][]string{SanitizedModelEnvironment(nil), ResourceGuardEnvironment()} {
		expectEnvironmentEntry(t, environment, cpuQuotaPercentEnvironment, "25")
		expectEnvironmentEntry(t, environment, goMemoryLimitEnvironment, "384")
		if strings.Contains(strings.Join(environment, "\n"), "SECRET_NOT_TO_INHERIT") {
			t.Fatal("ambient credentials crossed the guard environment boundary")
		}
	}
}

func TestSelectedProfileMustBeProvenByReservedAndContainerControls(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(strconv.FormatBool(external), func(t *testing.T) {
			var fixture *envelopeFixture
			if external {
				fixture = newExternalEnvelopeFixture(t)
			} else {
				fixture = newEnvelopeFixture(t)
			}
			selectBusyResourceProfile(t)
			controls := map[string]string{
				"cpu.max": "25000 100000\n", "memory.high": "536870912\n",
				"memory.max": "671088640\n", "memory.swap.max": "0\n",
			}
			for name, value := range controls {
				fixture.writeControl(name, value)
			}
			check := func() error {
				scheduling := func(int) (schedulingEnvelope, error) {
					return schedulingEnvelope{nice: rkcNice, ioClass: rkcIOClassIdle}, nil
				}
				if !external {
					return requireProcessLowPriority(fixture.proc, fixture.cgroup, fixture.pid, scheduling)
				}
				return requireExternalProcessLowPriority(fixture.proc, fixture.cgroup, fixture.pid,
					externalEnvelopeDependencies{
						verifyFilesystems: func(string, string) error { return nil }, inspectScheduling: scheduling,
					}, false)
			}
			if err := check(); err != nil {
				t.Fatalf("selected profile was rejected: %v", err)
			}
			for _, drift := range []struct{ name, value string }{
				{"cpu.max", "25001 100000\n"}, {"memory.high", "537919488\n"},
				{"memory.max", "672137216\n"}, {"memory.swap.max", "1\n"},
			} {
				fixture.writeControl(drift.name, drift.value)
				if err := check(); !errors.Is(err, ErrLowPriorityEnvelope) {
					t.Errorf("selected %s drift was admitted: %v", drift.name, err)
				}
				fixture.writeControl(drift.name, controls[drift.name])
			}
			fixture.writeControl("memory.max", "402653184\n")
			if err := check(); !errors.Is(err, ErrLowPriorityEnvelope) {
				t.Fatalf("pressure ceiling above hard ceiling was admitted: %v", err)
			}
			fixture.writeControl("cpu.max", "12500 100000\n")
			fixture.writeControl("memory.high", "268435456\n")
			fixture.writeControl("memory.max", "402653184\n")
			if err := check(); err != nil {
				t.Fatalf("stricter selected controls were rejected: %v", err)
			}
			t.Setenv(cpuQuotaPercentEnvironment, "101")
			if err := check(); !errors.Is(err, ErrLowPriorityEnvelope) {
				t.Fatalf("invalid selected profile was admitted: %v", err)
			}
		})
	}
}

func TestProfileHostReserveAndDefaultEnvironmentIsolation(t *testing.T) {
	clearResourceProfile(t)
	profile, err := resourceProfileFromEnvironment()
	if err != nil || profile.hostMemoryCheck() != nil {
		t.Fatalf("disabled host reserve = %v", err)
	}
	values := []string{"PATH=/safe/path", "EXPLICIT_VALUE=kept"}
	child := profile.childEnvironment(values, 128*profileMiB, 128*profileMiB)
	child[0] = "changed"
	if values[0] != "PATH=/safe/path" {
		t.Fatal("default environment reused caller backing storage")
	}
	if len(child) != len(values) {
		t.Fatalf("unselected profile changed historical environment: %v", child)
	}
}

func TestSelectedHostReserveChecksAndUnsupportedPlatforms(t *testing.T) {
	profile := resourceProfile{hostMemoryMinimum: 512 * profileMiB}
	path := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(path, []byte("MemAvailable: 524288 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := profile.hostMemoryCheckForPlatform("linux", path); err != nil {
		t.Fatalf("exact reserve was rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte("MemAvailable: 524287 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := profile.hostMemoryCheckForPlatform("linux", path); !errors.Is(err, ErrHostMemoryReserve) {
		t.Fatalf("below-reserve host was admitted: %v", err)
	}
	if err := profile.hostMemoryCheckForPlatform("darwin", path); !errors.Is(err, ErrHostMemoryReserve) {
		t.Fatalf("unsupported host-memory platform was admitted: %v", err)
	}
	if err := profile.hostMemoryCheckForPlatform("linux", path+".missing"); !errors.Is(err, ErrHostMemoryReserve) {
		t.Fatalf("missing host-memory evidence was admitted: %v", err)
	}
}

func expectEnvironmentEntry(t *testing.T, values []string, name, expected string) {
	t.Helper()
	count := 0
	for _, entry := range values {
		key, value, _ := strings.Cut(entry, "=")
		if key == name {
			count++
			if value != expected {
				t.Errorf("%s = %q, want %q", name, value, expected)
			}
		}
	}
	if count != 1 {
		t.Errorf("%s has %d environment entries, want 1", name, count)
	}
}
