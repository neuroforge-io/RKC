//go:build !linux

package resourceguard

import "errors"

func currentSchedulingEnvelope(int) (schedulingEnvelope, error) {
	return schedulingEnvelope{}, errors.New("RKC low-priority envelope inspection requires Linux cgroup v2")
}
