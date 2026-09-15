// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.numa

package runtime

// mNUMAState is the per-M NUMA stand-down convergence state, embedded as
// m.numa (runtime2.go). With the experiment on it holds whether this M's
// affinity and task mempolicy have already converged to the default
// task policy since the process stood down from single-node
// confinement (see numaFixThreadPlacement in numa_linux.go).
type mNUMAState struct {
	restored bool
}

// placementDone reports whether this M's placement has already converged
// after stand-down (see numaFixThreadPlacement).
func (s *mNUMAState) placementDone() bool { return s.restored }

// setPlacementDone latches convergence; called only after every syscall in
// numaFixThreadPlacement's (or numaStandDownIfNeeded's) restore sequence
// has succeeded.
func (s *mNUMAState) setPlacementDone() { s.restored = true }
