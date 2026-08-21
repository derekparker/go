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

// MCentralGrowForTest allocates a fresh span of npages pages for
// spanClass spc, homed to node via the real mheap.alloc entry point
// (mcentral.grow's own path) with genuine effectively true (bypassing
// numaRefillNode/getcpu -- the same test-bypass pattern task 8's
// NumaHeapGrowForTest uses for mheap.grow), marks exactly one object
// in it allocated via nextFreeFast (mallocgc's own fast-path
// primitive -- see MCentralFreeSpanForTest for why a bare allocCount
// write is not enough), and returns the span's base address, or 0 on
// failure (a real OOM).
//
// Review C1 fix history: an earlier version of this function
// bypassed mheap.alloc entirely, claiming an exact address range
// directly via h.pages.allocRange + h.initSpan, to defeat
// mheap.alloc's free-page-reuse (see npages's doc below) without
// needing a retry loop. That approach required hand-mirroring
// allocSpan's HaveSpan accounting block, which an even earlier
// version got wrong (see MCentralFreeSpanForTest's doc comment for
// that history) -- and, once the accounting was fixed, a reviewer
// reproduced a SEPARATE, more fundamental problem on real 2-node
// hardware: h.pages.allocRange -> pageAlloc.update panicked with
// "index out of range" indexing p.summary, a page-allocator-internal
// invariant this test-only reimplementation was not upholding
// correctly (the exact mechanism was not fully root-caused; the
// hardware-specific trigger, and the fact that the local single-node
// sandbox never reproduced it, both point at something in how sparse,
// widely-separated (~2 TiB apart per task 8) real per-node address
// ranges interact with pageAlloc's summary growth, which the local
// sandbox's much closer-together addresses never exercised).
//
// Going through the real, unmodified mheap.alloc/allocSpan path
// instead sidesteps the entire class of bug: every page-allocator-
// internal invariant is upheld by construction (it's the same code
// every real allocation uses), and the accounting block comes for
// free -- no hand-mirroring needed at all. The cost is needing a
// retry loop for node-targeting instead of an exact-address claim;
// see npages's doc and growForNodeUntilHomed (numa_refill_test.go).
func MCentralGrowForTest(spc uint8, node int32, npages uintptr) uintptr {
	var base uintptr
	systemstack(func() {
		s := mheap_.alloc(npages, spanClass(spc), node)
		if s == nil {
			return
		}
		if nextFreeFast(s) == 0 {
			throw("MCentralGrowForTest: nextFreeFast failed on a freshly-allocated span")
		}
		base = s.base()
	})
	return base
}

// MCentralFreeSpanForTest is the exact inverse of MCentralGrowForTest's
// allocation (review C1): clears the one fabricated allocation
// (freeSpanLocked requires allocCount == 0 and sweepgen == h.sweepgen
// for an in-use span, so both are reset here immediately before
// freeing) and returns the span to the heap via mheap.freeSpan, which
// exactly reverses whatever accounting mheap.alloc/allocSpan applied
// when the span was built. Callers must hold exclusive ownership of
// the span at base (e.g. having just popped it out of mcentral via
// MCentralUncacheAndFindForTest) -- nothing else may be concurrently
// touching it.
//
// An earlier version of this test suite had no equivalent of this
// function at all: the span this probe built was popped out of
// mcentral by the search and never freed, leaking it permanently (a
// reviewer-reproduced TestReadMemStats failure under -count=2, since
// each run's two spans -- one per node -- leaked this way). Every
// caller now calls this after a successful find.
func MCentralFreeSpanForTest(base uintptr) {
	s := spanOf(base)
	s.allocCount = 0
	s.sweepgen = mheap_.sweepgen
	mheap_.freeSpan(s)
}

// MCentralUncacheAndFindForTest simulates the tail of one refill round
// trip for a span this test already built via MCentralGrowForTest at
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
