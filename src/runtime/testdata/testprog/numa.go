// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
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
	register("NUMASoftAffinityForkParent", NUMASoftAffinityForkParent)
	register("NUMASoftAffinityExecParent", NUMASoftAffinityExecParent)
	register("NUMASoftAffinitySetDefaultGOMAXPROCS", NUMASoftAffinitySetDefaultGOMAXPROCS)
	register("NUMAPlacementSpread", NUMAPlacementSpread)
}

// getMempolicySyscall: get_mempolicy(2)'s syscall number differs per
// arch. Rather than hand-maintain a second per-arch table alongside
// package syscall's own generated one (final review F3: an earlier
// version of this table only covered amd64/arm64, matching
// numa_linux_affinity.go's original build-tag scope; both are now
// linux-wide), use syscall.SYS_GET_MEMPOLICY directly -- it is
// generated for every linux GOARCH in src/syscall/zsysnum_linux_*.go.
var getMempolicySyscall = uintptr(syscall.SYS_GET_MEMPOLICY)

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
// run on, runs them for a bounded duration, then walks
// /proc/self/task/*/status and classifies each thread's
// Cpus_allowed_list against /sys/devices/system/node/nodeN/cpulist:
// "narrowed" if every CPU in the mask belongs to the same single NUMA
// node, unnarrowed otherwise (spans >1 node, e.g. a thread that never
// ran user code, such as sysmon).
//
// M1 (review): despite each goroutine calling runtime.Gosched() in a
// loop, that does NOT give numaNoteSchedule "many" chances to run per
// M, as an earlier version of this comment claimed. LockOSThread sets
// mp.lockedg synchronously (dolockOSThread), and schedule()'s very
// first check -- "if mp.lockedg != 0 { stoplockedm(); execute(...) }"
// -- takes a fast path that bypasses numaNoteSchedule entirely whenever
// that is true. So each spawned goroutine gets exactly ONE
// numaNoteSchedule opportunity: the schedule() pass that first runs it,
// BEFORE it reaches its own runtime.LockOSThread() call inside the
// goroutine body. Every later Gosched() in that goroutine's loop
// reschedules via the lockedg fast path and never touches the hook
// again. This is sufficient for what this probe checks (did each M get
// narrowed to *some* single node at least once) but does NOT exercise
// re-narrowing on a later migration -- and it is exactly why an earlier,
// less thorough version of this test could not have caught review C1
// (see I2 below): one narrowing opportunity per M, spread across
// GOMAXPROCS goroutines by wherever the OS scheduler happened to first
// run each one, still passes a check that only asks "is every M
// narrowed to *a* single node", never "do Ms collectively span more
// than one" -- which is exactly what C1's process-wide collapse onto
// one node would satisfy too.
//
// Run with GOMAXPROCS set by the test to a value LARGER than any single
// node's CPU count, so fill-one-socket confinement (Workstream A) never
// engages -- this isolates node-mask soft affinity specifically, the
// same way TestNUMAConfineSkipsNarrowedAffinity isolates confinement
// from Layer 1.
//
// Prints one line: "softaffinity narrowed=<N> total=<M> gomaxprocs=<P>
// nodes=<comma-separated distinct node ids seen>" (I2, review: the
// distinct-node list is the assertion that actually catches C1's
// single-node collapse -- a passing narrowed count alone does not).
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
				// and an explicit yield. See the M1 doc note above:
				// once this goroutine's own LockOSThread call above
				// has run, Gosched here no longer reaches
				// numaNoteSchedule -- it just keeps this M's CPU
				// genuinely busy so the OS scheduler's initial
				// placement (this M's one narrowing opportunity,
				// already past by this point) had real concurrent
				// load to spread across nodes.
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

	narrowed, total, nodes := probeThreadAffinity(nodeOf)
	nodeStrs := make([]string, len(nodes))
	for i, nd := range nodes {
		nodeStrs[i] = strconv.Itoa(nd)
	}
	fmt.Printf("softaffinity narrowed=%d total=%d gomaxprocs=%d nodes=%s\n", narrowed, total, n, strings.Join(nodeStrs, ","))
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
// counted as unnarrowed). distinctNodes is the sorted, de-duplicated
// list of every node id seen among narrowed threads -- I2 (review): an
// earlier version of this function computed this same per-thread `nodes`
// set and then threw it away, checking only len(nodes)==1 per thread;
// that is exactly why the original test could not distinguish "every M
// individually narrowed to *a* node, healthily spread across the
// machine" from review C1's "every M individually narrowed to *a*
// node -- the SAME one, process-wide collapse". Callers must check
// len(distinctNodes) > 1 on a multi-node host to actually catch that.
func probeThreadAffinity(nodeOf map[int]int) (narrowed, total int, distinctNodes []int) {
	seen := map[int]bool{}
	entries, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return 0, 0, nil
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
			for nd := range nodes {
				seen[nd] = true
			}
		}
	}
	for nd := range seen {
		distinctNodes = append(distinctNodes, nd)
	}
	sort.Ints(distinctNodes)
	return narrowed, total, distinctNodes
}

