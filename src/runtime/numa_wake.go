// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// M wake-latency instrumentation for adaptive enforcement stand-down
// (v4 Task A5; design: numa-design/v4-a5-adaptive-enforcement-design.md).
// This file is the calibration phase (design review H2): counters and a
// GODEBUG=numa=2 per-window diagnostic only -- the trip/response logic
// lands after the thresholds are frozen from measured data.
//
// No build tag: every function here is referenced only from call sites
// behind the compile-time goexperiment.Numa constant, so the off binary
// carries none of it (zero-function-diff census), and the per-M stamp
// state lives in mNUMAState (zero-size off).

import (
	"internal/runtime/atomic"
)

var (
	// numaWakeEWMA is an EWMA of M wake latency in ns (alpha = 1/8:
	// ewma += (delta - ewma) >> 3), folded by each M as it wakes in
	// mPark; numaWakeCount counts folded wakes. Plain atomics --
	// concurrent folds can lose updates under a thundering herd, and
	// that is fine: the consumer (sysmon, per >=100ms window) needs
	// order-of-magnitude readings, not exact ones.
	numaWakeEWMA  atomic.Int64
	numaWakeCount atomic.Uint64

	// Calibration-diagnostic window state, touched only by sysmon.
	numaWakeLastEval  int64
	numaWakeLastCount uint64
)

// numaStampMWake records the wake time on mp just before its
// notewakeup(&mp.park). Called (behind goexperiment.Numa) at the three
// production scheduler wake sites -- startTheWorldWithSema, startm,
// startlockedm; see the design's site inventory (review M1) for why
// checkdead's faketime site is deliberately unstamped, which the
// zero-stamp invariant in numaNoteMWake makes safe.
func numaStampMWake(mp *m) {
	mp.numa.stampWake(nanotime())
}

// numaNoteMWake folds this M's just-measured wake latency, called
// (behind goexperiment.Numa) from mPark after noteclear. The stamp is
// read-and-clear, and a zero stamp NEVER folds (review M2, invariant):
// wakes from unstamped sites -- the runtime rwmutex parks on the same
// m.park note (review M3), faketime, any future site -- must not turn
// into a nanotime()-since-boot "latency" that saturates the EWMA.
//
// mPark is nosplit-adjacent; this is one vDSO clock read and two global
// atomic ops, on a path that just returned from a futex sleep.
func numaNoteMWake(mp *m) {
	stamp := mp.numa.takeWakeStamp()
	if stamp == 0 {
		return
	}
	delta := nanotime() - stamp
	if delta < 0 {
		return
	}
	ewma := numaWakeEWMA.Load()
	numaWakeEWMA.Store(ewma + (delta-ewma)>>3)
	numaWakeCount.Add(1)
}

// numaWakeSysmonTick is the sysmon-side window evaluation. Calibration
// phase: under GODEBUG=numa=2 it prints, at most once per
// numaWakeWindow, the elapsed-normalized wake rate and the latency EWMA
// -- the two candidate trip signals -- so the thresholds can be frozen
// from the real gate workloads (design review H2) before any response
// logic exists. Rate is normalized by measured elapsed time (review
// M4): sysmon's cadence is adaptive and unbounded above.
func numaWakeSysmonTick(now int64) {
	if debug.numa < 2 {
		return
	}
	if numaWakeLastEval == 0 {
		numaWakeLastEval = now
		numaWakeLastCount = numaWakeCount.Load()
		return
	}
	elapsed := now - numaWakeLastEval
	if elapsed < numaWakeWindow {
		return
	}
	count := numaWakeCount.Load()
	wakes := count - numaWakeLastCount
	numaWakeLastEval = now
	numaWakeLastCount = count
	// Normalized to wakes per second (elapsed is >= numaWakeWindow but
	// unbounded above).
	perSec := int64(wakes) * 1e9 / elapsed
	println("numa: wake window", elapsed/1e6, "ms wakes", wakes, "rate/s", perSec, "ewma-ns", numaWakeEWMA.Load())
}

// numaWakeWindow is the minimum evaluation window for the wake-rate
// signal (design: 100ms).
const numaWakeWindow = 100 * 1e6
