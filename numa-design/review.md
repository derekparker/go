# NUMA topology and confinement review

This review compares the Obsidian project note “Go runtime NUMA topology
and confinement,” the `numa-topology-confinement` branch, and the retained
benchmark record as of 2026-09-14.

## Conclusions

The topology and confinement idea is viable as an experiment, but the
original eight-commit branch was not ready to propose as written.

The principal problem was that it applied a BIND-to-all-nodes task policy
at startup and a BIND-to-all-nodes VMA policy during heap growth even when
confinement declined. The benchmark record itself measures a 5–9%
regression from that exemption on balancer-friendly, unconfined work.
This contradicted the note's stated requirement to gate the exemption on
confinement.

The branch also replaced an operator's existing memory policy, retained
VMA policies after stand-down, allocated a 64 KiB cache of node CPU masks
for a decision made once, parsed and retained an unused NUMA distance
matrix, and used an eager all-M affinity walk whose own comment admitted
that a stale TID could refer to an unrelated thread.

The working-tree revision addresses those issues by:

* applying memory policy only after confinement eligibility is known;
* declining when the initial task memory policy is not `MPOL_DEFAULT`;
* using only `MPOL_PREFERRED` for the selected node;
* removing heap-path `mbind` calls and all persistent VMA policies;
* restoring `MPOL_DEFAULT` on stand-down;
* removing the unsafe eager all-M affinity walk;
* constructing the selected node's CPU mask once instead of retaining a
  64 KiB cache;
* removing the unused distance matrix and parser; and
* removing `NumAllowedNodes`, whose implementation always meant “online
  nodes” and therefore did not match its name.

The net review change removes substantially more code than it adds and
removes policy work from the heap growth path.

## Corrections to the project note

The following statements in the note should not be carried into an
upstream proposal without qualification:

* **“runs as if under `numactl`”** is true only for runtime-created
  threads. A thread created by a C constructor before runtime
  initialization does not inherit the runtime's affinity or task memory
  policy.
* **“kernel balancer exemption”** is no longer a separate unconditional
  layer. It is a consequence of the explicit `MPOL_PREFERRED` task policy
  while confinement is active. Pre-runtime foreign threads remain under
  their prior policy.
* **“fill one socket”** should be “confine to one NUMA node.” Linux NUMA
  nodes and physical sockets are not interchangeable on every machine.
* The two 29.6% and 33.1% confined results are useful motivation, not
  proposal-grade final evidence. They measured wall time only, predate the
  reduced branch, and come from one machine.
* The recorded +1.0% single-node cost was measured on the former full
  tree, not this reduced phase-1 branch.
* The `numa-gate` tool is present in another local checkout at
  `~/Code/golang-fips/go/numa-gate`, not at the path recorded in the note.
  The note should name the repository containing the tool rather than a
  machine-specific skill-installation path.

## Validation

The branch was rebased onto upstream Go commit `116291c88a` and the
reviewed tree was benchmarked as `ba2d41763e`.

### Functional and build checks

The experiment-on toolchain builds on Darwin/arm64, and these tests pass:

```
GOEXPERIMENT=numa go test internal/runtime/numa runtime
```

The experiment-off runtime tests pass as well, and the Linux/amd64
experiment-on runtime test binary cross-compiles.

On the two-node Linux host, the complete `internal/runtime/numa` and
`runtime` test suites pass in both build modes. All nine NUMA-specific
tests pass, including live confinement, operator affinity and memory-policy
precedence, and both stand-down paths. An unrelated `TestCgoNoEscape`
heap-count assertion failed once in an interrupted run and passed in the
complete rerun.

The experiment-off function census compares a minimal non-test binary
built from upstream and reviewed toolchains. Both contain 1,432 `TEXT`
symbols, with identical symbol sets and per-function instruction counts.
No linked symbol contains `numa`. A stricter disassembly comparison still
finds operand differences caused by shifted runtime-global addresses, so
byte-identical machine code is not claimed. The concise result is in
`off-census.txt`.

The experiment-on build's feature-declined allocation cost was measured
on a single-node Darwin/arm64 host with 20 alternating samples per arm.
`Malloc8` was 0.29% slower and `Malloc16` was 0.94% slower, for a 0.62%
geomean increase, below the 2% threshold. This is a provisional
structural-cost result: non-Linux NUMA behavior is a no-op, and only the
summary rather than raw samples was retained in
`single-node-alloc.txt`.

### Performance gates

All gates used 10 rotating recorded rounds after a warmup and collected
wall time, cycles, instructions, effective clock, CPU utilization, and
NUMA counters.

The benchmark tree was `ba2d41763e`. The final reviewed source adds one
post-gate fail-closed check for CPU IDs ≥8192. The measured host uses IDs
0–255, so that check does not change any executed path in these gates.

* **Confined garbage, 128P:** an externally pinned HWP session improved
  wall time 21.79%, cycles 16.75%, user plus system CPU time 16.30%, and
  STW time 19.83% (all p≤0.001). Effective clock differed by 1.01%, within
  the gate's 2% limit, and RSS was null. The workload kept only 40–42 CPUs
  busy. Nine of ten experiment runs had zero host-wide hint faults and
  migrations; one recorded 183 faults and 182 migrations, compared with
  about 174,000 faults per stock run. The gate's mechanism threshold
  passed, but the project note's stricter exactly-zero-every-run criterion
  did not. An earlier unpinned session also improved cycles 20.12%, but
  its wall result remains invalid because clock differed by 12.29%.
* **Unconfined garbage, 256P:** wall improved 3.15% (p=0.003) and cycles
  improved 4.74% (p=0.019) at matched clock. STW and RSS were null. The
  experiment correctly retained automatic balancing because confinement
  declined. The workload kept only 49 CPUs busy.
* **Unconfined JSON, 256P:** cycles, instructions, and STW were
  statistically null, with high run-to-run variance and only 46–49 CPUs
  busy. Its wall result is invalid because clock differed by 11%. The
  reported RSS difference is not comparable: adaptive benchmarking chose
  different iteration counts while this workload retains memory across
  iterations.
* **Scheduler microbenchmarks:** wall and cycles were statistically null
  in two sessions. Instructions per process increased 9.14% (p=0.019) in
  the first session and 4.73% (p=0.060) in the second. The significant
  result did not reproduce, although the same-direction point estimate
  warrants another-day replication.
* **GC pause:** wall, cycles, and instructions were statistically null at
  matched clock. It kept 167 of 256 CPUs busy.

The gate analyzer's unconditional “NUMA arms must have zero hint faults”
rule belongs to the removed BIND-all design. For an unconfined arm,
stock-like nonzero hint faults are now expected; zero faults remain a hard
requirement only when confinement engages.

### Remaining validation

Before submission, the pinned confined result needs to reproduce in a
second session on another day, with exactly zero experiment-arm hint
faults if the project note's strict mechanism rule is retained. Scheduler
instruction counts also need another-day replication. A second Linux NUMA
platform remains outstanding. The single-node allocation-cost result
should be repeated with raw samples retained if it is to be cited
upstream.
