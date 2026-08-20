# Pathology benchmark design: demonstrating the NUMA-balancer tax and its BIND-all recovery

Status: executed (2026-08-20). All three candidates run per this design; results in `RESULTS.md`
§"Pathology benchmark (A/B/C)". Committed per explicit task instruction after execution.

Purpose: produce a statistically defensible demonstration that (1) Linux automatic NUMA
balancing measurably damages an unpinned Go program on `numa-dell` (arm B worse than arm A
on ≥1 pre-declared metric), and (2) the `GOEXPERIMENT=numa` BIND-all patch series recovers
it (arm C significantly better than B; stretch C ≥ A). This document decides whether the
patch series is submittable upstream with a performance story, or only with the
mechanism-suppression (vmstat 0/0) + no-regression story.

Grounding sources: `numa-runtime-background.md` §6.2/§8, `bind-all-policy.md`,
`numa-v2-design.md` §1/§12, `gc-pause-bench/main.go`, `RESULTS.md` (Layer 1 promotion
evidence ~line 1075; ship-metric retrospective ~line 152; v1 noise history), and the
original issue https://github.com/golang/go/issues/14406 (fetched 2026-08-19).

Fixed measurement protocol (not redesigned here): `numa-dell`, 256 CPUs, 2 nodes,
**even CPUs = node 0, odd = node 1** (interleaved numbering — always use
`numactl --cpunodebind=0`, never a naive `--physcpubind=0-127` range), ~15 GiB DRAM/node,
kernel 6.12, `kernel.numa_balancing=1` stays on. Arms:

