// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package runtime

// numaGetCPUNode returns the id of the NUMA node the calling thread is
// currently running on, via the getcpu(2) syscall.
//
// This wrapper exists so that numa_linux.go -- built for every GOOS=linux
// architecture -- never references runtime.getcpu directly. getcpu only
// has an assembly implementation on amd64 and arm64 (see
// sys_linux_amd64.s, sys_linux_arm64.s). Without this split, a direct
// getcpu call from numaCurrentNode (reachable from schedinit's
// confinement decision in any ordinary GOEXPERIMENT=numa binary, not just
// tests) would fail to *link* on every other Linux architecture with
// "relocation target runtime.getcpu not defined" -- see
// numa_linux_getcpu_other.go for the fallback used there.
//
// ok is false if the getcpu syscall itself fails.
//
//go:nosplit
func numaGetCPUNode() (node uint32, ok bool) {
	var cpu uint32
	if r := getcpu(&cpu, &node); r != 0 {
		return 0, false
	}
	return node, true
}
