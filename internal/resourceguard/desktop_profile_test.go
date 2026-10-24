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

func TestDesktopProfileDefaultsAndCoherentNarrowing(t *testing.T) {
	clearResourceProfile(t)
	t.Setenv(ResourceProfileEnvironment, "default")
	profile, err := resourceProfileFromEnvironment()
	if err != nil || profile.selected || profile.memoryHighMiB != 4096 || profile.memoryMaxMiB != 4608 ||
		profile.goMemoryMiB != 4096 || profile.swapMaxMiB != 256 || profile.hostMemoryMinimum != 0 {
		t.Fatalf("explicit default changed compatibility policy: %+v, %v", profile, err)
	}
	for _, test := range []struct {
		name                      string
		high, maximum, goMiB      string
		wantHigh, wantMax, wantGo int64
	}{
		{"desktop", "", "", "", 8192, 10240, 6144},
		{"ceilings", "12288", "16384", "12288", 12288, 16384, 12288},
		{"busy", "512", "640", "384", 512, 640, 384},
		{"narrow heap default", "512", "640", "", 512, 640, 512},
		{"minimum", "64", "64", "", 64, 64, 64},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearResourceProfile(t)
			t.Setenv(ResourceProfileEnvironment, "desktop")
			t.Setenv(memoryHighMiBEnvironment, test.high)
			t.Setenv(memoryMaxMiBEnvironment, test.maximum)
			t.Setenv(goMemoryLimitEnvironment, test.goMiB)
			profile, err := resourceProfileFromEnvironment()
			if err != nil || !profile.selected || profile.name != "desktop" || profile.cpuPercent != 100 ||
				profile.memoryHighMiB != test.wantHigh || profile.memoryMaxMiB != test.wantMax ||
				profile.goMemoryMiB != test.wantGo || profile.swapMaxMiB != 0 || profile.hostMemoryMinimum != 8192*profileMiB {
				t.Fatalf("desktop envelope = %+v, %v", profile, err)
			}
		})
	}
}

func TestDesktopProfileRejectsInvalidSelectionAndCeilingsPrivately(t *testing.T) {
	for _, test := range []struct{ name, value string }{
		{ResourceProfileEnvironment, "PRIVATE_PROFILE_SENTINEL"},
		{ResourceProfileEnvironment, "Desktop"}, {ResourceProfileEnvironment, "desktop "},
		{memoryHighMiBEnvironment, "12289"}, {memoryMaxMiBEnvironment, "16385"},
		{memoryMaxMiBEnvironment, "8191"}, {memorySwapMiBEnvironment, "257"},
		{goMemoryLimitEnvironment, "8193"}, {HostAvailableMemoryMinimumEnvironment, "0"},
		{HostAvailableMemoryMinimumEnvironment, "4095"}, {HostAvailableMemoryMinimumEnvironment, "65537"},
	} {
		t.Run(test.name+"_"+test.value, func(t *testing.T) {
			clearResourceProfile(t)
			t.Setenv(ResourceProfileEnvironment, "desktop")
			t.Setenv(test.name, test.value)
			if _, err := resourceProfileFromEnvironment(); err == nil || !strings.Contains(err.Error(), test.name) || strings.Contains(err.Error(), "PRIVATE_PROFILE_SENTINEL") {
				t.Fatalf("invalid desktop profile error = %v", err)
			}
			if _, err := NewCommand(context.Background(), Config{Executable: "/unused", UnsafeDisableCgroup: true}); err == nil {
				t.Fatal("invalid desktop profile reached command construction")
			}
		})
	}
}

