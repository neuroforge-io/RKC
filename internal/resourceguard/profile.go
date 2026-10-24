package resourceguard

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

const (
	// ResourceProfileEnvironment selects the explicit desktop memory envelope.
	// Empty or "default" retains the historical limits and behavior.
	ResourceProfileEnvironment = "RKC_RESOURCE_PROFILE"
	cpuQuotaPercentEnvironment = "RKC_CPU_QUOTA_PERCENT"
	memoryHighMiBEnvironment   = "RKC_MEMORY_HIGH_MIB"
	memoryMaxMiBEnvironment    = "RKC_MEMORY_MAX_MIB"
	memorySwapMiBEnvironment   = "RKC_MEMORY_SWAP_MAX_MIB"
	goMemoryLimitEnvironment   = "RKC_GO_MEMORY_LIMIT_MIB"
	profileMiB                 = int64(1024 * 1024)
)

var profileEnvironmentNames = []string{
	ResourceProfileEnvironment,
	cpuQuotaPercentEnvironment, memoryHighMiBEnvironment, memoryMaxMiBEnvironment,
	memorySwapMiBEnvironment, goMemoryLimitEnvironment, HostAvailableMemoryMinimumEnvironment,
}

// resourceProfile is the validated, explicitly selected development envelope.
// An absent profile retains historical per-command RSS budgets. Once a profile
// is selected, even a separately managed model service must remain within it.
type resourceProfile struct {
	name              string
	selected          bool
	cpuPercent        int64
	memoryHighMiB     int64
	memoryMaxMiB      int64
	swapMaxMiB        int64
	goMemoryMiB       int64
	hostMemoryMinimum int64
}

func resourceProfileFromEnvironment() (resourceProfile, error) {
	name, err := resourceProfileNameFromEnvironment()
	if err != nil {
		return resourceProfile{}, err
	}
	profile := resourceProfile{name: name, selected: name == "desktop"}
	for _, name := range profileEnvironmentNames {
		if value := os.Getenv(name); name != ResourceProfileEnvironment && value != "" {
			profile.selected = true
		}
	}
	defaultHigh, defaultMax, defaultSwap := int64(4096), int64(4608), int64(256)
	maximumHigh, maximumMax := defaultHigh, defaultMax
	if name == "desktop" {
		defaultHigh, defaultMax, defaultSwap = 8192, 10240, 0
		maximumHigh, maximumMax = 12288, 16384
	}
	settings := []struct {
		name               string
		fallback, min, max int64
		target             *int64
	}{
		{cpuQuotaPercentEnvironment, 100, 1, 100, &profile.cpuPercent},
		{memoryHighMiBEnvironment, defaultHigh, 64, maximumHigh, &profile.memoryHighMiB},
		{memoryMaxMiBEnvironment, defaultMax, 64, maximumMax, &profile.memoryMaxMiB},
		{memorySwapMiBEnvironment, defaultSwap, 0, 256, &profile.swapMaxMiB},
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
	defaultGo := profile.memoryHighMiB
	if name == "desktop" && defaultGo > 6144 {
		defaultGo = 6144
	}
	profile.goMemoryMiB, err = profileInteger(goMemoryLimitEnvironment, defaultGo, 64, profile.memoryHighMiB)
	if err != nil {
		return resourceProfile{}, err
	}
	profile.hostMemoryMinimum, err = HostAvailableMemoryMinimumBytesFromEnvironment()
	if err != nil {
		return resourceProfile{}, err
	}
	return profile, nil
}

func resourceProfileNameFromEnvironment() (string, error) {
	switch os.Getenv(ResourceProfileEnvironment) {
	case "", "default":
		return "default", nil
	case "desktop":
		return "desktop", nil
	default:
		return "", fmt.Errorf("%s must be default or desktop", ResourceProfileEnvironment)
	}
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
		maximum = profile.memoryMaxMiB * profileMiB
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
	result[ResourceProfileEnvironment] = profile.name
	return sortedEnvironment(result)
}

func (profile resourceProfile) hostMemoryCheck() error {
	return profile.hostMemoryCheckForPlatform(runtime.GOOS, "/proc/meminfo")
}

func (profile resourceProfile) hostMemoryAdmissionCheckForPlatform(platform, path string, maximum int64) error {
	if profile.name != "desktop" {
		return nil
	}
	if platform != "linux" {
		return fmt.Errorf("%w: Linux /proc/meminfo is required for desktop admission", ErrHostMemoryReserve)
	}
	return checkDesktopHostMemoryAdmission(path, maximum, profile.hostMemoryMinimum)
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
