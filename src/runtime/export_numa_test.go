// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Export guts for testing.
//
// numaTopology and numaCurrentNode live in numa_linux.go, which is
// restricted to GOOS=linux by its filename. An unconstrained export file
// referencing them would break every non-Linux build of the runtime test
// archive, so this file carries its own //go:build linux tag.
//
// numaCurrentNode calls getcpu, which only has an assembly implementation
// for amd64 and arm64 (see sys_linux_amd64.s, sys_linux_arm64.s); other
// Linux architectures are out of scope for this layer. Without the
// (amd64 || arm64) restriction, `GOEXPERIMENT=numa go test runtime` fails
// to link on those architectures with "relocation target runtime.getcpu
// not defined", even though ordinary (non-test) binaries are unaffected
// because nothing outside this file calls numaCurrentNode.

//go:build linux && (amd64 || arm64)

package runtime

import (
	"internal/runtime/syscall/linux"
	"unsafe"
)

func NumaNumNodes() int32           { return numaTopology.NumNodes }
func NumaNumAllowedNodes() int32    { return numaTopology.NumAllowedNodes }
func NumaNodeOfCPU(cpu int) int32   { return numaTopology.NodeOfCPU(cpu) }
func NumaCurrentNodeForTest() int32 { return numaCurrentNode() }

// NumaTaskMemPolicyModeForTest returns this process's current task memory
// policy mode via get_mempolicy(2) (mode only, no MPOL_F_MEMS_ALLOWED, no
// nodemask), for TestNUMABindAllTaskPolicy to check against MPOL_BIND (2).
//
// The raw mode word can have MPOL_F_STATIC_NODES/MPOL_F_RELATIVE_NODES
// OR'd in by the kernel; those are masked out here so callers only ever
// see one of the MPOL_* mode values. Returns -1 if the get_mempolicy
// syscall itself fails, distinguishing a syscall error from a genuine
// MPOL_DEFAULT (mode 0) result -- the latter is the expected value on a
// single-node host, where numaSetProcessBindAll must never have called
// set_mempolicy at all.
func NumaTaskMemPolicyModeForTest() int32 {
	var mode int32
	_, _, errno := linux.Syscall6(linux.SYS_GET_MEMPOLICY, uintptr(unsafe.Pointer(&mode)), 0, 0, 0, 0, 0)
	if errno != 0 {
		return -1
	}
	return mode &^ _MPOL_MODE_FLAGS
}
