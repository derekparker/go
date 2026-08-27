// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.numa

package runtime

// mNUMAState is the per-M NUMA stand-down convergence state, embedded as
// m.numa (runtime2.go). With the experiment on it holds:
//   - whether this M's affinity and task mempolicy have already converged
//     to Layer-1 BIND-all since the process stood down from fill-one-
//     socket-first confinement (see numaFixThreadPlacement in
//     numa_linux.go);
//   - which NUMA node (if any) numaNoteSchedule last narrowed this M's
//     CPU affinity to (design §12.4 soft affinity, numa_linux.go).
type mNUMAState struct {
	bindAllDone bool // this thread's placement converged after stand-down

	// enforceEpoch is the numaEnforceEpoch value current when this M
	// last successfully applied placement affinity (v4 Task A5): a
	// stand-down bumps the global, making every cache stale so Ms
	// re-apply after a re-arm. Own-M writes only.
	enforceEpoch uint32

	// lastNode stores (node id + 1): the node numaNoteSchedule last
	// narrowed this M's CPU affinity to, or 0 (its zero value) if
	// numaNoteSchedule has never narrowed this M at all. The +1 offset
	// makes "never narrowed" distinguishable from a genuine node-0
	// reading with a single field -- the same zero-value-safe idiom
	// bindAllDone above already relies on -- so the very first eligible
	// schedule() pass for any M always applies affinity, regardless of
	// which node it happens to observe first.
	lastNode int8

	// nextCheck is the nanotime() deadline before which numaNoteSchedule
	// skips its getcpu(2) syscall entirely (see
	// numaSoftAffinityCheckInterval, numa_linux.go). A real-hardware
	// PingPongHog benchmark showed firing getcpu on literally every
	// schedule() pass costs ~+56% on a tight goroutine-switching
	// workload -- nanotime() (vDSO-backed, not a syscall trap) is the
	// cheap per-pass read that replaces it; getcpu itself only runs once
	// per interval per M. Zero-value-safe: 0 is always <= any real
	// nanotime() reading taken after process start, so the first
	// eligible pass on any M is always due.
	nextCheck int64
}

// placementDone reports whether this M's placement has already converged
// after stand-down (see numaFixThreadPlacement).
func (s *mNUMAState) placementDone() bool { return s.bindAllDone }

// setPlacementDone latches convergence; called only after every syscall in
// numaFixThreadPlacement's (or numaStandDownIfNeeded's) restore sequence
// has succeeded.
func (s *mNUMAState) setPlacementDone() { s.bindAllDone = true }

// softAffinityNode returns the node id numaNoteSchedule last narrowed
// this M's CPU affinity to, and whether it has ever done so at all (see
// lastNode's doc comment for the +1 encoding).
func (s *mNUMAState) softAffinityNode() (node int8, ok bool) {
	if s.lastNode == 0 {
		return 0, false
	}
	return s.lastNode - 1, true
}

// setSoftAffinityNode records that numaNoteSchedule just narrowed this
// M's CPU affinity to node.
func (s *mNUMAState) setSoftAffinityNode(node int8) { s.lastNode = node + 1 }

// clearSoftAffinityNode resets lastNode to "never narrowed" and
// nextCheck to "always due" (M4, review) -- see numaWidenBeforeClone,
// numa_linux.go: after widening this M's real kernel affinity back to
// full ahead of a fork/clone, both caches must be cleared, or the next
// numaNoteSchedule pass would either see no node change (lastNode stale)
// or not even check yet (nextCheck stale, up to
// numaSoftAffinityCheckInterval in the future) -- either way delaying or
// skipping the re-narrow that keeps this M's actual affinity from
// silently staying wide. Zeroing nextCheck makes the very next
// schedule() pass due immediately (0 <= any real nanotime() reading).
func (s *mNUMAState) clearSoftAffinityNode() {
	s.lastNode = 0
	s.nextCheck = 0
}

// softAffinityCheckDue reports whether now has reached this M's next
// getcpu-check deadline.
func (s *mNUMAState) softAffinityCheckDue(now int64) bool { return now >= s.nextCheck }

// armSoftAffinityCheck records deadline as this M's next getcpu-check
// time (see softAffinityCheckDue). The caller (numaNoteSchedule,
// numa_linux.go) computes deadline as now + numaSoftAffinityCheckInterval
// -- that constant lives in the Linux-only numa_linux.go, so it must not
// be referenced from this file: unlike numa_linux.go, this file compiles
// on every GOOS whenever goexperiment.numa is set (an earlier version
// referenced the constant directly here and broke the darwin/windows
// GOEXPERIMENT=numa build). Called every time numaNoteSchedule actually
// pays the getcpu syscall -- both when it finds a node change and when
// it doesn't -- so a busy M that stays on the same node also stays
// throttled, not just one that migrates.
func (s *mNUMAState) armSoftAffinityCheck(deadline int64) {
	s.nextCheck = deadline
}

// appliedEpoch / setAppliedEpoch cache the enforcement epoch at apply
// time (v4 Task A5; see enforceEpoch's field comment).
func (s *mNUMAState) appliedEpoch() uint32     { return s.enforceEpoch }
func (s *mNUMAState) setAppliedEpoch(e uint32) { s.enforceEpoch = e }