- **A**: stock Go, `numactl --cpunodebind=0 --membind=0` (128 logical CPUs, one node's memory).
- **B**: stock Go, unpinned. Success requirement: B measurably worse than A on ≥1 primary metric.
- **C**: `GOEXPERIMENT=numa` build, unpinned. Requirement: C significantly better than B.

n≥10 interleaved rounds per arm; `benchstat` (Mann-Whitney U, α=0.05) authoritative;
`/proc/vmstat` deltas per arm tie mechanism→effect; idle checks per the established
`ps aux --sort=-%cpu` protocol.

---

## 1. Mechanism analysis: how the balancer damages a Go program

### 1.1 The three cost channels

**(a) PROT_NONE hint faults.** `task_numa_work()` periodically walks the process's VMAs
(default cadence: first scan `numa_balancing_scan_delay_ms=1000` after task start; adaptive
period between `scan_period_min_ms=1000` and `scan_period_max_ms=60000`; up to
`scan_size_mb=256` of address space marked per scan) and installs PROT_NONE hinting PTEs.
The *next thread to touch* each marked page takes a minor fault: fault entry,
`do_numa_page`, `task_numa_fault` accounting, `mpol_misplaced()` decision, PTE restore —
roughly 1–3 µs per fault, plus `mmap_lock`-read traffic that contends with the runtime's
own `mmap`/`munmap`/`madvise` (scavenger `MADV_FREE`, heap growth `MAP_FIXED` remaps).
Where it hurts depends entirely on *who touches the cold page next*:

- If the mutator's hot loop touches it: per-access latency spikes spread thinly through
  application time — real but diffuse, hard to see above noise.
- If a **GC mark worker** touches it: the fault lands inside the GC window. Concurrent
  mark reads essentially every pointer-bearing page of the live heap *and every live
  goroutine stack* en masse, right after the balancer may have unmapped them. Every
  touched cold page = one hint fault (or one PMD-level fault under THP) charged to the
  GC cycle. This is the *concentrating* mechanism: mark converts a diffuse background tax
  into a burst that lengthens the mark phase, raises GC CPU, and pushes work into mutator
  assists.
- If it is touched **while the world is stopped**: the fault (and any migration it
  triggers) extends STW directly. This is the worst case and it is exactly what the 2016
  report hit (below) — but on modern Go the STW windows touch very few cold pages, so
  this channel is now mostly closed (see 1.3).

**(b) Page migration.** When `mpol_misplaced()` decides the page belongs near the
accessor, `migrate_misplaced_folio()` copies it: page allocation on the target node,
copy (4 KiB, or a **2 MiB folio under THP — this host runs THP=always**, so migrations
move 512-page batches), page-table rewrite, and a TLB shootdown IPI broadcast to every
CPU with the mm active — on a 256-CPU box with 128+ runtime threads, each shootdown
perturbs the whole process. Migration holds page locks; any thread (mutator, mark worker,
or an STW-stopped-world coordinator) touching that page stalls until the copy finishes.
The Layer-1 promotion run measured **2,517,110 pages migrated** during one 8 GiB `garbage`
run on stock Go (`RESULTS.md` Task 8) — ~9.6 GiB of cross-socket page copies inside one
~30 s benchmark.

**(c) Interaction with Go's GC.** Go is pathologically well-shaped prey for this
mechanism, for three reasons:

1. **GC scans everything, from everywhere.** Mark workers run on whatever node their Ms
   occupy and scan the entire reachable heap with no locality (`background.md` §5.3). The
   balancer's fault accounting therefore sees *every* page accessed from *both* nodes in
   alternation — mutator on node 0, worker on node 1 — and concludes pages are misplaced
   in both directions, driving migration ping-pong that can never converge.
2. **Cold-in-bulk access pattern.** Long-lived heap objects and parked-goroutine stacks
   are untouched by the application between GC cycles — precisely the pages the balancer
   marks — and then GC touches all of them within a few hundred ms. The steady state is:
   balancer unmaps between cycles, GC pays the fault bill every cycle.
3. **The bill lands in the latency-sensitive window.** Faults taken by dedicated mark
   workers lengthen mark; lengthened mark raises assist debt on mutators; migrations'
   page locks and TLB shootdowns hit STW coordination paths too.

### 1.2 Which mechanism the ORIGINAL #14406 hit

The 2016 reproducer (Rick Hudson): **1,000,100 goroutines** (1 M idle with persistent
stacks + 100 stack-grow/shrink, `GODEBUG=gcshrinkstackoff=1`), a **small heap** (100 MB
ballast + 10 MB churn every 100 ms), 2-minute run. Symptom: mark-termination STW of
**121–1010 ms** (healthy: ~10 ms) and **1316 s of system time** in 120 s of wall clock.
Profile: `page_fault` → `do_numa_page` → `migrate_misplaced_page`. Workaround
`numactl --membind 0,1`: mark termination 124–138 ms, system time 37 s (~35×).

Two decisive observations:

- The heap was ~100 MB. The pathology was overwhelmingly on **goroutine stack pages**:
  1 M stacks ≈ 2+ GiB of memory that only the GC ever touches.
- In Go 1.6, **mark termination re-scanned all stacks with the world stopped**. So the
  hint faults *and the migrations they triggered* were taken inside STW — channel (a/b)
  executing in the STW window, serialized. That is how 10 ms became 1010 ms.

Go 1.8's hybrid write barrier removed the STW stack rescan; stacks are scanned
concurrently. **The original amplifier (faults inside STW) no longer exists.** The same
kernel behavior now lands in concurrent mark and mutator time instead. This single fact
explains most of our failed attempts to reproduce a large STW p99 delta (below), and it
dictates the metric choice: on modern Go the defensible primary metrics are
**throughput (ns/op)** and **whole-GC-cycle time**, not STW p99.

### 1.3 What remains true on modern Go (what we CAN demonstrate)

- The balancer is fully active on an unmodified Go process (only the implicit default
  policy carries `MPOL_F_MOF`; `background.md` §6.2) — confirmed at scale: 4.24 M hint
  faults / 2.52 M migrations in one garbage run.
- The BIND-all patches suppress it completely (0/0, matching the membind oracle) —
  confirmed repeatedly (`RESULTS.md` 1a pack, Gate 7, Task 8, Prometheus).
- The remaining question — the one this design answers — is whether the *time-domain*
  cost of ~4 M faults + ~2.5 M migrations per run is measurable above this machine's
  noise with a pre-declared metric and n=10. The single unclaimed sample says yes:
  stock 3,020,198 ns/op vs numa 2,817,269 ns/op (~6.7%) with exactly that vmstat signature.

---

## 2. Why gc-pause-bench at `-idle=100000` failed, and the fixes

`RESULTS.md` ~line 152: "STW p99 is too noisy at `-idle=100000` to show a 30% win …
possibly with a heavier idle/stack profile closer to the original issue." Reading
`gc-pause-bench/main.go` against the mechanism gives four concrete defects:

1. **Wrong metric for the modern runtime.** Primary metric was STW p99 from
   `MemStats.PauseNs`. Post-Go-1.8 there is no STW stack rescan; balancer faults land in
   *concurrent mark*, which `PauseNs` deliberately excludes. The bench measured the one
   window the mechanism no longer inflates, on a box whose scheduler noise at
   `-idle=100000`, 256 P is documented to swamp sub-ms order statistics.
2. **The heap is invisible to GC.** The 4 GiB working set is `[][]byte` chunks —
   **noscan**. Mark never reads the chunk interiors, so GC takes ~zero hint faults on
   96%+ of the memory. The only GC-touched cold memory was 100 k × 2 KB ≈ 200 MB of
   stacks (the original issue had 10× the stacks and, critically, scanned them in STW).
3. **The background toucher steals the faults.** `touchAll` every 100 ms means the
   *mutator* absorbs every hinting fault within 100 ms of the balancer installing it,
   and keeps re-validating PTEs before GC ever runs. The fault bill was paid outside the
   measured window by design.
4. **Back-to-back `runtime.GC()` gives the balancer no scan window.** The measurement
   loop forces 100 GCs as fast as they complete. With a ≥1 s scan period and 256 MB/scan,
   the balancer cannot re-mark meaningful memory between consecutive cycles — only the
   first couple of measured cycles ever see cold PTEs, diluting p50/p99 toward the
   no-fault case.

Knob/code fixes (implemented as candidate (b) in §3): pointer-dense heap that mark must
actually read; toucher off during measurement; an idle gap between forced GCs of several
seconds so the balancer re-marks memory; 1 M idle goroutines + `gcshrinkstackoff=1`
matching the original; primary metric = whole `runtime.GC()` wall time (captures
concurrent mark), per-cycle, benchstat-aggregated; first cycles discarded as warmup.

---

## 3. Ranked candidate workloads

One pre-declared primary metric per candidate; every other number recorded is secondary/
exploratory and may not be promoted to a claim post hoc. All arms of a candidate run the
same GOMAXPROCS (see §4). Interleave rounds with rotating arm order
(A,B,C / B,C,A / C,A,B …) to cancel the documented ~1.5% position bias
(`RESULTS.md` Layer 1 confound note). vmstat delta captured around every individual run
via `numa-design/gate-vmstat.sh snap/diff`.

### Candidate 1 (primary): `x/benchmarks` garbage at node-fitting heap

**What it does.** The known-good signal generator: `garbage` at large heap already
produced 4.24 M hint faults / 2.52 M migrations and a 6.7% single-sample ns/op delta.
This candidate simply makes that observation statistically defensible and adds arm A.

**Heap sizing.** 8 GiB does NOT fit arm A: measured peak RSS at `-benchmem=8192` is
14.3–15.7 GiB (`RESULTS.md`), i.e. a ~1.8–1.9× RSS/target multiplier, vs ~15 GiB DRAM on
node 0 (of which only ~10.8 GB was free at the last check). **Use `-benchmem=4096`**:
expected peak RSS ≈ 7.3–7.8 GiB, fitting node 0 with ≥2 GiB headroom. Verify with one
untimed pilot run of arm A and abort the candidate if peak-RSS-bytes > 10 GiB.

**Primary metric:** median **ns/op** (benchstat `sec/op`, n=10 per arm).
**Primary comparison:** C vs B. **Secondary (the B-worse-than-A requirement):** B vs A on
the same metric. Exploratory: STW-ns/op, peak-RSS-bytes, user+sys-ns/op, vmstat deltas.

**Expected effect.** B vs C: C faster by ~3–7% (scaled down from the 6.7% 8 GiB sample;
half the heap ⇒ smaller fault bill, but 128 P instead of 256 P concentrates it). B vs A:
A faster by a similar or smaller margin (A gets balancer exemption *plus* full locality —
it should be the best arm). Direction: B is the slowest arm.

**Runtime per round.** ~30–45 s per run (observed: 10000 ops × ~3 ms at 8 GiB; less at
4 GiB) + vmstat snaps ⇒ ~2.5 min per 3-arm round ⇒ **~30–40 min for the full n=10 sweep**
(+1 discarded warmup round).

**Command lines** (once: `GOBIN=/tmp/pb/base go install golang.org/x/benchmarks/garbage@latest`
and `GOBIN=/tmp/pb/numa GOEXPERIMENT=numa go install golang.org/x/benchmarks/garbage@latest`,
`GOROOT=/home/deparker/go-numa`, `GOTOOLCHAIN=local`; record the resolved x/benchmarks
version — it must be identical for both installs):

```
# per round r in 1..10, arm order rotated by r mod 3; vmstat snap before/after each line
# Arm A:
numactl --cpunodebind=0 --membind=0 env GOMAXPROCS=128 /tmp/pb/base/garbage -benchmem=4096 -benchnum=1 >> A.out
# Arm B:
env GOMAXPROCS=128 /tmp/pb/base/garbage -benchmem=4096 -benchnum=1 >> B.out
# Arm C:
env GOMAXPROCS=128 /tmp/pb/numa/garbage -benchmem=4096 -benchnum=1 >> C.out
# then: benchstat B.out C.out   (primary)   and   benchstat A.out B.out   (secondary)
```

Mechanism tie: per-round vmstat must show B ≫ 0 hint faults (expect ≥ 5×10^5 at 4 GiB)
and A = C = 0/0. A B-round with ~0 faults is a setup fault (balancer settled/off), not
evidence — investigate, do not average in.

Optional supplement (exploratory only, no A arm): one n=10 B-vs-C pass at
`-benchmem=8192`, GOMAXPROCS=256 — the exact configuration of the existing 6.7% sample —
to check whether the effect grows with heap size. ~35 min extra; run only if the primary
sweep shows a significant C-vs-B win and time permits.

### Candidate 2: gc-pause-bench "heavy profile" — GC-cycle time on a scannable cold heap

**What it does.** Reconstructs the #14406 shape on the modern runtime: a large
pointer-dense long-lived heap + 1 M parked goroutines, left *idle* between forced GCs so
the balancer re-marks memory, then measures how long each GC cycle takes when mark must
eat the accumulated hint faults. This is the mechanism-pure demonstration: nothing runs
between cycles except the balancer.

**Code changes to `numa-design/gc-pause-bench/main.go`** (all additive, flag-gated, so
the old profile stays reproducible):

1. New flag `-ptrheap=true`: replace `allocChunks`' `[][]byte` with a pointer-dense
   graph. Concretely: `type node struct { next *node; pad [56]byte }` (64 B); allocate
   `heapMB<<20/64` nodes, linking each into one of 4096 rings rooted in a global
   `[]*node`; retain everything. Mark must dereference `next` in every 64 B ⇒ GC reads
   every 4 KiB page of the heap every cycle. (Keep `-ptrheap=false` for the legacy
   noscan mode.)
2. New flag `-toucher=false` (default false in heavy mode): the 100 ms `touchAll`
   goroutine must NOT run during measurement — §2 defect 3. Warmup may keep one full
   `touchAll` pass to commit pages (first-touch), then stop.
3. New flag `-gcgap=5s`: sleep between measured `runtime.GC()` calls, giving
   `task_numa_work` (≥1 s period, 256 MB/scan) time to re-install PROT_NONE on a fresh
   slice of the heap/stacks — §2 defect 4.
4. New flag `-discard=2`: drop the first k measured cycles (balancer state ramping).
5. Per-cycle benchstat-consumable output: after each measured cycle print
   `BenchmarkGCCycleWall 1 <wallNs> ns/op` to stdout (one line per cycle). Keep the JSON
   summary on the side.
6. Heavy defaults for this candidate: `-heap=4096 -idle=1000000 -stacks=200 -warm=45`
   and run under `GODEBUG=gcshrinkstackoff=1` (matches the original reproducer; prevents
   stack shrink from freeing the cold stack pages between cycles).

Footprint: 4 GiB nodes + 1 M × 2 KB stacks (~2 GiB) + runtime ⇒ ~7–8 GiB RSS; fits
node 0 under the same ≥10 GB-free precondition as candidate 1.

**Primary metric:** median **wall-clock ns per forced GC cycle** (`BenchmarkGCCycleWall`,
benchstat over all measured cycles: 8 cycles × 10 rounds = 80 samples/arm).
**Primary comparison:** C vs B. **Secondary:** B vs A. Exploratory: STW p99 (recorded but
explicitly NOT a claim — §2 defect 1), `/gc/pauses` if added later, vmstat deltas.

**Expected effect and direction.** Between two GCs 5 s apart the balancer re-marks up to
~1.3 GB (256 MB/s at min period). Arm B's next mark eats those faults (THP ⇒ PMD-level:
~650 2-MiB faults, each a candidate 2 MiB cross-socket copy + shootdown); arms A/C eat
zero. GC of a 4 GiB pointer heap + 1 M stacks at 128 P should take ~300–800 ms; expected
B penalty tens-to-hundreds of ms per cycle ⇒ **B slower than C by ~10–40%** on cycle
wall time. This metric is also nearly CPU-count-neutral (the world is otherwise idle;
only ~32 mark workers run), so B-worse-than-A is expected to hold cleanly here.

