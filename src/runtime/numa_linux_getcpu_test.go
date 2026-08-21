// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// TestNUMAGetcpu lives in its own file, separate from numa_linux_test.go,
// because it is the only test in that file that does not depend on
// numaSetThreadAffinity (see numa_linux_affinity.go /
// numa_linux_affinity_other.go): getcpu(2) now has an assembly
// implementation on every GOOS=linux architecture (see the getcpu
// wrapper in each sys_linux_*.s), but sched_setaffinity does not --
// numaHasSetAffinity is still amd64/arm64-only. Keeping this test in a
// widely-tagged file, while numa_linux_test.go stays restricted to
// amd64/arm64, lets it compile, link, and vet on every linux
// architecture.
//
// It calls NumaGetCPUNodeForTest (numaGetCPUNode directly -- see
// export_numa_getcpu_test.go), not numaCurrentNode's more obvious
// NumaCurrentNodeForTest-shaped wrapper, so that it also *executes* the
// real getcpu syscall and asserts on the result on single-node hosts,
// not only multi-node ones. numaCurrentNode (numa_linux.go)
// short-circuits to a hardcoded 0 without ever calling getcpu whenever
// numaTopology.NumNodes < 2, so a version of this test built on top of
// it would pass vacuously -- never touching the new assembly at all --
// on every single-node host. That matters here specifically because
// single-node is exactly what most available test hardware (qemu, CI
// runners) presents for the 11 architectures this task added getcpu to;
// only amd64 has actually run this test on multi-node hardware
// (numa-dell) -- every other execution of it, including every local run
// on this single-node dev box, exercises only the "single-node" side of
// getcpu, never the multi-node topology-discovery interaction.

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
