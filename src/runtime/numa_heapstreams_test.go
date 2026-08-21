// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.numa

package runtime_test

import (
	"runtime"
	"testing"
)

// TestNUMAHeapArenaStreams is task 8's red test (design §12.3): heap
// growth forced onto two different (simulated-pinned) NUMA nodes must
// land in arenas tagged with those nodes' ids, via the same
// h.arenas[l1][l2] two-load lookup spanOf already uses.
//
// This does not require real NUMA hardware: mheap.grow takes its target
// node as an explicit argument (getcpu happens once at the call site,
// never inside grow itself -- see numaGrowNode), so the test drives
// growth for two node ids directly through NumaHeapGrowForTest, the way
// two threads pinned to different nodes would each cause their own
// grow(npage, thatNode) call.
func TestNUMAHeapArenaStreams(t *testing.T) {
	maxNodes := runtime.NumaMaxHeapNodesForTest()
	if maxNodes < 2 {
		t.Skip("numaMaxHeapNodes < 2: no distinct per-node streams to test (I5 collapse)")
	}
	// numaMaxHeapNodes alone is a build-time constant and does not mean
	// mallocinit actually populated stream 1's hint chain (review
	// C1/C2): race builds, riscv64's 39-bit VMA layout, and 32-bit all
	// keep numaMaxHeapNodes at 8 while disabling per-node distribution
	// at run time. NumaHeapGrowForTest bypasses numaGrowNode's own
	// check by taking node directly, so this test must check it too, or
	// risk growing into a stream with no hints (the exact fatal
	// "too many address space collisions" this review round caught).
	if !runtime.NumaHeapStreamsEnabledForTest() {
		// Positive C1 coverage (review "RECOMMENDED"), not just an
		// absence-of-crash skip: assert numaGrowNode itself actually
		// reports the disabled state (stream 0, homed false) before
		// skipping, so a future change that re-enables streams under
		// -race (or any other numaHeapStreamsEnabled==false case)
		// without updating numaGrowNode to match gets caught here
		// instead of only surfacing as the C1 crash again.
		if stream, homed := runtime.NumaGrowNodeForTest(); stream != 0 || homed {
			t.Fatalf("numaHeapStreamsEnabled is false but numaGrowNode returned (stream=%d, homed=%v), want (0, false)", stream, homed)
		}
		t.Skip("numaHeapStreamsEnabled is false: streams are not populated on this build/run (race, tight-VA, or 32-bit)")
	}

	base0 := growUntilNewArena(t, 0)
	base1 := growUntilNewArena(t, 1)

	node0 := runtime.NumaArenaNodeForTest(base0)
	node1 := runtime.NumaArenaNodeForTest(base1)

	if node0 != 0 {
		t.Errorf("arena grown for node 0: numaArenaNode = %d, want 0", node0)
	}
	if node1 != 1 {
		t.Errorf("arena grown for node 1: numaArenaNode = %d, want 1", node1)
	}

	// The real design §12.3 property under test: per-node streams are
	// address-partitioned, hint addresses >= 1 TiB apart per node, not
	// merely "arenas that happen to compare unequal" (review M1 --
	// node0 == node1 is already dead once both node0 == 0 and node1 ==
	// 1 are individually asserted above; this checks the property those
	// two assertions don't).
	// uint64, not uintptr: this test only runs when numaHeapStreamsEnabled
	// (skipped above otherwise, which always excludes 32-bit -- see
	// numaHeapStreamsEnabled's doc comment), but the constant itself
	// must still compile on every arch this file builds for, and
	// uintptr(1)<<40 overflows a 32-bit uintptr at compile time.
	const oneTiB = uint64(1) << 40
	diff := uint64(base1) - uint64(base0)
	if base0 > base1 {
		diff = uint64(base0) - uint64(base1)
	}
	if diff < oneTiB {
		t.Fatalf("node 0 and node 1 arenas only %#x apart, want >= %#x (1 TiB, design §12.3)", diff, oneTiB)
	}
}

// growUntilNewArena grows node's stream, doubling the request each time
// mheap.grow succeeds without registering a new heap arena (i.e. it
// just extended existing headroom in that stream's current arena), and
// returns the base address of the first newly-registered arena.
//
// A fixed npage (review M2 asked for 1<<14, ~128 MiB) is not reliable
// here on its own: stream 0 in particular accumulates arbitrary
// headroom from every allocation earlier in the same test binary --
// none when this test runs in isolation, potentially much more as part
// of the full `go test -short runtime` suite, where many other tests
// have already grown stream 0 first. Node 1's stream never has this
// problem (nothing else in this binary touches it), but the same
// helper handles both for symmetry.
//
// grew and newArena are handled distinctly (review NEW-2): !grew means
// mheap.grow itself failed (a real OOM, or something similarly wrong)
// and is fatal immediately, with no retry -- retrying a genuine
// allocation failure by asking for even more memory only makes things
// worse. grew && !newArena just means this npage fit in headroom
// already mapped for this stream, which is expected and retried with a
// larger npage. The final Fatalf (if the cap is reached) reports the
// npage that was actually just attempted and failed to produce a new
// arena, not a doubled-but-never-tried value -- an earlier version of
// this loop checked the cap after doubling, so its failure message
// named a size this function had never actually asked mheap.grow for.
//
// Re-review NEW-3/task-8-helper-doc: the returned base is valid ONLY
// for heapArena-metadata lookups (as TestNUMAHeapArenaStreams uses it,
// via NumaArenaNodeForTest) -- see NumaHeapGrowForTest's doc comment
// (export_numa_heapstreams_test.go) for why it is NOT necessarily
// inside the page-allocator-registered range this same grow call
// extended.
func growUntilNewArena(t *testing.T, node int32) uintptr {
	t.Helper()
	npage := uintptr(1 << 14)
	const maxNpage = uintptr(1) << 20 // ~8 GiB at 8 KiB pages; generous cap
	for {
		base, grew, newArena := runtime.NumaHeapGrowForTest(npage, node)
		if !grew {
			t.Fatalf("NumaHeapGrowForTest(node %d, npage %d) failed: mheap.grow itself returned false (real OOM?)", node, npage)
		}
		if newArena {
			return base
		}
		if npage > maxNpage {
			t.Fatalf("NumaHeapGrowForTest(node %d) never registered a new heap arena, gave up after npage=%d", node, npage)
		}
		npage *= 2
	}
}
