// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.numa

package runtime_test

import (
	"runtime"
	"runtime/metrics"
	"testing"
)

// TestNUMASpanRefillMetrics is task 9's red test (design §12.4):
// mcentral's per-node refill routing must be visible through the
// /numa/span-refills/local:spans and /numa/span-refills/remote:spans
// runtime/metrics counters (the in-vivo locality proxy the design
// calls for between perf-counter runs), and the routing itself must
// keep each node's spanSet node-pure (mcentral.uncacheSpan routes by
// the span's home node, not the freeing thread's node -- see its doc
// comment).
func TestNUMASpanRefillMetrics(t *testing.T) {
	t.Run("ChurnIncrementsCounters", testSpanRefillCountersIncrement)
	t.Run("OneProcLocalSanity", testSpanRefillOneProcLocalSanity)
	t.Run("NodePureRouting", testSpanRefillNodePureRouting)
	t.Run("CounterMatchesHomeNode", testSpanRefillCounterMatchesHomeNode)
}

// testSpanRefillCountersIncrement is the primary red-test property:
// the counters exist (readable via runtime/metrics), never decrease,
// and their sum increases as a direct result of allocation churn that
// forces mcache.refill (and therefore mcentral.cacheSpan) to actually
// run.
func testSpanRefillCountersIncrement(t *testing.T) {
	before := readSpanRefillCounters(t)
	allocChurnForRefill()
	after := readSpanRefillCounters(t)

	if after.local < before.local {
		t.Errorf("local refill counter decreased: %d -> %d", before.local, after.local)
	}
	if after.remote < before.remote {
		t.Errorf("remote refill counter decreased: %d -> %d", before.remote, after.remote)
	}
	sumBefore := before.local + before.remote
	sumAfter := after.local + after.remote
	if sumAfter <= sumBefore {
		t.Errorf("span-refills sum did not increase from allocation churn: before=%d (local=%d remote=%d) after=%d (local=%d remote=%d)",
			sumBefore, before.local, before.remote, sumAfter, after.local, after.remote)
	}
}

// testSpanRefillOneProcLocalSanity is the brief's "1P local sanity"
// check: exercises the counters under GOMAXPROCS(1) and logs the
// local/remote split as diagnostic evidence, but does NOT assert a
// bound on the ratio.
//
// Reviewer-caught finding (real 2-node hardware, isolated -run
// 'TestReadMemStats|TestNUMASpanRefillMetrics' -- not reproduced when
// this subtest ran later in the full -short suite, nor on the local
// single-node sandbox): a hard ratio bound here is unsound, not just
// occasionally flaky. GOMAXPROCS(1) limits how many Ps Go schedules
// onto, but does not pin the underlying OS thread's CPU affinity --
// the kernel scheduler remains free to migrate that thread across
// NUMA nodes at any time, and observably does so, especially right
// after the GOMAXPROCS transition itself (tearing down up to 255 Ps
// causes scheduler churn). Task 9 (this task) delivers ingredients
// (a) homing and (b) routing only; ingredient (c), threads that stay
// put, is Task 10's node-mask soft affinity from the scheduler --
// design §12.1 states plainly that any proper subset of the three
// ingredients is expected to measure ~zero locality, and that is
// exactly what was observed: remote refills reached ~97% in one
// reproduction, a single thread legitimately bounced across nodes
// mid-churn with nothing yet holding it in place. A fixed ratio bound
// asserted here cannot distinguish that expected, documented gap from
// a genuine routing regression, so it isn't a sound gate before Task
// 10 lands; ChurnIncrementsCounters already provides the hard
// pass/fail signal that the counters move and are wired up correctly.
func testSpanRefillOneProcLocalSanity(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))

	before := readSpanRefillCounters(t)
	allocChurnForRefill()
	after := readSpanRefillCounters(t)

	localDelta := after.local - before.local
	remoteDelta := after.remote - before.remote
	total := localDelta + remoteDelta
	t.Logf("GOMAXPROCS=1 churn: local delta=%d remote delta=%d", localDelta, remoteDelta)
	if total == 0 {
		t.Fatal("no refills observed during churn; test isn't exercising cacheSpan")
	}
}

