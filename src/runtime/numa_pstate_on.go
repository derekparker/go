// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.numa

package runtime

// pNUMAState is the per-P NUMA placement state, embedded as p.numa
// (runtime2.go). homeNode stores (node id + 1); 0 (the zero value)
// means "no home assigned" -- placement inactive, or this P not yet
// assigned. Same +1 zero-value idiom as mNUMAState.lastNode.
//
// Written only by numaAssignPHomes (from schedinit while m0 is the only
// runtime thread, and from procresize under sched.lock with the world
// stopped). Read without synchronization from schedule()/stealWork/
// numaGrowNode -- safe because every write happens single-threaded or
// world-stopped, and readers run only in a started world.
//
// Like mNUMAState, this file compiles on every GOOS whenever
// goexperiment.numa is set; nothing here may reference Linux-only
// symbols.
type pNUMAState struct {
	homeNode int8
}

// home returns the node id numaAssignPHomes assigned this P to, and
// whether one has been assigned at all (see homeNode's +1 encoding).
func (s *pNUMAState) home() (node int8, ok bool) {
	if s.homeNode == 0 {
		return 0, false
	}
	return s.homeNode - 1, true
}

// setHome records node as this P's assigned home.
func (s *pNUMAState) setHome(node int8) { s.homeNode = node + 1 }

// clearHome resets this P to "no home assigned" -- numaAssignPHomes's
// placement-inactive branch, so a stale assignment can never outlive
// the predicate that authorized it (the pairing rule, design §2).
func (s *pNUMAState) clearHome() { s.homeNode = 0 }

// numaStealFilter gates stealWork's same-node-first pass 0 (v4 stage 2
// design §6). Temporarily a build-time switch for the G2 ablation: the
// sched-micro gate measured CreateGoroutines +19% with the filter on,
// and the filter's contribution to the locality wins is unproven -- the
// ablation re-runs the primary gate with it off to decide keep/drop.
const numaStealFilter = false

// numaScheduleHook gates the schedule()-path numaNoteSchedule call --
// second ablation switch for the G2 sched-micro bisection (the steal
// filter was exonerated: +15% persists with it off).
const numaScheduleHook = false
