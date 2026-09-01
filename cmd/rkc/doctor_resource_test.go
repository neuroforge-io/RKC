package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/resourceguard"
)

func TestHostMemoryReserveDoctorCheck(t *testing.T) {
	sentinel := errors.New("memory reserve sentinel")
	tests := []struct {
		name       string
		parse      func() (int64, error)
		check      func() error
		wantStatus string
		wantDetail string
		wantFix    bool
		wantChecks int
	}{
		{
			name:       "disabled",
			parse:      func() (int64, error) { return 0, nil },
			check:      func() error { return sentinel },
			wantStatus: "pass", wantDetail: "disabled", wantChecks: 0,
		},
		{
			name:       "enabled and available",
			parse:      func() (int64, error) { return 1536 * 1024 * 1024, nil },
			check:      func() error { return nil },
			wantStatus: "pass", wantDetail: "1536 MiB", wantChecks: 1,
		},
		{
			name:       "invalid configuration",
			parse:      func() (int64, error) { return 0, sentinel },
			check:      func() error { return nil },
			wantStatus: "fail", wantDetail: "fails closed", wantFix: true, wantChecks: 0,
		},
		{
			name:       "reserve unavailable",
			parse:      func() (int64, error) { return 1536 * 1024 * 1024, nil },
			check:      func() error { return sentinel },
			wantStatus: "fail", wantDetail: sentinel.Error(), wantFix: true, wantChecks: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := 0
			result := hostMemoryReserveCheckUsing(test.parse, func() error {
				checks++
				return test.check()
			})
			if result.Name != "host-memory-reserve" || result.Fatal || result.Status != test.wantStatus ||
				!strings.Contains(result.Detail, test.wantDetail) || (result.Remediation != "") != test.wantFix || checks != test.wantChecks {
				t.Fatalf("doctor check = %+v, calls=%d", result, checks)
			}
		})
	}
}

func TestHostMemoryReserveDoctorCheckRejectsMissingDependencies(t *testing.T) {
	for _, result := range []doctorCheck{
		hostMemoryReserveCheckUsing(nil, resourceguard.CheckHostAvailableMemory),
		hostMemoryReserveCheckUsing(func() (int64, error) { return 0, nil }, nil),
	} {
		if result.Status != "fail" || result.Fatal || result.Remediation == "" || !strings.Contains(result.Detail, "not configured") {
			t.Fatalf("missing dependency doctor check = %+v", result)
		}
	}
}
