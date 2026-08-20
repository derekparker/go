// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.numa

package runtime

// mNUMAState is empty when the experiment is off: m.numa (runtime2.go)
// then costs zero bytes, so sizeof(m) and every field's offset are
// byte-identical to a build with no NUMA field at all. See
// numa_mstate_on.go for the experiment-on definition and the C2 layout
// rule this split exists to satisfy.
type mNUMAState struct{}

func (s *mNUMAState) placementDone() bool { return false }
func (s *mNUMAState) setPlacementDone()   {}
