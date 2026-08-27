# Task A5 — adaptive enforcement stand-down: design

Status: DRAFT (pending adversarial review). Plan: Task A5 in
`2026-08-26-numa-v4-placement-plan.md` (pre-registered fec32e3892). Line
references: tree at 2366d4f137.

## Problem (from the P5 verdict)

Per-M node-affinity enforcement (the `schedule()`-path hook) contributes about
half of the G2-primary win (−8.09% with vs −3.39% without) but costs
wake-latency-bound workloads +15–19% (CreateGoroutines / Capture): a narrowed
M cannot be woken onto the other node's idle CPUs, and perf stat shows the
regression is mostly off-CPU wake latency (+6.7% user cycles/op vs +23% wall).
Each enforcement setting fails exactly one pre-registered gate. A5's goal:
detect the losing regime at runtime and stand enforcement down there, so BOTH
gates pass — enforcement on for garbage-shaped workloads (rare M parks, the
−8% regime), off under wake storms.

## Signal: direct wake latency, qualified by wake rate

**Primary signal — measured wake latency.** The runtime owns both ends of the
regressed path. Every production scheduler M wake goes through
`notewakeup(&mp.park)` with `mp` in hand at exactly THREE sites
(proc.go:1846 startTheWorldWithSema, :3211 startm, :3380 startlockedm —
review M1: the fourth candidate at :6640 is checkdead's faketime playground
path, unreachable in gated builds, and is deliberately NOT stamped; injectglist
wakes flow through startm and are covered). Scheduler parks funnel through
`mPark` (proc.go:2033) — with one non-scheduler bypass, review M3: the runtime
rwmutex parks Ms on the SAME m.park note (rwmutex.go:93,134) and wakes them
unstamped; the zero-stamp invariant below makes that harmless by construction,
and any future unstamped site inherits the same safety. Instrumentation:

- Waker: `mp.numa.wakeStamp = nanotime()` immediately before `notewakeup`
  (a tiny gated helper at the three sites; each is a slow path already
  containing a futex syscall — one vDSO clock read is noise there).
- Wakee: in `mPark`, after `noteclear`, gated on `goexperiment.Numa`:
  **read-and-clear the stamp, and fold ONLY when the stamp was nonzero**
  (review M2, stated as an invariant: an unstamped wake — rwmutex, faketime,
  any future site — must never fold, or `nanotime() - 0` saturates the EWMA
  for dozens of folds). mPark is nosplit (review L1): the fold is a vDSO read
  plus two global ops, within budget; the backstop widen below uses the
  global `numaSavedAffinity`, no large mask local.

```go
	// numaWakeEWMA is an EWMA of M wake latency in ns (alpha = 1/8,
	// integer: ewma += (delta - ewma) >> 3), updated by every M as it
	// wakes; numaWakeCount counts wakes. Both plain atomics; precision
	// beyond "order of magnitude per window" is not required.
	numaWakeEWMA  atomic.Int64
	numaWakeCount atomic.Uint64
```

**Qualifier — wake rate.** High latency alone must not trip the detector: GC
phase boundaries wake O(GOMAXPROCS) Ms in a thundering herd, and those bursts
measure slow while being irrelevant to throughput (they are rare). The losing
regime needs BOTH frequent wakes AND sustained latency: sysmon evaluates over
a window (below), and the trip condition is
`wakes-in-window ≥ numaWakeRateTrip && EWMA ≥ numaWakeLatencyTrip`.

**Decision point — sysmon.** sysmon already runs periodically off any P; each
pass ≥ `numaWakeWindow` (100ms) since the last evaluation reads and resets
`numaWakeCount`, reads the EWMA, and applies the trip condition. Zero cost on
any scheduler hot path. **Rate is normalized by measured elapsed time**
(review M4): sysmon's cadence is adaptive and it deep-sleeps entirely on idle
(proc.go:6709-6719), so windows are "≥100ms, unbounded above" — the trip
compares `count * numaWakeWindow / elapsed`, never a raw per-window count
(a post-idle evaluation otherwise folds an idle tail plus a resumption burst
into one inflated window). **Trips are also gated on enforcement being live**
(review M5b): no trip is counted unless `numaSoftAffinityEligible() &&
!numaConfined && !numaStoodDown` — a confined or stood-down process's
enforcement is already inert, and burning the lifetime cap on no-op
stand-downs would permanently latch a mechanism that was never the cause.

**Constants come from calibration, not estimation (review H2 — REQUIRED
before the response is implemented):** both threshold assumptions were
unmeasured, and the latency arm plausibly fails to discriminate (STW-restart
herd wakes are exactly the SLOW wakes, so the garbage regime's EWMA may sit
above any workable threshold — protection would then rest on rate alone;
meanwhile CreateGoroutines' actual mp.park wake RATE is not derivable from
code: wakep is nmspinning-gated, so op rate ≫ wake rate). Pre-registered
calibration step: implement the COUNTERS ONLY first (stamps, zero-guarded
fold, EWMA, count — plus a per-window diagnostic print under
`GODEBUG=numa=2`, diagnostic runs only), run the real garbage-4GiB-256P
pathology config and the real sched micros once each, and freeze
`numaWakeRateTrip` / `numaWakeLatencyTrip` (or DROP the EWMA arm entirely if
rate alone separates the regimes cleanly — the simpler pre-listed option,
which also removes the stamp machinery) from that data. The frozen values are
recorded in RESULTS.md before the response lands.

## Response: stand down enforcement, keep the memory side

On trip, sysmon calls `numaEnforceStandDown()`:

