package resourceguard

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestHostAvailableMemoryMinimumBytesFromEnvironment(t *testing.T) {
	tests := []struct {
		name  string
		value *string
		want  int64
	}{
		{name: "unset"},
		{name: "empty", value: stringPointer("")},
		{name: "blank", value: stringPointer(" \t")},
		{name: "disabled", value: stringPointer("0")},
		{name: "reserve", value: stringPointer("1536"), want: 1536 * 1024 * 1024},
		{name: "trimmed reserve", value: stringPointer(" 1536 "), want: 1536 * 1024 * 1024},
		{name: "maximum", value: stringPointer("65536"), want: 65536 * 1024 * 1024},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.value == nil {
				t.Setenv(HostAvailableMemoryMinimumEnvironment, "")
				if err := os.Unsetenv(HostAvailableMemoryMinimumEnvironment); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv(HostAvailableMemoryMinimumEnvironment, *test.value)
			}
			got, err := HostAvailableMemoryMinimumBytesFromEnvironment()
			if err != nil || got != test.want {
				t.Fatalf("host reserve = %d, %v; want %d", got, err, test.want)
			}
		})
	}
}

func TestHostAvailableMemoryMinimumRejectsMalformedValuesPrivately(t *testing.T) {
	sentinel := "HOST_MEMORY_PRIVATE_SENTINEL"
	for _, value := range []string{
		"-1", "+1", "1.5", "65537", "0001", "000001", "999999999999999999999999", sentinel,
	} {
		t.Run(strconv.Itoa(len(value)), func(t *testing.T) {
			t.Setenv(HostAvailableMemoryMinimumEnvironment, value)
			got, err := HostAvailableMemoryMinimumBytesFromEnvironment()
			if got != 0 || err == nil || !strings.Contains(err.Error(), HostAvailableMemoryMinimumEnvironment) {
				t.Fatalf("malformed host reserve = %d, %v", got, err)
			}
			if strings.Contains(err.Error(), value) || strings.Contains(err.Error(), sentinel) {
				t.Fatalf("malformed host reserve leaked input: %v", err)
			}
		})
	}
}

func TestCheckHostAvailableMemory(t *testing.T) {
	minimum := int64(1536 * 1024 * 1024)
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "above reserve", content: "MemTotal: 8000000 kB\nMemAvailable: 2097152 kB\n"},
		{name: "exact reserve", content: "MemAvailable: 1572864 kB\n"},
		{name: "below reserve", content: "MemAvailable: 1572863 kB\n", wantErr: "below configured minimum"},
		{name: "missing", content: "MemFree: 2097152 kB\n", wantErr: "is missing"},
		{name: "duplicate", content: "MemAvailable: 2097152 kB\nMemAvailable: 2097152 kB\n", wantErr: "is malformed"},
		{name: "wrong unit", content: "MemAvailable: 2097152 MB\n", wantErr: "is malformed"},
		{name: "missing unit", content: "MemAvailable: 2097152\n", wantErr: "is malformed"},
		{name: "negative", content: "MemAvailable: -1 kB\n", wantErr: "is malformed"},
		{name: "not numeric", content: "MemAvailable: unknown kB\n", wantErr: "is malformed"},
		{name: "overflow", content: "MemAvailable: 9223372036854775807 kB\n", wantErr: "is malformed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "meminfo")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			err := checkHostAvailableMemory(path, minimum)
			if test.wantErr == "" && err != nil {
				t.Fatalf("host reserve check = %v", err)
			}
			if test.wantErr != "" && (!errors.Is(err, ErrHostMemoryReserve) || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("host reserve error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestCheckHostAvailableMemoryFailsClosedOnUnreadableOrOversizedInput(t *testing.T) {
	minimum := int64(1024)
	missing := filepath.Join(t.TempDir(), "missing")
	if err := checkHostAvailableMemory(missing, minimum); !errors.Is(err, ErrHostMemoryReserve) || !strings.Contains(err.Error(), "read Linux MemAvailable") {
		t.Fatalf("missing meminfo = %v", err)
	}
	directory := t.TempDir()
	if err := checkHostAvailableMemory(directory, minimum); !errors.Is(err, ErrHostMemoryReserve) || !strings.Contains(err.Error(), "read Linux MemAvailable") {
		t.Fatalf("unreadable meminfo stream = %v", err)
	}
	oversized := filepath.Join(t.TempDir(), "oversized")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("x", int(maximumMeminfoBytes+1))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkHostAvailableMemory(oversized, minimum); !errors.Is(err, ErrHostMemoryReserve) || !strings.Contains(err.Error(), "bounded inspection limit") {
		t.Fatalf("oversized meminfo = %v", err)
	}
	oversizedLine := filepath.Join(t.TempDir(), "oversized-line")
	if err := os.WriteFile(oversizedLine, []byte(strings.Repeat("x", maximumMeminfoLineBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkHostAvailableMemory(oversizedLine, minimum); !errors.Is(err, ErrHostMemoryReserve) || !strings.Contains(err.Error(), "scan Linux MemAvailable") {
		t.Fatalf("oversized-line meminfo = %v", err)
	}
	if err := checkHostAvailableMemory(oversized, 0); err == nil || errors.Is(err, ErrHostMemoryReserve) {
		t.Fatalf("invalid minimum = %v", err)
	}
}

func TestCheckHostAvailableMemoryDisabled(t *testing.T) {
	t.Setenv(HostAvailableMemoryMinimumEnvironment, "0")
	if err := CheckHostAvailableMemory(); err != nil {
		t.Fatalf("disabled host reserve = %v", err)
	}
}

func TestCheckHostAvailableMemoryPlatformBoundary(t *testing.T) {
	t.Setenv(HostAvailableMemoryMinimumEnvironment, "1536")
	if err := checkHostAvailableMemoryForPlatform("darwin", "unused"); !errors.Is(err, ErrHostMemoryReserve) || !strings.Contains(err.Error(), "Linux /proc/meminfo is required") {
		t.Fatalf("portable host reserve = %v", err)
	}
	path := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(path, []byte("MemAvailable: 2097152 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkHostAvailableMemoryForPlatform("linux", path); err != nil {
		t.Fatalf("Linux host reserve = %v", err)
	}
	t.Setenv(HostAvailableMemoryMinimumEnvironment, "invalid-private-value")
	if err := checkHostAvailableMemoryForPlatform("linux", path); err == nil || !strings.Contains(err.Error(), HostAvailableMemoryMinimumEnvironment) || strings.Contains(err.Error(), "invalid-private-value") {
		t.Fatalf("invalid host reserve configuration = %v", err)
	}
}

func stringPointer(value string) *string {
	return &value
}
