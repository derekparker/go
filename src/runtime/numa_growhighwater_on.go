// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.numa

package runtime

import "internal/runtime/atomic"

// numaGrowHighWaterNode is the highest per-node heap arena stream
// index mheap.grow has ever touched (review I3). Updated via
// numaGrowHighWaterNodeUpdate, from within mheap.grow itself, using
// the same monotonic-max CAS-retry pattern sweepClass.update
// (mgcsweep.go) already establishes for a process-global "highest
// index touched so far" counter.
//
// Deliberately NO init() here (re-review NEW-1): an earlier version
// called numaGrowHighWaterNode.Store(-1) from a package init() to mark
// "nothing touched yet" as a sentinel distinct from "node 0 touched".
// That init() runs from runtime.main, via doInit -- LONG after
// schedinit/mallocinit/procresize have already grown the heap and
// therefore already advanced this counter via real mheap.grow calls.
// The Store(-1) silently clobbered that already-accumulated mark. On
// real 2-node hardware this produced exactly the failure mode I3 was
// supposed to prevent: an early node-1 grow correctly advanced the
// mark to 1, a later node-0 grow (after init() clobbered it back to
// -1) left it at 0, and code bounding a search by this value would
// skip node 1's now-populated sets entirely -- reproduced as
// finishsweep_m's "attempt to clear non-empty span set" throw with an
// earlier (already-reverted, see mgcsweep.go) version of
// nextSpanForSweep that used this bound.
//
// The fix is to not need a sentinel at all: atomic.Int32's zero value,
// 0, is already a sound starting bound with no init() required --
// before mheap.grow has ever run for any node, no heapArena exists for
// any node, so every node's spanSets are empty and bounding the search
// to just node 0 costs nothing (there is nothing to find elsewhere
// yet). Every subsequent read is either 0 (still nothing grown beyond
// stream 0) or a value numaGrowHighWaterNodeUpdate installed strictly
// before the grow call that could have populated that node's sets --
// see numaGrowHighWaterNodeUpdate's doc comment for why that ordering
// holds.
//
// This is a pure optimization hint, never a correctness bound in its
// own right: c.partial/full[*][node] for any node > the high-water
// mark is always empty by construction, since a span can only land in
// node N's set if its home arena was grown for node N (design §12.3),
// and mheap.grow is the only place that ever grows a given stream.
// cacheSpan's remote-fallback loop uses numaGrowLoopBound to skip
// streams that have therefore never been touched, rather than always
// walking the full [0, numaMaxHeapNodes) range on hardware with far
// fewer real nodes than numaMaxHeapNodes (8). nextSpanForSweep
// deliberately does NOT use this bound -- see its own doc comment
// (mgcsweep.go) for why staying full-range there is a conservative
// choice, not a soundness requirement, now that this clobber is fixed.
//
// Build-tagged (I5), like numaMaxHeapNodes itself: an off-build
// package-level var would add BSS/data to every Go binary regardless
// of the experiment, even though every call site is already
// goexperiment.Numa-guarded -- the guard alone doesn't remove the
// variable's own footprint from the off build.
var numaGrowHighWaterNode atomic.Int32

// numaGrowHighWaterNodeUpdate records that idx has been touched by
// mheap.grow, advancing numaGrowHighWaterNode if idx is higher than
// what's recorded so far. h.lock is held by every caller (mheap.grow),
// but readers (numaGrowLoopBound) are not under that lock, hence the
// atomic type and the CAS-retry rather than a plain compare-then-store.
//
// Ordering this update runs BEFORE mheap.grow's own sysAlloc call is
// what makes numaGrowLoopBound's bound sound (re-review NEW-1): a span
// can only ever land in node N's spanSets if some heapArena was tagged
// node N via numaArenaSetNode, which only happens from within
// mheap.grow's sysAlloc call for stream N -- so by the time any such
// arena (and therefore any such span) can exist, this function has
// already run for N and the high-water mark is already >= N. The
// unswept role a span sits in after a sweepgen rotation is the same
// underlying storage as the swept role it was pushed into, not a
// separate population step, so this single ordering guarantee covers
// both roles.
func numaGrowHighWaterNodeUpdate(idx int32) {
	old := numaGrowHighWaterNode.Load()
	for idx > old && !numaGrowHighWaterNode.CompareAndSwap(old, idx) {
		old = numaGrowHighWaterNode.Load()
	}
}

// numaGrowLoopBound returns the inclusive upper bound cacheSpan's
// remote-fallback loop should use for node indices (review I3):
// numaGrowHighWaterNode's current value, which starts at 0 (no init()
// needed -- see that variable's doc comment) and only ever increases.
// See numa_growhighwater_off.go for the off-build value (always 0).
func numaGrowLoopBound() int32 {
	return numaGrowHighWaterNode.Load()
}
