// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Export guts for testing -- split out of export_numa_test.go.
//
// NumaCurrentNodeForTest lives here, rather than in export_numa_test.go,
// so it can carry a wider build tag: it only calls through to
// numaCurrentNode -> numaGetCPUNode, which now has a working getcpu(2)
// implementation on every GOOS=linux architecture (see
// numa_linux_getcpu.go and the getcpu wrapper in each sys_linux_*.s), not
// just amd64/arm64. export_numa_test.go's exports remain restricted to
// amd64/arm64 because they depend on numaSetThreadAffinity, which does
// not have that broader implementation (see numa_linux_affinity.go /
// numa_linux_affinity_other.go and numaHasSetAffinity).
//
// This file keeps the same goexperiment.numa tag as export_numa_test.go
// and numa_linux_getcpu_test.go, for the same dead-code-elimination
// reason documented in export_numa_test.go: this is package runtime, so
// without the tag NumaCurrentNodeForTest would give an experiment-off
// test binary a reachable call path into NUMA-only code.

//go:build linux && goexperiment.numa

package runtime

func NumaCurrentNodeForTest() int32 { return numaCurrentNode() }
