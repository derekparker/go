// phase-shift measures the "migration storm" pathology used to motivate
// GOEXPERIMENT=numa's BIND-all task mempolicy: allocate a stable
// pointer-dense working set, then chase it with pinned reader goroutines
// whose CPU affinity flips between NUMA nodes every phase. Each flip makes
// the entire working set "misplaced" from the kernel NUMA balancer's point
// of view; before migration can converge, the phase flips again. See
// numa-design/pathology-bench-design.md, candidate 3.
//
//   - Arm B (stock Go, unpinned): the kernel NUMA balancer chases the
//     perpetually-flipping working set, paying hint-fault + migration +
//     TLB-shootdown costs every phase — a migration storm that can never
//     converge.
//   - Arm C (GOEXPERIMENT=numa, unpinned): BIND-all suppresses the balancer
//     entirely. Readers are always ~50% remote (only the *readers* are
//     pinned; the memory is not), but pay no fault/copy/shootdown tax.
//   - Arm A (numactl --cpunodebind=0 --membind=0): cannot express node
//     inversion — its allowed cpuset contains no odd (node 1) CPUs, so the
//     odd-phase pin request is a no-op. Reported for context only, per
//     pathology-bench-design.md §3 candidate 3 ("Arm A is
//     informational-only... the bench must fall back to no-op pinning").
//
// Usage:
//
//	./phase-shift [-heap=6144] [-readers=64] [-phase=30s] [-phases=4]
//
// Prints one line to stdout when the run completes:
//
//	BenchmarkPhaseChase 1 <ns/read> ns/op
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	heapMB     = flag.Int("heap", 6144, "pointer-dense working set to allocate (MiB)")
	numReaders = flag.Int("readers", 64, "number of pinned reader goroutines")
	phaseSec   = flag.Float64("phase", 30.0, "duration of each phase (s)")
	numPhases  = flag.Int("phases", 4, "number of alternating-node phases")
	procs      = flag.Int("procs", 0, "GOMAXPROCS (0 = runtime default)")
	verbose    = flag.Bool("v", false, "log phase transitions and pin failures")
)

const ringCount = 4096

// node is the pointer-dense working-set element: one pointer plus padding
// to 64B, linked into a ring. Chasing .next forces a real memory read of
// every node's cache line — the "access" the NUMA balancer's fault
// accounting reacts to.
type node struct {
	next *node
	pad  [56]byte
}

func main() {
	flag.Parse()
	if *procs > 0 {
		runtime.GOMAXPROCS(*procs)
	}

	fmt.Fprintf(os.Stderr, "phase-shift GOMAXPROCS=%d heap=%dMiB readers=%d phase=%.0fs phases=%d\n",
		runtime.GOMAXPROCS(0), *heapMB, *numReaders, *phaseSec, *numPhases)

	allowed, err := schedGetaffinity()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sched_getaffinity: %v\n", err)
		os.Exit(1)
	}
	evenMask, evenN := parityMask(allowed, 0) // node 0 (even CPU numbers, per numa-dell's interleaved numbering)
	oddMask, oddN := parityMask(allowed, 1)   // node 1 (odd CPU numbers)
	fmt.Fprintf(os.Stderr, "allowed cpuset: %d even (node0) CPUs, %d odd (node1) CPUs\n", evenN, oddN)
	masks := [2]cpuSet{evenMask, oddMask}
	// Arm A's cpuset (numactl --cpunodebind=0) has zero odd CPUs: the
	// odd-phase pin request would fail with EINVAL (empty target set), so
	// fall back to leaving the OS-assigned affinity alone. This makes arm A
	// informational only — it cannot express node inversion.
	noopPin := evenN == 0 || oddN == 0

	fmt.Fprintf(os.Stderr, "Allocating %dMiB pointer-dense working set...\n", *heapMB)
	rings := allocRings(*heapMB << 20)

	ringsPerReader := ringCount / *numReaders
	if ringsPerReader < 1 {
		ringsPerReader = 1
	}
	readerRings := func(idx int) []*node {
		start := (idx * ringsPerReader) % ringCount
		end := start + ringsPerReader
		if end > ringCount {
			end = ringCount
		}
		if start >= end {
			return rings[:1]
		}
		return rings[start:end]
	}

	phase := new(atomic.Int32) // 0 = pin to node0 (even), 1 = pin to node1 (odd)
	done := make(chan struct{})
	var wg sync.WaitGroup
	counts := make([]uint64, *numReaders)

	const chunk = 1 << 14 // pointer hops between done/phase checks
	for i := 0; i < *numReaders; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			myRings := readerRings(idx)
			ringIdx := 0
			cur := myRings[ringIdx]
			lastPhase := int32(-1)
			var n uint64

			for {
				select {
				case <-done:
					counts[idx] = n
					return
				default:
				}

				if p := phase.Load(); p != lastPhase {
					if !noopPin {
						if err := schedSetaffinity(&masks[p]); err != nil && *verbose {
							fmt.Fprintf(os.Stderr, "reader %d: sched_setaffinity phase %d: %v\n", idx, p, err)
						}
					}
					lastPhase = p
				}

				for i := 0; i < chunk; i++ {
					cur = cur.next
					n++
					if cur == myRings[ringIdx] {
						ringIdx = (ringIdx + 1) % len(myRings)
						cur = myRings[ringIdx]
					}
				}
			}
		}(i)
	}

	fmt.Fprintf(os.Stderr, "Running %d phases x %.0fs (noop-pin=%v)...\n", *numPhases, *phaseSec, noopPin)
	t0 := time.Now()
	for p := 0; p < *numPhases; p++ {
		target := p % 2
		phase.Store(int32(target))
		if *verbose {
			fmt.Fprintf(os.Stderr, "  phase %d: pin readers to node%d\n", p, target)
		}
		time.Sleep(time.Duration(*phaseSec * float64(time.Second)))
	}
	elapsed := time.Since(t0)
	close(done)
	wg.Wait()

	var total uint64
	for _, c := range counts {
		total += c
	}
	nsPerRead := float64(elapsed.Nanoseconds()) / float64(total)

	fmt.Fprintf(os.Stderr, "\n=== Phase Chase Results ===\n")
	fmt.Fprintf(os.Stderr, "total reads=%d elapsed=%v ns/read=%.2f noop-pin(armA)=%v\n",
		total, elapsed, nsPerRead, noopPin)

	// nsPerRead is the aggregate cost across all *numReaders parallel
	// readers, so it is normally sub-nanosecond (e.g. ~1-2 ns/read with 64
	// readers) — print with decimal precision. Truncating to an integer
	// here previously collapsed every round to exactly 1 or 2, destroying
	// all variance and making the metric useless to benchstat.
	fmt.Fprintf(os.Stdout, "BenchmarkPhaseChase 1 %.4f ns/op\n", nsPerRead)
}

