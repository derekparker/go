# Proposal: NUMA-node confinement for the Go runtime on Linux

Author: Derek Parker

Last updated: 2026-09-14

Discussion at https://go.dev/issue/NNNNN.

## Abstract

This proposal adds an off-by-default `GOEXPERIMENT=numa` experiment on
Linux. When a Go program explicitly selects a `GOMAXPROCS` value that fits
within one NUMA node, the runtime confines its own threads to one node. It
sets their CPU affinity to that node's CPUs and applies an
`MPOL_PREFERRED` task memory policy for the same node.

The change is intended for programs that are smaller than the machine on
which they run. Today such a program may execute and allocate across
several NUMA nodes unless its operator also invokes a tool such as
`numactl`. The experiment gives the runtime a conservative equivalent for
runtime-created threads, without a public API or allocator redesign.

The runtime does nothing when it cannot establish that confinement is
safe. In particular, it preserves CPU affinity and memory policy supplied
by an operator.

## Background

Linux normally schedules an unpinned process across all CPUs in its
affinity mask. Anonymous memory is placed on the node of the thread that
first faults it. Linux automatic NUMA balancing may later sample accesses
with hint faults and migrate pages or tasks.

This works reasonably for programs that use an entire machine. It is a
poor default for a Go program whose useful parallelism fits within one
NUMA node. The Go scheduler may move its threads between nodes, first
touch may distribute its heap between those nodes, and later accesses may
be remote. Automatic NUMA balancing reacts after the fact and incurs its
own faults and migrations.

Operators can avoid this behavior with, for example:

```
numactl --cpunodebind=0 --preferred=0 program
```

In many deployments the program already communicates its intended
parallelism through `GOMAXPROCS`, but the operator must separately
describe the corresponding machine placement. The runtime already uses
OS and container information to choose a default `GOMAXPROCS`; this
proposal explores similarly conservative placement when the user has
chosen the value explicitly.

Prototype measurements were made on a two-node, 256-CPU Intel Sapphire
Rapids system. Historical wall-only sessions showed improvements of
29.6% and 33.1% for a GC-heavy workload at `GOMAXPROCS=128`.

The reduced implementation was subsequently rebased onto upstream and
measured for 10 rotating rounds under `perf stat`, with Intel HWP pinned
to its guaranteed-performance level. Confinement reduced wall time by
21.79%, cycles by 16.75%, user plus system CPU time per operation by
16.30%, and STW time per GC by 19.83% (all p≤0.001). Effective clock
differed by 1.01%, within the measurement protocol's 2% limit, and peak
RSS was statistically unchanged. Nine of ten experiment runs had zero
host-wide automatic-NUMA hint faults and migrations; one saw 183 faults
and 182 migrations, compared with about 174,000 faults per stock run.
This is a candidate result until it reproduces in a second pinned session
on another day.

## Proposal

### Topology discovery

During runtime initialization on Linux, an experiment-enabled binary
reads:

* `/sys/devices/system/node/online`; and
* `/sys/devices/system/node/nodeN/cpulist` for each online node.

Discovery uses fixed-size, allocation-free storage because it runs during
early runtime initialization. The prototype represents at most 64 NUMA
nodes and CPU IDs below 8192. If the online-node list exceeds either
representation, a required file cannot be read or parsed, or the required
Linux syscalls are unavailable, confinement is disabled.

The runtime does not read or retain the NUMA distance matrix because
confinement selects exactly one node and has no use for inter-node
distance.

### Eligibility

Confinement is considered after the initial P set is created and before
the runtime creates additional OS threads. It is applied only when all of
the following are true:

1. The experiment is enabled and at least two NUMA nodes were discovered.
2. `GOMAXPROCS` was explicitly set in the environment.
3. The process's initial CPU affinity contains every CPU represented by
   the discovered topology. A `taskset`, cpuset, or other narrower mask
   therefore takes precedence.