**Runtime per round.** Setup (alloc + 1 M goroutine spawn + 45 s warm) ~90 s +
(2 discard + 8 measured) × (5 s gap + ~0.5 s GC) ≈ 55 s ⇒ ~2.5–3 min/round ⇒ 3 arms ×
(10+1) rounds ≈ **~85–95 min** for the full sweep. If over budget, cut to `-gcgap=4s`,
6 measured cycles (60 samples/arm still ample for benchstat).

**Command lines** (build: `go build -o /tmp/pb/gcpause-base ./gc-pause-bench` and
`GOEXPERIMENT=numa go build -o /tmp/pb/gcpause-numa ./gc-pause-bench` from the worktree's
`numa-design/` with `GOROOT=/home/deparker/go-numa`):

```
FLAGS="-ptrheap=true -toucher=false -heap=4096 -idle=1000000 -stacks=200 -warm=45 -gcgap=5s -discard=2 -n=8"
# Arm A:
numactl --cpunodebind=0 --membind=0 env GOMAXPROCS=128 GODEBUG=gcshrinkstackoff=1 /tmp/pb/gcpause-base $FLAGS >> A.out
# Arm B:
env GOMAXPROCS=128 GODEBUG=gcshrinkstackoff=1 /tmp/pb/gcpause-base $FLAGS >> B.out
# Arm C:
env GOMAXPROCS=128 GODEBUG=gcshrinkstackoff=1 /tmp/pb/gcpause-numa $FLAGS >> C.out
```