// allocRings allocates size bytes as node structs (64B each), linked into
// ringCount independent rings and returns the ring roots. The returned
// slice is the GC root that keeps the whole working set reachable and
// stable for the duration of the run — nothing is freed or reallocated
// between phases, so any migration activity vmstat records is entirely
// due to the readers' flipping CPU affinity, not allocation churn.
func allocRings(size int) []*node {
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
		prev.next = head // close the ring
		roots[r] = head
	}
	return roots
}

// --- CPU affinity (raw sched_setaffinity/sched_getaffinity syscalls; no
// external dependency needed beyond the standard library). ---

const (
	cpuSetBits  = 1024
	cpuSetWords = cpuSetBits / 64
)

type cpuSet [cpuSetWords]uint64

func (s *cpuSet) set(cpu int) { s[cpu/64] |= 1 << uint(cpu%64) }

func (s *cpuSet) isSet(cpu int) bool { return s[cpu/64]&(1<<uint(cpu%64)) != 0 }

// schedGetaffinity returns the calling thread's current CPU affinity mask.
func schedGetaffinity() (cpuSet, error) {
	var set cpuSet
	_, _, errno := syscall.RawSyscall(syscall.SYS_SCHED_GETAFFINITY, 0, uintptr(len(set)*8), uintptr(unsafe.Pointer(&set)))
	if errno != 0 {
		return set, errno
	}
	return set, nil
}

// schedSetaffinity pins the calling thread (pid=0 means "the calling
// thread" per sched_setaffinity(2)) to the given CPU set. Must be called
// after runtime.LockOSThread so it affects the OS thread actually running
// the reader goroutine.
func schedSetaffinity(set *cpuSet) error {
	_, _, errno := syscall.RawSyscall(syscall.SYS_SCHED_SETAFFINITY, 0, uintptr(len(set)*8), uintptr(unsafe.Pointer(set)))
	if errno != 0 {
		return errno
	}
	return nil
}

// parityMask returns the subset of allowed whose CPU numbers ≡ parity
// (mod 2), plus how many CPUs it contains. On numa-dell, even CPU numbers
// are node 0 and odd are node 1 (interleaved numbering).
func parityMask(allowed cpuSet, parity int) (cpuSet, int) {
	var m cpuSet
	n := 0
	for cpu := 0; cpu < cpuSetBits; cpu++ {
		if allowed.isSet(cpu) && cpu%2 == parity {
			m.set(cpu)
			n++
		}
	}
	return m, n
}
