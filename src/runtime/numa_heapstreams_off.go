// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.numa

package runtime

// numaMaxHeapNodes is 1 with the experiment off (I5): every
// numaMaxHeapNodes-sized array (mheap.arenaHints, mheap.curArena, and
// from task 9 on, per-node mcentral spanSets) collapses to a single
// element, which the Go spec guarantees has identical size and
// alignment to the bare (pre-array) field or type it replaced -- no BSS
// growth, no layout change. See numa_heapstreams_on.go for the on-build
// value and the reasoning.
const numaMaxHeapNodes = 1

// numaHeapArenaNodeBytes is the array length of heapArena.node
// (controller adjudication): 0 with the experiment off. A [0]uint8
// array has size 0 -- placed as a non-trailing heapArena field (before
// checkmarks, not after zeroedBase) so it also doesn't trigger the
// compiler's trailing-zero-size-field padding, this costs nothing:
// sizeof(heapArena) returns to its pre-task-8 value off-build. Access
// only via numaArenaNode/numaArenaSetNode, which compile to "return 0"
// / a no-op here (indexing a zero-length array is never reached, let
// alone valid, off-build).
const numaHeapArenaNodeBytes = 0

// heapArenaNode and setHeapArenaNode are heapArena.node's accessors on
// this build -- see numa_heapstreams_on.go's doc comment for why these
// are build-tag-split rather than a single runtime-guarded body.
// ha.node[0] would be a compile-time out-of-bounds error here
// (numaHeapArenaNodeBytes == 0), so these bodies never mention it.
func heapArenaNode(ha *heapArena) int32          { return 0 }
func setHeapArenaNode(ha *heapArena, node int32) {}