Mechanism tie: B's per-round vmstat hint-fault delta must be ≥ 10^5 (with THP, raw fault
counts are lower than 4 K-page equivalents; calibrate on the pilot round and record the
observed floor); A and C must be 0/0.

### Candidate 3 (exploratory, run only if 1 and 2 both fail C-vs-B): phase-shifting locality inversion

**What it does.** The "balancer actively harmful" scenario BIND-all prevents: allocate a
stable ~6 GiB pointer working set, then alternate 30 s phases in which the *readers* are
pinned to one node's CPUs, flipping nodes each phase (via `unix.SchedSetaffinity` on
`LockOSThread`ed reader goroutines — bench-side pinning, no runtime changes; masks are
even CPUs vs odd CPUs per the interleave). Each flip makes the entire working set
"misplaced"; the balancer migrates ~6 GiB toward the readers, and before it converges the
phase flips again — perpetual migration storms on arm B. Arm C never migrates: readers
are steadily ~50% remote but pay no fault/copy/shootdown tax. New program
`numa-design/phase-shift/main.go` (~150 lines: ring allocation as in candidate 2's node
graph; 64 reader goroutines chasing pointers and counting reads; 4 phases × 30 s;
prints `BenchmarkPhaseChase 1 <ns/read> ns/op` for the whole run).

