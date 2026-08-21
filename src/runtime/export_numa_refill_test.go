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
	"internal/goexperiment"
	"internal/runtime/atomic"
	"unsafe"
)

// MCentralGrowAndSpanForTest attempts ONE grow-and-claim cycle for
// spanClass spc, node: grows npage pages via mheap.grow directly
// (bypassing numaRefillNode/getcpu, like task 8's own
// NumaHeapGrowForTest), then immediately builds a spanNPages-page span
// at the base of the range that grow call just registered with the
// page allocator, marking exactly one object in it allocated, all
// before releasing mheap_.lock. grew false means mheap.grow itself
// failed (a real OOM, fatal for the caller) and base is meaningless;
// otherwise base is always valid -- there is no retry/newArena
// signal to check (see the fix history below for why an earlier
// version needed one and why it turned out not to).
//
// Review re-review NEW-3 fix history, three rounds:
//
// Round 1: an intermediate version went through mheap.alloc instead of
// mheap.grow directly (retrying with a growing npages, checking either
// numaArenaNode(base) == node or, later, whether a new heapArena
// registered). Neither retry criterion reliably escaped
// self-contamination under repeated invocation (-count=20): every
// rejected attempt gets freed back to the general free pool (review
// C1 -- rejecting a span without freeing it would itself be a leak),
// and mheap.alloc's own free-page-reuse (see allocSpan) happily
// satisfies a LATER, larger request from THAT freed memory before
// ever reaching mheap.grow again -- observed as a 19/20 Skip rate on
// real hardware even with the new-heapArena check.
//
// Round 2: switched to task 8's own growUntilNewArena (which grows raw
// address space directly via mheap.grow and never frees anything back
// to the general pool, so it can't self-contaminate) for the address,
// paired with a SEPARATE call building the span via h.pages.allocRange
// at that address. This reproduced a "fatal error: index out of
// range" panic inside pageAlloc.update, indexing p.summary -- first on
// real 2-node hardware, then (this round's own finding) locally too,
// under -shuffle=1, requesting node 0. The initial hypothesis (a race
// between growUntilNewArena's own lock release and a second,
// independent lock acquisition for the claim step) was WRONG: fusing
// both steps under one continuous mheap_.lock hold (an earlier version
// of this very function) reproduced the identical crash, ruling out a
// race entirely -- see below for the actual mechanism.
//
// Round 3 (this version) is the real root cause and fix. Both
// growUntilNewArena and the round-2 fusion used
// arenaBase(heapArenas[len(heapArenas)-1]) -- the highest-address
// heapArena registered by this grow call -- as the claim address. But
// heapArena registration (in mheap.sysAlloc) and page-allocator
// registration (in mheap.grow, via h.pages.grow) use two DIFFERENT
// granularities that only sometimes coincide: sysAlloc rounds its
// reservation up to a whole number of heapArenaBytes (~64 MiB) and
// registers a heapArena for every such chunk in that rounded
// reservation, while h.pages.grow only registers the page allocator's
// summary/chunk metadata for the exact byte range mheap.grow actually
// asked for (npage pages, rounded only to the much smaller
// pallocChunkBytes, ~4 MiB). sysAlloc's rounding registers one or more
// heapArenas that sit PAST the page allocator's actual grown range
// whenever the two roundings don't coincide: a "reserved and
// heapArena-metadata-valid, but not yet page-allocator-registered"
// slack region. That's not limited to "the requested ask isn't a
// whole heapArenaBytes multiple" (an earlier version of this comment
// said only that) -- it also happens whenever h.curArena[idx].base
// itself isn't heapArenaBytes-aligned at the point a NEW sysAlloc
// reservation is made, which is common after any contiguous in-place
// extension of an already-partially-consumed arena (h.curArena[idx]
// only ever gets heapArenaBytes-realigned by drawing a fresh av from
// sysAlloc, not by every grow call), since sysAlloc's own rounding is
// relative to that base, not to a fixed heapArenaBytes grid.
// heapArenas[len-1] can land in that slack, and indexing
// pageAlloc.p.summary for an address there is exactly "index out of
// range" -- deterministic given the right size/alignment, not a race,
// and reproducible with no other goroutine involved at all.
//
// The actual page-allocator-registered range from a given mheap.grow
// call is exactly [v, nBase) where v was h.curArena[idx].base before
// the call and nBase is h.curArena[idx].base after -- mheap.grow
// always calls h.pages.grow(v, nBase-v) unconditionally on every
// successful call, so that range is always genuinely fresh, forward-
// only address space (this also means the "self-contaminating /
// reused headroom" concern the round-1/round-2 retry-until-new-arena
// loop was designed around does not actually apply to a direct
// mheap.grow call the way it did to mheap.alloc -- see growForNodeUntilHomed,
// numa_refill_test.go, for why that loop is gone in this round too).
// Reading h.curArena[idx].base immediately after grow returns gives
// nBase directly, with no heapArenaBytes-rounding slack anywhere
// near it; claiming spanNPages pages ending at that exact frontier
// (spanNPages is tiny relative to npage, so it never reaches back
// before v) is always within the range h.pages.grow just registered.
//
// Mirrors allocSpan's HaveSpan accounting block exactly (sysUsed for
// scavenged pages, gcController.heapReleased/heapFree/heapInUse, and
// the consistent memstats.heapStats deltas) -- an earlier version of
// this function's predecessor left those untouched, so every span
// built was invisible to ReadMemStats/gcController bookkeeping despite
// genuinely claiming real address space and pages from the page
// allocator, corrupting heap accounting (a reviewer-reproduced
// TestReadMemStats failure under -count=2). MCentralFreeSpanForTest is
// the exact inverse via mheap.freeSpan, which reverses precisely this
// same block.
//
// The one object marked allocated is via nextFreeFast itself (the
// exact function mallocgc's own fast path uses), not a bare
// allocCount write, so allocCache/freeindex stay consistent with it --
// see MCentralFreeSpanForTest's doc comment for why a bare write isn't
// enough. This bypasses mcache.refill's normal
// smallAllocCount/tinyAllocs bookkeeping entirely -- a transient
// memstats residual, not a leak: it's never incremented here, and
// MCentralFreeSpanForTest's plain freeSpan never decrements it either,
// so the two omissions cancel and there is no net effect on final
// counts, only a window (between this call and the matching
// MCentralFreeSpanForTest) where a concurrent ReadMemStats could
// observe smallAllocCount undercounting this one object relative to
// allocCount/heapInUse.
func MCentralGrowAndSpanForTest(spc uint8, node int32, npage, spanNPages uintptr) (base uintptr, grew bool) {
	systemstack(func() {
		// idx mirrors mheap.grow's own internal index computation
		// exactly (mheap.go, top of (*mheap).grow) -- this is the
		// same array slot mheap.grow itself will read and write
		// h.curArena[idx] through below. Review NEW-5: this hand
		// duplication has no compiler-enforced link to mheap.grow's
		// own copy -- if a future change ever alters how mheap.grow
		// derives idx without updating this block to match, the
		// vBefore check just below is what catches the desync
		// (rather than this function silently computing a claim
		// address from the wrong, untouched array slot). MUST STAY
		// IN SYNC WITH mheap.grow'S OWN idx DERIVATION.
		idx := int32(0)
		if goexperiment.Numa && node >= 0 {
			idx = node
		}

		lock(&mheap_.lock)
		// vBefore is h.curArena[idx].base immediately before the grow
		// call below -- i.e. v, using mheap.grow's own naming for the
		// low end of the range its h.pages.grow call is about to
		// register. Captured under the same lock hold as the grow
		// itself, so no concurrent mheap.grow(_, node) call (there is
		// only ever one goroutine at a time inside this systemstack
		// closure while mheap_.lock is held, and no other caller in
		// this file grows the same node concurrently) can move it out
		// from under this read.
		vBefore := mheap_.curArena[idx].base
		_, grew = mheap_.grow(npage, node)
		if !grew {
			unlock(&mheap_.lock)
			return
		}
		// h.curArena[idx].base is now nBase: the exact upper bound of
		// the range this grow call just registered with the page
		// allocator via h.pages.grow(v, nBase-v) (see this function's
		// doc comment, round 3). Claiming spanNPages pages ending
		// exactly at that frontier keeps the claim inside [v, nBase)
		// with no heapArenaBytes-rounding slack involved.
		arenaBaseAddr := mheap_.curArena[idx].base - spanNPages*pageSize
		if arenaBaseAddr < vBefore {
			// idx above did not land on the array slot mheap.grow
			// itself just wrote to grow returned true, yet
			// h.curArena[idx].base didn't advance (or advanced by
			// less than spanNPages*pageSize) -- exactly the signature
			// of idx's hand-duplicated derivation going out of sync
			// with mheap.grow's own (review NEW-5). Throwing here
			// turns that desync into an immediate, loud failure
			// instead of a silent claim of stale or unrelated address
			// space.
			throw("MCentralGrowAndSpanForTest: idx out of sync with mheap.grow -- computed claim address before the pre-grow frontier")
		}

		// Still holding mheap_.lock from the grow above.
		scav := mheap_.pages.allocRange(arenaBaseAddr, spanNPages)
		s := mheap_.allocMSpanLocked()
		unlock(&mheap_.lock)

		mheap_.initSpan(s, spanAllocHeap, spanClass(spc), arenaBaseAddr, spanNPages, scav)

		nbytes := spanNPages * pageSize
		if scav != 0 {
			sysUsed(unsafe.Pointer(arenaBaseAddr), nbytes, scav)
			gcController.heapReleased.add(-int64(scav))
		}
		gcController.heapFree.add(-int64(nbytes - scav))
		gcController.heapInUse.add(int64(nbytes))
		stats := memstats.heapStats.acquire()
		atomic.Xaddint64(&stats.committed, int64(scav))
		atomic.Xaddint64(&stats.released, -int64(scav))
		atomic.Xaddint64(&stats.inHeap, int64(nbytes))
		memstats.heapStats.release()

		if nextFreeFast(s) == 0 {
			throw("MCentralGrowAndSpanForTest: nextFreeFast failed on a freshly-initialized span")
		}
		base = s.base()
	})
	return base, grew
}