// testSpanRefillNodePureRouting is a white-box check of the routing
// ingredient's other required property (task 9 brief): a span
// returned via mcentral.uncacheSpan lands in the spanSet indexed by
// its OWN home node, not wherever the freeing thread happens to be
// running -- exactly the "node-pure set behavior where observable"
// this task calls for. This does not require real multi-node
// hardware: growForNodeUntilHomed homes a span to an explicit node,
// bypassing getcpu entirely, the same portability trick task 8's
// NumaHeapGrowForTest uses.
func testSpanRefillNodePureRouting(t *testing.T) {
	maxNodes := runtime.NumaMaxHeapNodesForTest()
	if maxNodes < 2 {
		t.Skip("numaMaxHeapNodes < 2: no distinct per-node sets to test (I5 collapse)")
	}
	if !runtime.NumaHeapStreamsEnabledForTest() {
		t.Skip("numaHeapStreamsEnabled is false: streams are not populated on this build/run (race, tight-VA, or 32-bit)")
	}

	// spanClass 135 is sizeclass 67, noscan (32768-byte elements, the
	// LARGEST size class -- see internal/runtime/gc.SizeClassToSize):
	// the routing logic under test doesn't depend on which class it
	// is, but growForNodeUntilHomed's retries need enough headroom
	// (mspan.nelems is a uint16) to force a genuine per-node grow even
	// when other NUMA tests earlier in the same binary (e.g.
	// TestNUMAHeapArenaStreams) have already left substantial free
	// memory sitting around in the "wrong" node's arena -- the
	// largest size class gives ~2 GiB of safe npages headroom before
	// nelems could overflow, comfortably more than that contamination
	// could plausibly leave behind.
	const spc = 135

	// Matches searchSpanSetForTest's fixed-size backing array (review
	// M5) -- comfortably more than this test ever needs to walk
	// through in practice.
	const searchLimit = 256

	// Each node's alloc -> find -> free cycle runs to completion
	// before moving to the next node, rather than allocating both
	// nodes' spans up front: growForNodeUntilHomed can Skip partway
	// through (its own doc comment explains why), and if node 0's
	// span were left allocated while node 1's attempt Skipped, it
	// would never reach its own MCentralFreeSpanForTest call --
	// exactly the kind of leak review C1 was about, just relocated to
	// this Skip path instead of the original success path.

	// The node-0 span must be findable in node 0's partial-swept set...
	base0 := growForNodeUntilHomed(t, spc, 0)
	if runtime.MCentralUncacheAndFindForTest(spc, base0, 0, searchLimit) {
		// Review C1: free it back (the exact inverse of
		// MCentralGrowAndSpanForTest's allocation) now that the probe
		// is done with it, rather than leaving it a permanent leak.
		runtime.MCentralFreeSpanForTest(base0)
	} else {
		t.Errorf("span homed to node 0 not found in node 0's partial-swept set after uncacheSpan")
	}
	// ...and the node-1 span must be findable in node 1's, not node 0's
	// (the actual node-purity property: routing by home node, not by
	// whichever set we happen to look in).
	base1 := growForNodeUntilHomed(t, spc, 1)
	if runtime.MCentralUncacheAndFindForTest(spc, base1, 1, searchLimit) {
		runtime.MCentralFreeSpanForTest(base1)
	} else {
		t.Errorf("span homed to node 1 not found in node 1's partial-swept set after uncacheSpan")
	}
}