- Latch `numaEnforceStoodDown` (atomic bool). `numaNoteSchedule` adds it to
  the existing gate load (`numaConfined || numaStoodDown ||
  numaEnforceStoodDown` — same cacheline cluster of rarely-written globals;
  the placement MEMORY side — P homes, refill keying, stream windows — stays
  fully active: this lands the process in the measured hook-off state, which
  still wins −3.39% on garbage-shaped work).
- Eager best-effort widen: an allm, procid-guarded
  `sched_setaffinity(tid, saved-wide-mask)` walk in the SHAPE of WS-A's
  stand-down — but affinity-only (review M5a): **no `set_mempolicy`, no
  `numaSetProcessBindAll` refresh, no `bindAllDone` latch** — those are
  WS-A-confinement semantics; A5 must leave the memory side untouched, and
  its convergence state must be per-trip resettable (the epoch below), not
  one-way.
- **Enforcement epoch (review H1 — load-bearing, not optional):** the eager
  walk widens KERNEL masks but cannot touch other Ms' `mp.numa.lastNode`
  caches (owner-written plain fields; a cross-M write is a data race), so
  after re-arm every walked M would hit `numaNoteSchedule`'s steady-state
  `last == home` early-return forever and never re-narrow — silently
  degenerating the whole design into a one-way latch. Fix: a global
  `numaEnforceEpoch` (uint32, bumped by sysmon at each stand-down);
  `mNUMAState` records the epoch at apply time; the steady-state check
  requires `appliedEpoch == numaEnforceEpoch` in addition to `last == home`
  (one extra load from the same rarely-written global cluster). Stale-epoch
  Ms re-apply on their next pass after re-arm.
- Per-M convergence backstop: each M's next `mPark` checks the latch and
  widens itself if its own mask is still narrow, **and clears its own
  soft-affinity cache** (`clearSoftAffinityNode`, own-M, safe — review H1's
  second half). An M that never parks keeps its narrow mask — same accepted
  residual as WS-A stand-down, and such an M is by definition not in the
  wake-storm population.
- **Operator override (review L4):** `GODEBUG=numaenforce=0/1/auto`
  (default auto) — 0 pins enforcement off, 1 pins it on (detector inert),
  auto is this design. Cheap, and doubles as the test lever.

**Re-arm policy: bounded re-arm, not one-way.** A pure one-way latch would let
a single startup burst permanently forfeit enforcement on a long-running
server. Instead: after `numaEnforceCooldown` = 10s of consecutive
below-threshold windows, re-arm (clear the latch; Ms re-narrow lazily via the
existing hook), with a lifetime cap `numaEnforceMaxTrips` = 8 — after the
8th trip the latch is permanent. Oscillation is bounded by construction:
≤ 8 transitions per process lifetime, ≥ 10s apart, each transition costing
one allm walk. (Review probe: is the cap + cooldown argument airtight; is
lazy re-narrowing after re-arm correct against the C1-class inheritance
bugs — it reuses the existing hook path, which widen-before-clone already
guards.)

## What this does NOT touch

The stand-down here is enforcement-only and independent of WS-A's confinement
stand-down (different latch, different trigger, different response scope);
`numaStoodDown` (confinement) still implies placement-inactive as before.
No steal changes, no wakeup-target selection changes (P6), no getcpu, no new
hot-path syscalls; the only hot-ish additions are one stamp write per M wake
and one EWMA fold per M park — both on paths already paying futex syscalls.

## Off-build / census

Stamp sites and the mPark fold are behind `goexperiment.Numa` compile-time
guards (fold to nothing off); `wakeStamp` joins `mNUMAState` (zero-size off);
sysmon's evaluation call is similarly gated. Census bar unchanged: zero
function diffs off.

## Tests

- Unit: EWMA fold arithmetic; trip-condition table (rate/latency
  combinations); cap/cooldown state machine (pure functions where possible).
- Hardware (numa-dell, TestNUMA suite): a wake-storm testprog
  (CreateGoroutines-shaped) asserting threads observed widened
  (`Cpus_allowed_list` back to full) within a bounded time; the placement
  spread test must still pass in a storm-free run (no false trips).
- Gates (pre-registered in the plan): G2-sched-micros ALL FOUR ≤ +2% at 256P
  (n≥10, benchstat) AND G2-primary garbage 256P ≥ 5% improvement (fresh
  single session — the detector must not trip there; verified additionally
  by asserting the trip counter is 0 after the sweep via GODEBUG print in a
  separate non-measured run), plus census / -race / battery.

---

## Review verdict (2026-08-27): APPROVED-WITH-CHANGES — all folded above

**H1** re-arm was broken as drafted (stale per-M soft-affinity caches defeat
lazy re-narrow; the walk cannot write other Ms' caches without a race) →
enforcement epoch + own-M cache clear in the mPark backstop. **H2** both trip
constants unmeasured and the latency arm plausibly non-discriminating (STW
herds ARE the slow wakes; CreateGoroutines' true wake rate unknown —
nmspinning-gated wakep) → pre-registered counters-first calibration on the
real gate workloads before the response is implemented; EWMA dropped if rate
alone separates. **M1** :6640 is checkdead's faketime path, not injectglist —
inventory corrected to three sites. **M2** zero-stamp fold guard as an
invariant. **M3** rwmutex parks on m.park unstamped — "single funnel"
restated. **M4** elapsed-normalized rate. **M5** affinity-only convergence
(no set_mempolicy / BindAll / bindAllDone) and trips gated on enforcement
being live. **L1** nosplit budget noted. **L4** GODEBUG=numaenforce added.
Clean: production wake-site completeness (three sites), stamp/fold
race-freedom given M2, sysmon cadence in both gate regimes, single-writer
trip state machine, off-build census feasibility, detector cost plausibility,
and the considered-alternatives sweep (rate-only detection is the one live
simplification, contingent on calibration).
