// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Export guts for testing mcentral's per-node refill routing (design
// §12.4, task-9-brief.md).
//
// Like export_numa_heapstreams_test.go, this file carries no
// linux/(amd64||arm64) restriction: mcentral.grow/uncacheSpan and
// numaArenaNode are OS-agnostic runtime code, and the exports below
// take their target node as an explicit argument rather than deriving
// it from getcpu, so node-purity is testable on any GOOS/GOARCH the
// goexperiment.numa build supports without real multi-node hardware
// (the same portability trick task 8's NumaHeapGrowForTest uses).
//
// The goexperiment.numa tag matters for the same reason it does there:
// this file is package runtime, so without the tag it would give the
// experiment-off test binary a reachable path into the array-shaped
// (numaMaxHeapNodes > 1) side of mcentral, which must never be
// reachable when the experiment is off.

//go:build goexperiment.numa

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// MCentralSpanAtForTest constructs a fully-accounted mspan for
// spanClass spc covering npages pages starting at base, marks exactly
// one object in it allocated, and returns base (for symmetry with
// other exports in this file: callers pass it straight through to
// MCentralUncacheAndFindForTest / MCentralFreeSpanForTest).
//
// base MUST already be genuinely free, unclaimed address space that
// this test controls exclusively -- e.g. the base of a brand new
// heapArena this test just registered via task 8's
// NumaHeapGrowForTest (paired with its growUntilNewArena helper,
// which guarantees a *new* arena rather than headroom reused from an
// existing one). This deliberately bypasses mheap.alloc's normal
// entry path (allocSpan, via mcentral.grow or otherwise): that path's
// h.pages.alloc searches for free pages ANYWHERE in the heap before
// ever calling mheap.grow, so even a node-targeted mheap.alloc call
// routinely gets satisfied from whichever node happens to have
// leftover free pages lying around in a busy test binary --
// silently defeating a node-purity probe that needs memory
// deterministically homed to a specific node (this is exactly what an
// earlier version of this test hook did, and why it flaked once other
// NUMA tests had already grown both nodes' streams earlier in the
// same test binary).
//
// Claims [base, base+npages*pageSize) from the page allocator directly
// (h.pages.allocRange, the same primitive allocSpan's own physical-
// page-alignment path uses) before building the span, so this is safe
// against concurrent allocation elsewhere in the process -- it is NOT
// safe to call with a base this test does not exclusively own yet.
//
// Review C1 fix: bypassing allocSpan's entry path also bypasses its
// HaveSpan accounting block (sysUsed for scavenged pages,
// gcController.heapReleased/heapFree/heapInUse, and the consistent
// memstats.heapStats deltas) -- an earlier version of this function
// left those untouched, so every span this probe built was invisible
// to ReadMemStats/gcController bookkeeping despite genuinely claiming
// real address space and pages from the page allocator, corrupting
// heap accounting (a reviewer-reproduced TestReadMemStats failure
// under `-count=2`, since each run's two spans -- one per node --
// leaked this way). This function now mirrors that accounting block
// exactly (h.initSpan itself is unchanged and still called the same
// way); MCentralFreeSpanForTest below is the exact inverse via
// mheap.freeSpan, which reverses precisely this same block.
//
// Review C1 fix, other half: the earlier version also fabricated
// "allocCount = 1" as a bare field write, with no corresponding
// allocCache/freeindex advance -- i.e. no real "alloc bit" set,
// leaving allocCount inconsistent with every other piece of the
// span's free-slot bookkeeping. This function now marks the one
// object allocated via nextFreeFast itself (the exact function
// mallocgc's own fast path uses), which is the only way to advance
// allocCount, freeindex, and allocCache together consistently.
func MCentralSpanAtForTest(spc uint8, base, npages uintptr) uintptr {
	systemstack(func() {
		lock(&mheap_.lock)
		scav := mheap_.pages.allocRange(base, npages)
		s := mheap_.allocMSpanLocked()
		unlock(&mheap_.lock)
		mheap_.initSpan(s, spanAllocHeap, spanClass(spc), base, npages, scav)

		// Mirrors allocSpan's HaveSpan accounting block exactly (typ
		// is always spanAllocHeap here, so the typ-switches below are
		// simplified to that one case).
		nbytes := npages * pageSize
		if scav != 0 {
			sysUsed(unsafe.Pointer(base), nbytes, scav)
			gcController.heapReleased.add(-int64(scav))
		}
		gcController.heapFree.add(-int64(nbytes - scav))
		gcController.heapInUse.add(int64(nbytes))
		stats := memstats.heapStats.acquire()
		atomic.Xaddint64(&stats.committed, int64(scav))
		atomic.Xaddint64(&stats.released, -int64(scav))
		atomic.Xaddint64(&stats.inHeap, int64(nbytes))
		memstats.heapStats.release()

		// Mark exactly one object allocated -- see the doc comment
		// above. The returned pointer is unused; this probe never
		// actually touches the object, only the span's bookkeeping.
		if nextFreeFast(s) == 0 {
			throw("MCentralSpanAtForTest: nextFreeFast failed on a freshly-initialized span")
		}
	})
	return base
}

