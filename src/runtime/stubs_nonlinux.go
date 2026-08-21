// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !linux

package runtime

import "unsafe"

// sbrk0 returns the current process brk, or 0 if not implemented.
func sbrk0() uintptr {
	return 0
}

// numaSchedinit is a no-op on non-Linux platforms: NUMA topology discovery
// is Linux-only (see numa_linux.go).
func numaSchedinit() {
}

// numaBindArena is a no-op on non-Linux platforms: NUMA BIND-all mbind is
// Linux-only (see numa_linux.go). Its call site in mheap.grow is also
// gated on goexperiment.Numa, so this body never runs with the
// experiment off; it exists purely so mheap.go, which is not
// Linux-specific, has something to call on every GOOS.
//
//go:nosplit
func numaBindArena(addr unsafe.Pointer, size uintptr) {
}

// numaConfineIfSmall is a no-op on non-Linux platforms: fill-one-socket-
// first confinement is Linux-only (see numa_linux.go). Its call site in
// schedinit is also gated on goexperiment.Numa, so this body never runs
// with the experiment off; it exists purely so proc.go, which is not
// Linux-specific, has something to call on every GOOS.
func numaConfineIfSmall(procs int32) {
}

// numaStandDownIfNeeded is a no-op on non-Linux platforms: confinement
// (and therefore stand-down) is Linux-only (see numa_linux.go). Its call
// site in startTheWorldWithSema is also gated on goexperiment.Numa, so
// this body never runs with the experiment off; it exists purely so
// proc.go, which is not Linux-specific, has something to call on every
// GOOS. Always reports no stand-down (false), so the guarded
// numaStandDownWiden call at the end of startTheWorldWithSema never runs
// either.
func numaStandDownIfNeeded(procs int32, customGOMAXPROCS bool) bool {
	return false
}

// numaStandDownWiden is a no-op on non-Linux platforms, for the same
// reason as numaStandDownIfNeeded above; see numa_linux.go for the real
// implementation.
func numaStandDownWiden() {
}

// numaFixThreadPlacement is a no-op on non-Linux platforms: per-thread
// stand-down convergence is Linux-only (see numa_linux.go). Its call site
// in stopm is also gated on goexperiment.Numa, so this body never runs
// with the experiment off; it exists purely so proc.go, which is not
// Linux-specific, has something to call on every GOOS.
func numaFixThreadPlacement() {
}

// numaGrowNode always returns (0, false) on non-Linux platforms: NUMA node
// discovery via getcpu is Linux-only (see numa_linux.go). homed == false
// (review I1) is correct here regardless -- there is no genuine per-node
// reading to home to on a platform with no getcpu path. mheap.grow's
// callers (allocSpan, via numaGrowNodeArg) call this on every GOOS,
// unconditionally, not just with goexperiment.Numa set, so this stub exists
// purely so those call sites compile everywhere; heapArena.node's only
// consumer today is numaArenaNode's diagnostic/test lookup.
func numaGrowNode() (stream int32, homed bool) {
	return 0, false
}

// numaHeapHomingActive is always false on non-Linux platforms, for the same
// reason as numaGrowNode above; see numa_linux.go for the real
// implementation. Its call site (via numaBindGrowth, from mheap.grow) is
// gated on goexperiment.Numa, so this body never runs with the experiment
// off.
//
//go:nosplit
func numaHeapHomingActive() bool {
	return false
}

// numaBindGrowth is a no-op on non-Linux platforms, for the same reason as
// numaBindArena above (which it wraps): NUMA mbind is Linux-only. Its call
// site in mheap.grow is gated on goexperiment.Numa, so this body never runs
// with the experiment off.
//
//go:nosplit
func numaBindGrowth(addr unsafe.Pointer, size uintptr, node int32) {
}

// numaNoteSchedule is a no-op on non-Linux platforms: node-mask soft
// affinity (design §12.4) is Linux-only (see numa_linux.go). Its call
// site in schedule() is also gated on goexperiment.Numa, so this body
// never runs with the experiment off; it exists purely so proc.go, which
// is not Linux-specific, has something to call on every GOOS.
func numaNoteSchedule() {
}

// numaWidenForFork is a no-op on non-Linux platforms, for the same
// reason as numaNoteSchedule above: it exists purely so
// syscall_runtime_BeforeFork (proc.go), which is not Linux-specific, has
// something to call on every GOOS; its call site is gated on
// goexperiment.Numa, so this body never runs with the experiment off.
//
//go:nosplit
func numaWidenForFork(mp *m) {
}
