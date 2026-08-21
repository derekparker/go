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
// 8 is a small, fixed stream count -- not sized to any particular
// machine's node count. Real NUMA node ids at or beyond this bound do
// not get their own stream: I5 requires standing down to stream 0 for
// them rather than sharing via node % numaMaxHeapNodes, which would
// falsely suggest locality where none exists. See numaGrowNode.
const numaMaxHeapNodes = 8
