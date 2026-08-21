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
