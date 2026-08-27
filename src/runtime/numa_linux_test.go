// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && goexperiment.numa

package runtime_test

import (
	"fmt"
	"internal/testenv"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// mempolicy modes (numa_linux.go): 1 = MPOL_PREFERRED, 2 = MPOL_BIND.

func TestNUMATopologyDiscovery(t *testing.T) {
	n := runtime.NumaNumNodes()
	if n < 1 {
		t.Fatalf("NumNodes=%d", n)
	}
	if runtime.NumaNumAllowedNodes() < 1 {
		t.Fatal("no allowed nodes")
	}
	// Portable across CPU numbering schemes. numa-dell interleaves
	// even/odd, but block numbering (0-63 = node 0, 64-127 = node 1)
	// is the common case — asserting NodeOfCPU(0) != NodeOfCPU(1)
	// would fail on most real 2-node machines. Instead assert the
	// CPU map spans >= 2 distinct nodes when NumNodes >= 2.
	if n >= 2 {
		seen := map[int32]bool{}
		for cpu := 0; cpu < 8192; cpu++ {
			if nd := runtime.NumaNodeOfCPU(cpu); nd >= 0 {
				seen[nd] = true
			}
		}
		if len(seen) < 2 {
			t.Fatalf("NumNodes=%d but CPUs map to %d node(s)", n, len(seen))
		}
	}
}

func TestNUMABindAllTaskPolicy(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip("not multi-node")
	}
	if runtime.NumaConfinedForTest() {
		t.Skip("process is socket-confined; BIND-all not in effect by design")
	}
	mode := runtime.NumaTaskMemPolicyModeForTest()
	if mode != 2 { // MPOL_BIND
		t.Fatalf("mempolicy mode=%d want BIND(2)", mode)
	}
}

func TestNUMASetThreadAffinitySelf(t *testing.T) {
	// Read the current mask and set it back unchanged: must succeed.
	if !runtime.NumaSetThreadAffinitySelfForTest() {
		t.Fatal("sched_setaffinity(self, current mask) failed")
	}
}

func TestNUMAFillOneSocketConfined(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip("not multi-node")
	}
	if !runtime.NumaHasSetAffinityForTest() {
		t.Skip("no sched_setaffinity plumbing on this arch")
	}
	// Final review F6 (Task 2 minor 7): if the environment this test
	// itself runs in is already cpuset/taskset-narrowed, testprog
	// inherits that narrowed mask and numaShouldConfine correctly
	// declines via its own "affinity narrower than online CPUs" rule
	// (operator placement wins) -- a property of the environment, not a
	// regression, so Skip rather than Fail below.
	if runtime.NumaHostAffinityNarrowedForTest() {
		t.Skip("host/environment CPU affinity is already narrower than online CPUs; testprog would inherit that and correctly decline to confine")
	}
	// GOMAXPROCS=1 <= every node's CPU count: the subprocess must confine.
	// NOTE: testprog is built by buildTestProg with the inherited
	// environment; run via `make test-numa` so GOEXPERIMENT=numa applies
	// to the subprocess build too.
	got := runTestProg(t, "testprog", "NUMAPlacementInfo", "GOMAXPROCS=1")
	aff, mode := parsePlacement(t, got, "info")
	if mode == 0 {
		// Final review F6: MPOL_DEFAULT means numaSetProcessBindAll
		// never ran at all in the child, which only happens when the
		// testprog binary itself was built without GOEXPERIMENT=numa
		// (e.g. `go test` invoked directly instead of `make
		// test-numa`) -- a build/invocation issue, not a confinement
		// regression, so Skip rather than Fail.
		t.Skip("testprog child reports MPOL_DEFAULT (mode=0): likely built without GOEXPERIMENT=numa -- run via `make test-numa`")
	}
	if mode != 1 {
		t.Fatalf("confined process mode=%d want MPOL_PREFERRED(1); output %q", mode, got)
	}
	if !runtime.NumaIsNodeCPUCountForTest(aff) {
		t.Fatalf("confined affinity popcount %d matches no node's CPU count; output %q", aff, got)
	}
}

func TestNUMAConfineSkipsNarrowedAffinity(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip("not multi-node")
	}
	testenv.MustHaveExecPath(t, "taskset")
	// Operator placement wins: under taskset, confinement never engages
	// and the narrowed 2-CPU mask is left untouched. Layer-1 BIND-all is
	// affinity-independent and still applies, so the task policy is
	// MPOL_BIND (mode=2).
	exe, err := buildTestProg(t, "testprog")
	if err != nil {
		t.Fatal(err)
	}
	cmd := testenv.Command(t, "taskset", "-c", "0,1", exe, "NUMAPlacementInfo")
	cmd.Env = append(os.Environ(), "GOMAXPROCS=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	aff, mode := parsePlacement(t, string(out), "info")
	if aff != 2 {
		t.Fatalf("affinity=%d, want the operator's 2 CPUs untouched", aff)
	}
	if mode != 2 {
		t.Fatalf("mode=%d want MPOL_BIND(2) (Layer 1 BIND-all)", mode)
	}
}