**Primary metric:** overall **ns per pointer-read** across the full run (one sample per
round, n=10/arm). Primary comparison: C vs B. Expected: C faster by 5–20% (migration
storms cost B copies + shootdowns + fault stalls on every hot page, every phase), OR —
the honest alternative — B *wins* because migration converges within a phase and local
access beats C's permanent 50% remote. Either outcome is informative; only the first
supports the pathology claim, and the pre-declared claim is only made if C > B.

**Arm A is informational-only** for this candidate (option iii, §4): pinned to node 0 it
cannot express inversion (the odd-CPU affinity mask intersected with its cpuset is empty;
the bench must fall back to no-op pinning). Record it, claim nothing from it.

**Sizing/runtime:** 6 GiB nodes ⇒ ~7 GiB RSS (fits node 0 for the informational A arm);
~2.5 min/round ⇒ ~80 min sweep. GOMAXPROCS=128 all arms.

**Why ranked third:** it is a synthetic scenario with bench-side thread pinning (a
reviewer can object that pinned readers are not "a Go program"), and its outcome is
genuinely uncertain in direction. Candidates 1–2 are closer to the issue's mechanism and
to real workloads.

---

## 4. The A-vs-B trap

Arm A has half the CPUs (128 logical = 64 cores × 2 HT on one socket). For a CPU-saturated
workload, B trivially beats A on wall time regardless of any NUMA tax — fine for the
C-vs-B payoff, fatal for "B worse than A".