4. The initial task memory policy is `MPOL_DEFAULT`. A policy established
   by `numactl` or another launcher therefore takes precedence.
5. `getcpu` identifies the current node and that node has at least
   `GOMAXPROCS` CPUs.

The current node is chosen rather than always choosing node zero. This
respects the kernel's initial placement and avoids introducing a global
preference shared by every concurrently starting process.

`GODEBUG=numa=1` reports whether confinement engaged and, if not, the
reason it declined.

### Confinement

The runtime changes two pieces of per-thread Linux state on the initial
runtime thread:

* `sched_setaffinity` restricts execution to CPUs in the selected node.
* `set_mempolicy(MPOL_PREFERRED)` prefers memory from that node.

`MPOL_PREFERRED` permits normal fallback if the preferred node has
insufficient memory. The prototype deliberately does not use a
single-node `MPOL_BIND`, which could turn available remote memory into an
artificial out-of-memory condition.

Runtime-created threads inherit both settings. An explicit preferred
policy also prevents Linux automatic NUMA balancing from scanning the
affected anonymous mappings through those threads: unlike the kernel's
internal default local policy, a user-created preferred policy does not
carry `MPOL_F_MOF`.

The experiment does not apply a VMA policy in the heap growth path.
Doing so would add an `mbind` syscall under the heap lock for every
roughly 4 MiB of growth, and those policies would survive if confinement
later stood down. The prototype originally did this and thereby exposed
unconfined execution to a measured 5–9% regression on workloads helped by
automatic NUMA balancing. The reduced design keeps memory-policy changes
strictly conditional on confinement.

### Standing down

Confinement is one-way. It stands down if a later `GOMAXPROCS` value
exceeds the selected node's CPU count or if the program calls
`runtime.SetDefaultGOMAXPROCS`.

The M that performs the resize restores its startup affinity and
`MPOL_DEFAULT` policy after the world restarts. Other runtime Ms restore
their own state the next time they park. The implementation does not walk
the runtime's M list and issue affinity changes by saved TID: an exited
thread's TID may have been reused, making such a best-effort optimization
unsafe.

Once it stands down, the process does not automatically confine again.
This keeps the transition and its synchronization simple.

### C and foreign threads

The runtime can rely on inheritance only for threads created after it
applies confinement. A C constructor can create a thread before Go
runtime initialization; that thread retains its existing CPU affinity and
memory policy if it later calls Go.

Consequently, this proposal is equivalent to the shown `numactl`
invocation only for runtime-created threads. Programs that create
pre-runtime foreign threads and require process-wide confinement must
continue to use a launcher such as `numactl`. This limitation is preferred
to adding permanent per-VMA policies or trying to mutate unknown foreign
threads.

## Rationale

### Why infer placement from `GOMAXPROCS`?

`GOMAXPROCS` is already the program's statement of desired CPU
parallelism. When it fits within one node, keeping those CPUs and their
memory together is a direct interpretation with a measurable benefit.
The explicit-setting requirement avoids feeding the runtime's narrowed
affinity back into its automatically updated default.

### Why preserve existing placement?

An operator may have selected CPUs for isolation, licensing, latency, or
coordination with other services. A memory policy may express bandwidth
or tiering requirements not visible in the CPU topology. The runtime has
less information than the operator, so either setting causes the
experiment to decline.

### Why no allocator changes?

Confinement makes ordinary Linux first-touch placement useful: runtime
threads can execute only on the selected node and prefer that node for
allocation. Per-node heaps, span lists, P homes, and scheduler changes
would add substantial fixed state and hot-path complexity. They address
full-machine programs, which are outside this proposal.

### Alternatives

* **Require `numactl`.** This is effective and remains necessary for
  pre-runtime foreign threads. It duplicates `GOMAXPROCS` configuration
  in deployment manifests and is commonly omitted.
* **Use single-node `MPOL_BIND`.** This provides stricter placement but
  can fail allocations despite available memory on another node.
