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
