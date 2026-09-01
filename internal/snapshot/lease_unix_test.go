//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package snapshot

import (
	"os"
	"testing"
)

func TestLockFileExclusiveRejectsInvalidDescriptor(t *testing.T) {
	file := os.NewFile(1<<30, "invalid-rkc-lease")
	if file == nil {
		t.Fatal("os.NewFile returned nil for invalid descriptor")
	}
	acquired, err := lockFileExclusive(file)
	if acquired || err == nil {
		t.Fatalf("invalid descriptor lock = acquired=%t err=%v", acquired, err)
	}
}
