// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Export guts for testing.
//
// numaTopology and numaCurrentNode live in numa_linux.go, which is
// restricted to GOOS=linux by its filename. An unconstrained export file
// referencing them would break every non-Linux build of the runtime test
// archive, so this file carries its own //go:build linux tag.
//
// This file's (amd64 || arm64) restriction is a test-scoping one, not a
// link-safety one: every export below except NumaCurrentNodeForTest
// (split out into export_numa_getcpu_test.go, which carries a wider tag
// -- see that file) depends on numaSetThreadAffinity, which only has a
// real implementation on amd64/arm64 (see numa_linux_affinity.go /
// numa_linux_affinity_other.go and numaHasSetAffinity); elsewhere it is
// always a false-returning stub. So on every other Linux architecture, a
// test relying on these exports would either always-skip or always-fail
// rather than exercise real behavior. This file (and numa_linux_test.go,
// which carries the same restriction) is kept scoped to the
// architectures affinity actually works on -- the ones CI/this task's
// hardware exercise. Widening this to every getcpu-capable architecture
// is tracked as future work alongside numaHasSetAffinity itself (see
// numa_linux.go's numaShouldConfine).
//
// The additional goexperiment.numa tag matters independently of the
// above: this file is package runtime (not runtime_test), so it is
// compiled into the runtime test archive even with the experiment off.
// Without this tag, an export like NumaSetThreadAffinitySelfForTest below
// would give the experiment-off test binary a reachable call path into
// confinement code, defeating the dead-code elimination Gate 8's census
// checks for. Every export in this file is only ever referenced from
// numa_linux_test.go, which already carries this same tag, so gating the
// whole file this way costs nothing.

//go:build linux && (amd64 || arm64) && goexperiment.numa

package runtime

import (
	"internal/runtime/syscall/linux"
	"unsafe"
)

func NumaNumNodes() int32         { return numaTopology.NumNodes }
func NumaNumAllowedNodes() int32  { return numaTopology.NumAllowedNodes }
func NumaNodeOfCPU(cpu int) int32 { return numaTopology.NodeOfCPU(cpu) }

// NumaTaskMemPolicyModeForTest returns this process's current task memory
// policy mode via get_mempolicy(2) (mode only, no MPOL_F_MEMS_ALLOWED, no
// nodemask), for TestNUMABindAllTaskPolicy to check against MPOL_BIND (2).
//
// The raw mode word can have MPOL_F_STATIC_NODES/MPOL_F_RELATIVE_NODES
// OR'd in by the kernel; those are masked out here so callers only ever
// see one of the MPOL_* mode values. Returns -1 if the get_mempolicy
// syscall itself fails, distinguishing a syscall error from a genuine
// MPOL_DEFAULT (mode 0) result -- the latter is the expected value on a
// single-node host, where numaSetProcessBindAll must never have called
// set_mempolicy at all.
func NumaTaskMemPolicyModeForTest() int32 {
	var mode int32
	_, _, errno := linux.Syscall6(linux.SYS_GET_MEMPOLICY, uintptr(unsafe.Pointer(&mode)), 0, 0, 0, 0, 0)
	if errno != 0 {
		return -1
	}
	return mode &^ _MPOL_MODE_FLAGS
}

// NumaSetThreadAffinitySelfForTest re-applies the calling thread's own
// affinity mask.
func NumaSetThreadAffinitySelfForTest() bool {
	var buf [numaCPUMaskBytes]byte
	r := sched_getaffinity(0, uintptr(len(buf)), &buf[0])
	if r <= 0 {
		return false
	}
	return numaSetThreadAffinity(0, &buf)
}

// NumaHasSetAffinityForTest reports whether this platform implements
// numaSetThreadAffinity (see numaHasSetAffinity in numa_linux_affinity.go
// / numa_linux_affinity_other.go).
func NumaHasSetAffinityForTest() bool { return numaHasSetAffinity }

// NumaIsNodeCPUCountForTest reports whether n equals some node's CPU count.
func NumaIsNodeCPUCountForTest(n int) bool {
	for i := int32(0); i < numaTopology.NumNodes; i++ {
		if int(numaTopology.Nodes[i].NumCPUs) == n {
			return true
		}
	}
	return false
}

// NumaConfinedForTest reports whether fill-one-socket-first confinement is
// currently active for this process (see numaConfined in numa_linux.go).
func NumaConfinedForTest() bool { return numaConfined.Load() }