**Decision: option (ii) — equalize GOMAXPROCS=128 across all three arms of every
candidate** (for arm A this matches its cpuset anyway; for B/C it must be set explicitly
since the default would be 256). This makes A-vs-B a memory-placement comparison at equal
parallelism.

Residual confound, stated honestly: at GOMAXPROCS=128, arm A's 128 threads saturate one
socket's 64 cores (every core runs 2 hyperthreads), while B/C's 128 threads spread over
128 physical cores (≈1 thread/core, more per-thread core resources). This biases
CPU-bound phases **in B's favor**, i.e. against the "B worse than A" requirement — so if
B still loses to A, the result is conservative and stronger, not weaker. Per candidate:

- **Candidate 1 (garbage, throughput):** the HT confound is real. Keep (ii) as primary;
  if B ≥ A but C > B holds, **declare A informational for this candidate** (fallback to
  option iii) and rest the B-worse-than-A requirement on candidate 2. Do not chase A-vs-B
  with GOMAXPROCS=64 contortions — that changes the workload.
- **Candidate 2 (GC-cycle time):** effectively option (i) as well — during the measured
  window only ~32 mark workers (25% of 128) plus fault handling run, nowhere near CPU
  saturation on either arm, so CPU count barely helps B and the HT confound is minimal.
  This is the candidate expected to show B worse than A most cleanly.
- **Candidate 3:** A cannot participate (no inversion inside one node) — option (iii),
  informational only; the claim there is C-vs-B alone.

---

## 5. Confound controls

- **GOMAXPROCS:** 128 in all arms, all candidates (§4). Recorded in every output line.
- **Balancer scan clock:** the first scan fires ~1 s after process start
  (`scan_delay_ms=1000`) and the period adapts 1 s–60 s; 256 MB address space per scan.
  Candidate 1's runs are long (~30 s+) and continuously allocating, so the effect
  integrates within each run — no extra settle time needed beyond the run itself.
  Candidate 2 builds the settle time in (`-warm=45`, `-gcgap=5s`, `-discard=2`).
  Candidate 3's 30 s phases exceed the min period by 30×. Do not restart arms mid-round:
  each run is a fresh process, so each gets the fresh 1 s scan-delay behavior — identical
  across arms by construction.
- **THP:** host is THP=always — hint PTEs at PMD granularity and 2 MiB-folio migration
  batches. Do not change it (protocol is fixed; changing it would also change what
  production sees). Record `/sys/kernel/mm/transparent_hugepage/enabled` and the vmstat
  THP counters (`thp_migration_*`) with each sweep so the fault-count arithmetic in the
  writeup uses the right granularity.
- **Free-RAM-per-node check before every arm-A run:** `numactl --hardware`; require
  node 0 free ≥ expected peak RSS + 2 GiB (i.e. ≥ 10 GB for candidates 1–2). If not met,
  wait/reclaim, never proceed — an arm-A run that spills or reclaims measures the wrong
  thing (`--membind=0` under pressure stalls in reclaim rather than spilling).
- **Warmup rounds:** one full unrecorded A/B/C round per candidate before the 10 recorded
  rounds (build caches, page cache, balancer steady state).