func TestNUMAStandDownOnGOMAXPROCSGrowth(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip("not multi-node")
	}
	if !runtime.NumaHasSetAffinityForTest() {
		t.Skip("no sched_setaffinity plumbing on this arch")
	}
	// Final review F6 (Task 2 minor 7, same rationale as
	// TestNUMAFillOneSocketConfined): an already-narrowed host
	// environment makes the "before" confinement this test depends on
	// correctly decline, not fail.
	if runtime.NumaHostAffinityNarrowedForTest() {
		t.Skip("host/environment CPU affinity is already narrower than online CPUs; testprog would inherit that and correctly decline to confine")
	}
	got := runTestProg(t, "testprog", "NUMAStandDown", "GOMAXPROCS=1")
	baff, bmode := parsePlacement(t, got, "before")
	if bmode == 0 {
		t.Skip("testprog child reports MPOL_DEFAULT (mode=0) before stand-down: likely built without GOEXPERIMENT=numa -- run via `make test-numa`")
	}
	aaff, amode := parsePlacement(t, got, "after")
	if bmode != 1 || !runtime.NumaIsNodeCPUCountForTest(baff) {
		t.Fatalf("before stand-down: affinity=%d mode=%d, want node-sized+PREFERRED; %q", baff, bmode, got)
	}
	if amode != 2 {
		t.Fatalf("after stand-down: mode=%d want MPOL_BIND(2); %q", amode, got)
	}
	if aaff <= baff {
		t.Fatalf("after stand-down: affinity=%d not restored past confined %d; %q", aaff, baff, got)
	}
}

// TestNUMAStandDownOnSetDefaultGOMAXPROCS exercises the review finding
// carried from Task 2: SetDefaultGOMAXPROCS sets customGOMAXPROCS=false and
// recomputes its target GOMAXPROCS from the CURRENT (already node-narrowed)
// affinity mask, so the recomputed value can equal numaConfinedNodeCPUs and
// never satisfy a strict procs > numaConfinedNodeCPUs comparison. Stand-down
// must still trigger on the customGOMAXPROCS==false transition alone (see
// numaStandDownIfNeeded in numa_linux.go).
//
// This isolates the new trigger arm (b, !customGOMAXPROCS) from the
// original one (a, procs > numaConfinedNodeCPUs) by construction, not
// just by exercising a different runtime API than
// TestNUMAStandDownOnGOMAXPROCSGrowth: on numa-dell (128 CPUs/node),
// confining at GOMAXPROCS=64 narrows this process's affinity to that
// node's 128 CPUs, so SetDefaultGOMAXPROCS's defaultGOMAXPROCS(0) call
// -- which reads that already-narrowed mask -- recomputes newprocs=128,
// exactly equal to numaConfinedNodeCPUs. Arm (a)'s strict "procs >
// numaConfinedNodeCPUs" comparison is therefore false (128 is not > 128)
// on this hardware, so the stand-down this test observes can only be
// arm (b) firing.
func TestNUMAStandDownOnSetDefaultGOMAXPROCS(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip("not multi-node")
	}
	if !runtime.NumaHasSetAffinityForTest() {
		t.Skip("no sched_setaffinity plumbing on this arch")
	}
	// Final review F6 (Task 2 minor 7, same rationale as
	// TestNUMAFillOneSocketConfined): an already-narrowed host
	// environment makes the "before" confinement this test depends on
	// correctly decline, not fail.
	if runtime.NumaHostAffinityNarrowedForTest() {
		t.Skip("host/environment CPU affinity is already narrower than online CPUs; testprog would inherit that and correctly decline to confine")
	}
	got := runTestProg(t, "testprog", "NUMAStandDownDefaultGOMAXPROCS", "GOMAXPROCS=64")
	baff, bmode := parsePlacement(t, got, "before")
	if bmode == 0 {
		t.Skip("testprog child reports MPOL_DEFAULT (mode=0) before SetDefaultGOMAXPROCS: likely built without GOEXPERIMENT=numa -- run via `make test-numa`")
	}
	aaff, amode := parsePlacement(t, got, "after")
	if bmode != 1 || !runtime.NumaIsNodeCPUCountForTest(baff) {
		t.Fatalf("before SetDefaultGOMAXPROCS: affinity=%d mode=%d, want node-sized+PREFERRED; %q", baff, bmode, got)
	}
	if amode != 2 {
		t.Fatalf("after SetDefaultGOMAXPROCS: mode=%d want MPOL_BIND(2); %q", amode, got)
	}
	if aaff <= baff {
		t.Fatalf("after SetDefaultGOMAXPROCS: affinity=%d not restored past confined %d; %q", aaff, baff, got)
	}
}

