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
// The additional goexperiment.numa tag matters independently of the
// above: this file is package runtime (not runtime_test), so it is
// compiled into the runtime test archive even with the experiment off.
// Without this tag, an export like NumaSetThreadAffinitySelfForTest below
// would give the experiment-off test binary a reachable call path into
// confinement code, defeating the dead-code elimination the series'
// binary census checks for. Every export in this file is only ever
// referenced from numa_linux_test.go, which already carries this same
// tag, so gating the whole file this way costs nothing.

//go:build linux && goexperiment.numa

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
// numaSetThreadAffinity (see numaHasSetAffinity in
// numa_linux_affinity.go).
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

// NumaHostAffinityNarrowedForTest reports whether this process's own
// affinity mask, as it was at the very start of this process (before
// this process's own GOEXPERIMENT=numa scheduling could have narrowed
// anything itself), was already narrower than the machine's online CPU
// count -- i.e., whether the surrounding environment (which a
// freshly-exec'd testprog subprocess inherits by default, absent an
// explicit taskset of its own) is cpuset/taskset-narrowed for reasons
// outside this package's control.
//
// Deliberately reads numaStartupFullAffinity (a snapshot
// numaDetectStartupAffinity took once, unconditionally, from schedinit
// while m0 was still the only thread) rather than re-reading
// sched_getaffinity(0, ...) live on the calling M: the startup-time
// snapshot is captured before any of this process's own
// GOEXPERIMENT=numa narrowing could possibly have run, so it can never
// confuse the runtime's own doing with the environment's.
func NumaHostAffinityNarrowedForTest() bool {
	return !numaStartupFullAffinity
}
