// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.numa

package runtime

// mNUMAState is the per-M NUMA stand-down convergence state, embedded as
// m.numa (runtime2.go). With the experiment on it holds whether this M's
// affinity and task mempolicy have already converged to the BIND-all
// task policy since the process stood down from fill-one-socket-first
// confinement (see numaFixThreadPlacement in numa_linux.go).
type mNUMAState struct {
	bindAllDone bool // this thread's placement converged after stand-down
}

// placementDone reports whether this M's placement has already converged
// after stand-down (see numaFixThreadPlacement).
func (s *mNUMAState) placementDone() bool { return s.bindAllDone }

// setPlacementDone latches convergence; called only after every syscall in
// numaFixThreadPlacement's (or numaStandDownIfNeeded's) restore sequence
// has succeeded.
func (s *mNUMAState) setPlacementDone() { s.bindAllDone = true }
