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
	gcRuns  = flag.Int("n", 100, "number of GC cycles to measure")
	idleG   = flag.Int("idle", 100000, "idle goroutines (stack memory spread across heap)")
	stackG  = flag.Int("stacks", 100, "stack-growing goroutines (deep stacks for GC to scan)")
	verbose = flag.Bool("v", false, "print each individual pause")
	procs   = flag.Int("procs", 0, "GOMAXPROCS (0 = use runtime default)")
	jsonOut = flag.Bool("json", false, "emit one JSON summary object to stdout")
	label   = flag.String("label", "", "run label for JSON output (baseline|numa|membind)")
)

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

	fmt.Fprintf(os.Stderr, "gc-pause-bench GOMAXPROCS=%d heap=%dMiB warm=%.0fs n=%d idle=%d stacks=%d\n",
		runtime.GOMAXPROCS(0), *heapMB, *warmSec, *gcRuns, *idleG, *stackG)

	// Step 1: Allocate heap in 64MiB chunks.
	// Chunked allocation ensures mheap grows repeatedly, giving Stage 1's
	// round-robin mbind a chance to spread arenas across NUMA nodes.
	fmt.Fprintf(os.Stderr, "Allocating heap...\n")
	chunks := allocChunks(*heapMB << 20)

	// Step 2: Touch every page to commit physical memory.
	// First-touch policy: pages land on the NUMA node of the touching thread.
	// With GOEXPERIMENT=numa, mbind overrides this with round-robin placement.
	fmt.Fprintf(os.Stderr, "Committing pages (first-touch)...\n")
	touchAll(chunks)

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

	// Step 4: Background toucher — accesses all heap pages from whichever
	// OS threads the scheduler assigns. Since Stage 1 has no goroutine
	// affinity, threads run on both NUMA nodes; some accesses are cross-node.
	// The kernel NUMA balancer observes these cross-node accesses and marks
	// pages as migration candidates (PROT_NONE).
	var touchOps atomic.Int64
	stopTouch := make(chan struct{})
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stopTouch:
				return
			case <-t.C:
				touchAll(chunks)
				touchOps.Add(1)
			}
		}
	}()

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
	// Primary metric: STW time from MemStats.PauseNs after each runtime.GC().
	// Secondary metric: wall-clock time around runtime.GC().
	fmt.Fprintf(os.Stderr, "Measuring %d GC cycles...\n", *gcRuns)

	pauses := make([]time.Duration, 0, *gcRuns)
	stwPauses := make([]uint64, 0, *gcRuns)
	for i := 0; i < *gcRuns; i++ {
		t0 := time.Now()
		runtime.GC()
		pauses = append(pauses, time.Since(t0))

		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		idx := (int(ms.NumGC) - 1 + 256) % 256
		stwPauses = append(stwPauses, ms.PauseNs[idx])
	}

	close(stopTouch)
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

	out := os.Stdout
	if *jsonOut {
		out = os.Stderr
	}

	fmt.Fprintf(out, "\n=== GC Pause Results ===\n")
	fmt.Fprintf(out, "heap=%-6dMiB  n=%-4d  GOMAXPROCS=%-4d  idle-goroutines=%d  stack-goroutines=%d\n",
		*heapMB, *gcRuns, runtime.GOMAXPROCS(0), *idleG, *stackG)
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
		enc := json.NewEncoder(os.Stdout)
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

// touchAll reads one byte per page across all chunks.
// Called from both the setup phase and the background toucher goroutine.
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