// MCentralFreeSpanForTest is the exact inverse of MCentralSpanAtForTest
// (review C1): clears the one fabricated allocation (freeSpanLocked
// requires allocCount == 0 and sweepgen == h.sweepgen for an in-use
// span, so both are reset here immediately before freeing) and returns
// the span to the heap via mheap.freeSpan, which reverses precisely
// the accounting block MCentralSpanAtForTest mirrored. Callers must
// hold exclusive ownership of the span at base (e.g. having just
// popped it out of mcentral via MCentralUncacheAndFindForTest) --
// nothing else may be concurrently touching it.
func MCentralFreeSpanForTest(base uintptr) {
	s := spanOf(base)
	s.allocCount = 0
	s.sweepgen = mheap_.sweepgen
	mheap_.freeSpan(s)
}

// MCentralUncacheAndFindForTest simulates the tail of one refill round
// trip for a span this test already built via MCentralSpanAtForTest at
// base: runs it through the real uncacheSpan, then searches spanClass
// spc's partial sets for exactly wantNode (no fallback to any other
// node -- unlike cacheSpan, this is a direct, single-node probe of
// uncacheSpan's home-node routing, design §12.4) for a span at base.
//
// Both of wantNode's partial roles (swept and unswept -- see
// mcentral's partial field doc comment) are searched, not just
// whichever uncacheSpan happened to push into: a background GC cycle
// completing between uncacheSpan's push (at whatever mheap_.sweepgen
// it observes) and this function's own read of mheap_.sweepgen for
// the search would otherwise swap which of the two underlying
// spanSets "swept" currently means, and this is a white-box probe
// running concurrently with a live, busy runtime (the full test
// suite's background sweeper and other tests' GC cycles), not an
// isolated unit test -- searching both sides is robust to that
// regardless of how many cycles elapsed in between, since there are
// only ever two underlying slots for a given node.
//
// On success, the target span is left popped out of mcentral (owned
// exclusively by the caller, ready for MCentralFreeSpanForTest);
// every OTHER span the search happens to pop while looking is pushed
// back immediately, so this doesn't disturb the set for any other
// concurrent user of this spanclass+node.
func MCentralUncacheAndFindForTest(spc uint8, base uintptr, wantNode int32, limit int) bool {
	s := spanOf(base)
	mheap_.central[spc].mcentral.uncacheSpan(s)

	sg := mheap_.sweepgen
	c := &mheap_.central[spc].mcentral
	return searchSpanSetForTest(c.partialSwept(sg, wantNode), base, limit) ||
		searchSpanSetForTest(c.partialUnswept(sg, wantNode), base, limit)
}

// searchSpanSetForTest drains up to limit spans from set looking for
// one based at base, pushing back everything else it finds. See
// MCentralUncacheAndFindForTest.
//
// Review M5: the spans popped while searching (and not yet
// identified as the target) are held in a fixed-size array, not an
// append-grown slice. An append here would risk a reentrant mallocgc
// call while this probe is directly manipulating mcentral/spanSet
// state -- not actually unsafe in this specific calling context (no
// lock is held across the pop/push calls below, and this runs on a
// normal goroutine stack), but avoiding a reentrant allocation
// entirely is the simpler, more defensible choice for a white-box
// allocator test. limit is capped at the array's capacity, which is
// far more than this test ever needs to walk through in practice
// (a freshly-built, mostly-empty probe span in a small region of the
// set).
func searchSpanSetForTest(set *spanSet, base uintptr, limit int) bool {
	const maxOthers = 4096
	if limit > maxOthers {
		limit = maxOthers
	}
	var others [maxOthers]*mspan
	n := 0
	found := false
	for i := 0; i < limit; i++ {
		got := set.pop()
		if got == nil {
			break
		}
		if got.base() == base {
			found = true
			break
		}
		others[n] = got
		n++
	}
	for i := 0; i < n; i++ {
		set.push(others[i])
	}
	return found
}
