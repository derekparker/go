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
