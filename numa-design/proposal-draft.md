# Proposal draft: `GOEXPERIMENT=numa` — a NUMA-aware Go runtime for Linux

Status: DRAFT for upstream filing (gap item 7 of `upstream-readiness.md`).
This is the distilled design document; the full working corpus (pre-registered
plans, raw benchmark archives, review records) lives in `numa-design/` on the
implementation branch and backs every number cited here.

## Summary

Stock Go on a multi-socket Linux machine leaves memory locality entirely to
the kernel's NUMA balancer: the heap's pages migrate reactively, threads and
the memory they touch drift apart, and the balancer burns hint faults and page
migrations chasing an allocator that gives it no structure to work with. On a
2-node, 256-CPU host, one full run of a GC-heavy benchmark costs the kernel
**2.78 million NUMA hint faults**; span-refill locality (how often a P's new
span is backed by memory on its own node) sits at **50–75%**.

This proposal makes the runtime NUMA-aware behind `GOEXPERIMENT=numa`,
off by default, Linux-only, with no new API. With the experiment on, the same
machine reaches:

- **93–97% span-refill locality at every GOMAXPROCS width, with exactly 0
  NUMA-balancer hint faults** — the heap is exempted from the balancer
  because the runtime already put pages where they belong;
- **−8 to −9% wall time** on the pathological full-width GC workload
  (4 GiB live heap, GOMAXPROCS=256, p≤0.005);
- **−30 to −33% wall time** when `GOMAXPROCS` fits one node and the process
  is not externally pinned (the runtime confines itself to one socket,
  matching hand-tuned `numactl` non-inferiorly);
- scheduler microbenchmarks statistically indistinguishable from stock, via
  an adaptive detector that stands enforcement down under sustained wake
  storms — the one regime where NUMA thread affinity hurts.

With the experiment off, the build is **provably inert**: a per-function
binary census (objdump, address-stripped) shows zero changed functions
against the same tree without the patches, at every commit.

## Background and problem

Three mechanisms lose locality in stock Go on NUMA hardware:

1. **The page allocator is node-blind.** `pageAlloc.find` returns the
   lowest free address; which node backs it is an accident of first-touch
   and balancer history. A P on node 1 refilling its mcache gets whatever
   span is cheapest in address order, not in distance.
2. **Threads drift.** The scheduler moves Ms and steals Gs with no notion
   of where the G's heap is. The kernel balancer then migrates pages to
   chase threads (or threads to chase pages), which is expensive, reactive,
   and permanently behind.
3. **The GC redistributes.** Sweep and reuse recycle spans across the
   machine, so even memory that started local diffuses.

The kernel's automatic NUMA balancing is the only mitigation, and it costs
real work: hint faults are minor page faults over the whole heap, taken
repeatedly, forever.

## Design

Four layers, each independently valuable, each declining itself when its
preconditions fail (non-NUMA host, pinned process, missing syscalls,
truncated topology):

### Layer 0/1 — topology discovery and balancer exemption

At startup the runtime reads the NUMA topology (nodes, CPU masks) from
sysfs. Every heap chunk's VMA gets an `MPOL_BIND` policy over **all**
allowed nodes via `mbind(2)`. A bind-to-everything policy changes no
placement decision, but it opts those VMAs out of automatic NUMA balancing
— against every thread, including cgo threads created before the runtime
initialized. The runtime is asserting: *I will handle locality; stop paying
hint faults on my heap.* (Nodemask width is fixed at maxnode=65 across all
13 linux GOARCHes; the syscall plumbing is part of this layer. The process
is never bound to a single node this way — see non-goals.)

Measured alone (v2 campaign): eliminates balancer traffic wholesale; on
balancer-*friendly* unconfined workloads this trades ~5% (the balancer was
genuinely helping there), which the later layers win back with interest.

### Fill-one-socket confinement

If `GOMAXPROCS` fits within one node's CPU count and the operator has not
pinned the process (the inherited affinity mask is full-machine), the
runtime confines itself to the boot node: threads and memory on one socket
instead of scattered across all of them. One-way stand-down: any sign of
operator placement (narrowed affinity, cpusets) and the runtime defers.
Measured: **−29.6% to −33.1%** wall vs stock unpinned at ≤node width,
non-inferior to explicit `numactl --cpunodebind --membind`.

### Full-machine placement (P homes + per-node heap streams)

When GOMAXPROCS spans nodes:

- **P homes.** Ps are partitioned across nodes proportionally to each
  node's CPU count (largest-remainder), at schedinit and again on
  `GOMAXPROCS` changes. An M running a P is softly affine to the P's home
  node.
- **Per-node heap arena streams.** The heap's arena hint chain becomes
  per-node: growth for node n's Ps lands in node n's address stream, in
  disjoint multi-TiB address windows derived from the hint layout at
  startup (never assumed: the randomized-heap-base layout is
  measured, and a window is inferred from the longest constant-spacing
  hint run).
- **Windowed page allocation.** `pageAlloc` gets per-node search addresses
  over those windows; a P's span refill and page-cache fill search its home
  node's window first, growing the heap homed on a miss, with a bounded
  fallback ladder (windowed → homed-grow-once → retry → unconditional
  fallback) so allocation can never fail or spin for locality's sake. A
  per-node latch drops a node to the shared path permanently if its window
  is exhausted or unusable — locality is best-effort, correctness is not.
- **Node-keyed span recycling.** mcentral's partial/full sets are keyed by
  the backing node (remote sets live in one cold global block; the
  mcentral struct itself keeps its stock shape), so sweep returns spans to
  their node and refills prefer local spans. First-touch does the rest: a
  page first touched by a node-homed thread is faulted on that node.