- **Idle checks:** `ps aux --sort=-%cpu` (not `uptime`, which decays slowly after 256P
  runs) before every round, per the protocol already established in `RESULTS.md`.
- **Order rotation:** arm order rotates per round (ABC/BCA/CAB) to cancel the ~1.5%
  position bias documented in the Layer 1 gate notes.
- **Statistical plan:** benchstat (Mann-Whitney U), α=0.05, exactly ONE primary
  metric+comparison per candidate, declared above before any run. B-vs-A is the single
  pre-declared secondary. Everything else (STW, RSS, user+sys, IMC if collected) is
  exploratory and may motivate a *new pre-registered* sweep but never a claim from this
  one. n=10 rounds; if a primary comparison lands at 0.05<p<0.10 with consistent sign,
  extend to n=15 once (pre-declared here); never extend twice.
- **vmstat hygiene:** snap immediately before/after each run; nothing else may run on the
  box between snaps. B-rounds with anomalous ~0 fault deltas are excluded as setup
  failures (with the exclusion recorded), not averaged.
- **Toolchain hygiene:** both binaries from the same GOROOT/SHA; record `go version`,
  kernel, x/benchmarks pin, `numa_balancing` value, and THP mode in the results header,
  per the existing RESULTS.md conventions.

---

## 6. Kill criteria — the honest failure report

Per candidate, the candidate is **dead** when, at n=10 (extended once to 15 if
borderline):

- C vs B on the primary metric: benchstat "~" (p≥0.05), AND
- the mechanism is *confirmed present*: B's per-round vmstat shows the balancer working
  (candidate 1: ≥5×10^5 hint faults and ≥10^5 pages migrated per run; candidate 2:
  ≥ the pilot-calibrated floor; candidate 3: visible migration bursts at each phase flip)
  while C shows 0/0.

(If B's vmstat is ~0, that is a benchmark bug, not a kill — fix the workload once and
rerun; if it cannot be made to trigger the balancer, the candidate is invalid rather than
failed.)

**The hardware cannot demonstrate the pathology** — the full honest-failure verdict —
when all three candidates die by the rule above. The failure report must then say,
explicitly:

> On this 2-node, ~15 GiB/node, THP=always, kernel-6.12 box, automatic NUMA balancing
> demonstrably operates on stock Go (millions of hint faults and page migrations per
> run, reproduced across workloads) and the patch series demonstrably silences it
> (0/0, matching the `numactl --membind` oracle). However, the *time-domain* cost of
> that balancer activity is below this machine's noise floor at n=10–15 for every
> pre-declared metric we tried. The 2016 issue's 100× STW amplification depended on the
> STW stack rescan removed in Go 1.8 and on larger/more-node topologies; we could not
> reproduce a statistically significant slowdown here.

In that world the upstream submission cannot claim a measured performance recovery from
this machine; it can claim (a) mechanism suppression with the membind-oracle equivalence,
(b) zero regression under the Layer 0/1 gates, and (c) the historical issue + kernel-code
analysis as the motivation — and should say a larger-node-count box (e.g. the 4-node
EPYC of #78044) is where a time-domain demonstration should be re-attempted. Partial
outcomes are reported as-is: e.g. "C>B proven on candidate 2 but B≥A everywhere" supports
the recovery claim while conceding the A comparison to the HT confound (§4).

Secondary kill signal worth recording either way: if candidate 1's C-vs-B at
`-benchmem=4096`/128P fails but the exploratory 8 GiB/256P B-vs-C supplement reproduces
the 6.7% delta significantly, the effect is real but requires more memory pressure than
one node can hold — that is a *positive* result for the patch series (the pathology grows
exactly where arm A stops being possible) and should be written up as such, with the
A-vs-B requirement conceded as untestable at that scale on this box.

---

## Execution order and total budget

1. Candidate 1 sweep (~40 min incl. warmup round + pilot RSS check).
2. Candidate 2 code changes (local, ~1 h dev, no remote time), then sweep (~90 min).
3. Candidate 3 only if both C-vs-B comparisons failed (~80 min + ~1 h dev).

Expected remote wall-clock: **~2.5 h** for the likely path (candidates 1+2), worst case
~4.5 h including candidate 3 and one n=15 extension.