func TestDesktopChildBudgetsAndUsageProofRemainWithinSelectedEnvelope(t *testing.T) {
	for _, budget := range []int64{0, 64*profileMiB + 17, 128*profileMiB + 17, 8192 * profileMiB, 16384 * profileMiB} {
		t.Run(strconv.FormatInt(budget, 10), func(t *testing.T) {
			fixture := newEnvelopeFixture(t)
			t.Setenv(ResourceProfileEnvironment, "desktop")
			t.Setenv(cpuQuotaPercentEnvironment, "25")
			profile, err := resourceProfileFromEnvironment()
			if err != nil {
				t.Fatal(err)
			}
			high, maximum := profile.commandMemory(budget)
			wantMax := int64(10240) * profileMiB
			if budget != 0 && budget < wantMax {
				wantMax = budget / profileMiB * profileMiB
			}
			if maximum != wantMax || high > maximum || high > 8192*profileMiB || high < 64*profileMiB {
				t.Fatalf("desktop child budget enlarged or invalid: high=%d max=%d want=%d", high, maximum, wantMax)
			}
			child := profile.childEnvironment([]string{"PATH=/safe/path", "RKC_RESOURCE_PROFILE=default"}, high, maximum)
			expectEnvironmentEntry(t, child, ResourceProfileEnvironment, "desktop")
			expectEnvironmentEntry(t, child, HostAvailableMemoryMinimumEnvironment, "8192")
			wantGo := int64(6144)
			if wantGo > high/profileMiB {
				wantGo = high / profileMiB
			}
			expectEnvironmentEntry(t, child, goMemoryLimitEnvironment, strconv.FormatInt(wantGo, 10))
			expectEnvironmentEntry(t, child, "GOMEMLIMIT", strconv.FormatInt(wantGo, 10)+"MiB")
			for _, entry := range child {
				name, value, _ := strings.Cut(entry, "=")
				t.Setenv(name, value)
			}
			descendant, err := resourceProfileFromEnvironment()
			if err != nil || descendant.goMemoryMiB*profileMiB > high || descendant.memoryMaxMiB*profileMiB != maximum {
				t.Fatalf("child cannot parse its narrowed profile: %+v %v", descendant, err)
			}
			fixture.writeControl("cpu.max", "25000 100000\n")
			fixture.writeControl("memory.high", strconv.FormatInt(high, 10))
			fixture.writeControl("memory.max", strconv.FormatInt(maximum, 10))
			fixture.writeControl("memory.swap.max", "0")
			// A fixture counter proves admission above the old 4.5 GiB ceiling;
			// no matching allocation or live cgroup is created.
			fixture.writeControl("memory.current", strconv.FormatInt(maximum, 10))
			check := func() error {
				return requireProcessLowPriority(fixture.proc, fixture.cgroup, fixture.pid, func(int) (schedulingEnvelope, error) {
					return schedulingEnvelope{nice: rkcNice, ioClass: rkcIOClassIdle}, nil
				})
			}
			if err := check(); err != nil {
				t.Fatalf("child rejected its exact actual controls/usage: %v", err)
			}
			fixture.writeControl("memory.current", strconv.FormatInt(maximum+1, 10))
			if err := check(); !errors.Is(err, ErrLowPriorityEnvelope) {
				t.Fatalf("usage above actual narrowed ceiling was admitted: %v", err)
			}
		})
	}
}

func TestDesktopReservedAndContainerProofRejectProfileDrift(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(strconv.FormatBool(external), func(t *testing.T) {
			var fixture *envelopeFixture
			if external {
				fixture = newExternalEnvelopeFixture(t)
			} else {
				fixture = newEnvelopeFixture(t)
			}
			t.Setenv(ResourceProfileEnvironment, "desktop")
			controls := map[string]string{
				"cpu.weight": "1", "cpu.max": "100000 100000", "io.weight": "default 1",
				"memory.high": strconv.FormatInt(8192*profileMiB, 10), "memory.max": strconv.FormatInt(10240*profileMiB, 10),
				"memory.current": strconv.FormatInt(6144*profileMiB, 10), "memory.swap.max": "0", "memory.swap.current": "0",
				"pids.max": "128", "pids.current": "1",
			}
			for name, value := range controls {
				fixture.writeControl(name, value)
			}
			scheduling := func(int) (schedulingEnvelope, error) {
				return schedulingEnvelope{nice: rkcNice, ioClass: rkcIOClassIdle}, nil
			}
			check := func() error {
				if external {
					return requireExternalProcessLowPriority(fixture.proc, fixture.cgroup, fixture.pid,
						externalEnvelopeDependencies{verifyFilesystems: func(string, string) error { return nil }, inspectScheduling: scheduling}, false)
				}
				return requireProcessLowPriority(fixture.proc, fixture.cgroup, fixture.pid, scheduling)
			}
			if err := check(); err != nil {
				t.Fatalf("desktop controls rejected: %v", err)
			}
			if !external {
				parent, err := currentParentRKCUnitUsing(fixture.proc, fixture.pid, check,
					func(string) (string, error) { return fixture.relative, nil })
				if err != nil || parent != filepath.Base(fixture.relative) {
					t.Fatalf("desktop parent lost proven lifetime identity: %q %v", parent, err)
				}
			}
			for name, value := range map[string]string{
				"memory.high": strconv.FormatInt(8192*profileMiB+1, 10), "memory.max": strconv.FormatInt(10240*profileMiB+1, 10),
				"memory.swap.max": "1", "memory.current": strconv.FormatInt(10240*profileMiB+1, 10),
			} {
				fixture.writeControl(name, value)
				if err := check(); !errors.Is(err, ErrLowPriorityEnvelope) {
					t.Errorf("desktop %s drift admitted: %v", name, err)
				}
				fixture.writeControl(name, controls[name])
			}
			t.Setenv(ResourceProfileEnvironment, "default")
			if err := check(); !errors.Is(err, ErrLowPriorityEnvelope) {
				t.Fatalf("desktop controls admitted without explicit selector: %v", err)
			}
		})
	}
}