Measured (2-node/256-CPU): refill locality **53–75% → 93–97%** at every
width (steady-state protocol, 25/25 launches ≥90%); the full-width
pathological GC workload improves **−8.1% to −8.8%** wall (p≤0.005 across
three independent sessions); peak RSS unchanged.

### Adaptive enforcement (the safety valve)

Soft thread affinity narrows wake choices, which adds OS-level wake latency
in exactly one regime: sustained cross-CPU wake storms (a herd of Ms
parked/woken at high frequency). The runtime counts scheduler M-wakes and,
from sysmon, evaluates an elapsed-normalized rate; **2048 wakes/s sustained
over 8 consecutive 100 ms windows** stands enforcement down (affinity
widened, placement bookkeeping kept), with epoch-based re-arm, a 10 s
cooldown and a lifetime trip cap. Calibrated from full-trace wake-rate
distributions: GC-heavy workloads *burst* above the rate but never sustain
it; genuine storms sustain it and trip within ~800 ms. With this in tree,
all four scheduler microbenchmarks are statistically indistinguishable from
stock **and** the −8% full-width win is preserved — the previously
conflicting gates pass on one tree.

`GODEBUG=numaenforce=0/1/2` (auto / always-on / always-off) overrides the
detector; `GODEBUG=numa=1` (and `=2`) prints decision and detector
diagnostics.

## What it costs (disclosed, all attributed)

- **ON-build 1P allocation microbenchmark: ~+3.5% geomean** at the shipped
  node capacity (N=4). This is a dispersed structural footprint — arrays
  and loops sized by the compile-time node capacity on hot allocation paths
  — measured ~linear in that capacity (+1.9% at N=1, +3.5% at N=4, +5.5%
  at N=8) and not recoverable by refactoring (two targeted refactors
  measured null). The capacity constant is the explicit knob; N=4 covers
  1–4-node deployments and larger boxes can rebuild with N=8. The 1P
  *real-workload* proxy (encoding/json benchmark) measured +1.35% at N=8.
- **Balancer-friendly unconfined workloads pay ~5%** for the Layer-1
  balancer exemption (v2 finding) — the deliberate trade the design makes;
  the placement layers exist to win it back structurally.
- **DRAM remote-access hardware counters barely move** (−0.4…−4.4%
  relative) despite 93–97% refill locality, because that proxy is dominated
  by loads to shared runtime state that no allocator placement can move.
  This proposal claims wall time and direct refill-locality counters, not
  DRAM counter reductions.

## Safety and compatibility

- **Experiment off: provably inert.** Zero-function-diff binary census at
  every commit; `p`/`m` struct sizes unchanged; per-node arrays collapse to
  size 1 via build-tagged constants (no BSS growth in off builds).
- **Experiment on, wrong environment: every feature declines itself.**
  Non-NUMA host, single populated node, pre-narrowed affinity mask,
  cpusets, CPU-bearing node ids beyond the stream capacity, missing
  syscalls, tight-VA layouts (race mode, riscv64/39-bit) — each condition
  independently stands the relevant layer down, logged under
  `GODEBUG=numa=1`.
- **Operator placement always wins.** Any externally narrowed affinity is
  honored and never widened; confinement is one-way (stand down, never
  re-confine).
- Every bug found during development has a regression test, including a
  page-allocator invariant property test (randomized mixed windowed/global
  operation churn with a full invariant scan per step) that reproduces the
  hardest crash class in milliseconds.

## Staging plan

**Tranche 1 — confinement + balancer exemption.** Layer 0/1 and
fill-one-socket confinement: small, self-contained, no allocator changes,
−30% for the "service sized to a socket" deployment. Reviewable as a short
CL series (topology, syscall plumbing, policy application, confinement,
tests).

**Tranche 2 — full-machine placement.** P homes, per-node streams, windowed
page allocation, node-keyed mcentral, adaptive enforcement. Larger, and
independently gated by tranche 1's plumbing having landed.

A standalone preliminary fix discovered by this work leads the series: the
randomized-heap-base prefix mask is misaligned with the prefix position in
stock Go (`randHeapBasePrefixMask` uses `heapAddrBits` where the prefix is
placed at `randHeapAddrBits`), producing duplicate/non-monotonic arena
hints on ~75% of launches. Harmless in stock (hints are fallbacks), but a
real bug with a one-line fix and a regression test, independent of NUMA.

## Non-goals

- **No new public API.** All control is GODEBUG; `GOEXPERIMENT=numa` gates
  compilation. Graduation to default-on is explicitly out of scope for this
  proposal.
- **Not a memory-policy surface.** The runtime never binds the process or
  heap to a single node (`MPOL_BIND`-all only, plus first-touch); no
  user-visible placement control is added or planned.
- **Linux-only initially.** The design isolates OS specifics (topology
  discovery, mbind, affinity) behind per-OS files; other OSes are future
  work, not stubs-with-behavior.
- **No scheduler rewrite.** P homes bias existing mechanisms (wake,
  steal order untouched in the final tree); the scheduler's structure is
  unchanged.

## Validation status and open items

Everything above is measured on a 2-node Sapphire Rapids 8592+ (256 CPU)
host under a pre-registered gate discipline (frozen bars before
measurement, benchstat Mann–Whitney authoritative, single-session
interleaved rotating-order sweeps, raws archived). Remaining before filing:

1. ~~Window-run inference anomaly~~ — resolved: baseline prefix-mask bug,
   fixed with regression test (see staging plan).
2. Second hardware platform (arm64 multi-node and/or 4-node x86) — the
   harness makes this mechanical; not yet run.
3. Full `all.bash` in both build modes on the final tree — in progress.
4. ~~Node-capacity constant~~ — frozen at N=4 with pre-registered
   verification gates (N4-G1..G5).
