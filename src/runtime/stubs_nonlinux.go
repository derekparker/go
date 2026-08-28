// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !linux

package runtime

import "unsafe"

// sbrk0 returns the current process brk, or 0 if not implemented.
func sbrk0() uintptr {
	return 0
}

// numaSchedinit is a no-op on non-Linux platforms: NUMA topology discovery
// is Linux-only (see numa_linux.go).
func numaSchedinit() {
}

// numaBindArena is a no-op on non-Linux platforms: NUMA BIND-all mbind is
// Linux-only (see numa_linux.go). Its call site in mheap.grow is also
// gated on goexperiment.Numa, so this body never runs with the
// experiment off; it exists purely so mheap.go, which is not
// Linux-specific, has something to call on every GOOS.
//
//go:nosplit
func numaBindArena(addr unsafe.Pointer, size uintptr) {
}
