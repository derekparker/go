# Proposal: `GOEXPERIMENT=numa` — a NUMA-aware Go runtime for Linux (series edition)

This is the filing-ready proposal for the `numa-cl-series` branch: what the
series contains commit by commit, why the work exists, the upstream issues
it addresses, and the prior work it builds on. The distilled design
rationale lives in `proposal-draft.md`; the measurement corpus and audit
trail live in `RESULTS.md` and `bench-data/`.

## Motivation

Go currently delegates NUMA locality entirely to the Linux kernel's
automatic NUMA balancer. The runtime allocates heap pages wherever the page
allocator's address-ordered search lands, schedules Ms with no notion of
where a G's memory lives, and recycles spans across the whole machine
through node-blind central free lists. On multi-socket hardware the
consequences are measurable and severe:

- A GC-heavy workload at full width (256 CPUs, 2-node Sapphire Rapids)
  makes the kernel take **2.78 million NUMA hint faults** per benchmark
  run — minor page faults over the whole heap, repeatedly, forever — to
  claw back locality the allocator never provided. Span-refill locality
  (a P refilling its span cache from same-node memory) still only reaches
  **50–75%**.
- A Go process sized to a single socket (`GOMAXPROCS` ≤ one node) but not
  externally pinned scatters across sockets and runs **~30% slower** than
  the same binary under `numactl --cpunodebind --membind`.

This is not new: the earliest golang-dev NUMA threads and Dmitry Vyukov's
2014 NUMA-aware scheduler design describe the same structural blindness,
and the recently filed golang/go#78044 reports up to 2× degradation on
modern multi-node EPYC hardware and names the same causes this series
addresses (node-blind free lists, node-blind work distribution, cross-node
migration). The series makes the runtime NUMA-aware behind an off-by-default
`GOEXPERIMENT=numa`, Linux-only, with no new public API, and with the
experiment off it is provably inert (per-function binary census at every
commit of the series).

Measured on the reference hardware with the experiment on: refill locality
**93–97% at every width with exactly 0 balancer hint faults**, **−8 to
−10%** wall time at full width on the pathological GC workload (p ≤ 0.005),
**−30 to −33%** for the confined single-socket case, RSS flat, and
scheduler microbenchmarks statistically indistinguishable from stock.

## Associated issues

- **golang/go#78044 — runtime: degraded performance on multi-NUMA-node
  machines** (open, 2026-03). The direct motivating issue. Reports ~2×
  penalties on multi-node hardware; identifies the global free list and
  node-blind work distribution; floats node-segregated free lists and
  preferential node allocation with fallback — which is what CLs 8–11
  implement (per-node heap streams, windowed page allocation with a
  bounded fallback ladder, node-keyed mcentral), without the user-facing
  NUMA API the issue reporter listed as the less-preferred alternative.
- **golang/go#12298 — runtime: NUMA optimization for channels** (2015,
  backlog). Early request for NUMA placement; this series' first-touch +
  P-home model addresses the underlying locality need without
  channel-specific machinery.
- **golang/go#73193 — runtime: CPU limit-aware GOMAXPROCS default**
  (landed, Go 1.25). Precedent: the runtime adapting itself to its machine
  and container environment automatically, with no API, superseding a
  userland workaround (`go.uber.org/automaxprocs`). The series' topology
  discovery and fill-one-socket confinement follow exactly this shape —
  and always defer to explicit operator placement.

## Prior work this series builds on

- **Dmitry Vyukov, "NUMA-aware scheduler for Go" (2014 design doc,
  golang-dev)**: P↔node binding, per-node run queues and M pools,
  node-preferring steal order. Never implemented. This series keeps its
  central insight — bind Ps to nodes and let memory follow — but
  deliberately avoids the scheduler rewrite: run queues, wake ordering and
  steal order are untouched (a node-filtered steal was prototyped and
  deleted when ablation showed it contributed nothing), and thread
  affinity was prototyped, measured, and dropped the same way: the
  2026-09-04 ablation showed the memory layers carry the win without it
  (+2.4% wall, nil user+sys, locality unchanged; RESULTS.md).
- **CL 714801, "runtime: prefer to restart Ps on the same M after
  STW" (Michael Pratt, for #65694, Go 1.26)**: the scheduler's first
  step toward stable M/P affinity, with "a more general affinity for
  specific Ms" explicitly named as future work. It is in this series'
  baseline (it predates the fork point), so every measured delta is on
  top of it. The series leans on exactly that stability instead of
  shipping kernel thread affinity of its own: the 2026-09-04 ablation
  showed the pairing plus kernel wake-place locality keeps threads on
  their memory's node without any `sched_setaffinity` (see the
  revision note under The Series).
- **Linux automatic NUMA balancing**: the mechanism stock Go leans on
  today. The series treats it as the baseline to beat and to exempt: a
  `MPOL_BIND`-to-all-nodes VMA policy on every heap chunk changes no
  placement decision but opts the heap out of hint faulting entirely,
  because the runtime now does the placement itself.
- **The Go pathology-measurement discipline built for this work** (branch
  `claude/numa-v2-implementation-a6eb4c`): three campaign generations
  (v2 balancer exemption → v3 locality plumbing → v4 placement + windows +
  adaptive enforcement), every gate pre-registered with frozen bars before
  measurement, benchstat Mann–Whitney authoritative, raw archives and
  adversarial design reviews retained. The series is the distillation of
  that branch; the branch is the evidence.
