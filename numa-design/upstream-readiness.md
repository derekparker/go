# Upstream readiness: can this branch back a Go proposal?

Assessment date: 2026-08-27, tree at Task-LF close (post-A5, post-LF3).
Author context: this document is the honest self-assessment the proposal
itself must survive; every number below has a pre-registered gate, an
archived raw, and a session record in `RESULTS.md`.

## Verdict

**Yes — as the basis for a staged GOEXPERIMENT proposal, presented as a
measured, working solution with disclosed costs. Not yet as a
finished, file-it-tomorrow proposal**: the gap list below is short but real,
and every item on it is the kind upstream reviewers find on day one.

The strongest asset is not any single number — it is the evidence
discipline: pre-registered gates with frozen bars, adversarial design
reviews with recorded verdicts, a reproducible pathology harness, honest
FAIL attribution (three separate cost regressions were bisected to root
cause rather than argued away), and an off-build that is provably inert
(zero-function-diff census at every commit). That is precisely the shape of
case runtime maintainers ask for and rarely get.

## What the branch delivers (the proposal's payload)

1. **Layer 0/1 — topology + balancer exemption.** NUMA topology discovery;
   per-chunk `MPOL_BIND`-all VMA policies that exempt the Go heap from the
   kernel NUMA balancer against every thread including pre-runtime cgo
   threads. Measured: **0 balancer hint faults vs 2.78M** for stock over the
   same work, while faster. Syscall plumbing for all 13 linux GOARCHes.
2. **Fill-one-socket confinement (WS-A).** Explicit GOMAXPROCS ≤ one node ⇒
   confine to the boot node, one-way stand-down, operator placement always
   wins. Measured: **−29.6%…−33.1%** wall vs stock unpinned at ≤node width;
   non-inferior to `numactl` pinning.
3. **P-home placement + windowed page allocation (v4).** Ps partitioned
   across nodes; refill routing and heap growth keyed by P home; per-node
   address-windowed page allocation. Measured: unpinned span-refill locality
   **53–75% → 93–97%** at every width; **−8.1% wall (p=0.000)** on the
   4 GiB / 256P pathological workload — the regime confinement cannot help.
4. **Adaptive enforcement (A5).** Thread-affinity enforcement with a
   calibrated wake-storm detector (2048 wakes/s sustained 8×100ms windows),
   epoch-based re-arm, bounded oscillation, `GODEBUG=numaenforce` override.
   Measured: **both** previously-conflicting gates pass on one tree — the
   −8.1% is preserved AND all four scheduler micros are statistically
   indistinguishable from stock.
5. **Safety story.** Experiment off: byte-identical function census, `p`/`m`
   struct sizes unchanged, no behavior delta. Experiment on, non-NUMA or
   pinned hosts: every feature declines itself (single-node, narrowed
   affinity, truncated topology, missing syscalls). Regression tests for
   every bug found on the way, including a page-allocator invariant
   property test.

## Costs the proposal must disclose (all attributed, none hidden)

- **ON-build 1P alloc micro: +5.5% geomean at the current node capacity
  (N=8).** Fully attributed: a dispersed, ~linear-in-N structural footprint
  (+1.9% floor at N=1, +3.5% at N=4); no single extraction recovers it (two
  measured-null refactors say so). The capacity constant is an explicit
  proposal decision point. The 1P *real-workload* gate (json) reads +1.35%.
- **Layer-1 BIND-all costs ~5% on balancer-friendly unconfined workloads**
  (v2 finding) — the trade the whole design makes deliberately.
- **DRAM remote-share counters barely move** (−0.4…−4.4% vs a 10% aspiration)
  and the proposal must NOT claim them: the proxy is dominated by
  shared-runtime-state loads no allocator can move. The claims are wall
  time and direct refill-locality counters.
- 1P STW/GC-metadata exploratory growth (tens of µs absolute; stream
  metadata) — flagged, not yet chased.

## Gap list before filing (ordered; the proposal is credible only with these)

1. **Resolve the window-run inference anomaly** — ✅ RESOLVED 2026-08-27:
   a baseline stock-Go bug (`randHeapBasePrefixMask` misaligned with the
   randomized prefix position leaked 2 random bits into the prefix byte,
   collapsing distinct hints to duplicates on ~75% of launches). Fixed with
   regression test `TestArenaHintChainsSane`; now a standalone CL 0 that
   leads the series. See RESULTS.md "Upstream gap item 1".
2. **Second (and ideally third) hardware platform.** Every number is from
   one 2-node Sapphire Rapids box. Minimum credible: one arm64 multi-node
   and/or one 4-node x86; re-run the pathology harness + locality sweep
   there. The harness makes this mechanical.
3. **Full `all.bash` + trybot-equivalent sweep** on the final tree (the
   branch has run targeted batteries; upstream needs the whole suite, both
   build modes).
4. **Decide the node-capacity constant** — ✅ DECIDED 2026-08-27 (user
   approved N=4): frozen at `numaMaxHeapNodes = 4` with pre-registered
   verification gates N4-G1..G5 (v4 plan); N=8 documented as a build-time
   variant.
5. **Series hygiene pass**: squash red/green and fix-wave pairs, rewrite
   comments that cite internal review tags ("review M2", "Task LF3") into
   self-contained rationale, and split into a reviewable CL series — the
   v3 final review already prescribed the first tranche (Layer 0/1 + WS-A
   as ~7 CLs); v4 placement/windows/A5 is a second tranche that can trail.
6. **Chase the two unreproduced local test flakes** (or at minimum convert
   them into tracked issues with the captured context).
7. Proposal mechanics — ✅ DRAFTED 2026-08-27: distilled design doc at
   `proposal-draft.md` (staging plan and non-goals included); CL series
   breakdown at `cl-series-plan.md`.

## Recommended proposal shape

- **Track: GOEXPERIMENT proposal** ("GOEXPERIMENT=numa: NUMA-aware runtime
  for Linux"), off by default, no exported API, GODEBUG knobs only —
  the lowest-friction track for runtime changes of this size, with
  default-on as an explicitly out-of-scope future step.
- **Framing: two independently valuable halves.** (a) Confinement +
  balancer exemption — small, self-contained, big measured win for the
  "service pinned to a socket" deployment; (b) full-machine placement —
  larger, the −8% at 256P. The staging lets (a) land while (b) is reviewed.
- **The pitch paragraph writes itself from the gates**: stock Go on a
  2-node box burns millions of NUMA-balancer faults to achieve 50–75%
  allocation locality; this experiment achieves 93–97% locality with zero
  balancer work, 8% faster at full width and 30% faster confined, costs
  nothing when off, and detects-and-yields in the one regime where its
  enforcement would hurt.

## Bottom line

The implementation is a workable solution and the measurement corpus is
proposal-grade. The remaining work is validation breadth and packaging, not
design: roughly items 1–4 above before filing, 5–7 alongside the filing.
