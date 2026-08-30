// Copyright 2026 NeuroForgeIO. Licensed under the MIT License.

package server

import (
	"os"
	"runtime"
	"testing"
)

// BenchmarkLoadAtlas measures the verified live-load path against an existing
// atlas without making a repository-sized fixture part of the test suite.
// Set RKC_BENCH_ATLAS and use -benchtime=1x for a single bounded observation.
func BenchmarkLoadAtlas(b *testing.B) {
	root := os.Getenv("RKC_BENCH_ATLAS")
	if root == "" {
		b.Skip("set RKC_BENCH_ATLAS to a verified atlas directory")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		dataset, err := Load(root)
		if err != nil {
			b.Fatal(err)
		}
		runtime.KeepAlive(dataset)
	}
}
