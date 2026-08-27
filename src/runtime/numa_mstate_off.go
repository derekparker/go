// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.numa

package runtime

// mNUMAState is empty when the experiment is off: m.numa (runtime2.go)
// then costs zero bytes at its (non-last) position in m, so sizeof(m)
// and every field's offset -- verified by test, not just inspection --
// are byte-identical to a build with no NUMA field at all. See
// numa_mstate_on.go for the experiment-on definition, and m.numa's own
// doc comment in runtime2.go for why the field is placed immediately
// before self rather than last (a zero-size field placed last would
// trigger the compiler's trailing-zero-size padding rule and grow
// sizeof(m) even here).
type mNUMAState struct{}

func (s *mNUMAState) placementDone() bool { return false }
func (s *mNUMAState) setPlacementDone()   {}

func (s *mNUMAState) softAffinityNode() (node int8, ok bool) { return 0, false }
func (s *mNUMAState) setSoftAffinityNode(node int8)          {}
func (s *mNUMAState) clearSoftAffinityNode()                 {}

func (s *mNUMAState) softAffinityCheckDue(now int64) bool { return false }
func (s *mNUMAState) armSoftAffinityCheck(deadline int64) {}

// homeStreakAdvance's real implementation (hysteresis for the placement
// hook) lives in numa_mstate_on.go; with the experiment off its only
// caller (numaNoteSchedule's placement path) is dead code, but
// numa_linux.go still compiles, so the method must exist.
func (s *mNUMAState) homeStreakAdvance(home int8) bool { return false }