// MCentralFreeSpanForTest is the exact inverse of
// MCentralGrowAndSpanForTest's allocation (review C1): clears the one
// fabricated allocation
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

// MCentralReturnCacheSpanForTest returns a span MCentralCacheSpanForTest
// (i.e. the real, unmodified cacheSpan) just handed to a test, to
// wherever it actually came from (re-review NEW-4).
//
// cacheSpan's contract only promises the returned span is popped out
// of mcentral and ready to allocate from -- it does NOT promise the
// span is one this test itself built. In a live, busy runtime (the
// full test suite, with a background sweeper and other goroutines
// refilling from the very same spanClass concurrently), cacheSpan can
// just as easily return some OTHER, genuinely pre-existing span --
// one that may already have real, live objects allocated in it from
// elsewhere in the process. An earlier version of
// testSpanRefillCounterMatchesHomeNode (numa_refill_test.go) called
// MCentralFreeSpanForTest unconditionally on whatever cacheSpan
// returned; MCentralFreeSpanForTest forces allocCount to 0 before
// freeing regardless of its actual value -- a latent use-after-free on
// exactly that pre-existing-span case, forcibly freeing memory a
// concurrent goroutine still holds live pointers into.
//
// The two cases are told apart by allocCount itself, the same test
// uncacheSpan's own precondition already relies on (it throws if
// allocCount == 0): a span this process just built fresh -- whether
// this test's own MCentralGrowAndSpanForTest call, or c.grow inside
// cacheSpan itself -- has allocCount == 0 (nothing has ever been
// allocated from it) and is safe to free outright. Any other span,
// allocCount > 0, is not this test's to free; it is put back into
// mcentral, exactly where cacheSpan took it from, via the real
// uncacheSpan.
func MCentralReturnCacheSpanForTest(spc uint8, base uintptr) {
	s := spanOf(base)
	if s.allocCount == 0 {
		MCentralFreeSpanForTest(base)
		return
	}
	mheap_.central[spc].mcentral.uncacheSpan(s)
}

