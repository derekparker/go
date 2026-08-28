// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package runtime

import (
	"internal/runtime/syscall/linux"
	"unsafe"
)

// numaHasSetAffinity reports whether this platform implements
// numaSetThreadAffinity. Confinement (fill-one-socket-first) engages
// only where it is true; elsewhere numaShouldConfine already fails via
// numaGetCPUNode, and this constant keeps the stand-down path honest.
//
// True on every linux GOARCH: SYS_SCHED_SETAFFINITY is
// defined for all 13 linux GOARCHes in internal/runtime/syscall/linux's
// per-arch defs_linux_*.go files, verified against each arch's
// src/syscall/zsysnum_linux_*.go.
const numaHasSetAffinity = true

// numaSetThreadAffinity sets the CPU affinity mask of thread tid
// (0 = the calling thread) to *mask. It reports whether the kernel
// accepted the mask. Errors are not distinguished: a false return
// simply means confinement (or a stand-down restore for one thread)
// did not take effect, which callers treat as stand-down.
//
//go:nosplit
func numaSetThreadAffinity(tid int32, mask *[numaCPUMaskBytes]byte) bool {
	_, _, errno := linux.Syscall6(linux.SYS_SCHED_SETAFFINITY,
		uintptr(tid), numaCPUMaskBytes, uintptr(unsafe.Pointer(&mask[0])), 0, 0, 0)
	return errno == 0
}
