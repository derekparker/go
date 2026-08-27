// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.numa

package runtime

// pNUMAState is empty when the experiment is off: p.numa (runtime2.go)
// then costs zero bytes at its (non-last) position in p, so sizeof(p)
// and every field's offset are byte-identical to a build with no NUMA
// field at all. See numa_pstate_on.go for the experiment-on definition,
// and numa_mstate_off.go for the m-side original of this pattern --
// including why the field must never be placed last (a zero-size field
// placed last would force trailing padding and grow the struct).
type pNUMAState struct{}

func (s *pNUMAState) home() (node int8, ok bool) { return 0, false }
func (s *pNUMAState) setHome(node int8)          {}
func (s *pNUMAState) clearHome()                 {}
