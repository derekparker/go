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

// MCentralSpanAtForTest constructs a minimal, valid mspan for
// spanClass spc covering npages pages starting at base, and returns
// its base address (== base, for symmetry with other exports in this
// file).
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
func MCentralSpanAtForTest(spc uint8, base, npages uintptr) uintptr {
	systemstack(func() {
		lock(&mheap_.lock)
		scav := mheap_.pages.allocRange(base, npages)
		s := mheap_.allocMSpanLocked()
		unlock(&mheap_.lock)
		mheap_.initSpan(s, spanAllocHeap, spanClass(spc), base, npages, scav)
	})
	return base
}

// MCentralUncacheAndFindForTest simulates the tail of one refill round
// trip for a span this test already built via MCentralSpanAtForTest at
// base: marks it allocated (as a real cache/uncache cycle would have
// left it) and runs it through the real uncacheSpan, then searches
// spanClass spc's partial sets for exactly wantNode (no fallback to
// any other node -- unlike cacheSpan, this is a direct, single-node
// probe of uncacheSpan's home-node routing, design §12.4) for a span
// at base.
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
// The search drains up to limit spans from each role looking for
// base, pushing back everything else it finds so it doesn't disturb
// the set for any other concurrent user of this spanclass+node -- this
// makes the probe tolerant of a busy set without needing exclusive
// access to mcentral.
func MCentralUncacheAndFindForTest(spc uint8, base uintptr, wantNode int32, limit int) bool {
	s := spanOf(base)
	s.allocCount = 1
	s.sweepgen = mheap_.sweepgen
	mheap_.central[spc].mcentral.uncacheSpan(s)

	sg := mheap_.sweepgen
	c := &mheap_.central[spc].mcentral
	return searchSpanSetForTest(c.partialSwept(sg, wantNode), base, limit) ||
		searchSpanSetForTest(c.partialUnswept(sg, wantNode), base, limit)
}

// searchSpanSetForTest drains up to limit spans from set looking for
// one based at base, pushing back everything else it finds. See
// MCentralUncacheAndFindForTest.
func searchSpanSetForTest(set *spanSet, base uintptr, limit int) bool {
	var others []*mspan
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
		others = append(others, got)
	}
	for _, o := range others {
		set.push(o)
	}
	return found
}