// testSpanRefillCounterMatchesHomeNode is re-review NEW-3's
// mutation-kill coverage for the I1 fix: nothing in the rest of this
// file drives mcentral.cacheSpan itself (NodePureRouting exercises
// uncacheSpan's placement via MCentralUncacheAndFindForTest;
// ChurnIncrementsCounters/OneProcLocalSanity exercise cacheSpan only
// indirectly, through real getcpu-dependent thread placement they
// can't control), so a mutation collapsing cacheSpan's `local :=
// !goexperiment.Numa || numaArenaNode(s.base()) == node` to an
// unconditional `local := true` passed every prior subtest in this
// file.
//
// This drives one real refill (MCentralCacheSpanForTest, i.e.
// mcentral.cacheSpan itself, not a stand-in) through an
// explicitly-homed, pre-placed span and asserts that whichever counter
// actually moved agrees with numaArenaNode(returned span's base) ==
// the refill node read the same way cacheSpan itself reads it
// (NumaRefillNodeForTest). This is definitional and, unlike
// OneProcLocalSanity's dropped ratio bound, does not depend on the
// calling thread staying on one NUMA node: it doesn't matter which
// node cacheSpan routes for, only whether the counter agrees with the
// ACTUAL span it returned. The refill node is read a second time,
// immediately before driving the refill (not reused from the earlier
// read used to pick otherNode below), to keep the window in which the
// calling thread could migrate as short as possible -- the same kind
// of short-window residual task 8's own TestArenaCollision accepts.
func testSpanRefillCounterMatchesHomeNode(t *testing.T) {
	maxNodes := runtime.NumaMaxHeapNodesForTest()
	if maxNodes < 2 {
		t.Skip("numaMaxHeapNodes < 2: no distinct per-node sets to test (I5 collapse)")
	}
	if !runtime.NumaHeapStreamsEnabledForTest() {
		t.Skip("numaHeapStreamsEnabled is false: streams are not populated on this build/run (race, tight-VA, or 32-bit)")
	}

	initialNode, genuine := runtime.NumaRefillNodeForTest()
	if !genuine {
		t.Skip("numaRefillNode not genuine on this run")
	}

	// Bias toward a node other than wherever this thread currently
	// looks like it's running, so the placed span is likely to be
	// found "remote" -- needed to have a real chance of catching the
	// local:=true mutation (which only ever predicts "local"). This is
	// only a bias, not a requirement for correctness: the assertion
	// below checks the actual observed relationship, whatever it
	// turns out to be, not that this specific span is found remote.
	otherNode := int32(0)
	if otherNode == initialNode {
		otherNode = 1
	}

	// A spanClass distinct from NodePureRouting's (135) and from
	// anything allocChurnForRefill touches (its largest allocation is
	// 2055 bytes), so nothing else in this process concurrently
	// populates the refill node's own set for this class and confuses
	// which node cacheSpan actually finds something in.
	const spc = 133 // sizeclass 66, noscan (28672-byte elements)
	// Matches searchSpanSetForTest's fixed-size backing array (review M5).
	const searchLimit = 256

	base := growForNodeUntilHomed(t, spc, otherNode)
	runtime.MCentralPlaceForTest(spc, base)

	// reclaimPlaced searches for and frees the span placed above,
	// WITHOUT calling uncacheSpan on it again (MCentralFindForTest,
	// not MCentralUncacheAndFindForTest -- base is already sitting in
	// mcentral from the MCentralPlaceForTest call; re-uncacheSpan-ing
	// it would push a second, duplicate reference into the spanSet's
	// block list). Used on every exit path once base has been placed,
	// so nothing here can leak it (review C1) or corrupt mcentral by
	// freeing a span that's still linked into a spanSet.
	reclaimPlaced := func() {
		if runtime.MCentralFindForTest(spc, base, otherNode, searchLimit) {
			runtime.MCentralFreeSpanForTest(base)
		} else {
			t.Errorf("could not find and reclaim the span placed at %#x in node %d's set; may have leaked", base, otherNode)
		}
	}

	refillNode, genuine := runtime.NumaRefillNodeForTest()
	if !genuine {
		reclaimPlaced()
		t.Skip("numaRefillNode not genuine on this run")
	}

	before := readSpanRefillCounters(t)
	gotBase := runtime.MCentralCacheSpanForTest(spc)
	after := readSpanRefillCounters(t)

	if gotBase == base {
		// The common, expected case: cacheSpan found and returned
		// exactly the span this test placed, which cacheSpan's own
		// pop already removed from mcentral -- safe to free directly.
		runtime.MCentralFreeSpanForTest(gotBase)
	} else {
		// cacheSpan returned something else instead of the span placed
		// above -- reclaim the placed span (still presumably sitting
		// in mcentral untouched), and return whatever cacheSpan
		// actually gave us via MCentralReturnCacheSpanForTest, NOT an
		// unconditional MCentralFreeSpanForTest (re-review NEW-4): that
		// something else may be a genuinely pre-existing, real,
		// possibly still-in-use span cacheSpan legitimately routed to
		// this test from elsewhere in the process, and forcing it free
		// would be a use-after-free of live data. See
		// MCentralReturnCacheSpanForTest's doc comment
		// (export_numa_refill_test.go) for how it tells that case
		// apart from a genuinely fresh, safe-to-free span.
		reclaimPlaced()
		runtime.MCentralReturnCacheSpanForTest(spc, gotBase)
	}

	localDelta := after.local - before.local
	remoteDelta := after.remote - before.remote
	gotLocal := localDelta == 1 && remoteDelta == 0
	gotRemote := remoteDelta == 1 && localDelta == 0
	if !gotLocal && !gotRemote {
		t.Fatalf("span-refills counters moved unexpectedly for one refill: local delta=%d remote delta=%d", localDelta, remoteDelta)
	}

	wantLocal := runtime.NumaArenaNodeForTest(gotBase) == refillNode
	if wantLocal != gotLocal {
		t.Errorf("counter/home-node mismatch: numaArenaNode(span.base())==refillNode is %v, but the span-refills counter that moved says local=%v (span base=%#x home node=%d refill node=%d)",
			wantLocal, gotLocal, gotBase, runtime.NumaArenaNodeForTest(gotBase), refillNode)
	}
}

