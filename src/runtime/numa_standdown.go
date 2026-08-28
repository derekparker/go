// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "internal/runtime/atomic"

// numaStoodDown latches the one-way NUMA confinement stand-down: once
// true, the process never re-confines, and every M converges its own
// affinity + task mempolicy at its next park (numaFixThreadPlacement,
// numa_linux.go). Only ever set true on Linux (numaStandDownIfNeeded in
// numa_linux.go); stays permanently false on every other GOOS, where
// confinement itself never engages.
//
// Declared here rather than in numa_linux.go (which is restricted to
// GOOS=linux by its filename) so stopm's hook in proc.go -- compiled on
// every GOOS -- can inline numaStoodDown.Load() directly at the call
// site instead of always paying a full call into numaFixThreadPlacement
// just to check one bool. That matters because stopm runs on every M
// park: for the (overwhelmingly common) case where the process was
// never confined, or was confined but has not yet stood down, this
// keeps the steady-state cost to a single inlined atomic load with a
// not-taken branch.
var numaStoodDown atomic.Bool
