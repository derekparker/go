// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.numa

package runtime

// numaMaxHeapNodes is the number of per-node heap arena hint streams
// (mheap.arenaHints, mheap.curArena) and, from task 9 on, per-node
// mcentral spanSets. It must be build-tagged (I5): this constant
// multiplies static arrays, so an unconditional value would add BSS
// (mcentral alone is ~168 B x 136 span classes x 4 sets today; an
// unconditional x8 there is ~180 KB) to every Go binary, including ones
// built with the experiment off.
//
// 4 is a small, fixed stream count -- not sized to any particular
// machine's node count. The measured structural cost of the ON build's
// 1P allocation path is ~linear in this constant (~+0.5%/node over a
// +1.9% floor at 1), so it is deliberately the smallest capacity that
// covers the common 1-, 2- and 4-node deployments; larger boxes can
// raise it at build time. Real NUMA node ids at or beyond this bound do
// not get their own stream: I5 requires standing down to stream 0 for
// them rather than sharing via node % numaMaxHeapNodes, which would
// falsely suggest locality where none exists. See numaGrowNode and the
// placement decline in numaPlacementInit.
const numaMaxHeapNodes = 4

// numaHeapArenaNodeBytes is the array length of heapArena.node
// (controller adjudication): 1 on this build. See
// numa_heapstreams_off.go for the off-build value and the reasoning
// (a bare unconditional uint8 field cost 8 bytes off-build to
// alignment padding; a build-tagged zero-length array costs nothing).
const numaHeapArenaNodeBytes = 1

// heapArenaNode and setHeapArenaNode are heapArena.node's accessors on
// this build. Split into build-tagged files (rather than a single body
// gated by a runtime goexperiment.Numa check, as their callers
// numaArenaNode/numaArenaSetNode in mheap.go are) because
// numaHeapArenaNodeBytes is 0 off-build, making ha.node[0] a
// *compile-time* out-of-bounds error there even in unreachable code --
// seeing this file's source at all requires the goexperiment.numa
// build tag, which is exactly what guarantees numaHeapArenaNodeBytes
// is 1 here.
func heapArenaNode(ha *heapArena) int32 { return int32(ha.node[0]) }
func setHeapArenaNode(ha *heapArena, node int32) {
	ha.node[0] = uint8(node)
}
