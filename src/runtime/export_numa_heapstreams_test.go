// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Export guts for testing per-node heap arena streams (design §12.3,
// task-8-brief.md).
//
// Unlike export_numa_test.go, this file carries no linux/(amd64||arm64)
// restriction: mheap.grow, heapArena.node, and numaArenaNode are
// OS-agnostic runtime code (mheap.go, malloc.go), not
// confinement/affinity code. mheap.grow accepts an explicit node
// argument rather than deriving it from getcpu internally, so these
// exports let TestNUMAHeapArenaStreams exercise per-node address
// partitioning on any GOOS/GOARCH the goexperiment.numa build supports,
// without needing real multi-node hardware.
//
// The goexperiment.numa tag matters for the same dead-code-elimination
// reason export_numa_test.go documents: this file is package runtime,
// so without the tag it would give the experiment-off test binary a
// reachable path that exercises the array-shaped (numaMaxHeapNodes > 1)
// side of mheap.grow, which must never be reachable when the experiment
// is off.

//go:build goexperiment.numa

package runtime

// NumaMaxHeapNodesForTest exports numaMaxHeapNodes.
func NumaMaxHeapNodesForTest() int32 { return int32(numaMaxHeapNodes) }

// NumaHeapStreamsEnabledForTest exports numaHeapStreamsEnabled (review
// C1/C2): whether mallocinit actually populated per-node arena hint
// chains beyond stream 0. numaMaxHeapNodes alone is not enough to tell
// -- it is a build-time constant (8 on this build) that stays >= 2 even
// when streams are disabled at run time (race builds, riscv64's 39-bit
// VMA layout, 32-bit): NumaHeapGrowForTest calls mheap.grow directly
// with an explicit node, bypassing numaGrowNode's own
// numaHeapStreamsEnabled check, so callers forcing a nonzero stream
// (e.g. TestNUMAHeapArenaStreams) must check this themselves or risk
// growing into a stream mallocinit left with an empty hint chain.
func NumaHeapStreamsEnabledForTest() bool { return numaHeapStreamsEnabled }

// NumaArenaNodeForTest exports numaArenaNode.
func NumaArenaNodeForTest(p uintptr) int32 { return numaArenaNode(p) }

// NumaHeapGrowForTest grows the heap via mheap.grow with an explicit
// node argument -- bypassing numaGrowNode's getcpu call, the same way a
// pinned allocation would land on a chosen node, but portable to any
// test host -- and returns the base address of a newly-registered heap
// arena from that growth (via h.heapArenas, which is append-only) along
// with whether growth succeeded and registered at least one new arena.
//
// Run on the system stack, matching every other export in this package
// that touches mheap_ directly under its lock (see
// CheckScavengedBitsCleared in export_test.go for the same pattern).
func NumaHeapGrowForTest(npage uintptr, node int32) (base uintptr, ok bool) {
	systemstack(func() {
		lock(&mheap_.lock)
		before := len(mheap_.heapArenas)
		_, grew := mheap_.grow(npage, node)
		if grew && len(mheap_.heapArenas) > before {
			ai := mheap_.heapArenas[len(mheap_.heapArenas)-1]
			base = arenaBase(ai)
			ok = true
		}
		unlock(&mheap_.lock)
	})
	return base, ok
}
