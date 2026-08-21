// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Export guts for testing -- split out of export_numa_test.go.
//
// NumaGetCPUNodeForTest lives here, rather than in export_numa_test.go,
// so it can carry a wider build tag: it calls numaGetCPUNode directly,
// which now has a working getcpu(2) implementation on every GOOS=linux
// architecture (see numa_linux_getcpu.go and the getcpu wrapper in each
// sys_linux_*.s), not just amd64/arm64.
//
// It deliberately does NOT go through numaCurrentNode (numa_linux.go),
// even though that would be the more obviously-named wrapper:
// numaCurrentNode short-circuits to a hardcoded 0 without ever calling
// numaGetCPUNode whenever numaTopology.NumNodes < 2. That short-circuit
// makes perfect sense for numaCurrentNode's real caller (there is no
// other node to report on a single-node host), but it would make a test
// built on top of it vacuous on every single-node host -- including
// qemu/CI runners, which is most of the hardware available to actually
// execute TestNUMAGetcpu for the 11 architectures added alongside this
// file. Calling numaGetCPUNode directly exercises the real getcpu
// syscall unconditionally, single-node or not.
//
// export_numa_test.go's exports remain restricted to amd64/arm64
// because they depend on numaSetThreadAffinity, which does not have
// that broader implementation (see numa_linux_affinity.go /
// numa_linux_affinity_other.go and numaHasSetAffinity).
//
// This file keeps the same goexperiment.numa tag as export_numa_test.go
// and numa_linux_getcpu_test.go, for the same dead-code-elimination
// reason documented in export_numa_test.go: this is package runtime, so
// without the tag NumaGetCPUNodeForTest would give an experiment-off
// test binary a reachable call path into NUMA-only code.

//go:build linux && goexperiment.numa

package runtime

// NumaGetCPUNodeForTest returns -1 if the getcpu(2) syscall itself
// fails -- the same failure sentinel numaCurrentNode uses -- and the
// node id otherwise. Unlike numaCurrentNode, it always calls getcpu,
// even on a single-node host; see the file doc comment above.
func NumaGetCPUNodeForTest() int32 {
	node, ok := numaGetCPUNode()
	if !ok {
		return -1
	}
	return int32(node)
}
