//go:build !windows

package main

func pathComparisonNames(parent, candidate string) (string, string, error) {
	return parent, candidate, nil
}
