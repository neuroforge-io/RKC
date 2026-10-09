package resourceguard

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

const (
	cpuQuotaPercentEnvironment = "RKC_CPU_QUOTA_PERCENT"
	memoryHighMiBEnvironment   = "RKC_MEMORY_HIGH_MIB"
	memoryMaxMiBEnvironment    = "RKC_MEMORY_MAX_MIB"
	memorySwapMiBEnvironment   = "RKC_MEMORY_SWAP_MAX_MIB"
	goMemoryLimitEnvironment   = "RKC_GO_MEMORY_LIMIT_MIB"
	profileMiB                 = int64(1024 * 1024)
)

var profileEnvironmentNames = []string{
	cpuQuotaPercentEnvironment, memoryHighMiBEnvironment, memoryMaxMiBEnvironment,
	memorySwapMiBEnvironment, goMemoryLimitEnvironment, HostAvailableMemoryMinimumEnvironment,
}

// resourceProfile is the validated, equal-or-smaller development envelope.
// An absent profile retains historical per-command RSS budgets. Once a profile
// is selected, even a separately managed model service must remain within it.
type resourceProfile struct {
	selected          bool
	cpuPercent        int64
	memoryHighMiB     int64
	memoryMaxMiB      int64
	swapMaxMiB        int64
	goMemoryMiB       int64
	hostMemoryMinimum int64
}

func resourceProfileFromEnvironment() (resourceProfile, error) {
	var profile resourceProfile
	for _, name := range profileEnvironmentNames {
		if value := os.Getenv(name); value != "" {
			profile.selected = true
		}
	}
	settings := []struct {
		name               string
		fallback, min, max int64
		target             *int64
	}{
		{cpuQuotaPercentEnvironment, 100, 1, 100, &profile.cpuPercent},
		{memoryHighMiBEnvironment, 4096, 64, 4096, &profile.memoryHighMiB},
		{memoryMaxMiBEnvironment, 4608, 64, 4608, &profile.memoryMaxMiB},
		{memorySwapMiBEnvironment, 256, 0, 256, &profile.swapMaxMiB},
	}
	for _, setting := range settings {
		value, err := profileInteger(setting.name, setting.fallback, setting.min, setting.max)
		if err != nil {
			return resourceProfile{}, err
		}
		*setting.target = value
	}
	if profile.memoryMaxMiB < profile.memoryHighMiB {
		return resourceProfile{}, fmt.Errorf("%s must not be below %s", memoryMaxMiBEnvironment, memoryHighMiBEnvironment)
	}
	var err error
	profile.goMemoryMiB, err = profileInteger(goMemoryLimitEnvironment, profile.memoryHighMiB, 64, profile.memoryHighMiB)
	if err != nil {
		return resourceProfile{}, err
	}
	profile.hostMemoryMinimum, err = HostAvailableMemoryMinimumBytesFromEnvironment()
	if err != nil {
		return resourceProfile{}, err
	}
	return profile, nil
}

func profileInteger(name string, fallback, minimum, maximum int64) (int64, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	invalid := func() (int64, error) {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, minimum, maximum)
	}
	if len(value) > 5 || len(value) > 1 && value[0] == '0' {
		return invalid()
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return invalid()
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < minimum || parsed > maximum {
		return invalid()
	}
	return parsed, nil
}

func (profile resourceProfile) commandMemory(maximum int64) (int64, int64) {
	if maximum == 0 {
		maximum = LowPriorityMemoryMaxBytes
	}
	if profile.selected && maximum > profile.memoryMaxMiB*profileMiB {
		maximum = profile.memoryMaxMiB * profileMiB
	}
	if profile.selected {
		// The child must be able to prove its actual unit against the exported
		// integer MiB ceiling. A fractional byte budget is narrowed, never
		// rounded up beyond the requested or ambient hard ceiling.
		maximum = maximum / profileMiB * profileMiB
	}
	high := maximum * 8 / 9
	if profile.selected {
		high = profile.memoryHighMiB * profileMiB
		if high > maximum {
			high = maximum * 8 / 9
		}
		// Descendants use integer MiB profiles. Round the pressure ceiling down
		// and retain the minimum supported Go envelope for a 64 MiB RSS budget.
		high = high / profileMiB * profileMiB
		if high < rkcMinimumMemoryBytes {
			high = rkcMinimumMemoryBytes
		}
	}
	return high, maximum
}

func (profile resourceProfile) childEnvironment(values []string, high, maximum int64) []string {
	if !profile.selected {
		return append([]string(nil), values...)
	}
	result := make(map[string]string, len(values)+7)
	for _, entry := range values {
		name, value, _ := strings.Cut(entry, "=")
		result[name] = value
	}
	goMiB := profile.goMemoryMiB
	if goMiB > high/profileMiB {
		goMiB = high / profileMiB
	}
	for name, value := range map[string]int64{
		cpuQuotaPercentEnvironment:            profile.cpuPercent,
		memoryHighMiBEnvironment:              high / profileMiB,
		memoryMaxMiBEnvironment:               maximum / profileMiB,
		memorySwapMiBEnvironment:              profile.swapMaxMiB,
		goMemoryLimitEnvironment:              goMiB,
		HostAvailableMemoryMinimumEnvironment: profile.hostMemoryMinimum / profileMiB,
	} {
		result[name] = strconv.FormatInt(value, 10)
	}
	result["GOMEMLIMIT"] = strconv.FormatInt(goMiB, 10) + "MiB"
	return sortedEnvironment(result)
}

func (profile resourceProfile) hostMemoryCheck() error {
	return profile.hostMemoryCheckForPlatform(runtime.GOOS, "/proc/meminfo")
}

func (profile resourceProfile) hostMemoryCheckForPlatform(platform, path string) error {
	if profile.hostMemoryMinimum == 0 {
		return nil
	}
	if platform != "linux" {
		return fmt.Errorf("%w: Linux /proc/meminfo is required", ErrHostMemoryReserve)
	}
	return checkHostAvailableMemory(path, profile.hostMemoryMinimum)
}