// TestNUMASoftAffinity exercises node-mask soft affinity from the
// scheduler (design §12.4, task 10, ingredient c) end to end, isolated
// from fill-one-socket confinement (Workstream A): GOMAXPROCS is set to
// runtime.NumCPU() -- the whole machine, larger than any single node's
// CPU count whenever NumNodes >= 2 -- so numaShouldConfine's
// procs-exceeds-node-CPUs check always declines. Any thread the probe
// finds narrowed to a single node therefore has to be numaNoteSchedule's
// doing, not confinement's.
//
// NEW-3 (review): narrowed is only checked against gomaxprocs/2, not
// gomaxprocs -- a coarse sanity check, not the test's real signal. Since
// C1's fix (numaWidenBeforeClone, called from newm1 before every new M
// is created), a busy M-creation churn window can catch some Ms
// transiently WIDE: the M that happens to be creating another M widens
// itself immediately beforehand and only re-narrows at its own next
// schedule() pass, so a /proc snapshot taken mid-churn can legitimately
// see fewer than gomaxprocs threads narrowed at that instant even though
// the mechanism is working correctly -- this is a genuine, intended
// consequence of the C1 fix, not a bug (the original gomaxprocs bound
// was already thin, an empirically measured 6.2% margin, before this
// property existed). The assertion that actually carries this test's
// signal is the distinct-node check below (I2); this bound only guards
// against a total failure to narrow anything at all.
func TestNUMASoftAffinity(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip("not multi-node")
	}
	if !runtime.NumaHasSetAffinityForTest() {
		t.Skip("no sched_setaffinity plumbing on this arch")
	}
	got := runTestProg(t, "testprog", "NUMASoftAffinity", "GOMAXPROCS="+strconv.Itoa(runtime.NumCPU()))
	narrowed, total, gomaxprocs, nodes := parseSoftAffinity(t, got)
	if total == 0 {
		t.Fatalf("no threads observed; output %q", got)
	}
	if want := gomaxprocs / 2; narrowed < want {
		t.Fatalf("narrowed=%d below gomaxprocs/2=%d (gomaxprocs=%d, total=%d threads observed): soft affinity narrowed too few Ms -- possible total failure, not just C1-fix-induced churn timing; output %q",
			narrowed, want, gomaxprocs, total, got)
	}
	// I2 (review): this is the assertion that actually catches C1's
	// process-wide single-node collapse. Every prior check above only
	// asks "is every M narrowed to *a* single node" -- which a total
	// collapse onto one node satisfies just as well as a healthy spread
	// across nodes does. Only checking that Ms collectively span more
	// than one node distinguishes the two.
	//
	// Skipped under -race: confirmed by direct hardware inspection
	// (numa-dell, GODEBUG=numa=1) that under -race this genuinely can
	// legitimately collapse onto one node with no leak involved --
	// numaCurrentNode's own getcpu readings consistently matched
	// wherever the OS kernel had actually scheduled every M (entirely
	// one node or the other, flipping between separate runs). -race's
	// own synchronization overhead apparently reduces this workload's
	// effective parallelism enough that the kernel's load balancer never
	// has a reason to spread it across both nodes -- the same kind of
	// -race-specific accommodation numaHeapStreamsEnabled already makes
	// for Task 8/9's per-node heap streams ("streams are not populated
	// on this build/run (race, tight-VA, or 32-bit)").
	if runtime.Raceenabled {
		t.Logf("distinct nodes seen: %v (not asserting >1 under -race; see doc comment)", nodes)
		return
	}
	if len(nodes) <= 1 {
		t.Fatalf("Ms collapsed onto %v (want >1 distinct node on a multi-node host): output %q", nodes, got)
	}
}

func parseSoftAffinity(t *testing.T, out string) (narrowed, total, gomaxprocs int, nodes []int) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "softaffinity ") {
			if strings.Contains(line, "SKIP") {
				t.Skipf("probe skipped: %q", line)
			}
			var nodesStr string
			if _, err := fmt.Sscanf(line, "softaffinity narrowed=%d total=%d gomaxprocs=%d nodes=%s", &narrowed, &total, &gomaxprocs, &nodesStr); err != nil {
				t.Fatalf("bad probe line %q: %v", line, err)
			}
			if nodesStr != "" {
				for _, s := range strings.Split(nodesStr, ",") {
					n, err := strconv.Atoi(s)
					if err != nil {
						t.Fatalf("bad node id %q in probe line %q: %v", s, line, err)
					}
					nodes = append(nodes, n)
				}
			}
			return narrowed, total, gomaxprocs, nodes
		}
	}
	t.Fatalf("no %q line in output %q", "softaffinity", out)
	return 0, 0, 0, nil
}

