// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// TestNUMAGetcpu lives in its own file, separate from numa_linux_test.go,
// because its rationale is subtle enough to deserve its own doc: it
// calls NumaGetCPUNodeForTest (numaGetCPUNode directly -- see
// export_numa_getcpu_test.go), not numaCurrentNode's more obvious
// NumaCurrentNodeForTest-shaped wrapper, so that it also *executes* the
// real getcpu(2) syscall and asserts on the result on single-node
// hosts, not only multi-node ones. numaCurrentNode (numa_linux.go)
// short-circuits to a hardcoded 0 without ever calling getcpu whenever
// numaTopology.NumNodes < 2, so a version of this test built on top of
// it would pass vacuously -- never touching the per-GOARCH assembly at
// all -- on every single-node host. That matters here specifically
// because single-node is exactly what most available test hardware
// (qemu, CI runners) presents for most of the linux architectures
// getcpu is implemented on; only amd64 has been exercised on multi-node
// hardware so far -- every other execution exercises only the
// "single-node" side of getcpu, never the multi-node
// topology-discovery interaction.

//go:build linux && goexperiment.numa

package runtime_test

import (
	"runtime"
	"testing"
)

func TestNUMAGetcpu(t *testing.T) {
	node := runtime.NumaGetCPUNodeForTest()
	if node < 0 {
		t.Fatal("getcpu failed")
	}
}
