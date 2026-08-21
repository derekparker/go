// Command routing-probe-unconfined is the corrected E1 harness (fixing
// the confinement confound the original E1 run had): GOMAXPROCS is set
// via runtime.GOMAXPROCS(n) AFTER process start, not via the GOMAXPROCS
// environment variable. WS-A's fill-one-socket confinement
// (numaShouldConfine) makes its one-shot decision at schedinit using
// sched.customGOMAXPROCS, which is only set true when GOMAXPROCS is
// explicit at process start (env var or equivalent) -- a post-startup
// runtime.GOMAXPROCS(n) call does not retroactively flip that decision
// (same mechanism Task 3's own SetDefaultGOMAXPROCS work documented).
// Leaving GOMAXPROCS unset at start means the process boots with the
// default (NumCPU()=256 on this machine, customGOMAXPROCS=false), so
// numaShouldConfine's own procs<=nodeCPUs check evaluates 256<=128 =
// false and confinement never engages, regardless of what -procs value
// is requested afterward. This isolates Workstream B's own three
// ingredients (homing + routing + soft affinity) from Workstream A's
// confinement, at any GOMAXPROCS the caller wants.
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
	procs := flag.Int("procs", 128, "GOMAXPROCS to request via runtime.GOMAXPROCS after startup")
	totalMB := flag.Int("totalmb", 2048, "total allocation volume across all workers, MB")
	flag.Parse()

	// Deliberately AFTER start: sched.customGOMAXPROCS was already
	// evaluated (false, since GOMAXPROCS was never set via env) by the
	// time this call runs.
	runtime.GOMAXPROCS(*procs)

	var wg sync.WaitGroup
	wg.Add(*procs)
	targetBytesPerWorker := (int64(*totalMB) * 1024 * 1024) / int64(*procs)
	sizes := []int{4096, 8192, 16384, 32768}
	for i := 0; i < *procs; i++ {
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
	fmt.Printf("procs=%d local=%d remote=%d total=%d local_share=%.4f%%\n",
		*procs, local, remote, total, share)
}
