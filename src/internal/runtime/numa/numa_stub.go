// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !linux

package numa

// ReadTopology on platforms without a topology source reports a single
// node containing every CPU, so callers can treat "no NUMA support" the
// same as "one NUMA node" without a separate code path.
//
// scratch is unused on this platform but is accepted for API parity with
// the Linux implementation.
func ReadTopology(t *Topology, scratch []byte) error {
	t.NumNodes = 1
	t.NumAllowedNodes = 1
	t.TruncatedNodes = false
	t.Nodes[0] = Node{ID: 0}
	for i := range t.CPUToNode {
		t.CPUToNode[i] = 0
	}
	return nil
}
