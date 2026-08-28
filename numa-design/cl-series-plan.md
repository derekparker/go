# CL series plan (upstream-readiness gap item 5)

How the branch becomes a reviewable upstream series. The working branch's
~100-commit history is the *audit archive* (pre-registrations, gate
verdicts, red/green pairs) and is not what gets mailed: the series is
regenerated as clean, self-contained CLs from the final tree, each of which
compiles, passes tests, and keeps the off-build census clean on its own.

## Principles

- **One concept per CL**, reviewable top-to-bottom without the design
  corpus; each CL description carries its own rationale and the relevant
  measured numbers.
- **Off-build inertness is per-CL**: every CL in the series keeps the
  zero-function-diff census (the one deliberate exception is CL 0, a stock
  bug fix).
- **Tests land with the code they test**, not in a trailing tests-CL,
  except the cross-cutting hardware battery.
- **No internal review tags in comments.** Comments citing campaign
  artifacts ("review M2", "Task LF3", "design §12.3", "I5", "NEW-1") are
  rewritten as self-contained rationale before the series is cut — the
  referent documents won't exist for upstream readers. Inventory: 97 sites
  across 13 files (malloc.go, mcentral.go, mgcsweep.go, mheap.go,
  mpagealloc_numa.go, numa_linux.go, numa_linux_affinity.go,
  numa_growhighwater_on.go, numa_mstate_on.go, numa_wake.go, os_linux.go,
  proc.go, stubs_nonlinux.go), plus test files.

## CL 0 — preliminary stock fix (independent of the experiment)

- `runtime: fix randomized-heap-base prefix mask misalignment` — the
  `randHeapBasePrefixMask`/`randHeapAddrBits` fix plus
  `TestArenaHintChainGeneration`. Mailable immediately, no proposal needed;
  the series is easier to review with sane hint chains as a precondition.

## Tranche 1 — topology, balancer exemption, confinement (~7 CLs)

1. `internal/runtime/numa: NUMA topology discovery on Linux` — sysfs
   parsing, node/CPU structures, unit tests with canned sysfs trees.
2. `runtime: linux syscall plumbing for mbind/set_mempolicy/getcpu` — all
   13 linux GOARCHes, fixed-width maxnode=65 nodemask convention, no
   callers yet.
3. `runtime: GOEXPERIMENT=numa scaffolding and GODEBUG=numa diagnostics` —
   experiment wiring, build-tagged collapse constants, decision logging.
4. `runtime: exempt the heap from automatic NUMA balancing (MPOL_BIND-all)`
   — per-chunk VMA policy at growth; the v2 measurements in the
   description (0 vs 2.78M hint faults; the ~5% balancer-friendly trade).
5. `runtime: cover pre-runtime and cgo threads` — policy application
   ordering so the exemption holds against every thread.
6. `runtime: fill-one-socket confinement` — boot-node confinement when
   GOMAXPROCS fits one node, operator-placement detection, one-way
   stand-down; −30% numbers in the description.
7. `runtime: NUMA hardware test battery + testprogs` — TestNUMA suite,
   soft-affinity/confinement testprog children.

## Tranche 2 — full-machine placement (~7 CLs, after tranche 1 lands)

8. `runtime: per-node heap arena hint streams` — arenaHints/curArena as
   per-node arrays, growth keyed by node, I5-style tight-VA fallback.
9. `runtime: per-node address windows in the page allocator` — window
   inference from the hint layout, windowed searchAddr maintenance,
   `allocNode`/`allocToCacheNode`/`findFrom`, the latch, and the churn
   property test + regression suite.
10. `runtime: route span allocation by P home` — allocSpan routing ladder
    (windowed → homed-grow-once → retry-once → unconditional fallback),
    windowed page-cache fill with homed grow on miss.
11. `runtime: node-keyed mcentral span recycling` — remote partial/full
    spanSet block keyed by backing node, sweep routing; mcentral keeps its
    stock shape (the measured-null-but-structural extraction).
12. `runtime: NUMA P homes` — pNUMAState, largest-remainder assignment at
    schedinit/procresize, placement predicate, numaNoteSchedule.
13. `runtime: soft NUMA thread affinity` — enforcement apply/widen paths,
    epoch bookkeeping.
14. `runtime: adaptive enforcement stand-down` — M-wake counting, sysmon
    window evaluation, sustainment detector (2048 wakes/s × 8×100 ms),
    cooldown/trip-cap, `GODEBUG=numaenforce`, calibration story in the
    description.

## Description boilerplate each CL carries

- What it does and why, self-contained.
- Benchmark deltas measured for that layer (from the archived gates), with
  machine description and n.
- Census statement: "experiment-off binary unchanged (per-function census)."

## Remaining mechanical steps to cut the series

1. Comment hygiene pass over the 97 tagged sites (tracked separately;
   comment-only, census-verifiable as zero-diff in both build modes).
2. `git diff master...HEAD` partitioned into the CLs above; each applied to
   a clean branch in order, building and testing at every step.
3. Per-CL census run (script exists: objdump address-stripped function
   diff).
