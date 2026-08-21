// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.numa

package runtime

import "internal/runtime/atomic"

// numaGrowHighWaterNode is the highest per-node heap arena stream
// index mheap.grow has ever touched (review I3): -1 means "no grow
// call has run yet" (process startup, before mallocinit's first
// grow). Updated via numaGrowHighWaterNodeUpdate, from within
// mheap.grow itself, using the same monotonic-max CAS-retry pattern
// sweepClass.update (mgcsweep.go) already establishes for a
// process-global "highest index touched so far" counter.
//
// This is a pure optimization hint, never a correctness bound:
// c.partial/full[*][node] for any node > the high-water mark is
// always empty by construction, since a span can only land in node
// N's set if its home arena was grown for node N (design §12.3), and
// mheap.grow is the only place that ever grows a given stream.
// cacheSpan's remote-fallback loop and the background sweeper's
// per-node loop use numaGrowLoopBound to skip streams that have
// therefore never been touched, rather than always walking the full
// [0, numaMaxHeapNodes) range on hardware with far fewer real nodes
// than numaMaxHeapNodes (8).
//
// Build-tagged (I5), like numaMaxHeapNodes itself: an off-build
// package-level var (plus the init() that seeds it) would add BSS/data
// to every Go binary regardless of the experiment, even though every
// call site is already goexperiment.Numa-guarded -- the guard alone
// doesn't remove the variable's own footprint from the off build.
var numaGrowHighWaterNode atomic.Int32

func init() {
	numaGrowHighWaterNode.Store(-1)
}

// numaGrowHighWaterNodeUpdate records that idx has been touched by
// mheap.grow, advancing numaGrowHighWaterNode if idx is higher than
// what's recorded so far. h.lock is held by every caller (mheap.grow),
// but readers (numaGrowLoopBound) are not under that lock, hence the
// atomic type and the CAS-retry rather than a plain compare-then-store.
func numaGrowHighWaterNodeUpdate(idx int32) {
	old := numaGrowHighWaterNode.Load()
	for idx > old && !numaGrowHighWaterNode.CompareAndSwap(old, idx) {
		old = numaGrowHighWaterNode.Load()
	}
}

// numaGrowLoopBound returns the inclusive upper bound cacheSpan's
// remote-fallback loop and the background sweeper's per-node loop
// should use for node indices (review I3): numaMaxHeapNodes - 1 (the
// full range, I3's safe default) before any grow call has ever run, or
// numaGrowHighWaterNode's current value otherwise -- streams above it
// are provably empty (see that variable's doc comment). See
// numa_growhighwater_off.go for the off-build value (always 0).
func numaGrowLoopBound() int32 {
	if hwm := numaGrowHighWaterNode.Load(); hwm >= 0 {
		return hwm
	}
	return numaMaxHeapNodes - 1
}