func TestDesktopConstructorPropagatesLimitsAndProvenParentLifetime(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux transient-service constructor")
	}
	installFakeResourceGuardCommands(t, "exit 0\n")
	clearResourceProfile(t)
	t.Setenv(ResourceProfileEnvironment, "desktop")
	t.Setenv("RKC_PRIVATE_API_KEY", "PRIVATE_NOT_IN_CHILD")
	command, err := newCommandWithParentUnit(context.Background(), Config{Executable: "/opt/rkc", Environment: []string{"PATH=/safe/path"}},
		func() error { return nil }, func() (string, error) { return "rkc-low-42.scope", nil })
	if err != nil {
		t.Fatal(err)
	}
	arguments := strings.Join(command.cmd.Args, "\n")
	for _, expected := range []string{
		"MemoryHigh=8589934592", "MemoryMax=10737418240", "MemorySwapMax=0", "CPUQuota=100%",
		"RKC_RESOURCE_PROFILE=desktop", "RKC_HOST_AVAILABLE_MEMORY_MIN_MIB=8192", "GOMEMLIMIT=6144MiB",
		"BindsTo=rkc-low-42.scope", "After=rkc-low-42.scope", "KillMode=control-group",
	} {
		if !strings.Contains(arguments, expected) {
			t.Errorf("desktop launcher omitted %q", expected)
		}
	}
	if command.maximumRSSBytes != 10240*profileMiB {
		t.Fatalf("zero budget retained default-profile hard limit: %d", command.maximumRSSBytes)
	}
	for _, environment := range [][]string{command.cmd.Env, SanitizedModelEnvironment(nil), ResourceGuardEnvironment()} {
		expectEnvironmentEntry(t, environment, ResourceProfileEnvironment, "desktop")
		if strings.Contains(strings.Join(environment, "\n"), "PRIVATE_NOT_IN_CHILD") {
			t.Fatal("desktop environment copied ambient credentials")
		}
	}
}

func TestDesktopHostReserveIsMandatoryAndCheckedWithoutAllocation(t *testing.T) {
	clearResourceProfile(t)
	t.Setenv(ResourceProfileEnvironment, "desktop")
	for _, test := range []struct {
		value string
		want  int64
	}{
		{"", 8192}, {"4096", 4096}, {"65536", 65536},
	} {
		t.Setenv(HostAvailableMemoryMinimumEnvironment, test.value)
		if got, err := HostAvailableMemoryMinimumBytesFromEnvironment(); err != nil || got != test.want*profileMiB {
			t.Fatalf("desktop host reserve = %d, %v", got, err)
		}
	}
	t.Setenv(HostAvailableMemoryMinimumEnvironment, "")
	path := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(path, []byte("MemAvailable: 8388607 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkHostAvailableMemoryForPlatform("linux", path); !errors.Is(err, ErrHostMemoryReserve) {
		t.Fatalf("desktop host below default reserve was admitted: %v", err)
	}
	if err := checkHostAvailableMemoryForPlatform("darwin", path); !errors.Is(err, ErrHostMemoryReserve) {
		t.Fatalf("desktop host without Linux proof was admitted: %v", err)
	}
}