// NUMASoftAffinityForkParent probes the fork/clone affinity-leak fix
// directly (review I3, numaWidenBeforeClone in numa_linux.go): waits
// for its own (single, unlocked -- main's M is never LockOSThread'd, so
// every Gosched here does reach numaNoteSchedule, unlike NUMASoftAffinity's
// worker goroutines; see that function's M1 doc note) M to be
// soft-affinity-narrowed to some node (polls its own affinity popcount
// until it is below the online CPU count), then forks+execs a child
// while still narrowed, and reports the child's own affinity popcount.
//
// The child is deliberately NOT a re-invocation of this same Go binary.
// An earlier version of this probe did exactly that (re-exec'd as
// NUMASoftAffinityForkChild) and it is subtly wrong: a re-exec'd
// GOEXPERIMENT=numa child, having inherited GOMAXPROCS from this
// process's own environment, is itself eligible for soft affinity --
// its own main goroutine's first schedule() pass can narrow IT before
// its own probe code ever runs, since reaching any Go code at all
// requires going through the scheduler at least once. Under a plain
// (non-race) build this race usually resolved in the probe's favor
// (fast enough to read its own affinity before its own first
// numaNoteSchedule pass), which is why it passed originally -- but
// under -race (much slower per-instruction execution), the child's own
// self-narrowing reliably won the race instead, and the probe then
// reported a narrowed popcount that had nothing to do with any leaked
// mask from the parent. This is a test design flaw, not a bug in
// numaWidenBeforeClone (confirmed by direct hardware inspection: under
// -race, numaCurrentNode's own getcpu readings consistently matched
// wherever the OS kernel had genuinely scheduled every M -- entirely
// one node or the other, flipping between runs -- because -race's own
// synchronization overhead apparently never gives the kernel's load
// balancer a reason to spread work across both nodes, not because any
// mask was narrowed by inheritance).
//
// Using a plain shell command reading /proc/self/status instead avoids
// this confound entirely: a shell (and the coreutils it execs) has no
// Go scheduler, no soft affinity of its own, nothing to self-narrow --
// its Cpus_allowed_list is exactly whatever it inherited via fork(2)/
// clone(2) affinity inheritance, unmodified by anything after that.
// This isolates the fork/clone leak mechanism specifically, which is
// what I3 asks for.
//
// Prints one line: "forkchild parentpop=<N> childpop=<M> online=<P>".
func NUMASoftAffinityForkParent() {
	online := runtime.NumCPU()
	var parentPop int
	narrowed := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.Gosched()
		if pop, ok := ownAffinityPopcount(); ok && pop > 0 && pop < online {
			parentPop = pop
			narrowed = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !narrowed {
		fmt.Println("forkchild SKIP parent never narrowed")
		return
	}
	out, err := exec.Command("/bin/sh", "-c", "grep Cpus_allowed_list: /proc/self/status").CombinedOutput()
	if err != nil {
		fmt.Printf("forkchild ERR exec failed: %v: %s\n", err, out)
		return
	}
	childPop := parseCpusAllowedListPopcount(string(out))
	fmt.Printf("forkchild parentpop=%d childpop=%d online=%d\n", parentPop, childPop, online)
}

// NUMASoftAffinityExecParent probes the execve affinity-leak fix
// directly (final review F1, numaWidenBeforeClone called from
// syscall_runtime_BeforeExec in proc.go): waits for its own (single,
// unlocked -- see NUMASoftAffinityForkParent's doc comment for why this
// matters) M to be soft-affinity-narrowed to some node, then calls
// syscall.Exec directly -- the execve(2) path syscall.Exec uses, which
// replaces this process's own image in place and never goes through
// os/exec's ForkExec/syscall_runtime_BeforeFork at all -- while still
// narrowed, and reports the child image's own affinity popcount.
//
// Unlike NUMASoftAffinityForkParent (which forks a child and inherits
// the parent's mask via fork(2)/clone(2)), this exercises inheritance
// across execve specifically: execve does not create a new thread, so
// without the BeforeExec-site fix, the replaced process image would
// simply keep running under this same thread's already-narrowed kernel
// affinity mask.
//
// The replacement image is a plain shell (not a re-invocation of this
// binary), for the same reason NUMASoftAffinityForkParent's doc comment
// gives: a shell has no Go scheduler and no soft affinity of its own to
// confound the reading with self-narrowing.
//
// Prints "execchild parentpop=<N> online=<P>" BEFORE the exec call
// (nothing after it runs in this binary -- the process image is gone),
// then, if the exec succeeds, the shell command itself prints a raw
// "Cpus_allowed_list:\t..." line (parsed by
// parseCpusAllowedListPopcount, the same helper
// NUMASoftAffinityForkParent's caller test uses) to the same inherited
// stdout.
func NUMASoftAffinityExecParent() {
	online := runtime.NumCPU()
	var parentPop int
	narrowed := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.Gosched()
		if pop, ok := ownAffinityPopcount(); ok && pop > 0 && pop < online {
			parentPop = pop
			narrowed = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !narrowed {
		fmt.Println("execchild SKIP parent never narrowed")
		return
	}
	fmt.Printf("execchild parentpop=%d online=%d\n", parentPop, online)
	err := syscall.Exec("/bin/sh", []string{"/bin/sh", "-c", "grep Cpus_allowed_list: /proc/self/status"}, os.Environ())
	// Only reached if the exec itself failed to start; on success this
	// process image is replaced and nothing after Exec ever runs.
	fmt.Printf("execchild ERR exec failed: %v\n", err)
}

// NUMASoftAffinitySetDefaultGOMAXPROCS probes the SetDefaultGOMAXPROCS /
// soft-affinity interplay fix (final review F2, getCPUCount in
// os_linux.go): a soft-affinity-narrowed M's LIVE sched_getaffinity mask
// must not leak into defaultGOMAXPROCS's recompute when
// SetDefaultGOMAXPROCS forces one.
//
// Run with GOMAXPROCS = NumCPU() (via env, the same technique
// NUMASoftAffinity uses) so fill-one-socket confinement (Workstream A)
// never engages -- isolating node-mask soft affinity's own interplay
// with SetDefaultGOMAXPROCS specifically, the same way
// TestNUMAConfineSkipsNarrowedAffinity isolates confinement from Layer
// 1. Like NUMASoftAffinityForkParent, main's own M is never
// LockOSThread'd, so every Gosched here does reach numaNoteSchedule.
// Polls until this M's own affinity narrows to a single node, then
// calls runtime.SetDefaultGOMAXPROCS() on the very next line (no
// intervening scheduler point, so still the same M) and reports the
// resulting GOMAXPROCS. Before the fix, SetDefaultGOMAXPROCS's recompute
// reads this M's live, narrowed mask and collapses GOMAXPROCS to the
// node's CPU count; after the fix it reads numaStartupAffinity instead
// and recovers the full online count.
//
// Prints one line: "sagmp after=<GOMAXPROCS> numcpu=<online>".
func NUMASoftAffinitySetDefaultGOMAXPROCS() {
	online := runtime.NumCPU()
	narrowed := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.Gosched()
		if pop, ok := ownAffinityPopcount(); ok && pop > 0 && pop < online {
			narrowed = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !narrowed {
		fmt.Println("sagmp SKIP never narrowed")
		return
	}
	runtime.SetDefaultGOMAXPROCS()
	fmt.Printf("sagmp after=%d numcpu=%d\n", runtime.GOMAXPROCS(0), online)
}

// parseCpusAllowedListPopcount parses a single "Cpus_allowed_list:\t..."
// line (the format /proc/*/status uses) and returns the number of CPUs
// it lists, or -1 if the line could not be parsed.
func parseCpusAllowedListPopcount(out string) int {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Cpus_allowed_list:"); ok {
			return len(parseCPUList(strings.TrimSpace(rest)))
		}
	}
	return -1
}

// ownAffinityPopcount returns the popcount of the calling process's
// current CPU affinity mask (sched_getaffinity, self).
func ownAffinityPopcount() (int, bool) {
	var buf [1024]byte
	n, _, errno := syscall.RawSyscall(syscall.SYS_SCHED_GETAFFINITY,
		0, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return 0, false
	}
	pop := 0
	for _, b := range buf[:n] {
		for b != 0 {
			b &= b - 1
			pop++
		}
	}
	return pop, true
}

// NUMAPlacementSpread (v4 stage 2) drives a parallel CPU+allocation
// workload WITHOUT LockOSThread -- so worker Ms keep flowing through
// schedule() and converge to their P's assigned home node -- then
// classifies every thread's Cpus_allowed_list and reports per-node
// counts. The assertions live in TestNUMAPlacementSpread: with
// placement active, threads must be narrowed to node-sized masks on
// MORE THAN ONE node (the anti-collapse shape from the soft-affinity
// C1 regression), roughly tracking the proportional P partition.
func NUMAPlacementSpread() {
	nodeOf := readNodeCPUMap()
	if len(nodeOf) == 0 {
		fmt.Println("placementspread SKIP no /sys/devices/system/node data")
		return
	}
	n := runtime.GOMAXPROCS(0)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
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

	// Classify every readable thread: single-node mask -> that node's
	// count; anything wider -> wide.
	counts := map[int]int{}
	wide, total := 0, 0
	entries, err := os.ReadDir("/proc/self/task")
	if err != nil {
		fmt.Println("placementspread SKIP cannot read /proc/self/task")
		return
	}
	for _, e := range entries {
		data, err := os.ReadFile("/proc/self/task/" + e.Name() + "/status")
		if err != nil {
			continue // thread exited; skip
		}
		var cpus []int
		for _, line := range strings.Split(string(data), "\n") {
			if rest, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
				cpus = parseCPUList(strings.TrimSpace(rest))
			}
		}
		if len(cpus) == 0 {
			continue
		}
		total++
		node, single := -1, true
		for _, c := range cpus {
			nd, ok := nodeOf[c]
			if !ok {
				single = false
				break
			}
			if node == -1 {
				node = nd
			} else if nd != node {
				single = false
				break
			}
		}
		if single && node >= 0 {
			counts[node]++
		} else {
			wide++
		}
	}
	var nodes []int
	for nd := range counts {
		nodes = append(nodes, nd)
	}
	sort.Ints(nodes)
	parts := make([]string, len(nodes))
	for i, nd := range nodes {
		parts[i] = fmt.Sprintf("%d:%d", nd, counts[nd])
	}
	fmt.Printf("placementspread total=%d wide=%d gomaxprocs=%d counts=%s\n", total, wide, n, strings.Join(parts, ","))
}