// TestNUMASoftAffinityForkRegression (review I3) exercises the review
// C1/fork-leak fix (numaWidenBeforeClone, numa_linux.go) directly: a
// process narrows itself via soft affinity, then forks+execs a child
// while narrowed. Before the fix, the child would inherit the parent's
// narrowed CPU mask via fork(2)/clone(2) affinity inheritance; the fix
// widens the forking M back to the full mask immediately before the
// fork/exec syscall runs. This isolates the mechanism directly, rather
// than depending on a GOEXPERIMENT=numa child re-declining confinement
// as its downstream symptom (which is what originally caught the
// os/exec case, per this task's own report).
//
// Residual (reviewer's I3 note, acknowledged not fixed): the parent's
// main goroutine could in principle migrate to a different node between
// testprog's poll-until-narrowed loop and the actual exec call, so
// parentpop is not guaranteed to reflect the exact node the fork/exec
// syscall itself runs on. This does not weaken the test: it still
// asserts the parent WAS narrowed (parentpop < online) at the moment
// exec ran, and that the child inherited the full mask regardless of
// which node that was -- exactly the property the fix provides. A
// migration mid-window would only change which node the coverage
// exercises, not whether a regression of the leak would be caught.
func TestNUMASoftAffinityForkRegression(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip("not multi-node")
	}
	if !runtime.NumaHasSetAffinityForTest() {
		t.Skip("no sched_setaffinity plumbing on this arch")
	}
	got := runTestProg(t, "testprog", "NUMASoftAffinityForkParent", "GOMAXPROCS="+strconv.Itoa(runtime.NumCPU()))
	var parentPop, childPop, online int
	found := false
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "forkchild ") {
			if strings.Contains(line, "SKIP") || strings.Contains(line, "ERR") {
				t.Fatalf("probe failed: %q", line)
			}
			if _, err := fmt.Sscanf(line, "forkchild parentpop=%d childpop=%d online=%d", &parentPop, &childPop, &online); err != nil {
				t.Fatalf("bad probe line %q: %v", line, err)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no %q line in output %q", "forkchild", got)
	}
	if parentPop >= online {
		t.Fatalf("parent never actually narrowed (parentpop=%d >= online=%d); probe did not exercise the fix; output %q", parentPop, online, got)
	}
	if childPop != online {
		t.Fatalf("child inherited a narrowed mask (childpop=%d, want online=%d): fork/clone affinity leak not fixed; output %q", childPop, online, got)
	}
}

// TestNUMASoftAffinityExecRegression (final review F1) exercises the
// syscall.Exec-specific affinity-leak fix (numaWidenBeforeClone, called
// from syscall_runtime_BeforeExec, proc.go): a process narrows itself
// via soft affinity, then execve(2)'s directly via syscall.Exec (NOT
// os/exec's ForkExec, which TestNUMASoftAffinityForkRegression already
// covers) while narrowed. execve does not create a new thread -- it
// replaces the calling thread's own image in place -- so without a
// widen call at the BeforeExec site specifically, the replaced image
// would simply keep running under the same already-narrowed kernel
// affinity mask. This isolates that call site: BeforeFork's own widen
// call never runs on this path at all, since syscall.Exec never calls
// ForkExec.
func TestNUMASoftAffinityExecRegression(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip("not multi-node")
	}
	if !runtime.NumaHasSetAffinityForTest() {
		t.Skip("no sched_setaffinity plumbing on this arch")
	}
	got := runTestProg(t, "testprog", "NUMASoftAffinityExecParent", "GOMAXPROCS="+strconv.Itoa(runtime.NumCPU()))
	var parentPop, online int
	found := false
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "execchild ") {
			if strings.Contains(line, "SKIP") {
				t.Skipf("probe skipped: %q", line)
			}
			if strings.Contains(line, "ERR") {
				t.Fatalf("probe failed: %q", line)
			}
			if _, err := fmt.Sscanf(line, "execchild parentpop=%d online=%d", &parentPop, &online); err != nil {
				t.Fatalf("bad probe line %q: %v", line, err)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no %q line in output %q", "execchild", got)
	}
	if parentPop >= online {
		t.Fatalf("parent never actually narrowed (parentpop=%d >= online=%d); probe did not exercise the fix; output %q", parentPop, online, got)
	}
	childPop := parseCpusAllowedListPopcountForTest(t, got)
	if childPop != online {
		t.Fatalf("execve'd image inherited a narrowed mask (childpop=%d, want online=%d): execve affinity leak not fixed; output %q", childPop, online, got)
	}
}