// MCentralUncacheAndFindForTest simulates the tail of one refill round
// trip for a span this test already built via MCentralGrowAndSpanForTest
// at base: runs it through the real uncacheSpan, then searches spanClass
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
	return MCentralFindForTest(spc, base, wantNode, limit)
}

// MCentralFindForTest is MCentralUncacheAndFindForTest's search half on
// its own, without the uncacheSpan call -- for a span that's already
// sitting in mcentral (e.g. via MCentralPlaceForTest, or one
// MCentralUncacheAndFindForTest already placed and a caller now needs
// to relocate again without re-placing it, which would push a second,
// duplicate reference to the same span into the spanSet's block list).
func MCentralFindForTest(spc uint8, base uintptr, wantNode int32, limit int) bool {
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
// allocator test. limit is capped at the array's capacity. Kept small
// (re-review M5 nit: an earlier version used 4096, an unnecessarily
// heavy 32 KiB zeroed stack array for every call) -- still far more
// than this test ever needs to walk through in practice (a
// freshly-built, mostly-empty probe span in a small region of the
// set).
func searchSpanSetForTest(set *spanSet, base uintptr, limit int) bool {
	const maxOthers = 256
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

// NumaRefillNodeForTest exports numaRefillNode (review NEW-3): lets a
// test read the same node/genuine pair mcentral.cacheSpan itself would
// read for a refill happening right now, so a test can predict which
// per-node spanSet a real cacheSpan call should route to without
// needing to control getcpu.
func NumaRefillNodeForTest() (node int32, genuine bool) {
	return numaRefillNode()
}

// MCentralPlaceForTest runs a span already built via
// MCentralGrowAndSpanForTest through the real uncacheSpan, placing it
// into its home node's spanSet
// (design §12.4) -- unlike MCentralUncacheAndFindForTest, this does not
// also search for and remove it again; the span is left sitting in
// mcentral for something else (e.g. a real MCentralCacheSpanForTest
// call) to find.
func MCentralPlaceForTest(spc uint8, base uintptr) {
	s := spanOf(base)
	mheap_.central[spc].mcentral.uncacheSpan(s)
}

// MCentralCacheSpanForTest drives spanClass spc's real, unmodified
// mcentral.cacheSpan -- the exact function under test, including its
// I1 local/remote classification and counter increments -- and returns
// the resulting span's base address. Review NEW-3: this is what makes
// the I1 fix's local/remote classification testable at all; every
// earlier test in this file exercised only mcentral.uncacheSpan's
// placement (via MCentralUncacheAndFindForTest) or mheap.grow/
// h.pages.allocRange (via MCentralGrowAndSpanForTest), neither of
// which goes through cacheSpan itself, so a mutation collapsing cacheSpan's
// local computation to an unconditional true passed every prior
// subtest.
func MCentralCacheSpanForTest(spc uint8) uintptr {
	var base uintptr
	systemstack(func() {
		s := mheap_.central[spc].mcentral.cacheSpan()
		if s == nil {
			throw("MCentralCacheSpanForTest: cacheSpan returned nil (real OOM?)")
		}
		base = s.base()
	})
	return base
}
