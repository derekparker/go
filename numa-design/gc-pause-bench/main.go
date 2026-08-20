// gc-pause-bench measures GC stop-the-world pause times on NUMA hardware,
// specifically the page-migration pathology described in golang/go#14406.
//
// The Linux NUMA auto-balancer periodically marks heap pages as inaccessible
// (PROT_NONE) to track which NUMA node accesses them. During GC STW, the
// runtime scans all reachable memory; each scan of a PROT_NONE-marked page
// triggers a page fault that the kernel may handle by migrating the page.
// These migrations add latency to GC mark-termination pauses.
//
// With GOEXPERIMENT=numa (Stage 1), each heap arena gets an explicit
// MPOL_PREFERRED policy via mbind(2). The kernel sees that these pages have
// a preferred node and is less aggressive about migrating them, reducing
// the page-fault overhead during GC STW.
//
// Usage (run on the NUMA machine):
//
//	# Build and run baseline (no NUMA experiment):
//	go build -o bench-baseline .
//	./bench-baseline [-heap=4096] [-warm=10] [-n=100] [-idle=100000]
//
//	# Build and run with NUMA arena placement:
//	GOEXPERIMENT=numa go build -o bench-numa .
//	./bench-numa [-heap=4096] [-warm=10] [-n=100] [-idle=100000]
//
// The warm-up period (-warm) is critical: the kernel NUMA balancer needs
// time to run its scan cycle and mark pages before GC starts scanning them.
// On a lightly loaded machine, 5-10 seconds is usually sufficient.
//
// # Heavy profile (pathology-bench-design.md candidate 2)
//
// The default profile above uses a noscan [][]byte heap that GC's mark
// phase never reads, plus a 100ms background toucher that absorbs balancer
// hint faults before GC ever runs, plus back-to-back runtime.GC() calls
// that give the balancer no window to re-mark memory between cycles. On
// modern Go (post-1.8 concurrent stack scanning) that combination measures
// ~nothing: the fault bill is paid by the toucher, not by GC.
//
// The "heavy" flags below reconstruct the #14406 shape instead: a
// pointer-dense heap that mark must actually dereference (-ptrheap), no
// toucher stealing the faults (-toucher=false), an idle gap between forced
// GCs so the balancer has time to re-mark memory (-gcgap), and discarding
// early cycles while balancer state ramps up (-discard). Each measured
// cycle prints a `BenchmarkGCCycleWall 1 <ns> ns/op` line to stdout so the
// whole run's samples can be fed straight to benchstat.
//
//	FLAGS="-ptrheap=true -toucher=false -heap=4096 -idle=1000000 -stacks=200 -warm=45 -gcgap=5s -discard=2 -n=8"
//	GODEBUG=gcshrinkstackoff=1 ./bench-baseline $FLAGS >> B.out
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	heapMB  = flag.Int("heap", 4096, "heap size to allocate (MiB)")
	warmSec = flag.Float64("warm", 10.0, "warm-up duration (s) before measuring; lets NUMA balancer mark pages")
	gcRuns  = flag.Int("n", 100, "number of GC cycles to measure and report (after discarding -discard leading cycles)")
	idleG   = flag.Int("idle", 100000, "idle goroutines (stack memory spread across heap)")
	stackG  = flag.Int("stacks", 100, "stack-growing goroutines (deep stacks for GC to scan)")
	verbose = flag.Bool("v", false, "print each individual pause")
	procs   = flag.Int("procs", 0, "GOMAXPROCS (0 = use runtime default)")
	jsonOut = flag.Bool("json", false, "emit one JSON summary object to stdout")
	label   = flag.String("label", "", "run label for JSON output (baseline|numa|membind)")

	ptrHeap = flag.Bool("ptrheap", false, "use a pointer-dense heap (mark must dereference every node) instead of the legacy noscan [][]byte heap")
	toucher = flag.Bool("toucher", true, "run a background goroutine that touches every heap page every 100ms; disable to let GC (not the toucher) pay the balancer's fault bill")
	gcGap   = flag.Duration("gcgap", 0, "sleep between measured runtime.GC() calls, giving the NUMA balancer time to re-mark memory between cycles")
	discard = flag.Int("discard", 0, "number of leading GC cycles to run but exclude from stats/output (balancer-state ramp-up)")
)

// node is the pointer-dense heap element used by -ptrheap=true. 64 bytes:
// one pointer plus padding, so GC mark must dereference "next" in every
// 64B object it scans, forcing a read of every 4KiB page of the heap on
// every mark pass (see package doc "Heavy profile").
type node struct {
	next *node
	pad  [56]byte
}

