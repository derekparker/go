// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64) && goexperiment.numa

package runtime_test

import (
	"fmt"
	"internal/testenv"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
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
	// GOMAXPROCS=1 <= every node's CPU count: the subprocess must confine.
	// NOTE: testprog is built by buildTestProg with the inherited
	// environment; run via `make test-numa` so GOEXPERIMENT=numa applies
	// to the subprocess build too.
	got := runTestProg(t, "testprog", "NUMAPlacementInfo", "GOMAXPROCS=1")
	aff, mode := parsePlacement(t, got, "info")
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
	got := runTestProg(t, "testprog", "NUMAStandDown", "GOMAXPROCS=1")
	baff, bmode := parsePlacement(t, got, "before")
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
	got := runTestProg(t, "testprog", "NUMAStandDownDefaultGOMAXPROCS", "GOMAXPROCS=64")
	baff, bmode := parsePlacement(t, got, "before")
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
	if narrowed < gomaxprocs {
		t.Fatalf("narrowed=%d below gomaxprocs=%d (total=%d threads observed): soft affinity did not narrow every worker M to a single node; output %q",
			narrowed, gomaxprocs, total, got)
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
