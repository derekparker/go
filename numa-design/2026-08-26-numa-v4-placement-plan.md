# NUMA Runtime v4 Implementation Plan — Placement & Cost Attribution

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve Workstream B's blocker in four gated stages, in the order the user approved: **(1)** re-adjudicate the 256P user+sys cost gate with the json harness's own CPU profiler disabled (the dominant contended lock in the original FAIL was `sighand->siglock` from the harness's SIGPROF delivery — a measurement-tool interaction); **(2)** goroutine/P-level node placement — partition Ps across nodes and anchor Ms to their P's home node, so the memory consumer stays where WS-B's homing+routing put its spans (the measured root cause of the Gate-2 IMC FAIL: 25–47% remote refills at every thread count, unpinned); **(3)** a lock-callchain attribution study for whatever cost gates still fail after (1) and (2); **(4)** node-aware page allocation so large allocations and page-cache refills stop diluting per-node homing. Every gate runs on the pathology A/B/C harness and the v3 statistical constraints.

**Architecture:** Stage 2 is the centerpiece: at `procresize`, each P gets a home node (contiguous partition proportional to node CPU counts); the existing `schedule()` soft-affinity hook changes its key from getcpu to the P's home node (deterministic, one fewer syscall on the hook path); mcentral refill routes by the P's home node instead of getcpu-at-refill; work-stealing prefers same-node victims first with an unconditional cross-node fallback (work conservation preserved). Placement engages only when the process actually spans nodes (not confined, multi-node, unconfined inherited affinity) — WS-A confinement and stage-2 placement are mutually exclusive by construction. Stage 4 makes `pageAlloc` prefer address ranges belonging to the requesting node's arena stream. All of it stays behind `GOEXPERIMENT=numa` with the zero-function-diff off-build bar.

**Tech Stack:** Go runtime (`src/runtime`), Linux `sched_setaffinity`/`mbind`/`getcpu`, remote host `numa-dell`, `golang.org/x/benchmarks` json/garbage (pinned `v0.0.0-20260819172200-70693762b6a0`), `numa-design/pathology-sweep.sh`, benchstat, `perf lock`/`perf record` for attribution.

**Spec:** `numa-design/numa-v2-design.md` §12.3–§12.4 (locality unit); WS-B verdict and audited gate numbers in `numa-design/RESULTS.md` (read the Task 11 sections INCLUDING corrections — E1/E2 attribution is the evidence base for this plan). **Prior plan:** `2026-08-20-numa-v3-locality-plan.md` (environment, machine facts, forbidden list — carried by reference AND restated where load-bearing).

---

## Standing verdict this plan starts from (do not re-litigate)

- WS-B routing is **correct**: pinned, 100.00% / 99.54% of span refills are local. The mechanism is not the problem.
- WS-B's unpinned payoff was too small: IMC remote share 48.50% → 46.32% (−4.49% relative; 95% CI [−7.6%, −1.2%] excludes the ≥10% bar; real but insufficient). Root cause: thread placement — 25–47% remote refills at every GOMAXPROCS unpinned, and consumers migrate away from their memory after refill.
- WS-B's recorded costs: 1P alloc micro **+3.73% FAIL**; 256P json user+sys **+19.96% FAIL** — with the dominant contended lock being `sighand->siglock` via `posix_cpu_timers_work`, i.e. SIGPROF delivery from the harness's own always-on CPU profiler (E2: WS-B shifts ~72e9 cycles user→kernel spinlock wait under that profiler).
- Syscall overhead is excluded as a cost source (≤0.05% of cycles). WS-A proved placement alone is worth −30% at 128P.

## Global Constraints (carried from v3 — do not soften)

- **Statistics:** benchstat (Mann–Whitney U, α=0.05) is authoritative on numa-dell; n≥10 interleaved rounds per arm; **single-session sweeps only** with rotating arm order; one pre-declared primary metric per gate; no difference-of-significance inferences; report ICC/effective-n for clustered samples.
- **Pre-registration:** this plan (and any amendment adding tasks) is committed **before** the measurements it governs. Raw `.out` files + sweep scripts + `go version -m` transcripts archived under `numa-design/bench-data/<campaign>/` in the same commit as the results. RESULTS.md entries describe what was measured in this plan's terms only.
- **Hard gates at every stage:** 1P json GOMAXPROCS=1 ≤ +2% (ns/op AND user+sys-ns/op, benchstat); 1P alloc micro (`Malloc(8|16|Types)`, `-count=10`) ≤ +2%; off-binary function census (objdump function-level diff, experiment off, HEAD vs stage-start) zero diffs; `-race` runtime tests pass (`-timeout=20m`); all 18+ TestNUMA* pass on numa-dell.
- **Machine hygiene:** idle check (`ps aux --sort=-%cpu`, skip header) before every measurement block; `kernel.numa_balancing=1` verified at session end; `GOTOOLCHAIN=local`; **no `GODEBUG=numa=1` in measured runs**; node-free ≥10 GB before single-node runs; **verify `go version -m` of every measured binary against the intended tree SHA before the sweep** (stale-binary incident, v3).
- **Environment:** `GOWORK=off` everywhere; build with `GOROOT_BOOTSTRAP=/home/deparker/sdk/gotip ./make.bash` from `src/`; remote tree `/home/deparker/go-numa` via `make push` (fast-forward only — **never force-push**; if the remote diverges, stop and report); **never push to `origin`** (go.googlesource.com); **never push to `fork`** (github) without explicit user authorization.
- **Git:** never `--no-verify`; no Co-authored-by trailers; every commit compiles and passes the tests it touches; commit messages explain why.
- **Forbidden list (v2/v3, in force):** no mcache flush / `numaFlushForeignCachedSpans`-shaped code; no per-malloc scans; no STW `set_mempolicy` toggles; **never** single-node `MPOL_BIND` for the whole process; `maxnode=65` fixed-width nodemask convention everywhere. **Amended by locked decision P1 below:** the v2 ban on "p.numaNode-by-index" barred *unenforced* index-derived node fictions; stage 2 assigns P home nodes **paired with M-affinity enforcement**, which is a different design — the ban is narrowed accordingly, and the design-review task must confirm the pairing is airtight (no code path consumes a P's home node while enforcement is disabled).
- **If a gate fails: stop, record the failure in RESULTS.md, do not start the next stage** (stage 3 is the pre-authorized exception: it exists to attribute failures).

---

## Locked design decisions

Changing one requires editing this section first (pre-registration discipline).

**Stage 1 (profiler-off re-adjudication):**

1. **One patched harness serves all four arms.** Copy the pinned x/benchmarks module (`v0.0.0-20260819172200-70693762b6a0`) out of the module cache on numa-dell (no network, byte-identical source), add an env-gated guard `if os.Getenv("BENCH_DISABLE_CPUPROF") == "" { ... }` around exactly the `pprof.StartCPUProfile(cpuprof)` / `pprof.StopCPUProfile()` pair in `driver.runBenchmarkOnce`, and build **two** json binaries from that patched source (experiment OFF and `GOEXPERIMENT=numa`). The env var — not a build flag — selects prof/noprof, so the prof and noprof arms of each toolchain are the **same binary** (zero build variance within a toolchain). The resulting `git diff`-style patch is archived in `bench-data/v4-task1-noprof/`.
2. **Primary metric (pre-registered): `user+sys-ns/op`, OFF-noprof vs NUMA-noprof, GOMAXPROCS=256, benchstat, n=10 interleaved rounds of all four arms with rotating order.** Verdict bar: the original gate's band — PASS if delta ≤ +2% or not significant. Secondary (labeled, no gate): `ns/op` same comparison; replication pair OFF-prof vs NUMA-prof (expected to reproduce the original +20%-scale FAIL); **profiler-interaction estimate** = (NUMA−OFF)under-prof − (NUMA−OFF)under-noprof, reported with CIs either way.
3. **Scope:** this stage re-adjudicates ONLY the 256P user+sys cost gate. The 1P alloc micro +3.73% FAIL is untouched by the profiler finding and remains a standing FAIL for stage-2/4 work to answer.

**Stage 2 (P/goroutine node placement):**

- **P1 — Home-node assignment at `procresize`:** Ps are partitioned contiguously in id order, quota per node proportional to that node's CPU count (largest-remainder rounding; every node with CPUs gets ≥1 P while Ps remain). Stored in a build-tagged per-P struct (`pNUMAState`, mirroring `mNUMAState`'s on/off file pattern — zero-size off). Recomputed on every `procresize` (GOMAXPROCS changes are handled by construction). The **pairing rule**: assignment is only ever consumed where enforcement (P2) is active; both are guarded by one predicate `numaPlacementActive()`.
- **P2 — Enforcement reuses the soft-affinity machinery, re-keyed:** the existing `schedule()`-path hook (`numaNoteSchedule`/`numaApplySoftAffinity`, 4ms throttle, widen-before-clone/fork/exec all preserved) changes its node key from getcpu to `pp.numa.homeNode`. This *replaces* getcpu-keyed soft affinity when placement is active — one mechanism, not two; the getcpu-keyed variant remains only as the fallback when placement is inactive but the WS-B experiment paths run. No new syscall class, no new hook points in the hot path.
- **P3 — Refill routing keyed by P home node:** `mcentral.cacheSpan` uses `pp.numa.homeNode` instead of getcpu when placement is active (deterministic; removes the per-refill syscall; consistent with what enforcement converges the thread to). getcpu routing remains the fallback when placement is inactive.
- **P4 — Locality-aware stealing, work-conserving:** in `stealWork`, iterate the existing randomized victim enumeration **twice when placement is active**: first pass skips victims whose home node differs from the stealing P's, second pass is exactly today's unrestricted order. No path may block or spin longer because of node preference — cross-node theft always remains reachable in the same `stealWork` invocation. (Exact placement of the two-pass split, and whether `runqsteal`'s timer handling needs the same treatment, are design-review items.)
- **P5 — Engagement predicate (`numaPlacementActive`):** experiment on AND multi-node AND topology untruncated AND NOT confined (WS-A) AND inherited affinity spans all online CPUs (operator placement wins) AND `sched_setaffinity` available on this arch. Confined processes: every P's home = confined node, hook inert (degenerate no-op by construction).
- **P6 — Out of scope for v4 stage 2** (scope control; revisit only if the decision gate fails and attribution points here): dedicated GC mark-worker node distribution, netpoller/timer wakeup node preference, global-runq node sharding, any goroutine-struct field.
- **P7 — Design-review before implementation:** stage 2 begins with a design round producing an addendum to THIS plan (appended implementation tasks with code), adversarially reviewed (fresh reviewer, self-contained prompt) before any runtime code is written. Review must specifically probe: P1's pairing rule, P4's work conservation under GC/spin edge cases, `procresize` re-assignment vs in-flight refills (a span routed to a node that a P no longer homes is *stale but safe* — it lands in that node's spanSet and is HWM-bounded like any remote span — confirm no invariant assumes otherwise), and interaction with stand-down.

**Stage 2 decision gates (pre-registered now, measured on numa-dell):**

- **G2-primary:** pathology garbage benchmark (`-benchmem=4096`), **GOMAXPROCS=256** (the spans-both-nodes regime WS-A cannot help), experiment-on unpinned vs stock unpinned, single-session, n≥10, rotating order. PASS = wall sec/op improves ≥5% (benchstat significant).
- **G2-locality:** unpinned local-refill share (`/numa/span-refills/*`) ≥90% at every GOMAXPROCS in {2, 8, 32, 128, 256} (E1's numbers were 75/73/53/62/57%).
- **G2-IMC:** IMC-measured remote-traffic share reduction ≥10% relative vs stock (the bar WS-B failed at −4.49%).
- **G2-sched-micros:** `BenchmarkPingPongHog`, `BenchmarkCreateGoroutinesParallel`, `BenchmarkCreateGoroutines`, `BenchmarkCreateGoroutinesCapture` ≤ +2% (benchstat, n≥10) at GOMAXPROCS=256 — the scheduler is being touched; these are hard gates.
- **G2-cost:** the standing hard gates (1P json, 1P alloc micro, 256P json user+sys with the stage-1 noprof harness, census, -race, TestNUMA*).

**Stage 3 (lock-callchain attribution — conditional):**

- Runs **only against gates still failing after stages 1–2** (it exists to attribute failures, not to decorate passes; if everything passes it shrinks to a single confirmation capture archived as exploratory). Method locked now: `perf lock record`/`perf lock contention -a -b` (or `perf record -e lock:*` fallback if BPF unavailable) on n≥3 interleaved captures per arm, arms = {stock, experiment-on, experiment-on with the failing feature's hook disabled by patch}, callchains with `--call-graph dwarf`, single session, idle-checked. Deliverable: RESULTS.md attribution table (lock symbol → cycles → owning subsystem) with the patched-arm delta as the causal estimate.

**Stage 4 (node-aware page allocation):**

- **P8 — Shape:** `pageAlloc` stays ONE structure (no per-node pageAlloc instances — that would fork chunk metadata, scavenger state, and searchAddr invariants). Node awareness enters as a **search preference**: `pageAlloc.alloc` gains a node-aware entry (`allocNode(npages, node)`) that first searches only the address ranges belonging to that node's arena stream (the per-node stream address ranges are already known from WS-B's `mheap` streams), falling back to today's unrestricted search on failure. Free/scavenge paths untouched.
- **P9 — Callers routed:** mheap span allocation on behalf of a node-routed mcentral grow (`numaGrowNode`/`numaBindGrowth` path) and large-object allocation (which today lands wherever `searchAddr` points, diluting homing). Stack allocation and everything else: untouched (scope control).
- **P10 — Cost guard:** the node-preferred search must be bounded — if the node's ranges have no free run of the right size, fall back immediately (one extra search over a subset, never a retry loop). Design review (same discipline as P7) before implementation; the fast-path bar is the 1P alloc micro ≤ +2% hard gate, which is currently a standing FAIL at +3.73% — **stage 4's gate is measured against stock, not against WS-B's current state**, i.e. the +3.73% must not compound; the stage-2/4 combined branch must bring the 1P alloc micro within +2% of stock or the stage fails.
- **G4:** G2-primary re-run ≥ stage-2 result (no regression) AND large-object locality probe (new: `/numa/span-refills` analog or a test-hook count of node-matched large allocs — design task specifies) shows ≥90% node-matched large allocations pinned, plus the standing hard gates.

---

## Environment (delta from v3 — everything else unchanged)

- Branch `claude/numa-v2-implementation-a6eb4c`, HEAD at plan commit; remote tree `/home/deparker/go-numa` (Makefile: `push` / `build` / `test-numa` / `topo` / `gate-json-1p`).
- numa-dell: 256 CPUs (even=node0, odd=node1), ~15 GiB/node, kernel 6.12, `numa_balancing=1`, THP=always. benchstat at `/tmp/numa-tools/benchstat` (re-install if the machine rebooted: `GOBIN=/tmp/numa-tools go install golang.org/x/perf/cmd/benchstat@latest` with any released toolchain).
- x/benchmarks pinned: `v0.0.0-20260819172200-70693762b6a0` (in numa-dell's module cache; stage 1 copies it out — no network dependency).

---

### Task 1: Profiler-off re-adjudication of the 256P user+sys cost gate

**Files:**
- Create: `numa-design/v4-task1-noprof-sweep.sh` (sweep driver, archived)
- Create: `numa-design/bench-data/v4-task1-noprof/` (patch, raw `.out` files, `go version -m` transcripts, benchstat outputs)
- Modify: `numa-design/RESULTS.md` (new section "v4 Task 1")

**Interfaces:**
- Consumes: tree toolchain at `/home/deparker/go-numa` (OFF and `GOEXPERIMENT=numa` builds — both already exist from v3; re-verify SHA), pinned x/benchmarks in numa-dell module cache.
- Produces: verdict on the 256P user+sys gate under a noprof harness; the profiler-interaction estimate; the patched-harness convention (`BENCH_DISABLE_CPUPROF=1`) that ALL later 256P json gates in this plan use.

- [ ] **Step 1: Write the sweep driver locally and commit (pre-registration).**

`numa-design/v4-task1-noprof-sweep.sh` — runs ON numa-dell, expects the two patched-harness binaries already built at `$BIN_OFF` and `$BIN_NUMA`:

```bash
#!/usr/bin/env bash
# v4 Task 1: 4-arm single-session sweep, GOMAXPROCS=256.
# Arms: off-prof, numa-prof, off-noprof, numa-noprof (noprof = BENCH_DISABLE_CPUPROF=1).
# Rotating arm order per round; n rounds; raw benchfmt appended per arm.
set -euo pipefail
BIN_OFF="${BIN_OFF:?path to patched-harness json binary, experiment OFF}"
BIN_NUMA="${BIN_NUMA:?path to patched-harness json binary, GOEXPERIMENT=numa}"
OUT="${OUT:-$HOME/v4-task1-out}"
N="${N:-10}"
PROCS=256 MEM=512 TIME=3s
mkdir -p "$OUT"
# idle check: fail if any foreign process is burning >50% of a CPU
busy=$(ps aux --sort=-%cpu | awk 'NR>1 && $3>50 {print $11}' | grep -v -e json -e ps || true)
[ -z "$busy" ] || { echo "machine not idle: $busy"; exit 1; }
[ "$(cat /proc/sys/kernel/numa_balancing)" = "1" ] || { echo "numa_balancing != 1"; exit 1; }
arms=(off-prof numa-prof off-noprof numa-noprof)
run_arm() {
  local arm="$1" bin env_no=""
  case "$arm" in
    off-*)  bin="$BIN_OFF" ;;
    numa-*) bin="$BIN_NUMA" ;;
  esac
  case "$arm" in *-noprof) env_no=1 ;; esac
  env GOMAXPROCS=$PROCS ${env_no:+BENCH_DISABLE_CPUPROF=1} \
    "$bin" -benchmem=$MEM -benchnum=1 -benchtime=$TIME >>"$OUT/$arm.out" 2>>"$OUT/$arm.log"
}
for a in "${arms[@]}"; do : >"$OUT/$a.out"; : >"$OUT/$a.log"; done
for round in $(seq 0 $((N-1))); do
  echo "== round $round =="
  for i in 0 1 2 3; do
    run_arm "${arms[$(( (round + i) % 4 ))]}"
  done
done
echo "done; raws in $OUT"
```

Commit this script + the plan before any remote execution:
```bash
git add numa-design/2026-08-26-numa-v4-placement-plan.md numa-design/v4-task1-noprof-sweep.sh
git commit -m "numa-design: pre-register v4 plan and Task 1 profiler-off sweep"
```

- [ ] **Step 2: Sync numa-dell (fast-forward only) and verify tree SHA.**

```bash
make push
ssh numa-dell 'cd /home/deparker/go-numa && git rev-parse HEAD'
```
Expected: HEAD equals the local plan commit. If the push is not a fast-forward: STOP, report.

- [ ] **Step 3: On numa-dell, make the patched harness copy and verify the diff.**

```bash
ssh numa-dell '
  set -e
  rm -rf ~/xbench-noprof
  cp -r "/home/deparker/go/pkg/mod/golang.org/x/benchmarks@v0.0.0-20260819172200-70693762b6a0" ~/xbench-noprof
  chmod -R u+w ~/xbench-noprof
'
```
Then edit `~/xbench-noprof/driver/driver.go` in `runBenchmarkOnce`: wrap `pprof.StartCPUProfile(cpuprof)` and `pprof.StopCPUProfile()` each in `if os.Getenv("BENCH_DISABLE_CPUPROF") == "" { ... }` (the file already imports `os`). Nothing else changes. Capture the diff for the archive:
```bash
ssh numa-dell 'diff -u "/home/deparker/go/pkg/mod/golang.org/x/benchmarks@v0.0.0-20260819172200-70693762b6a0/driver/driver.go" ~/xbench-noprof/driver/driver.go' > numa-design/bench-data/v4-task1-noprof/driver-noprof.patch
```
Expected: exactly two hunks, four added lines + two moved calls, nothing else.

- [ ] **Step 4: Build both json binaries from the patched source with the tree toolchain; record `go version -m`.**

```bash
ssh numa-dell '
  set -e; cd ~/xbench-noprof
  export PATH=/home/deparker/go-numa/bin:$PATH GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=mod
  GOBIN=~/v4-bins/off  go install ./json
  GOBIN=~/v4-bins/numa GOEXPERIMENT=numa go install ./json
  go version -m ~/v4-bins/off/json
  go version -m ~/v4-bins/numa/json
'
```
Expected: both binaries report the tree's devel toolchain at the plan-commit SHA; the numa one shows `GOEXPERIMENT=numa` in build settings. Save both transcripts to `numa-design/bench-data/v4-task1-noprof/`.

- [ ] **Step 5: Sanity half-round (not counted): one run of each arm, confirm noprof arms emit user+sys-ns/op lines and write no cpuprof data.**

Run the sweep script with `N=1`, inspect the four `.out` files: every arm has `Benchmark` lines with `user+sys-ns/op`; delete the sanity output directory afterwards so measured raws start clean.

- [ ] **Step 6: Run the measured sweep (single session, n=10).**

```bash
ssh numa-dell 'BIN_OFF=~/v4-bins/off/json BIN_NUMA=~/v4-bins/numa/json OUT=~/v4-task1-out N=10 bash /home/deparker/go-numa/numa-design/v4-task1-noprof-sweep.sh'
```
~40 runs ≈ 40–80 min. Do not run anything else on the machine meanwhile.

- [ ] **Step 7: benchstat verdicts.**

Convert each arm's benchfmt output and compare (benchstat accepts the raw files):
- Primary: `benchstat off-noprof.out numa-noprof.out` → `user+sys-ns/op` row. PASS = ≤ +2% or ~ (not significant).
- Secondary: same pair `ns/op`; replication `benchstat off-prof.out numa-prof.out`; interaction estimate from the two deltas.

- [ ] **Step 8: Archive + RESULTS.md + commit.**

Copy the four raws, logs, benchstat outputs, patch, and `go version -m` transcripts into `numa-design/bench-data/v4-task1-noprof/`. Write the RESULTS.md section (verdict, all four arms' numbers, interaction estimate, ICC note if per-round variance warrants). Verify `numa_balancing=1` still. Commit:
```bash
git add numa-design/bench-data/v4-task1-noprof numa-design/RESULTS.md
git commit -m "numa-design: v4 Task 1 — 256P user+sys gate re-adjudicated with profiler-off harness"
make push
```

**Decision rule (pre-registered):** if the primary PASSes, the 256P user+sys hard gate is recorded as PASS-under-noprof-harness and all later 256P json gates in this plan use `BENCH_DISABLE_CPUPROF=1`; the prof-arm numbers stand as documentation of the harness artifact. If it FAILs, the residual delta (point estimate + CI) becomes stage 3's first attribution target, and stage 2 still proceeds (its own G2-cost gate will adjudicate the combined tree).

---

### Task 2: Stage-2 design round — P/goroutine node placement

**Files:**
- Create: `numa-design/v4-placement-design.md` (the design addendum: exact struct/fields, hook points with line references, procresize integration, steal two-pass, engagement predicate, test list)
- Modify: `numa-design/2026-08-26-numa-v4-placement-plan.md` (append Tasks 3..N — full implementation tasks with code, per locked decisions P1–P7)

**Interfaces:**
- Consumes: locked decisions P1–P7 (above), Task 1's verdict, `src/runtime/numa_linux.go` soft-affinity machinery (`numaNoteSchedule`, `numaApplySoftAffinity`, `m.numa` state), `src/runtime/mcentral.go` routing (`cacheSpan` node key), `procresize` in `src/runtime/proc.go`, `stealWork`/`runqsteal` in `src/runtime/proc.go`.
- Produces: appended implementation tasks (each with failing-test-first steps, exact files/lines, commit points) + the review verdict recorded in the design doc. **No runtime code is written in this task.**

- [ ] **Step 1: Write `numa-design/v4-placement-design.md`** covering, with code-level specificity: `pNUMAState` on/off files and the p-struct embed point; largest-remainder quota algorithm with the numa-dell worked example (256 Ps → 128/128; 200 → 100/100; 3 → 2/1) and the ≥1-P rule; `procresize` integration point (after P array is sized, before Ps are released); re-key of `numaNoteSchedule` (key source switch, throttle unchanged); `cacheSpan` key switch; `stealWork` two-pass with the exact loop restructure; `numaPlacementActive` predicate and its relationship to `numaConfined`/`numaStoodDown`; stand-down and `procresize` re-assignment races (P7's stale-but-safe argument written out against the actual spanSet invariants); the full test list (unit + testprog placement probes + metrics-based locality assertions).
- [ ] **Step 2: Adversarial design review** — fresh reviewer, self-contained prompt containing the design doc, locked decisions, the forbidden-list amendment, and the four P7 probe areas. Review verdict (with any required design edits applied) recorded at the bottom of the design doc.
- [ ] **Step 3: Append implementation Tasks 3..N to this plan** (bite-sized, code included, gates from G2 wired into the final task), commit plan amendment + design doc together:
```bash
git add numa-design/v4-placement-design.md numa-design/2026-08-26-numa-v4-placement-plan.md
git commit -m "numa-design: v4 stage-2 placement design locked and implementation tasks appended"
```

---

### Task L (stage 3, conditional): lock-callchain attribution

Scope, arms, method, and n are locked in the stage-3 decision block above. This task is instantiated (appended to this plan with concrete steps) only when a gate FAIL after stages 1–2 gives it a target; if all gates pass it collapses to one archived confirmation capture noted in RESULTS.md.

### Task P (stage 4): node-aware page allocation — design round then implementation

Same two-phase structure as Task 2 (design doc `numa-design/v4-pagealloc-design.md` per locked decisions P8–P10, adversarial review, then appended implementation tasks gated by G4). Not started until stage 2's decision gates are adjudicated.

---

## Self-review checklist (run at plan completion)

- Spec coverage: all four approved avenues have a stage; gates pre-registered for each. ✓
- No placeholders in Tasks 1–2 (Task L / Task P are explicitly amendment-gated, which is the pre-registration mechanism, not a placeholder). ✓
- Type/name consistency: `pNUMAState`/`pp.numa.homeNode`/`numaPlacementActive` used consistently across P1–P5 and Task 2. ✓
- Every measurement step names its session discipline, primary metric, and archive path. ✓