- **A stock bug found on the way**: the randomized-heap-base prefix mask
  is misaligned with the prefix position in today's master, producing
  duplicate arena hints on ~75% of launches (branch
  `fix-randomized-heap-base-mask`, also CL 0 of this series) — independent
  of NUMA and worth landing first.

## The series

**Revision 2026-09-08 -- enforcement dropped.** The soft-affinity
ablation and real-workload storm probe (RESULTS.md 2026-09-04) showed
kernel thread affinity contributes +2.4% wall and nothing on the
user+sys primary on the flagship gate, with refill locality unchanged
without it, while an ordinary net/http server trips the wake-storm
detector immediately -- the machinery ships only to disable itself on
the most common deployment shape. The upstream series therefore drops
CLs 13 (soft NUMA thread affinity) and 14 (adaptive enforcement
stand-down), ending at node-keyed mcentral: 13 commits total. The
table below still lists all 15 as they exist on the current
`numa-cl-series` branch; the branch rebuild that removes the
enforcement code (and the matching implementation-branch change, gate
re-runs on the affinity-free tree included) is the next mechanical
step.

Branch `numa-cl-series` (pushed to the lab Forgejo), 15 commits from base
`8058a57773` (upstream master at the campaign fork point). Every commit
individually passes a full `make.bash`, the runtime NUMA/pagealloc test
battery in both build modes, gofmt, and a per-function experiment-off
binary census (zero changed functions; CL 0's stock fix is the sole
intended baseline delta). The end state is byte-identical to the
implementation branch's `src/` tree.

**CL 0 — standalone stock fix (independent of the experiment; also on
branch `fix-randomized-heap-base-mask` for pre-Gerrit review):**

| # | Commit | Title | Size |
|---|---|---|---|
| 0 | `54996ae7af` | runtime: fix randomized heap base prefix mask misalignment | 3 files, +55/−8 |

**Tranche 1 — topology, balancer exemption, confinement** (independently
valuable; lands the −30% single-socket win with no allocator changes):

| # | Commit | Title | Size |
|---|---|---|---|
| 1 | `c3d93b98f3` | internal/runtime/numa: NUMA topology discovery on Linux | 7 files, +857 |
| 2 | `e4fdc29e98` | runtime: linux syscall plumbing for mbind, set_mempolicy, and getcpu | 22 files, +304/−130 |
| 3 | `a1b385ff3a` | runtime: GOEXPERIMENT=numa scaffolding and GODEBUG=numa diagnostics | 8 files, +97 |
| 4 | `50f2b52f8d` | runtime: exempt the heap from automatic NUMA balancing with a BIND-all task policy | 1 file, +160 |
| 5 | `911ea8c5fa` | runtime: per-chunk VMA policy so the balancer exemption covers pre-runtime and cgo threads | 3 files, +105 |
| 6 | `01653a3de2` | runtime: fill-one-socket NUMA confinement with one-way stand-down | 8 files, +787/−10 |
| 7 | `88bb60654d` | runtime: NUMA hardware test battery and testprogs | 5 files, +506 |

**Tranche 2 — full-machine placement** (lands the 93–97% locality and
the −8..−10% full-width win):

| # | Commit | Title | Size |
|---|---|---|---|
| 8 | `6e89caccbe` | runtime: per-node heap arena hint streams | 13 files, +1109/−113 |
| 9 | `d854271871` | runtime: per-node address windows in the page allocator | 10 files, +1372/−50 |
| 10 | `d89bd0d979` | runtime: NUMA P homes | 9 files, +483/−13 |
| 11 | `e291adcb73` | runtime: route span allocation by P home | 5 files, +247/−9 |
| 12 | `891fcf86a8` | runtime: node-keyed mcentral span recycling | 10 files, +1381/−116 |
| 13 | `eec40813ac` | runtime: soft NUMA thread affinity *(dropped from the upstream series, 2026-09-08)* | 15 files, +1610/−81 |
| 14 | `e83443fe12` | runtime: adaptive NUMA enforcement stand-down *(dropped from the upstream series, 2026-09-08)* | 10 files, +441/−7 |

Ordering note vs the original plan: P homes moved ahead of span-routing
and mcentral keying (they consume the P-home key), and the per-chunk VMA
policy CL follows the task-policy CL — both re-orderings exist so each
commit is independently buildable and testable.

## What the series deliberately does not do

- No public API, no default-on, no non-Linux implementations (isolated
  behind per-OS files), no scheduler restructuring, no single-node memory
  binding of the process, and no claims about DRAM remote-access hardware
  counters (measured insensitive: dominated by shared runtime state).

## Disclosed costs

- ON-build 1P allocation micro ~+3.5% geomean at the shipped node
  capacity (N=4): a dispersed structural footprint, ~linear in the
  compile-time capacity constant (+1.9% N=1, +5.5% N=8), not recoverable
  by refactoring (two measured-null attempts); real-workload 1P proxy
  +1.35% (measured at N=8).
- The balancer exemption costs ~5% on balancer-friendly unconfined
  workloads — the deliberate trade the placement layers win back.

## Validation status

- Full `all.bash` green in both build modes on the implementation tree.
- Every series commit: full `make.bash`, targeted runtime batteries in
  both modes, gofmt, and a per-function experiment-off census (zero
  changed functions; CL 0's stock fix is the sole intended baseline
  delta).
- Remaining gap before filing: a second hardware platform (all numbers are
  from one 2-node Sapphire Rapids host; the harness makes the re-run
  mechanical).
