// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// M wake-rate detection for adaptive enforcement stand-down (v4 Task
// A5; design: numa-design/v4-a5-adaptive-enforcement-design.md, with
// the calibration verdict in RESULTS.md): the ONLY signal is the
// elapsed-normalized M wake rate -- calibration measured the primary
// regime at 49-84 wakes/s against the losing storm regimes at
// 7.8k-12.3k/s (~100x separation, no overlap), while wake LATENCY
// measurably cannot discriminate (the primary regime's rare wakes are
// the slow STW-herd ones). Rate-only detection also means the waker
// side is one counter increment and the park path carries nothing.
//
// No build tag: every function here is referenced only from call sites
// behind the compile-time goexperiment.Numa constant, so the off binary
// carries none of it (zero-function-diff census).

import (
	"internal/runtime/atomic"
)

// numaWakeCount counts M wakes (notewakeup(&mp.park)) at the three
// production scheduler wake sites -- startTheWorldWithSema, startm,
// startlockedm. checkdead's faketime site and the runtime rwmutex's
// m.park reuse are deliberately uncounted: both are outside the
// scheduler-churn population the detector discriminates on, and an
// uncounted wake can only make the detector more conservative.
var numaWakeCount atomic.Uint64

// Window state, touched only by sysmon.
var (
	numaWakeLastEval  int64
	numaWakeLastCount uint64
)

// numaWakeWindow is the minimum evaluation window (design: 100ms).
const numaWakeWindow = 100 * 1e6

// numaCountMWake is the waker-side count, called (behind
// goexperiment.Numa) just before notewakeup at the three sites.
func numaCountMWake() {
	numaWakeCount.Add(1)
}

// numaWakeSysmonTick evaluates the wake-rate window from sysmon: at
// most once per numaWakeWindow it computes the elapsed-normalized rate
// (design review M4: sysmon's cadence is adaptive and unbounded above,
// so a raw per-window count would inflate across idle stretches) and
// hands it to numaEnforceEval (the trip/re-arm state machine,
// numa_linux.go; no-op stub elsewhere). GODEBUG=numa=2 prints the
// window for diagnosis -- never enabled in measured runs.
func numaWakeSysmonTick(now int64) {
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
	perSec := int64(wakes) * 1e9 / elapsed
	if debug.numa >= 2 {
		println("numa: wake window", elapsed/1e6, "ms wakes", wakes, "rate/s", perSec)
	}
	if numaEnforceSysmonMasked.Load() {
		// Test hook: the enforcement state machine is sysmon-single-
		// writer; hermetic tests drive numaEnforceEval directly and
		// mask the live sysmon path for their duration.
		return
	}
	numaEnforceEval(perSec, elapsed)
}

// numaEnforceSysmonMasked suppresses sysmon's numaEnforceEval calls
// while a test owns the state machine (see export_numa_test.go).
var numaEnforceSysmonMasked atomic.Bool
