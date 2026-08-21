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
// /numa/span-refills:local and :remote runtime/metrics counters (the
// in-vivo locality proxy the design calls for between perf-counter
// runs), and the routing itself must keep each node's spanSet
// node-pure (mcentral.uncacheSpan routes by the span's home node, not
// the freeing thread's node -- see its doc comment).
func TestNUMASpanRefillMetrics(t *testing.T) {
	t.Run("ChurnIncrementsCounters", testSpanRefillCountersIncrement)
	t.Run("OneProcLocalSanity", testSpanRefillOneProcLocalSanity)
	t.Run("NodePureRouting", testSpanRefillNodePureRouting)
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
// check: with GOMAXPROCS(1), a single thread cannot be on two NUMA
// nodes at once, so every refill it causes -- including any grow call
// that feeds its own future refills -- stays on whichever node that
// thread is running on, so a large majority of the refills it causes
// should be local (the brief's own "stays ~0" phrasing for the
// remote counter, not "never moves").
//
// This is deliberately a ratio bound, not exact equality: other NUMA
// tests earlier in the same test binary (e.g.
// TestNUMAHeapArenaStreams) legitimately register real, usable heap
// arenas explicitly homed to node 1 via task 8's node-bypass test
// hooks, to test homing without needing multi-node hardware. Once
// such an arena exists, it is genuinely part of the heap: ordinary
// allocation anywhere in the process (this test's own churn, or any
// other concurrently-running test) can legitimately land a span there
// through the normal global page allocator, and a later refill
// correctly counts finding it as "remote" -- this is uncacheSpan
// routing correctly by the span's real home node, not a bug. A
// single-threaded run therefore stays overwhelmingly local rather
// than perfectly local when run as part of the full suite.
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
	// Remote should be a small minority; a generous 20% bound keeps
	// this robust to the cross-test contamination described above
	// while still catching a routing regression that sends most or
	// all single-thread refills remote.
	if remoteDelta*5 > total {
		t.Errorf("GOMAXPROCS=1: remote refills were %d/%d (>20%%), want a small minority (a single thread mostly stays on its own node's memory)", remoteDelta, total)
	}
}

// testSpanRefillNodePureRouting is a white-box check of the routing
// ingredient's other required property (task 9 brief): a span
// returned via mcentral.uncacheSpan lands in the spanSet indexed by
// its OWN home node, not wherever the freeing thread happens to be
// running -- exactly the "node-pure set behavior where observable"
// this task calls for. This does not require real multi-node
// hardware: growUntilNewArena (numa_heapstreams_test.go, task 8) grows
// a brand new arena homed to an explicit node, bypassing getcpu
// entirely, and MCentralSpanAtForTest builds a minimal span at its
// base -- deliberately not going through mheap.alloc's normal entry
// path, whose free-page search would happily hand back memory from
// whichever node has leftover free pages lying around, regardless of
// which node this probe actually asked for (see
// MCentralSpanAtForTest's doc comment: an earlier version of this
// test used mheap.alloc directly and flaked exactly this way once
// other NUMA tests had already grown both nodes' streams earlier in
// the same test binary).
func testSpanRefillNodePureRouting(t *testing.T) {
	maxNodes := runtime.NumaMaxHeapNodesForTest()
	if maxNodes < 2 {
		t.Skip("numaMaxHeapNodes < 2: no distinct per-node sets to test (I5 collapse)")
	}
	if !runtime.NumaHeapStreamsEnabledForTest() {
		t.Skip("numaHeapStreamsEnabled is false: streams are not populated on this build/run (race, tight-VA, or 32-bit)")
	}

	// spanClass 65 is sizeclass 32, noscan (1024-byte elements -- see
	// internal/runtime/gc.SizeClassToSize): the routing logic under
	// test doesn't depend on which class it is; noscan avoids needing
	// to initialize a real heap pointer bitmap for a span this probe
	// never actually allocates objects from.
	const spc = 65
	const spanNPages = 4 // comfortably inside a fresh 64 MiB arena

	arena0 := growUntilNewArena(t, 0)
	base0 := runtime.MCentralSpanAtForTest(spc, arena0, spanNPages)
	arena1 := growUntilNewArena(t, 1)
	base1 := runtime.MCentralSpanAtForTest(spc, arena1, spanNPages)

	const searchLimit = 1 << 16

	// The node-0 span must be findable in node 0's partial-swept set...
	if !runtime.MCentralUncacheAndFindForTest(spc, base0, 0, searchLimit) {
		t.Errorf("span homed to node 0 not found in node 0's partial-swept set after uncacheSpan")
	}
	// ...and the node-1 span must be findable in node 1's, not node 0's
	// (the actual node-purity property: routing by home node, not by
	// whichever set we happen to look in).
	if !runtime.MCentralUncacheAndFindForTest(spc, base1, 1, searchLimit) {
		t.Errorf("span homed to node 1 not found in node 1's partial-swept set after uncacheSpan")
	}
}

type spanRefillCounts struct {
	local, remote uint64
}

func readSpanRefillCounters(t *testing.T) spanRefillCounts {
	t.Helper()
	samples := []metrics.Sample{
		{Name: "/numa/span-refills:local"},
		{Name: "/numa/span-refills:remote"},
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
