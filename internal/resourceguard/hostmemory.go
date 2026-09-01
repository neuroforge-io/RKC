package resourceguard

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
)

const (
	// HostAvailableMemoryMinimumEnvironment reserves host-wide MemAvailable for
	// higher-priority work. Zero or an unset value disables this optional host
	// policy; the per-cgroup hard ceiling remains mandatory.
	HostAvailableMemoryMinimumEnvironment = "RKC_HOST_AVAILABLE_MEMORY_MIN_MIB"
	maximumHostAvailableMemoryMinimumMiB  = int64(64 * 1024)
	maximumMeminfoBytes                   = int64(64 * 1024)
	maximumMeminfoLineBytes               = 8 * 1024
)

// ErrHostMemoryReserve identifies a protected RKC workload that yielded
// because Linux MemAvailable fell below the explicitly configured reserve.
var ErrHostMemoryReserve = errors.New("host available memory is below the RKC reserve")

// HostAvailableMemoryMinimumBytesFromEnvironment parses the optional host
// reserve without echoing malformed environment contents into diagnostics.
func HostAvailableMemoryMinimumBytesFromEnvironment() (int64, error) {
	value, present := os.LookupEnv(HostAvailableMemoryMinimumEnvironment)
	if !present || strings.TrimSpace(value) == "" {
		return 0, nil
	}
	value = strings.TrimSpace(value)
	if len(value) > 5 || (len(value) > 1 && value[0] == '0') {
		return 0, invalidHostAvailableMemoryMinimumError()
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, invalidHostAvailableMemoryMinimumError()
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 || parsed > maximumHostAvailableMemoryMinimumMiB {
		return 0, invalidHostAvailableMemoryMinimumError()
	}
	return parsed * 1024 * 1024, nil
}

func invalidHostAvailableMemoryMinimumError() error {
	return fmt.Errorf(
		"%s must be an integer between 0 and %d MiB",
		HostAvailableMemoryMinimumEnvironment,
		maximumHostAvailableMemoryMinimumMiB,
	)
}

// CheckHostAvailableMemory enforces the configured Linux host reserve. It is
// intentionally independent of the workload cgroup: MemAvailable can fall
// because a higher-priority peer grows even while RKC itself stays small.
func CheckHostAvailableMemory() error {
	return checkHostAvailableMemoryForPlatform(runtime.GOOS, "/proc/meminfo")
}

func checkHostAvailableMemoryForPlatform(platform, path string) error {
	minimum, err := HostAvailableMemoryMinimumBytesFromEnvironment()
	if err != nil || minimum == 0 {
		return err
	}
	if platform != "linux" {
		return fmt.Errorf("%w: Linux /proc/meminfo is required", ErrHostMemoryReserve)
	}
	return checkHostAvailableMemory(path, minimum)
}

func checkHostAvailableMemory(path string, minimum int64) error {
	if minimum <= 0 {
		return errors.New("host available-memory minimum must be positive")
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: read Linux MemAvailable: %v", ErrHostMemoryReserve, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumMeminfoBytes+1))
	if err != nil {
		return fmt.Errorf("%w: read Linux MemAvailable: %v", ErrHostMemoryReserve, err)
	}
	if int64(len(data)) > maximumMeminfoBytes {
		return fmt.Errorf("%w: Linux meminfo exceeds the bounded inspection limit", ErrHostMemoryReserve)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024), maximumMeminfoLineBytes)
	var available int64
	found := false
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] != "MemAvailable:" {
			continue
		}
		if found || len(fields) != 3 || fields[2] != "kB" {
			return fmt.Errorf("%w: Linux MemAvailable is malformed", ErrHostMemoryReserve)
		}
		kilobytes, parseErr := strconv.ParseInt(fields[1], 10, 64)
		if parseErr != nil || kilobytes < 0 || kilobytes > math.MaxInt64/1024 {
			return fmt.Errorf("%w: Linux MemAvailable is malformed", ErrHostMemoryReserve)
		}
		available = kilobytes * 1024
		found = true
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: scan Linux MemAvailable: %v", ErrHostMemoryReserve, err)
	}
	if !found {
		return fmt.Errorf("%w: Linux MemAvailable is missing", ErrHostMemoryReserve)
	}
	if available < minimum {
		return fmt.Errorf(
			"%w: MemAvailable %d bytes is below configured minimum %d bytes",
			ErrHostMemoryReserve,
			available,
			minimum,
		)
	}
	return nil
}
