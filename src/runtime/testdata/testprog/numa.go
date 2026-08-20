// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package main

import (
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

func init() {
	register("NUMAPlacementInfo", NUMAPlacementInfo)
	register("NUMAStandDown", NUMAStandDown)
}

// getMempolicySyscall: get_mempolicy(2) numbers differ per arch.
var getMempolicySyscall = map[string]uintptr{
	"amd64": 239,
	"arm64": 236,
}[runtime.GOARCH]

const mpolModeFlags = 0xe000 // MPOL_F_* flag bits get_mempolicy may OR into mode

func placementLine(label string) {
	var buf [1024]byte
	n, _, errno := syscall.RawSyscall(syscall.SYS_SCHED_GETAFFINITY,
		0, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		fmt.Printf("%s ERR getaffinity errno=%d\n", label, errno)
		return
	}
	pop := 0
	for _, b := range buf[:n] {
		for b != 0 {
			b &= b - 1
			pop++
		}
	}
	var mode int32
	if getMempolicySyscall == 0 {
		fmt.Printf("%s SKIP no get_mempolicy number for %s\n", label, runtime.GOARCH)
		return
	}
	_, _, errno = syscall.RawSyscall6(getMempolicySyscall,
		uintptr(unsafe.Pointer(&mode)), 0, 0, 0, 0, 0)
	if errno != 0 {
		fmt.Printf("%s ERR get_mempolicy errno=%d\n", label, errno)
		return
	}
	fmt.Printf("%s affinity=%d mode=%d\n", label, pop, mode&^mpolModeFlags)
}

// NUMAPlacementInfo prints one line: "info affinity=<popcount> mode=<mempolicy mode>".
// Run with GOMAXPROCS set by the test to steer the confinement decision.
func NUMAPlacementInfo() {
	placementLine("info")
}

// NUMAStandDown confines at startup (small GOMAXPROCS via env), then
// raises GOMAXPROCS past one node and prints before/after placement.
// LockOSThread keeps the observing goroutine on the M that executes the
// GOMAXPROCS stop-the-world, so "after" deterministically reflects the
// stand-down thread's restored policy. The timed GOMAXPROCS call bounds
// the stand-down STW plus the allm affinity walk; the gate records this
// (and compares against a stock-build run of the same probe) so the
// walk's stop-the-world cost is on the record.
func NUMAStandDown() {
	runtime.LockOSThread()
	placementLine("before")
	start := time.Now()
	runtime.GOMAXPROCS(runtime.NumCPU())
	fmt.Printf("standdown-gomaxprocs-wall-ns=%d\n", time.Since(start).Nanoseconds())
	placementLine("after")
}