// growForNodeUntilHomed builds a fresh span for spanClass spc, homed
// to node (re-review NEW-3), via a single call to
// MCentralGrowAndSpanForTest, which grows AND claims the span's pages
// under one continuous mheap_.lock hold.
//
// This replaces three earlier, less reliable designs, the last two of
// which retried a growing npage in a loop (mirroring task 8's own
// growUntilNewArena) until some signal indicated "genuinely fresh"
// address space. The first checked whether the returned span's
// numaArenaNode matched node; the second (after switching to
// mheap.alloc and checking whether a new heapArena was registered,
// rather than trusting a possibly-coincidental node match) still went
// through mheap.alloc, whose own free-page reuse can satisfy even a
// large request from memory THIS TEST'S OWN earlier rejected attempts
// freed back (review C1 requires freeing rejected spans, not leaking
// them) -- self-contaminating and compounding across repeated
// invocation, observed as a 19/20 Skip rate under -count=20 on real
// hardware even with the new-heapArena check. The third switched to
// growUntilNewArena/mheap.grow directly (which never frees anything
// back to the general pool, so it can't self-contaminate the way
// mheap.alloc-based retries did) but used "a new heapArena got
// registered" as its signal for "safe to claim from" -- which turned
// out to be the wrong signal: heapArena registration (sysAlloc,
// rounded up to whole heapArenaBytes chunks) and page-allocator
// registration (mheap.grow's own h.pages.grow call, rounded only to
// the much smaller pallocChunkBytes) don't cover the same range
// whenever the two roundings don't coincide -- not only when the
// requested size itself isn't heapArenaBytes-aligned, but also
// whenever h.curArena[idx].base isn't heapArenaBytes-aligned at the
// point a new sysAlloc reservation is drawn (common after any
// contiguous in-place extension of an already-partially-consumed
// arena), since sysAlloc's own rounding is relative to that base, not
// a fixed grid. The highest-address newly-registered heapArena can sit
// in the resulting "reserved but not yet page-allocator-registered"
// slack, and claiming a span there
// hits "fatal error: index out of range" indexing pageAlloc's summary
// -- deterministic given the right size/alignment, not a race, and
// reproducible with no concurrent goroutine involved at all (first
// seen on real 2-node hardware, then reproduced locally under
// -shuffle=1 too, in this round). See MCentralGrowAndSpanForTest's
// doc comment (export_numa_refill_test.go) for the full mechanism and
// the fix: every successful mheap.grow call always registers a fresh,
// forward-only page-allocator range on its own, with no reuse
// possible from a direct mheap.grow call (unlike mheap.alloc) -- so
// there both is no self-contamination risk to retry away, and no
// "new arena" signal to wait for. One call is enough.
func growForNodeUntilHomed(t *testing.T, spc uint8, node int32) uintptr {
	t.Helper()
	// spanNPages is small and fixed -- it only needs to fit inside a
	// freshly-grown range (npage below is far larger), not to
	// out-grow anything, since that range is exclusively-owned,
	// never-before-touched address space.
	//
	// It must still be large enough that mspan.nelems ends up >= 2 for
	// every spanClass this file uses (135's and 133's elements are
	// 32768 and 28672 bytes -- the two largest size classes, chosen
	// for headroom in an earlier version of this test that no longer
	// needs it, but kept for their rarity: nothing else in this
	// process is likely to touch them). 16 pages (128 KiB) gives
	// nelems=4 for both. A too-small value here doesn't fail loudly:
	// nelems=1 with the one object nextFreeFast marks allocated makes
	// uncacheSpan correctly route the span to the *full* set (nelems -
	// allocCount == 0), which every test in this file only ever
	// searches the *partial* sets for -- "not found"/"could not
	// reclaim" failures that look like a routing bug but are actually
	// this parameter being wrong for the spanClass in use.
	const spanNPages = 16

	// npage no longer needs to grow across retries (see the doc
	// comment above) -- a single, fixed, generous size is enough.
	const npage = uintptr(1 << 14)
	base, grew := runtime.MCentralGrowAndSpanForTest(spc, node, npage, spanNPages)
	if !grew {
		t.Fatalf("MCentralGrowAndSpanForTest(node %d, npage %d) failed: mheap.grow itself returned false (real OOM?)", node, npage)
	}
	return base
}

type spanRefillCounts struct {
	local, remote uint64
}

func readSpanRefillCounters(t *testing.T) spanRefillCounts {
	t.Helper()
	samples := []metrics.Sample{
		{Name: "/numa/span-refills/local:spans"},
		{Name: "/numa/span-refills/remote:spans"},
	}
	metrics.Read(samples)
	for i := range samples {
		if samples[i].Value.Kind() == metrics.KindBad {
			t.Fatalf("metric %s not supported by this runtime", samples[i].Name)
		}
	}
	return spanRefillCounts{
		local:  samples[0].Value.Uint64(),
		remote: samples[1].Value.Uint64(),
	}
}

// allocChurnForRefill touches a spread of small-object size classes
// across several GC cycles so that mcache.refill -- and therefore
// mcentral.cacheSpan, the sole place span-refill routing happens --
// actually runs many times. Allocating only within an mcache's
// already-cached span never calls cacheSpan at all.
func allocChurnForRefill() {
	for round := 0; round < 6; round++ {
		keep := make([][]byte, 0, 6000)
		for i := 0; i < 6000; i++ {
			keep = append(keep, make([]byte, 8+(i%2048)))
		}
		runtime.GC()
		keep = nil
		_ = keep
	}
}
