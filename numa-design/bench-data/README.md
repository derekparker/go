# NUMA gate data

## Final gates

`gates/` contains the counter-backed gates used by the proposal review:

| Directory | Workload | Purpose |
|---|---|---|
| `confined-rebased-external-pin-0914` | garbage, 128P, 4 GiB | primary externally pinned-clock result |
| `confined-rebased-0914` | garbage, 128P, 4 GiB | unpinned corroborating cycle result; wall result invalid |
| `garbage-256-rebased-0914` | garbage, 256P, 4 GiB | unconfined non-regression |
| `json-256-rebased-0914` | JSON, 256P | unconfined high-variance non-regression |
| `sched-rebased-0912` | runtime scheduler microbenchmarks | first scheduler non-regression session |
| `sched-rebased-0914-b` | runtime scheduler microbenchmarks | same-day scheduler replication |
| `gc-pause-rebased-0912` | GC pause workload, 256P, 4 GiB | GC-pause non-regression |

Every directory contains `gate.meta`, the remote chain log, raw benchmark
and `perf stat` output, derived benchmark input, `benchstat.txt`,
per-run `/proc/vmstat` deltas, and the NUMA arm's startup sanity output.

The gates compare experiment-off and experiment-on builds from
`ba2d41763e36a9f30af6813920791a5dae76daaa`. The final source adds one
subsequent fail-closed check for CPU IDs ≥8192. The measured host uses CPU
IDs 0–255, so that check does not alter any measured path.

The pinned gate's `gate.meta` says `pin=0` because clock control was
provided by an external per-CPU watchdog rather than the gate's
`msr-tools` integration. Exact MSR values and restoration verification
are recorded in `../../pinned-clock-gate.md`.

Failed, partial, and pre-rebase gates are intentionally excluded from
this directory. They remain in the local pre-recut safety stash and are
not proposal evidence.

## Analyzer caveat

The analyzer predates the reduced design and marks any experiment-on arm
with nonzero hint faults as `MECHANISM FAIL`. That rule applies only when
confinement engages. An unconfined arm deliberately keeps
`MPOL_DEFAULT`, so stock-like automatic-NUMA hint faults are expected.