type summary struct {
	Label      string `json:"label"`
	HeapMiB    int    `json:"heap_mib"`
	N          int    `json:"n"`
	GOMAXPROCS int    `json:"gomaxprocs"`
	STWp50Ns   int64  `json:"stw_p50_ns"`
	STWp95Ns   int64  `json:"stw_p95_ns"`
	STWp99Ns   int64  `json:"stw_p99_ns"`
	STWMaxNs   int64  `json:"stw_max_ns"`
	STWMeanNs  int64  `json:"stw_mean_ns"`
	WallP99Ns  int64  `json:"wall_p99_ns"`
}

func main() {
	flag.Parse()

	if *procs > 0 {
		runtime.GOMAXPROCS(*procs)
	}

	// Disable automatic GC; we drive it manually for precise measurement.
	debug.SetGCPercent(-1)

	fmt.Fprintf(os.Stderr, "gc-pause-bench GOMAXPROCS=%d heap=%dMiB warm=%.0fs n=%d idle=%d stacks=%d ptrheap=%v toucher=%v gcgap=%v discard=%d\n",
		runtime.GOMAXPROCS(0), *heapMB, *warmSec, *gcRuns, *idleG, *stackG, *ptrHeap, *toucher, *gcGap, *discard)

	// Step 1+2: allocate the heap and commit it (first-touch).
	//
	// Legacy mode: [][]byte chunks (noscan — GC's mark phase never reads
	// the contents, only the [][]byte header). Chunked allocation ensures
	// mheap grows repeatedly, giving Stage 1's round-robin mbind a chance
	// to spread arenas across NUMA nodes.
	//
	// Heavy mode (-ptrheap): a pointer-dense graph of *node linked into
	// 4096 independent rings rooted from a retained slice. Every node has
	// a pointer field, so GC mark must scan (read) every node on every
	// cycle; building the rings already touches (writes) every node, so
	// no separate touchAll pass is needed to commit pages.
	var chunks [][]byte
	var rings []*node
	if *ptrHeap {
		fmt.Fprintf(os.Stderr, "Allocating pointer-dense heap...\n")
		rings = allocPtrHeap(*heapMB << 20)
	} else {
		fmt.Fprintf(os.Stderr, "Allocating heap...\n")
		chunks = allocChunks(*heapMB << 20)
		fmt.Fprintf(os.Stderr, "Committing pages (first-touch)...\n")
		touchAll(chunks)
	}

	// Step 3: Spawn idle goroutines.
	// Each goroutine has a 2KB initial stack; 100K goroutines = ~200MB of
	// stack memory spread across the heap. GC must scan all live stacks
	// during mark, increasing STW exposure to NUMA page-fault latency.
	fmt.Fprintf(os.Stderr, "Spawning %d idle + %d stack-growing goroutines...\n", *idleG, *stackG)
	done := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < *idleG; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-done
		}()
	}

	// Stack-growing goroutines block on done but have deep stack frames.
	// These ensure GC mark has a large stack scan workload.
	for i := 0; i < *stackG; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sink := growStack(256) // ~256 * 512B = 128KB stack per goroutine
			<-done
			_ = sink
		}()
	}

	// Step 4: Background toucher (legacy default: on) — accesses all heap
	// pages from whichever OS threads the scheduler assigns. Since Stage 1
	// has no goroutine affinity, threads run on both NUMA nodes; some
	// accesses are cross-node. The kernel NUMA balancer observes these
	// cross-node accesses and marks pages as migration candidates
	// (PROT_NONE).
	//
	// In heavy mode (-toucher=false) this goroutine does not run at all:
	// the point is that GC mark, not a mutator-side toucher, is what pays
	// the balancer's fault bill (pathology-bench-design.md §2 defect 3).
	var touchOps atomic.Int64
	stopTouch := make(chan struct{})
	if *toucher {
		go func() {
			t := time.NewTicker(100 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stopTouch:
					return
				case <-t.C:
					touchHeap(chunks, rings)
					touchOps.Add(1)
				}
			}
		}()
	}

	// Step 5: Warm-up — let the NUMA balancer run its scan cycle.
	// The balancer runs roughly every 1s by default. After 10s, most heap
	// pages will have been marked PROT_NONE at least once.
	fmt.Fprintf(os.Stderr, "Warming up (%.0fs) — letting NUMA balancer mark pages...\n", *warmSec)
	warmDur := time.Duration(*warmSec * float64(time.Second))
	ticker := time.NewTicker(2 * time.Second)
	warmDeadline := time.Now().Add(warmDur)
	for time.Now().Before(warmDeadline) {
		select {
		case <-ticker.C:
			fmt.Fprintf(os.Stderr, "  %.0fs elapsed, %d touch passes\n",
				time.Since(warmDeadline.Add(-warmDur)).Seconds(), touchOps.Load())
		}
	}
	ticker.Stop()
	fmt.Fprintf(os.Stderr, "Warm-up done (%d touch passes).\n", touchOps.Load())

	// Step 6: Measure GC pause times.
	//
	// Primary (legacy) metric: STW time from MemStats.PauseNs after each
	// runtime.GC(). Secondary metric: wall-clock time around runtime.GC().
	//
	// Heavy-profile addition: run -discard extra leading cycles (excluded
	// from stats/output — balancer-state ramp-up) before the -n measured
	// cycles, and sleep -gcgap between every cycle (measured or discarded)
	// so task_numa_work has time to re-install PROT_NONE hints on a fresh
	// slice of the heap/stacks between GCs (pathology-bench-design.md §2
	// defect 4). Each measured cycle prints a benchstat-consumable
	// "BenchmarkGCCycleWall 1 <ns> ns/op" line to stdout as it completes.
	totalCycles := *discard + *gcRuns
	fmt.Fprintf(os.Stderr, "Measuring %d GC cycles (%d discarded, %d measured, gcgap=%v)...\n",
		totalCycles, *discard, *gcRuns, *gcGap)

	pauses := make([]time.Duration, 0, *gcRuns)
	stwPauses := make([]uint64, 0, *gcRuns)
	for i := 0; i < totalCycles; i++ {
		if i > 0 && *gcGap > 0 {
			time.Sleep(*gcGap)
		}

		t0 := time.Now()
		runtime.GC()
		wall := time.Since(t0)

		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		idx := (int(ms.NumGC) - 1 + 256) % 256
		stw := ms.PauseNs[idx]

		if i < *discard {
			fmt.Fprintf(os.Stderr, "  [discard %d/%d] wall=%v stw=%v\n", i+1, *discard, wall, time.Duration(stw))
			continue
		}

		pauses = append(pauses, wall)
		stwPauses = append(stwPauses, stw)
		fmt.Fprintf(os.Stdout, "BenchmarkGCCycleWall 1 %d ns/op\n", wall.Nanoseconds())
	}

	if *toucher {
		close(stopTouch)
	}
	close(done)
	wg.Wait()

	// Step 7: Report.
	sortedWall := make([]time.Duration, len(pauses))
	copy(sortedWall, pauses)
	sort.Slice(sortedWall, func(i, j int) bool { return sortedWall[i] < sortedWall[j] })

	sortedSTW := make([]uint64, len(stwPauses))
	copy(sortedSTW, stwPauses)
	sort.Slice(sortedSTW, func(i, j int) bool { return sortedSTW[i] < sortedSTW[j] })

	durPct := func(sorted []time.Duration, p float64) time.Duration {
		i := percentileIndex(len(sorted), p)
		return sorted[i]
	}
	u64Pct := func(sorted []uint64, p float64) uint64 {
		i := percentileIndex(len(sorted), p)
		return sorted[i]
	}

	var wallTotal time.Duration
	highCount := 0
	const highThresh = 5 * time.Millisecond
	for _, d := range pauses {
		wallTotal += d
		if d > highThresh {
			highCount++
		}
	}
	wallMean := wallTotal / time.Duration(len(pauses))

	var stwTotal uint64
	for _, ns := range stwPauses {
		stwTotal += ns
	}
	stwMean := stwTotal / uint64(len(stwPauses))

	out := os.Stderr

	fmt.Fprintf(out, "\n=== GC Pause Results ===\n")
	fmt.Fprintf(out, "heap=%-6dMiB  n=%-4d  GOMAXPROCS=%-4d  idle-goroutines=%d  stack-goroutines=%d  ptrheap=%v  toucher=%v  gcgap=%v  discard=%d\n",
		*heapMB, *gcRuns, runtime.GOMAXPROCS(0), *idleG, *stackG, *ptrHeap, *toucher, *gcGap, *discard)
	fmt.Fprintf(out, "\nWall-clock time per runtime.GC() call (includes concurrent phases):\n")
	fmt.Fprintf(out, "  p50=%-10v  p75=%-10v  p90=%-10v  p95=%-10v  p99=%-10v  max=%-10v\n",
		durPct(sortedWall, 50), durPct(sortedWall, 75), durPct(sortedWall, 90),
		durPct(sortedWall, 95), durPct(sortedWall, 99), sortedWall[len(sortedWall)-1])
	fmt.Fprintf(out, "  mean=%-10v  total=%-10v  pauses>%v: %d/%d (%.1f%%)\n",
		wallMean, wallTotal, highThresh, highCount, len(pauses), 100*float64(highCount)/float64(len(pauses)))

	fmt.Fprintf(out, "\nRuntime STW-only time (from MemStats.PauseNs, excludes concurrent GC):\n")
	fmt.Fprintf(out, "  p50=%-10v  p95=%-10v  p99=%-10v  max=%-10v  mean=%-10v\n",
		time.Duration(u64Pct(sortedSTW, 50)), time.Duration(u64Pct(sortedSTW, 95)),
		time.Duration(u64Pct(sortedSTW, 99)), time.Duration(sortedSTW[len(sortedSTW)-1]),
		time.Duration(stwMean))

	if *verbose {
		fmt.Fprintf(out, "\nIndividual pause times:\n")
		for i, p := range pauses {
			marker := "   "
			if p > highThresh {
				marker = ">>>"
			}
			fmt.Fprintf(out, "  %s [%3d] wall=%v stw=%v\n", marker, i, p, time.Duration(stwPauses[i]))
		}
	}

	fmt.Fprintf(out, "\nNotes:\n")
	fmt.Fprintf(out, "  - Build with GOEXPERIMENT=numa to enable mbind(MPOL_PREFERRED) on arenas.\n")
	fmt.Fprintf(out, "  - Ensure kernel.numa_balancing=1 (check: cat /proc/sys/kernel/numa_balancing).\n")
	fmt.Fprintf(out, "  - Run on a machine with 2+ NUMA nodes for meaningful results.\n")
	fmt.Fprintf(out, "  - Larger -heap and longer -warm show more pronounced differences.\n")

	if *jsonOut {
		s := summary{
			Label:      *label,
			HeapMiB:    *heapMB,
			N:          *gcRuns,
			GOMAXPROCS: runtime.GOMAXPROCS(0),
			STWp50Ns:   int64(u64Pct(sortedSTW, 50)),
			STWp95Ns:   int64(u64Pct(sortedSTW, 95)),
			STWp99Ns:   int64(u64Pct(sortedSTW, 99)),
			STWMaxNs:   int64(sortedSTW[len(sortedSTW)-1]),
			STWMeanNs:  int64(stwMean),
			WallP99Ns:  int64(durPct(sortedWall, 99)),
		}
		// JSON summary goes on stderr, alongside the diagnostic text above:
		// stdout is reserved for the per-cycle BenchmarkGCCycleWall lines
		// so a run's stdout can be fed directly to benchstat.
		enc := json.NewEncoder(os.Stderr)
		if err := enc.Encode(&s); err != nil {
			fmt.Fprintf(os.Stderr, "json encode: %v\n", err)
			os.Exit(1)
		}
	}
}

