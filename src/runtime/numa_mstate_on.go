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

	// lastNode stores (node id + 1): the node numaNoteSchedule last
	// narrowed this M's CPU affinity to, or 0 (its zero value) if
	// numaNoteSchedule has never narrowed this M at all. The +1 offset
	// makes "never narrowed" distinguishable from a genuine node-0
	// reading with a single field -- the same zero-value-safe idiom
	// bindAllDone above already relies on -- so the very first eligible
	// schedule() pass for any M always applies affinity, regardless of
	// which node it happens to observe first.
	lastNode int8
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