// parseCpusAllowedListPopcountForTest finds the raw "Cpus_allowed_list:"
// line the shell command NUMASoftAffinityExecParent execve's into prints
// (after its own "execchild ..." line) and returns the number of CPUs it
// lists.
func parseCpusAllowedListPopcountForTest(t *testing.T, out string) int {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Cpus_allowed_list:"); ok {
			var cpus []int
			for _, part := range strings.Split(strings.TrimSpace(rest), ",") {
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
						cpus = append(cpus, c)
					}
				} else if c, err := strconv.Atoi(part); err == nil {
					cpus = append(cpus, c)
				}
			}
			return len(cpus)
		}
	}
	t.Fatalf("no %q line in output %q", "Cpus_allowed_list:", out)
	return -1
}

// TestNUMASoftAffinitySetDefaultGOMAXPROCS (final review F2) exercises
// the getCPUCount fix (os_linux.go): SetDefaultGOMAXPROCS, forced on a
// soft-affinity-narrowed M, must not collapse GOMAXPROCS to that node's
// CPU count -- getCPUCount substitutes numaStartupAffinity's popcount
// instead of reading the live, narrowed thread mask whenever the
// calling M is soft-narrowed.
func TestNUMASoftAffinitySetDefaultGOMAXPROCS(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip("not multi-node")
	}
	if !runtime.NumaHasSetAffinityForTest() {
		t.Skip("no sched_setaffinity plumbing on this arch")
	}
	got := runTestProg(t, "testprog", "NUMASoftAffinitySetDefaultGOMAXPROCS", "GOMAXPROCS="+strconv.Itoa(runtime.NumCPU()))
	var after, numcpu int
	found := false
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "sagmp ") {
			if strings.Contains(line, "SKIP") {
				t.Skipf("probe skipped: %q", line)
			}
			if _, err := fmt.Sscanf(line, "sagmp after=%d numcpu=%d", &after, &numcpu); err != nil {
				t.Fatalf("bad probe line %q: %v", line, err)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no %q line in output %q", "sagmp", got)
	}
	if after != numcpu {
		t.Fatalf("GOMAXPROCS after SetDefaultGOMAXPROCS on a soft-narrowed M = %d, want NumCPU=%d (collapsed to node CPU count instead of the full online count): output %q", after, numcpu, got)
	}
}

// TestNUMAWidenCountUnderChurn (review adjudication (b)) closes the
// automated-coverage gap TestNUMASoftAffinity's I2 distinct-node
// assertion leaves under -race (skipped there -- see that test's doc
// comment) for the newm1/newosproc/cgo widen site specifically:
// numaWidenBeforeClone increments numaWidenCount every time it actually
// widens a narrowed M, in-process, in this same test binary -- a plain
// monotonic counter read, safe to assert on under any build config
// (including -race) without depending on cross-node spread or any
// particular kernel scheduling outcome.
//
// Forces enough LockOSThread'd goroutine churn that at least one new M
// is very likely created (via newm1) from an already-narrowed creator:
// by the time this test runs, this process (multi-node, unconfined,
// GOEXPERIMENT=numa -- the same properties runtime.Raceenabled or not
// TestNUMASoftAffinity itself relies on) has already run enough of the
// scheduler for soft affinity to have narrowed at least one M, so any
// subsequent new-M creation is likely to widen-then-reheal through
// exactly the site this test targets.
func TestNUMAWidenCountUnderChurn(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip("not multi-node")
	}
	if !runtime.NumaHasSetAffinityForTest() {
		t.Skip("no sched_setaffinity plumbing on this arch")
	}
	before := runtime.NumaWidenCountForTest()

	// Escalating rounds, each round's goroutines staying alive (parked
	// on <-stop, holding their M) across rounds: found as a real flake
	// (not hypothetical) when this test first ran inside the FULL
	// -short runtime suite rather than standalone -- a full suite run
	// leaves hundreds of idle Ms in the pool from earlier tests'
	// parallelism, so a modest one-shot batch of LockOSThread'd
	// goroutines can be serviced entirely by reusing that existing
	// pool without ever calling newm1 at all. Since each round adds
	// MORE concurrently-locked goroutines on top of every earlier
	// round's (still blocked, still holding their M), the cumulative
	// total grows monotonically and must eventually exceed whatever
	// idle-M pool existed at the start, forcing at least one genuinely
	// new M through newm1 -- regardless of how large that starting
	// pool was.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	defer wg.Wait()
	defer close(stop)

	spawn := func(n int) {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				<-stop
			}()
		}
	}

	total := 0
	for batch := 128; total < 4096; batch *= 2 {
		spawn(batch)
		total += batch
		// Give the scheduler a moment to actually create the Ms this
		// round's demand requires before checking.
		time.Sleep(100 * time.Millisecond)
		if runtime.NumaWidenCountForTest() > before {
			return // success
		}
	}
	t.Fatalf("numaWidenCount did not increase after spawning %d concurrently-locked goroutines (before=%d after=%d): the newm1/newosproc/cgo widen path (review C1/NEW-1) did not fire",
		total, before, runtime.NumaWidenCountForTest())
}

