// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package runtime

// numaGetCPUNode returns the id of the NUMA node the calling thread is
// currently running on, via the getcpu(2) syscall.
//
// getcpu has an assembly implementation on every GOOS=linux architecture
// (see the getcpu wrapper in each sys_linux_*.s), so this file no longer
// needs an "_other.go" fallback complement the way it once did when
// getcpu was amd64/arm64-only -- numaGetCPUNode links unconditionally on
// every linux/GOARCH now. The wrapper itself is kept as a separate
// function (rather than inlining getcpu's call into numa_linux.go)
// purely so numa_linux.go, which is built for every GOOS=linux
// architecture, never has to reference runtime.getcpu directly.
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
