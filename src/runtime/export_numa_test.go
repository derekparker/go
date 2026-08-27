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
// link-safety one: every export below except NumaGetCPUNodeForTest
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

// NumaHostAffinityNarrowedForTest reports whether this process's own
// affinity mask, as it was at the very start of this process (before
// this process's own GOEXPERIMENT=numa scheduling could have narrowed
// anything itself), was already narrower than the machine's online CPU
// count -- i.e., whether the surrounding environment (which a
// freshly-exec'd testprog subprocess inherits by default, absent an
// explicit taskset of its own) is cpuset/taskset-narrowed for reasons
// outside this package's control.
//
// Deliberately reads numaStartupFullAffinity/numaStartupAffinity (a
// snapshot numaDetectStartupAffinity took once, unconditionally, from
// schedinit while m0 was still the only thread) rather than re-reading
// sched_getaffinity(0, ...) live on the calling M: an earlier version of
// this function did exactly that live re-read, and on real multi-node
// hardware it produced false positives -- the calling test goroutine's
// own M can itself be soft-affinity-narrowed by this SAME test binary's
// own node-mask soft affinity (task 10) at the moment this function
// runs, which has nothing to do with the environment and would
// otherwise make every test using this helper spuriously Skip. The
// startup-time snapshot is immune to that: it is captured before any
// scheduling, and therefore before any soft-affinity or confinement
// narrowing, could possibly have run.
func NumaHostAffinityNarrowedForTest() bool {
	return !numaStartupFullAffinity
}

// NumaWidenCountForTest returns the number of times numaWidenBeforeClone
// has actually widened a soft-narrowed M since process start (review
// adjudication (b)): a race-safe way to confirm the newm1/newosproc/cgo
// widen path fired at all during M-creation churn, closing the
// automated-coverage gap TestNUMASoftAffinity's I2 distinct-node
// assertion leaves under -race (see that test's doc comment).
func NumaWidenCountForTest() uint64 { return numaWidenCount.Load() }

// NumaPlacementQuotasForTest runs the pure quota partition function
// behind numaAssignPHomes on an arbitrary topology (v4 stage 2).
// len(cpus) must be <= numaMaxHeapNodes.
func NumaPlacementQuotasForTest(nprocs int32, cpus []int32) []int32 {
	quotas := make([]int32, len(cpus))
	numaPlacementQuotas(nprocs, cpus, quotas)
	return quotas
}

// NumaPlacementActiveForTest reports whether P-home placement is
// currently consumed (see numaPlacementActive in numa_linux.go).
func NumaPlacementActiveForTest() bool { return numaPlacementActive() }

// NumaPHomesForTest snapshots every current P's assigned NUMA home
// (-1 = unassigned). Reads allp without synchronization -- callers must
// not race it against a concurrent GOMAXPROCS change.
func NumaPHomesForTest() []int8 {
	homes := make([]int8, gomaxprocs)
	for i := range homes {
		if home, ok := allp[i].numa.home(); ok {
			homes[i] = home
		} else {
			homes[i] = -1
		}
	}
	return homes
}

// A5 adaptive-enforcement test hooks: drive the sysmon-side trip/re-arm
// state machine with synthetic window readings and observe the latch,
// epoch, and trip counter. Callers must reset with
// NumaEnforceResetForTest and are responsible for not racing real
// sysmon activity in ways that matter (the state machine is
// sysmon-single-writer in production; tests drive it from one
// goroutine, which preserves that).
func NumaEnforceEvalForTest(ratePerSec, elapsed int64) { numaEnforceEval(ratePerSec, elapsed) }
func NumaEnforceStoodDownForTest() bool                { return numaEnforceStoodDown.Load() }
func NumaEnforceEpochForTest() uint32                  { return numaEnforceEpoch.Load() }
func NumaEnforceTripsForTest() int32                   { return numaEnforceTrips }
func NumaEnforceResetForTest() {
	numaEnforceSysmonMasked.Store(true) // hermetic: live sysmon evals off
	numaEnforceStoodDown.Store(false)
	numaEnforceOverStreak = 0
	numaEnforceQuietNs = 0
	numaEnforceTrips = 0
	numaEnforcePermanent = false
}

// NumaEnforceReleaseForTest returns the state machine to live sysmon
// ownership (deferred by tests after NumaEnforceResetForTest).
func NumaEnforceReleaseForTest() {
	numaEnforceStoodDown.Store(false)
	numaEnforceOverStreak = 0
	numaEnforceQuietNs = 0
	numaEnforceTrips = 0
	numaEnforcePermanent = false
	numaEnforceSysmonMasked.Store(false)
}

// NumaWakeRateTripForTest / NumaEnforceTripStreakForTest export the
// frozen detection constants.
func NumaWakeRateTripForTest() int64      { return numaWakeRateTrip }
func NumaEnforceTripStreakForTest() int32 { return numaEnforceTripStreak }