* **Apply BIND-to-all-nodes task and VMA policies.** This suppresses
  automatic balancing without changing the allowed set. Measurements
  showed a 5–9% loss when confinement did not engage, and permanent VMA
  policies complicate stand-down.
* **Add a public NUMA API.** An API would expose Linux topology and policy
  concepts to applications and create a long-term compatibility
  commitment. It is unnecessary for this experiment.
* **Make the runtime fully NUMA-aware.** Per-node allocation and scheduling
  may help programs that fill a machine, but the current evidence does not
  establish a reproducible benefit that justifies that larger design.

## Compatibility

There is no language or public API change. `GOEXPERIMENT=numa` is off by
default. Builds without the experiment retain the existing behavior, and
non-Linux implementations are no-ops.

Experiment-enabled programs may observe different CPU affinity, memory
placement, performance, and values reported by OS-specific affinity
queries when confinement engages. Those effects are the purpose of the
experiment. Existing operator affinity or memory policy is preserved.

## Implementation

The prototype consists of:

1. allocation-free Linux NUMA topology discovery in
   `internal/runtime/numa`;
2. Linux syscall support for `getcpu`, `sched_setaffinity`,
   `get_mempolicy`, and `set_mempolicy`;
3. experiment and `GODEBUG` plumbing;
4. the startup eligibility and confinement operation;
5. one-way stand-down integrated with `GOMAXPROCS` resizing; and
6. parser, cross-build, runtime, and Linux hardware tests.

Derek Parker will prepare and send the CL series. It should land only
behind the experiment during a development window. Graduation or
default-on behavior would require a separate proposal based on experience
from at least one release.

Validation completed on the rebased reviewed tree includes:

* experiment-on and experiment-off runtime tests on a two-node Linux
  system, including all nine NUMA-specific hardware tests;
* Linux/amd64 and Darwin/arm64 builds;
* 10-round confined and unconfined performance gates with counter and
  effective-clock collection; and
* a 20-sample feature-declined Darwin/arm64 allocation measurement:
  +0.29% for `Malloc8`, +0.94% for `Malloc16`, and +0.62% geomean; and
* an experiment-off census in which all 1,432 linked `TEXT` symbols had
  identical symbol sets and instruction counts, with no NUMA symbol
  linked into the binary. Runtime-global address shifts mean raw
  byte-identical code is not claimed.

The performance gates used tree `ba2d41763e`. The final source adds a
fail-closed check for CPU IDs ≥8192; the measured host uses IDs 0–255, so
the added check does not alter its executed path.

Before sending the series, the following validation remains:

* repeat the pinned, matched-clock confined benchmark in a second session
  on another day, with a clean mechanism-counter run;
* repeat the scheduler gate on another day; wall and cycles were null in
  two same-day sessions, while a 9.14% significant whole-process
  instruction increase in the first session became a nonsignificant 4.73%
  increase in the second;
* retain raw samples for a repeat of the single-node allocation-cost
  measurement if that number is cited;
* reproduce the primary result on a second NUMA system.

## Open issues

### Container-derived `GOMAXPROCS`

The initial implementation confines only when `GOMAXPROCS` is explicit.
A value reduced by a cgroup CPU quota is also independent of CPU affinity
and may be a reasonable signal. It is not included yet because dynamic
quota changes and `SetDefaultGOMAXPROCS` must retain coherent stand-down
semantics, and the wider affected population needs separate measurement.

### Node choice

The prototype chooses the node on which the initial runtime thread is
running. A deterministic choice or a load-aware launcher can distribute
many identical processes more evenly, but each requires information the
runtime does not currently have. Experiment data should establish whether
the current-node rule causes operational problems.

### Hardware scope

All performance evidence currently comes from one two-node x86-64
machine. The fixed topology limits cover larger systems, but correctness
and performance must be tested on another topology and on Linux arm64
before considering graduation.
