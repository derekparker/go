// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

func init() {
	register("NUMAPlacementInfo", NUMAPlacementInfo)
	register("NUMAStandDown", NUMAStandDown)
	register("NUMAStandDownDefaultGOMAXPROCS", NUMAStandDownDefaultGOMAXPROCS)
	register("NUMASoftAffinity", NUMASoftAffinity)
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

// NUMAStandDownDefaultGOMAXPROCS confines at startup (small explicit
// GOMAXPROCS via env), then calls SetDefaultGOMAXPROCS and prints
// before/after placement. Unlike NUMAStandDown, this exercises the
// SetDefaultGOMAXPROCS reopened-feedback-loop scenario: SetDefaultGOMAXPROCS
// sets customGOMAXPROCS=false and recomputes its target GOMAXPROCS from the
// CURRENT (already node-narrowed) affinity mask, so the recomputed value
// can equal the confined node's CPU count and never satisfy a strict
// procs > numaConfinedNodeCPUs comparison. Stand-down must still trigger,
// on the customGOMAXPROCS==false transition alone.
func NUMAStandDownDefaultGOMAXPROCS() {
	runtime.LockOSThread()
	placementLine("before")
	runtime.SetDefaultGOMAXPROCS()
	placementLine("after")
}

// NUMASoftAffinity probes node-mask soft affinity (design §12.4, task
// 10): it spawns GOMAXPROCS CPU-bound, OS-thread-locked goroutines to
// force genuine concurrent use of every CPU the process is allowed to
// run on, runs them for a bounded duration (each yielding periodically
// via runtime.Gosched, so its M passes through schedule() -- where
// numaNoteSchedule's hook lives -- many times), then walks
// /proc/self/task/*/status and classifies each thread's
// Cpus_allowed_list against /sys/devices/system/node/nodeN/cpulist:
// "narrowed" if every CPU in the mask belongs to the same single NUMA
// node, unnarrowed otherwise (spans >1 node, e.g. a thread that never
// ran user code, such as sysmon).
//
// Run with GOMAXPROCS set by the test to a value LARGER than any single
// node's CPU count, so fill-one-socket confinement (Workstream A) never
// engages -- this isolates node-mask soft affinity specifically, the
// same way TestNUMAConfineSkipsNarrowedAffinity isolates confinement
// from Layer 1.
//
// Prints one line: "softaffinity narrowed=<N> total=<M> gomaxprocs=<P>".
func NUMASoftAffinity() {
	nodeOf := readNodeCPUMap()
	if len(nodeOf) == 0 {
		fmt.Println("softaffinity SKIP no /sys/devices/system/node data")
		return
	}

	n := runtime.GOMAXPROCS(0)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// CPU-bound work (forces real concurrent CPU use
				// across nodes) interleaved with allocation churn
				// and an explicit yield (forces a schedule() pass
				// on this M every iteration).
				buf := make([]byte, 4096)
				for j := range buf {
					buf[j] = byte(j)
				}
				sum := 0
				for j := 0; j < 200000; j++ {
					sum += j
				}
				_ = sum
				runtime.Gosched()
			}
		}()
	}
	time.Sleep(3 * time.Second)
	close(stop)
	wg.Wait()

	narrowed, total := probeThreadAffinity(nodeOf)
	fmt.Printf("softaffinity narrowed=%d total=%d gomaxprocs=%d\n", narrowed, total, n)
}

// readNodeCPUMap reads /sys/devices/system/node/node*/cpulist and
// returns a map from CPU id to the NUMA node id it belongs to.
func readNodeCPUMap() map[int]int {
	m := map[int]int{}
	entries, err := os.ReadDir("/sys/devices/system/node")
	if err != nil {
		return m
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "node") {
			continue
		}
		nodeID, err := strconv.Atoi(name[len("node"):])
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/sys/devices/system/node/" + name + "/cpulist")
		if err != nil {
			continue
		}
		for _, cpu := range parseCPUList(strings.TrimSpace(string(data))) {
			m[cpu] = nodeID
		}
	}
	return m
}

// parseCPUList parses a Linux CPU-list string (comma-separated ids and
// "lo-hi" ranges, the format both /sys/devices/system/node/*/cpulist and
// /proc/*/status's Cpus_allowed_list use) into a slice of CPU ids.
func parseCPUList(s string) []int {
	var out []int
	if s == "" {
		return out
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			loN, err1 := strconv.Atoi(lo)
			hiN, err2 := strconv.Atoi(hi)
			if err1 != nil || err2 != nil {
				continue
			}
			for c := loN; c <= hiN; c++ {
				out = append(out, c)
			}
		} else {
			c, err := strconv.Atoi(part)
			if err != nil {
				continue
			}
			out = append(out, c)
		}
	}
	return out
}

// probeThreadAffinity walks /proc/self/task/*/status and classifies each
// thread's Cpus_allowed_list: narrowed counts threads whose mask's CPUs
// all belong to a single NUMA node (per nodeOf); total counts every
// thread whose status could be read (a thread can exit between the
// directory listing and the read -- that one is simply skipped, not
// counted as unnarrowed).
func probeThreadAffinity(nodeOf map[int]int) (narrowed, total int) {
	entries, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		data, err := os.ReadFile("/proc/self/task/" + e.Name() + "/status")
		if err != nil {
			continue
		}
		var cpuList string
		for _, line := range strings.Split(string(data), "\n") {
			if rest, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
				cpuList = strings.TrimSpace(rest)
				break
			}
		}
		if cpuList == "" {
			continue
		}
		total++
		nodes := map[int]bool{}
		for _, cpu := range parseCPUList(cpuList) {
			if nd, ok := nodeOf[cpu]; ok {
				nodes[nd] = true
			}
		}
		if len(nodes) == 1 {
			narrowed++
		}
	}
	return narrowed, total
}