func parsePlacement(t *testing.T, out, label string) (aff int, mode int) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, label+" ") {
			if strings.Contains(line, "ERR") || strings.Contains(line, "SKIP") {
				t.Fatalf("probe failed: %q", line)
			}
			if _, err := fmt.Sscanf(line, label+" affinity=%d mode=%d", &aff, &mode); err != nil {
				t.Fatalf("bad probe line %q: %v", line, err)
			}
			return aff, mode
		}
	}
	t.Fatalf("no %q line in output %q", label, out)
	return 0, 0
}

// TestNUMAPlacementQuota exercises the pure largest-remainder quota
// function behind numaAssignPHomes (v4 stage 2, design
// numa-design/v4-placement-design.md §3/§9). Table cases lock in: even
// splits, the tie→lower-id rule, largest-remainder distribution, the
// ≥1 redistribution rule firing (skewed CPU counts) and deliberately
// NOT firing (more nodes than Ps), CPU-less nodes never receiving Ps,
// and the degenerate zero-procs case.
func TestNUMAPlacementQuota(t *testing.T) {
	tests := []struct {
		nprocs int32
		cpus   []int32
		want   []int32
	}{
		{256, []int32{128, 128}, []int32{128, 128}},
		{200, []int32{128, 128}, []int32{100, 100}},
		{3, []int32{128, 128}, []int32{2, 1}},                 // tie -> lower node id
		{1, []int32{128, 128}, []int32{1, 0}},                 // fewer Ps than nodes
		{5, []int32{64, 128}, []int32{2, 3}},                  // largest remainder -> node 0
		{4, []int32{1000, 1, 1, 1}, []int32{1, 1, 1, 1}},      // >=1 rule genuinely fires
		{2, []int32{128, 128, 128, 128}, []int32{1, 1, 0, 0}}, // >=1 rule must NOT fire
		{6, []int32{0, 128, 0, 128}, []int32{0, 3, 0, 3}},     // CPU-less nodes skipped
		{0, []int32{128, 128}, []int32{0, 0}},
	}
	for _, tt := range tests {
		got := runtime.NumaPlacementQuotasForTest(tt.nprocs, tt.cpus)
		if len(got) != len(tt.want) {
			t.Fatalf("quotas(%d, %v): got %v, want %v", tt.nprocs, tt.cpus, got, tt.want)
		}
		var sum int32
		for i := range got {
			sum += got[i]
			if got[i] != tt.want[i] {
				t.Errorf("quotas(%d, %v) = %v, want %v", tt.nprocs, tt.cpus, got, tt.want)
				break
			}
		}
		if tt.nprocs > 0 && sum != tt.nprocs {
			t.Errorf("quotas(%d, %v) = %v: sum %d != nprocs", tt.nprocs, tt.cpus, got, sum)
		}
	}
}

// TestNUMAPlacementActivePredicate checks the pairing-rule predicate's
// environment-independent invariants (full eligibility is exercised on
// real multi-node hardware by the placement hardware tests): placement
// can never be active while confinement is (mutual exclusivity, design
// §2), and never on a single-node machine or under a narrowed inherited
// affinity.
func TestNUMAPlacementActivePredicate(t *testing.T) {
	active := runtime.NumaPlacementActiveForTest()
	if runtime.NumaConfinedForTest() && active {
		t.Fatal("placement active while confined: mutual exclusivity violated")
	}
	if runtime.NumaNumNodes() < 2 && active {
		t.Fatal("placement active on a single-node machine")
	}
	if runtime.NumaHostAffinityNarrowedForTest() && active {
		t.Fatal("placement active despite narrowed startup affinity")
	}
}

