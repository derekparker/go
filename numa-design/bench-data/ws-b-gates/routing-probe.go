// Command routing-probe is Task 11's pinned-first routing-proof
// harness (numa-design/2026-08-20-numa-v3-locality-plan.md Task 11
// Step 1; design §12.4). It is not part of the module tree -- built
// directly (`go build -o probe routing-probe.go`, no go.mod needed
// for a single-file main package), same technique the Workstream A
// gate battery used for its off-binary census canary
// (RESULTS.md "Gate 8").
//
// Workload: GOMAXPROCS goroutines each allocate-and-touch (write +
// read-back XOR checksum, forcing genuine DRAM read/write traffic,
// not allocate-and-drop) a stream of objects across four size
// classes (4/8/16/32 KiB) for a fixed total volume. Objects are not
// retained past one loop iteration, so live-set/RSS stays small
// regardless of total volume while still exercising heap growth
// (mheap.grow) and mcentral refill at a realistic rate.
//
// Run externally pinned (`numactl --cpunodebind=N`, no --membind --
// memory placement is exactly what this probe is measuring) so soft
// affinity (ingredient c) is moot: this isolates routing (ingredient
// b, mcentral refill) and homing (ingredient a, per-node arena
// streams) from thread-placement stability, per the plan's mandated
// validation order (design §12.4).
//
// At exit, reads /numa/span-refills/{local,remote}:spans via
// runtime/metrics and prints the local share.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"runtime"
	"runtime/metrics"
	"sync"
)

func main() {
	totalMB := flag.Int("totalmb", 2048, "total allocation volume across all workers, MB")
	flag.Parse()

	procs := runtime.GOMAXPROCS(0)
	var wg sync.WaitGroup
	wg.Add(procs)
	targetBytesPerWorker := (int64(*totalMB) * 1024 * 1024) / int64(procs)
	sizes := []int{4096, 8192, 16384, 32768}
	for i := 0; i < procs; i++ {
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			var written int64
			for written < targetBytesPerWorker {
				sz := sizes[r.Intn(len(sizes))]
				b := make([]byte, sz)
				for j := range b {
					b[j] = byte(j ^ int(seed))
				}
				var sum byte
				for _, v := range b {
					sum ^= v
				}
				runtime.KeepAlive(sum)
				written += int64(sz)
			}
		}(int64(i) + 1)
	}
	wg.Wait()

	samples := []metrics.Sample{
		{Name: "/numa/span-refills/local:spans"},
		{Name: "/numa/span-refills/remote:spans"},
	}
	metrics.Read(samples)
	local := samples[0].Value.Uint64()
	remote := samples[1].Value.Uint64()
	total := local + remote
	share := 0.0
	if total > 0 {
		share = float64(local) / float64(total) * 100
	}
	fmt.Printf("gomaxprocs=%d local=%d remote=%d total=%d local_share=%.4f%%\n",
		procs, local, remote, total, share)
}
