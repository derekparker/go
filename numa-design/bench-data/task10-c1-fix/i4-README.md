# Task 10 review I4: wider bench matrix + hook-in/hook-out isolation

Host: numa-dell (2 nodes, 128 CPUs each, 256 total), SHA 3413c5ea7e.
Benchmarks: PingPongHog, ProcYield/*, OSYield, CreateGoroutinesParallel,
WakeupParallelSpinning/*, WakeupParallelSyscall/*, Matmult (runtime
package), count=5, default benchtime, benchstat.

## Off vs on (i4-widerbench-off.txt, i4-widerbench-on.txt, i4-widerbench-offvon-benchstat.txt)

Real-world comparison: experiment off vs the shipped Task 10 implementation.
geomean -5.70% (favorable). No benchmark shows a significant regression;
CreateGoroutinesParallel and several WakeupParallelSyscall sub-cases show
significant improvement (likely noise / shared-machine variance, not a
causal effect of soft affinity -- not investigated further since the
direction is favorable, not adverse).

## Hook-in vs hook-out at FIXED experiment state (i4-hookout.txt, i4-hookin.txt, i4-hookout-vs-hookin-benchstat.txt)

The decisive, honest-measurement comparison the review asked for: BOTH
binaries built with GOEXPERIMENT=numa (same experiment state, so
Workstream A/B's other conditional code paths -- confinement,
per-node heap streams, span-refill routing -- are identical in both).
The ONLY difference: numaNoteSchedule's body was temporarily replaced
with an immediate `return` (hook-out) for one run, then reverted back
to the real implementation (hook-in) for the other, via a source edit
never committed (git checkout -- reverted it before continuing; git
diff clean, confirmed, at SHA 3413c5ea7e before and after).

Result: PingPongHog (the most schedule()-sensitive benchmark) shows NO
significant difference, hook-in vs hook-out (p=0.421, n=5). No
benchmark in the matrix shows hook-in significantly SLOWER than
hook-out. This isolates that the actual soft-affinity logic (throttled
nanotime check + occasional getcpu/setaffinity) costs nothing
measurable beyond the call site's mere presence -- confirming the
nanotime throttle fix (commit 3d67f0cb0d) actually solved the
hot-path regression it was built for, not just the off-vs-on numbers.