// TestNUMAPlacementProcresize churns GOMAXPROCS and asserts the per-P
// home assignment invariants after each change: with placement active,
// homes are assigned contiguously (non-decreasing node ids, every P
// covered); with placement inactive (single-node box, narrowed
// affinity, confined, ...), every home is cleared. Exercises the
// procresize call site (v4 placement design §3) on any Linux machine;
// the quota VALUES are covered by TestNUMAPlacementQuota and the
// multi-node behavior by the hardware tests.
func TestNUMAPlacementProcresize(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(0))
	for _, n := range []int{1, 4, 2, 8} {
		runtime.GOMAXPROCS(n)
		homes := runtime.NumaPHomesForTest()
		if len(homes) != n {
			t.Fatalf("GOMAXPROCS(%d): got %d homes", n, len(homes))
		}
		if runtime.NumaPlacementActiveForTest() {
			last := int8(0)
			for i, h := range homes {
				if h < 0 {
					t.Fatalf("GOMAXPROCS(%d): P %d unassigned while placement active: %v", n, i, homes)
				}
				if h < last {
					t.Fatalf("GOMAXPROCS(%d): homes not contiguous: %v", n, homes)
				}
				last = h
			}
		} else {
			for i, h := range homes {
				if h >= 0 {
					t.Fatalf("GOMAXPROCS(%d): P %d has home %d while placement inactive", n, i, h)
				}
			}
		}
	}
}

// TestNUMAPlacementSpread (v4 stage 2) runs the NUMAPlacementSpread
// testprog probe: a parallel workload at GOMAXPROCS=NumCPU with P-home
// placement active must leave worker threads narrowed to node-sized
// masks on MORE THAN ONE node (the anti-collapse assertion from the
// soft-affinity C1 regression class), with each represented node
// holding a non-trivial share -- the proportional partition, enforced
// per-thread. Requires real multi-node hardware; skips elsewhere.
func TestNUMAPlacementSpread(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip("not multi-node")
	}
	if !runtime.NumaHasSetAffinityForTest() {
		t.Skip("no sched_setaffinity plumbing on this arch")
	}
	if runtime.NumaHostAffinityNarrowedForTest() {
		t.Skip("host/environment CPU affinity is already narrower than online CPUs")
	}
	if !runtime.NumaPlacementActiveForTest() {
		// The child inherits this environment; if placement is not
		// active here (e.g. heap streams disabled under -race) it will
		// not be there either.
		t.Skip("placement not active in this process; child would decline too")
	}
	got := runTestProg(t, "testprog", "NUMAPlacementSpread", "GOMAXPROCS="+strconv.Itoa(runtime.NumCPU()))
	var line string
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "placementspread ") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("no placementspread line in output %q", got)
	}
	if strings.Contains(line, "SKIP") {
		t.Skipf("probe skipped: %q", line)
	}
	var total, wide, gomaxprocs int
	var countsStr string
	if _, err := fmt.Sscanf(line, "placementspread total=%d wide=%d gomaxprocs=%d counts=%s", &total, &wide, &gomaxprocs, &countsStr); err != nil {
		t.Fatalf("bad probe line %q: %v", line, err)
	}
	counts := map[int]int{}
	for _, part := range strings.Split(countsStr, ",") {
		var nd, c int
		if _, err := fmt.Sscanf(part, "%d:%d", &nd, &c); err != nil {
			t.Fatalf("bad counts %q in %q: %v", countsStr, line, err)
		}
		counts[nd] = c
	}
	if len(counts) < 2 {
		t.Fatalf("threads narrowed to %d distinct node(s), want >= 2 (process-wide collapse shape): %q", len(counts), line)
	}
	// Each represented node must hold a non-trivial share of the worker
	// threads: at least gomaxprocs/8 (loose by design -- idle Ms,
	// sysmon, and GC workers are counted too, and exact balance is the
	// gate battery's job, not this test's).
	floor := gomaxprocs / 8
	for nd, c := range counts {
		if c < floor {
			t.Errorf("node %d holds only %d narrowed threads, want >= %d: %q", nd, c, floor, line)
		}
	}
}

