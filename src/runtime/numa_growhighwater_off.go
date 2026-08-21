// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.numa

package runtime

// numaGrowHighWaterNodeUpdate is a no-op off-build -- see
// numa_growhighwater_on.go. Its call site in mheap.grow is also gated
// on goexperiment.Numa, so this body never runs with the experiment
// off; it exists purely so mheap.go, which is not build-tag-split
// itself, has something to call on every build (matching
// numaBindArena's own precedent in stubs_nonlinux.go).
func numaGrowHighWaterNodeUpdate(idx int32) {}

// numaGrowLoopBound is always 0 off-build (I5): numaMaxHeapNodes == 1
// there, so there is exactly one stream/node index (0), and no
// high-water-mark tracking is needed or exists.
func numaGrowLoopBound() int32 { return 0 }
