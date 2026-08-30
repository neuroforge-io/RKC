//go:build !linux

package resourceguard

import "errors"

func currentSchedulingEnvelope(int) (schedulingEnvelope, error) {
	return schedulingEnvelope{}, errors.New("RKC low-priority envelope inspection requires Linux cgroup v2")
}

func lowerCurrentSchedulingEnvelope(int) error {
	return errors.New("RKC low-priority scheduling normalization requires Linux")
}

func currentEnvelopeFilesystems(string, string) error {
	return errors.New("RKC kernel envelope inspection requires Linux")
}
