// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package runtime

import (
	"internal/runtime/syscall/linux"
	"unsafe"
)

// numaHasSetAffinity reports whether this platform implements
// numaSetThreadAffinity. Confinement (fill-one-socket-first) engages
// only where it is true; elsewhere numaShouldConfine already fails via
// numaGetCPUNode, and this constant keeps the stand-down path honest.
const numaHasSetAffinity = true

// numaSetThreadAffinity sets the CPU affinity mask of thread tid
// (0 = the calling thread) to *mask. It reports whether the kernel
// accepted the mask. Errors are not distinguished: a false return
// simply means confinement (or a stand-down restore for one thread)
// did not take effect, which callers treat as stand-down.
//
// nosplit: numaWidenBeforeClone (numa_linux.go) calls this from two
// sites -- syscall_runtime_BeforeFork (proc.go, the os/exec path) and
// newosproc (os_linux.go, this runtime's own new-M path). The
// BeforeFork site runs under the "no more allocation or calls of
// non-assembly functions" constraint syscall.forkAndExecInChild1
// imposes on everything between it and the fork/clone syscall -- so this
// function, and everything it calls (linux.Syscall6), must stay nosplit.
// The newosproc site has no such constraint of its own, but nosplit is
// a strictly more restrictive property, so it is safe to call from
// there too.
//
//go:nosplit
func numaSetThreadAffinity(tid int32, mask *[numaCPUMaskBytes]byte) bool {
	_, _, errno := linux.Syscall6(linux.SYS_SCHED_SETAFFINITY,
		uintptr(tid), numaCPUMaskBytes, uintptr(unsafe.Pointer(&mask[0])), 0, 0, 0)
	return errno == 0
}
