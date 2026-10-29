//go:build !linux

package resourceguard

import "fmt"

// An explicitly configured kernel lease policy cannot establish visibility on
// this platform. Disabled policies never call this observer.
func checkERAISModelLease() error {
	return fmt.Errorf("%w: ERAIS model lease kernel observation is unsupported", ErrHigherPriorityActive)
}