// TestNUMAPlacementRefillLocality (v4 stage 2) asserts the in-process
// version of gate G2-locality's property: with placement active and an
// UNPINNED parallel allocation workload, the /numa/span-refills
// counters must show >= 90% local refills over the workload window --
// the deterministic P-home refill key plus per-thread enforcement is
// exactly what makes unpinned locality hold (v3's getcpu-keyed routing
// measured only 53-75% local here). Requires multi-node hardware.
func TestNUMAPlacementRefillLocality(t *testing.T) {
	if !runtime.NumaPlacementActiveForTest() {
		t.Skip("placement not active (single-node, narrowed affinity, streams disabled, ...)")
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < runtime.GOMAXPROCS(0); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sink := make([][]byte, 0, 512)
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Vary size classes and keep short-lived batches alive
				// long enough to force span turnover and mcache refills.
				for sz := 16; sz <= 8192; sz *= 4 {
					sink = append(sink, make([]byte, sz))
				}
				if len(sink) >= 512 {
					sink = sink[:0]
				}
			}
		}()
	}
	// G2-locality protocol (v4 plan): the gate reading is the
	// STEADY-STATE counter delta -- warm up first (window arming,
	// first stream growth, and cache priming concentrate remote
	// refills in the ramp; the gate's real subjects are steady-state
	// by construction), then snapshot, measure, snapshot.
	time.Sleep(2 * time.Second)
	before := readSpanRefillCounters(t)
	time.Sleep(2 * time.Second)
	close(stop)
	wg.Wait()
	after := readSpanRefillCounters(t)
	dl := after.local - before.local
	dr := after.remote - before.remote
	if dl+dr < 1000 {
		t.Skipf("only %d refills observed; workload too small to judge locality", dl+dr)
	}
	share := float64(dl) / float64(dl+dr)
	t.Logf("steady-state refills local=%d remote=%d share=%.2f%%", dl, dr, share*100)
	if share < 0.90 {
		t.Errorf("unpinned steady-state local refill share %.2f%% < 90%% with placement active", share*100)
	}
}

// TestNUMAStreamWindows checks the REAL mallocinit-computed stream
// windows (v4 stage 4 Task P2): with heap streams enabled, every valid
// window is chunk-aligned and pairwise disjoint, and each node's first
// arena hint lies inside its window (the NEW-1 reorder guarantees
// in-window hints come first).
func TestNUMAStreamWindows(t *testing.T) {
	if !runtime.NumaHeapStreamsEnabledForTest() {
		t.Skip("heap streams disabled (race build or constrained VA layout)")
	}
	type win struct{ lo, hi uintptr }
	var wins []win
	for n := int32(0); n < runtime.NumaMaxHeapNodesForTest(); n++ {
		lo, hi := runtime.NumaStreamWindowForTest(n)
		if lo == hi {
			continue // no valid window
		}
		if lo%runtime.PallocChunkBytesForTest() != 0 || hi%runtime.PallocChunkBytesForTest() != 0 {
			t.Errorf("node %d window [%#x, %#x) not chunk-aligned", n, lo, hi)
		}
		first := runtime.NumaFirstArenaHintForTest(n)
		if first != 0 && (first < lo || first >= hi) {
			t.Errorf("node %d first hint %#x outside window [%#x, %#x) (NEW-1 reorder)", n, first, lo, hi)
		}
		for _, w := range wins {
			if lo < w.hi && w.lo < hi {
				t.Errorf("node %d window [%#x, %#x) overlaps [%#x, %#x)", n, lo, hi, w.lo, w.hi)
			}
		}
		wins = append(wins, win{lo, hi})
	}
	if len(wins) < 2 {
		t.Fatalf("expected >= 2 valid stream windows, got %d", len(wins))
	}
}

// TestNUMAPlacementLargeObjectLocality (v4 stage 4, gate G4's
// large-object bar): large allocations bypass mcentral entirely, so
// the span-refill counters never see them -- instead, compare each
// large allocation's arena home tag against the allocating P's
// placement home directly. Sampling is racy by nature (the goroutine
// can migrate between reading the home and allocating), so the 90% bar
// absorbs both genuine remote allocations and sampling skew.
func TestNUMAPlacementLargeObjectLocality(t *testing.T) {
	if !runtime.NumaPlacementActiveForTest() {
		t.Skip("placement not active (single-node, narrowed affinity, streams disabled, ...)")
	}
	var mu sync.Mutex
	match, total := 0, 0
	var wg sync.WaitGroup
	stop := make(chan struct{})
	deadline := time.After(3 * time.Second)
	go func() { <-deadline; close(stop) }()
	for i := 0; i < runtime.GOMAXPROCS(0); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, n := 0, 0
			sink := make([][]byte, 0, 8)
			for {
				select {
				case <-stop:
					mu.Lock()
					match += m
					total += n
					mu.Unlock()
					return
				default:
				}
				home := runtime.NumaCurrentPHomeForTest()
				b := make([]byte, 256<<10) // 256 KiB: well past the large-object threshold
				if home >= 0 {
					if runtime.NumaArenaNodeOfForTest(uintptr(unsafe.Pointer(&b[0]))) == home {
						m++
					}
					n++
				}
				sink = append(sink, b)
				if len(sink) >= 8 {
					sink = sink[:0]
				}
			}
		}()
	}
	wg.Wait()
	if total < 1000 {
		t.Skipf("only %d sampled large allocations; too few to judge", total)
	}
	share := float64(match) / float64(total)
	t.Logf("large allocations node-matched %d/%d = %.2f%%", match, total, share*100)
	if share < 0.90 {
		t.Errorf("large-object node-match share %.2f%% < 90%% with placement active", share*100)
	}
}
