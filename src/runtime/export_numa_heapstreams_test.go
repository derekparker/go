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

// NumaGrowNodeForTest exports numaGrowNode's (stream, homed) pair
// (review I1), for positive coverage of numaHeapStreamsEnabled's
// effect on it (review "RECOMMENDED"): when streams are disabled
// (race, tight-VA, or 32-bit), numaGrowNode must return (0, false),
// not merely "some caller happened to skip before reaching a nonzero
// stream." See TestNUMAHeapArenaStreams.
func NumaGrowNodeForTest() (stream int32, homed bool) { return numaGrowNode() }

// NumaHeapGrowForTest grows the heap via mheap.grow with an explicit
// node argument -- bypassing numaGrowNode's getcpu call, the same way a
// pinned allocation would land on a chosen node, but portable to any
// test host.
//
// grew and newArena are reported separately (review NEW-2): grew is
// mheap.grow's own success/failure result (false only means grow
// itself failed, e.g. a real OOM); newArena additionally reports
// whether that growth registered a *new* heapArena (via
// h.heapArenas, which is append-only) as opposed to just extending
// headroom in an already-registered one. base is only meaningful when
// newArena is true -- it is the newly-registered arena's base address,
// for callers (e.g. TestNUMAHeapArenaStreams) that need a pointer
// inside a freshly-tagged heapArena.node to look up. Conflating these
// into a single ok bool, as an earlier version of this export did,
// made "grow failed" and "grow succeeded but reused headroom" the same
// signal to callers, which is wrong for anything that needs to react
// differently to a genuine failure (see growUntilNewArena in
// numa_heapstreams_test.go).
//
// Re-review NEW-3/task-8-helper-doc: base (when newArena is true) is
// valid ONLY for heapArena-metadata lookups (numaArenaNode, spanOf,
// and similar) -- it is NOT necessarily inside the page-allocator-
// registered range this same grow call extended via h.pages.grow.
// heapArena registration (mheap.sysAlloc, rounded up to whole
// heapArenaBytes chunks) and page-allocator registration (h.pages.grow,
// rounded only to the much smaller pallocChunkBytes) cover different
// ranges whenever those two roundings don't coincide -- both when the
// requested npage itself isn't heapArenaBytes-aligned, and whenever
// h.curArena[idx].base isn't heapArenaBytes-aligned at the point a new
// sysAlloc reservation is drawn (common after any contiguous in-place
// extension of an already-partially-consumed arena). base, being the
// highest-address newly-registered heapArena, can land in the
// resulting "reserved but not yet page-allocator-registered" slack. A
// caller that needs an address inside the page-allocator-registered
// range itself (e.g. to claim pages via pageAlloc.allocRange) must not
// use this base for that -- see MCentralGrowAndSpanForTest
// (export_numa_refill_test.go), which instead reads
// h.curArena[idx].base immediately after its own grow call for exactly
// this reason.
//
// Run on the system stack, matching every other export in this package
// that touches mheap_ directly under its lock (see
// CheckScavengedBitsCleared in export_test.go for the same pattern).
func NumaHeapGrowForTest(npage uintptr, node int32) (base uintptr, grew, newArena bool) {
	systemstack(func() {
		lock(&mheap_.lock)
		before := len(mheap_.heapArenas)
		_, grew = mheap_.grow(npage, node)
		if grew && len(mheap_.heapArenas) > before {
			ai := mheap_.heapArenas[len(mheap_.heapArenas)-1]
			base = arenaBase(ai)
			newArena = true
		}
		unlock(&mheap_.lock)
	})
	return base, grew, newArena
}
