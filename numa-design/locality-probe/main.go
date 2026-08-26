// locality-probe drives an unpinned parallel allocation workload at an
// in-process GOMAXPROCS (flag -procs) and reports the /numa/span-refills
// counter deltas over the workload window.
//
// GOMAXPROCS is set via runtime.GOMAXPROCS, NOT the environment, on
// purpose: an explicit env GOMAXPROCS <= one node's CPU count makes a
// GOEXPERIMENT=numa runtime CONFINE (fill-one-socket-first), which
// disables P-placement and would measure the wrong feature entirely --
// the exact confound v3's E1 sweep hit and fixed the same way. Started
// with no GOMAXPROCS in the environment, placement eligibility is
// decided at full width; the later in-process change keeps placement
// active and procresize re-partitions the P homes.
//
// Used by gate G2-locality (v4 plan): local share must be >= 90% at
// every probed width.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/metrics"
	"sync"
	"time"
)

func readCounters() (local, remote uint64, ok bool) {
	s := []metrics.Sample{
		{Name: "/numa/span-refills/local:spans"},
		{Name: "/numa/span-refills/remote:spans"},
	}
	metrics.Read(s)
	if s[0].Value.Kind() == metrics.KindBad || s[1].Value.Kind() == metrics.KindBad {
		return 0, 0, false
	}
	return s[0].Value.Uint64(), s[1].Value.Uint64(), true
}

func main() {
	procs := flag.Int("procs", runtime.GOMAXPROCS(0), "in-process GOMAXPROCS for the workload")
	secs := flag.Int("secs", 3, "workload duration in seconds")
	flag.Parse()
	if os.Getenv("GOMAXPROCS") != "" {
		fmt.Println("locality FATAL: GOMAXPROCS set in environment; that confines the process (see file comment)")
		os.Exit(1)
	}
	runtime.GOMAXPROCS(*procs)

	l0, r0, ok := readCounters()
	if !ok {
		fmt.Println("locality SKIP: /numa/span-refills metrics unsupported (experiment off?)")
		os.Exit(0)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < *procs; i++ {
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
				for sz := 16; sz <= 8192; sz *= 4 {
					sink = append(sink, make([]byte, sz))
				}
				if len(sink) >= 512 {
					sink = sink[:0]
				}
			}
		}()
	}
	time.Sleep(time.Duration(*secs) * time.Second)
	close(stop)
	wg.Wait()
	l1, r1, _ := readCounters()
	dl, dr := l1-l0, r1-r0
	share := 0.0
	if dl+dr > 0 {
		share = float64(dl) / float64(dl+dr) * 100
	}
	fmt.Printf("locality procs=%d local=%d remote=%d share=%.2f%%\n", *procs, dl, dr, share)
}