func percentileIndex(n int, p float64) int {
	i := int(math.Ceil(p/100.0*float64(n))) - 1
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

// allocChunks allocates size bytes as a slice of 64MiB chunks.
// Chunked allocation triggers repeated mheap.grow calls so Stage 1's
// round-robin mbind distributes arenas across NUMA nodes.
func allocChunks(size int) [][]byte {
	const chunkSize = 64 << 20
	n := size / chunkSize
	if n < 1 {
		n = 1
	}
	out := make([][]byte, n)
	for i := range out {
		out[i] = make([]byte, chunkSize)
	}
	return out
}

// allocPtrHeap allocates size bytes as node structs (64B each), linked into
// 4096 independent rings and returned as a slice of ring roots. The
// returned slice is the GC root that keeps everything reachable; every
// node is scanned by mark on every GC cycle because *node fields are
// pointers (unlike allocChunks' noscan [][]byte).
func allocPtrHeap(size int) []*node {
	const ringCount = 4096
	n := size / 64
	if n < ringCount {
		n = ringCount
	}
	perRing := n / ringCount

	roots := make([]*node, ringCount)
	for r := 0; r < ringCount; r++ {
		var head, prev *node
		for i := 0; i < perRing; i++ {
			nd := &node{}
			if head == nil {
				head = nd
			} else {
				prev.next = nd
			}
			prev = nd
		}
		// Close the ring so mark can't stop early at a nil tail.
		prev.next = head
		roots[r] = head
	}
	return roots
}

// touchHeap reads one byte per page across the legacy [][]byte heap, or
// walks one full lap of every ring in the pointer-dense heap, depending on
// which mode allocated the heap. Called from the initial commit step and
// (legacy mode only, or if -toucher=true is forced with -ptrheap) the
// background toucher goroutine.
func touchHeap(chunks [][]byte, rings []*node) {
	if rings != nil {
		for _, root := range rings {
			for cur := root.next; cur != root; cur = cur.next {
			}
		}
		return
	}
	touchAll(chunks)
}

// touchAll reads one byte per page across all chunks.
func touchAll(chunks [][]byte) {
	for _, c := range chunks {
		for i := 0; i < len(c); i += 4096 {
			_ = c[i]
		}
	}
}

// growStack recursively allocates stack frames to force a large stack.
// The stack must be alive during GC so the collector scans it.
//
//go:noinline
func growStack(depth int) uintptr {
	if depth == 0 {
		var leaf [512]byte
		return uintptr(leaf[0])
	}
	var frame [512]byte
	return uintptr(frame[0]) + growStack(depth-1)
}
