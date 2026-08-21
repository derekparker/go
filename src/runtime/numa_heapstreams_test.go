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

	const npage = 1 // any growth on a fresh stream registers a new arena

	base0, ok0 := runtime.NumaHeapGrowForTest(npage, 0)
	if !ok0 {
		t.Fatal("NumaHeapGrowForTest(node 0) did not register a new heap arena")
	}
	base1, ok1 := runtime.NumaHeapGrowForTest(npage, 1)
	if !ok1 {
		t.Fatal("NumaHeapGrowForTest(node 1) did not register a new heap arena")
	}

	node0 := runtime.NumaArenaNodeForTest(base0)
	node1 := runtime.NumaArenaNodeForTest(base1)

	if node0 != 0 {
		t.Errorf("arena grown for node 0: numaArenaNode = %d, want 0", node0)
	}
	if node1 != 1 {
		t.Errorf("arena grown for node 1: numaArenaNode = %d, want 1", node1)
	}
	if node0 == node1 {
		t.Fatalf("arenas grown for different nodes (0, 1) both report node %d -- streams are not address-partitioned", node0)
	}
}
