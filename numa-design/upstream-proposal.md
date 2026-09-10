# Proposal: `GOEXPERIMENT=numa`: NUMA-aware scheduling and memory placement for Linux

Author: Derek Parker

Last updated: September 2026

## Abstract

I propose making the Go runtime aware of NUMA topology on Linux, behind
a new, off-by-default `GOEXPERIMENT=numa`. With the experiment enabled,
the runtime would discover the machine's node topology at startup,
partition Ps across nodes, grow the heap into per-node address ranges,
route span allocation and recycling by the allocating P's node, and opt
the heap out of the kernel's automatic NUMA balancing, which becomes
redundant once the runtime places memory itself. A process sized to a single socket would confine
itself to one node automatically, matching what operators achieve today
with `numactl`.

The design adds no public API. Configuration is limited to `GODEBUG`
knobs for diagnostics. With the experiment disabled the
new code is compiled out entirely, and with it enabled, every feature
disables itself on hardware or in configurations where it cannot help:
single-node machines, processes already placed by an operator, and
systems where the required syscalls are unavailable.

## Background

Multi-socket servers are NUMA machines: a load or store to memory
attached to another socket costs more than a local access, in both
latency and contended interconnect bandwidth. Multi-die processors push
the same structure into a single package, which can present several
NUMA nodes to the operating system depending on its configuration.
Wherever the structure exists, the penalty for ignoring it shows up as
a tax on every access served by remote memory when local memory could
have served it.

The Go runtime is almost entirely blind to this structure:

- The page allocator hands out heap pages in address order, with no
  notion of which node the backing memory lives on.
- Central free lists (`mcentral`) recycle spans globally. A span whose
  pages are resident on node 0 is happily handed to a P running on
  node 1, and from then on every object allocated from it is remote for
  its user.
- The scheduler steals Gs and redistributes work with no locality
  preference, and Ms float freely across all CPUs, so even memory that
  starts out local does not stay local to the thread using it.

What keeps this workable today is the kernel. Linux's automatic NUMA
balancing periodically unmaps sampled ranges of a process's address
space so that the next touch takes a minor fault, records which node
touched what, and migrates pages (or tasks) to chase locality. This
works reasonably well for programs with stable working sets. A garbage
collected runtime is close to a worst case for it: the allocator
constantly recycles memory across threads, so by the time the balancer
has sampled a page and decided where it belongs, the runtime may have
reallocated it to a goroutine running somewhere else. The kernel pays
for the sampling continuously, in minor faults spread across the whole
heap, and never converges.

The result, measured on a two-socket machine (see the Evaluation
section): a GC-heavy workload at full width sustains millions of NUMA
hint faults over its run while span-refill locality, the fraction of
span cache refills that hand a P memory resident on its own node, still
only reaches 50 to 75%. And a Go process sized to one socket but not
externally pinned scatters across both, running roughly 30% slower than
the same binary under `numactl --cpunodebind --membind`.

This is a long-standing gap with a history:

- [#78044](https://go.dev/issue/78044) reports up to 2x degraded
  performance on modern multi-node EPYC hardware and correctly
  identifies the causes: node-blind free lists, node-blind work
  distribution, and cross-node migration. The reporter floats
  node-segregated free lists and preferential same-node allocation with
  fallback, and prefers a solution with no user-facing API.
- [#12298](https://go.dev/issue/12298) asked for NUMA-aware channel
  placement back in 2015; the underlying need is memory locality, not
  channel machinery.
- Dmitry Vyukov's 2014 design document,
  [NUMA-aware scheduler for Go](https://docs.google.com/document/u/0/d/1d3iI2QWURgDIsSR6G2275vMeQ_X7w-qxM2Vp7iGwwuM/pub),
  proposed binding Ps to nodes with per-node run queues, M pools, and
  node-preferring steal order. It was never implemented.
- [#73193](https://go.dev/issue/73193) (container-aware `GOMAXPROCS`,
  landed in Go 1.25) established the relevant precedent: the runtime
  adapting itself to the machine and container it finds itself on,
  automatically, with no new API, superseding a userland workaround.
- [CL 714801](https://go.dev/cl/714801) (`runtime: prefer to restart
  Ps on the same M after STW`, for [#65694](https://go.dev/issue/65694),
  Go 1.26) began giving the scheduler a stable M/P pairing across
  stop-the-world, explicitly anticipating "a more general affinity for
  specific Ms" as future work. This proposal does not duplicate that
  in kernel CPU masks; it treats thread placement as scheduler
  territory and measures what its absence costs (see "Thread
  placement" in the design).

This proposal follows the same shape as #73193: teach the runtime about
the hardware it is on, do the obviously right thing by default (under
an experiment, initially), and always defer to explicit operator
placement.

## Proposal

Add `GOEXPERIMENT=numa`, implemented for Linux only. When the
experiment is off, all of the code below is compiled out. When it is
on, the runtime gains the following, presented here roughly in
dependency order. The work splits naturally into two phases that can
land and be evaluated independently; the phase boundary is noted where
it falls.

### Topology discovery

At startup the runtime reads the NUMA topology from sysfs
(`/sys/devices/system/node`): the set of online nodes and each node's
CPU list. Two further facts come from the kernel directly rather than
from sysfs. The set of nodes the process may allocate memory from is
read back with `get_mempolicy(MPOL_F_MEMS_ALLOWED)`, which reflects a
container's `cpuset.mems` without any parsing. The process's CPU
affinity mask is read with `sched_getaffinity`; a mask narrower than
the online CPUs, whether from `numactl` or a container cpuset, is
treated as explicit operator placement, and confinement and placement
(below) both decline, leaving only the balancer exemption in effect.

Nodes with memory but no CPUs, and nodes with CPUs but no memory, both
exist and both are handled without special cases. The first kind is
common on current hardware: CXL memory expanders, high-bandwidth
memory exposed in flat mode, and persistent memory in system-RAM mode
all appear as CPU-less nodes. Such a node is in the allowed-memory
mask, so the exemption policy covers it and the kernel may spill to it,
but with no CPUs it is assigned no Ps and its heap window is never
grown. The second kind, a memoryless node, arises when a socket has no
DIMMs populated or a virtual machine exposes a vCPU-only node. Its
CPUs get Ps and a heap window like any other, but the kernel excludes
it from the allowed-memory mask, so its window carries the uniform
exemption policy rather than a preference for that node, and pages its
threads fault land on the nearest node with memory, as the kernel does
for any memoryless node. The window capacity `N` (below) counts only
CPU-bearing node ids, so sparse ids on CPU-less nodes do not exhaust it.

Discovery is best-effort by design. If sysfs is unreadable, the
topology is malformed, or only one usable node remains after the
affinity intersection, the experiment quietly disables itself and the
runtime behaves exactly as it does today. Topology is read once at
startup: CPUs or memory nodes brought online or taken offline later
(hotplug) are not tracked, and the runtime keeps the topology it first
saw for the life of the process.

The runtime also gains internal wrappers for three syscalls it does not
currently use, across all Linux GOARCHes: `mbind(2)`,
`set_mempolicy(2)`, and `getcpu(2)`. If any of them is unavailable (for
example, blocked by a seccomp filter), the features depending on it are
disabled individually.

### Exempting the heap from automatic NUMA balancing

Once the runtime is placing memory itself (or confining the process to
one node), kernel NUMA balancing over the Go heap is pure overhead: the
sampling faults cost real time and the migrations fight the runtime's
own placement. The kernel provides a clean opt-out: memory covered by
an explicit memory policy is skipped by the balancer. In particular, a
policy of `MPOL_BIND` with a nodemask containing *all* allowed nodes
changes no placement decision whatsoever, since every node the process
could use is in the mask, but marks the memory as deliberately placed
and therefore off-limits to balancing.

This is a documented contract, not an accident of the current
scheduler. The balancer's scan (`task_numa_work` in
`kernel/sched/fair.c`) skips every VMA whose governing policy lacks
the migrate-on-fault flag `MPOL_F_MOF`; `vma_policy_mof` in
`mm/mempolicy.c` resolves that policy as the VMA's own if it has one
and the faulting task's otherwise. The kernel sets that flag only on
its built-in default per-task policy, never on a policy a program
installs with `mbind` or `set_mempolicy`, unless the program passes
`MPOL_F_NUMA_BALANCING` to opt back in. That flag (Linux 5.12,
documented in `set_mempolicy(2)`: "when mode is `MPOL_BIND`, enable
the kernel NUMA balancing for the task") exists precisely because
explicitly placed memory is exempt by default, and the exemption has
held since automatic balancing was introduced in Linux 3.8. Should a
future kernel change the default, the sampling faults would return;
nothing here depends on the exemption for correctness, and the
Evaluation section observes it through `/proc/vmstat` rather than
assuming it.

The runtime applies this in two layers:

1. A process-wide task policy via `set_mempolicy(MPOL_BIND, all
   nodes)`, set once during runtime initialization. The two layers
   cover different gaps. The task policy is breadth: for the cost of
   one syscall at startup it covers every mapping whose pages are
   faulted in by a thread that inherited the policy, which in practice
   is all memory the runtime maps, including allocator metadata the
   per-mapping layer below never touches. Its nodemask is read back
   from the kernel (`get_mempolicy(MPOL_F_MEMS_ALLOWED)`) rather than
   derived from sysfs, so it respects a container's `cpuset.mems`
   without any parsing.
2. A per-mapping `mbind(MPOL_BIND, all nodes)` on each heap region as
   it is mapped. This layer is the guarantee for the heap specifically,
   and it exists because of how the kernel resolves policy at fault
   time: a fault is governed by the VMA's own policy if one is set, and
   otherwise by the policy of the *faulting thread*. A task policy is a
   per-thread attribute, inherited at thread creation; it is not a
   property of the process or of any mapping, and a mapping created by
   a policy-carrying thread does not remember that policy. So the task
   policy covers faults taken by the runtime's threads and their
   descendants, but not faults taken by threads that predate runtime
   initialization (threads started by C constructors in a cgo binary,
   or the host application's threads when Go is built as a c-archive or
   c-shared library) or that have since installed their own policy. And
   such threads do fault Go heap pages: a C-created thread that calls
   into Go runs Go code, allocates, and touches the heap like any
   other. This is demonstrated, not assumed: a small cgo program on the
   evaluation machine starts a pthread from a C constructor (which runs
   before the Go runtime initializes) and later calls an exported Go
   function from it. Queried with `get_mempolicy` from inside Go, the
   runtime's own threads report `MPOL_BIND` and the constructor thread
   reports `MPOL_DEFAULT`, the balancer-eligible policy, while running
   Go code and touching tens of megabytes of fresh heap (see
   Evaluation). Binding the heap VMA itself closes that gap: the policy
   travels with the memory range and governs the fault no matter which
   thread takes it.

Only Go heap memory is exempted, and the guarantee runs in one
direction: any thread touching the Go heap is covered, but nothing
here covers or constrains C code's own memory. Balancing continues to
apply to everything else in the process (cgo allocations, mapped
files), where the kernel's heuristics remain the right tool.

The elimination is preventive, not reactive: the balancer works by
periodically write-protecting sampled ranges so the next touch faults,
and it skips policy-covered memory when choosing what to sample, so
over the exempt heap those protections are never installed and the
faults simply never occur. There is nothing to absorb or handle more
cheaply; the work disappears. On the system measured in the Evaluation
section, the full-width GC-heavy benchmark takes 2.78 million NUMA
hint faults per run on stock Go and exactly 0 with the exemption in
place.

What the exemption does *not* do, measured on its own with neither
confinement nor placement, is make programs faster. It removes the
fault tax, but it also removes the balancer's page migrations, and on
workloads where those migrations were converging usefully they were
worth more than the faults cost: an unpinned process using half the
machine measured 5 to 9% slower with the exemption alone (Evaluation).
Where the balancer never converges, the exemption alone is neutral on
throughput and improves GC pause tails. Its value in this design is as
the substrate the placement layers need: they decide where memory
goes, and the balancer would otherwise fight that decision page by
page. The Implementation section draws the phasing consequence.

### Fill-one-socket confinement

One deployment shape deserves specific handling: a Go process sized to
a fraction of the machine, with `GOMAXPROCS` set explicitly (in the
environment or by `runtime.GOMAXPROCS`) to at most one node's worth of
CPUs, on a box shared with other processes. Today such
a process schedules across all sockets and takes remote-access
penalties for no benefit unless the operator remembers to pin it.

Under this proposal, when the runtime observes at startup that

- `GOMAXPROCS` was set explicitly and is no larger than the CPU count
  of a single node, and
- the process's affinity mask is unrestricted (the operator has not
  placed it),

it confines itself to its boot node (the node the initial thread was
running on): the thread affinity mask is narrowed to that node's CPUs
and the task memory policy is set to prefer that node, so the heap
lands there (spilling gracefully if the node fills) and the threads
stay there. The effect is equivalent to `numactl --cpunodebind=B
--membind=B` without the operator having to know about it. A
`GOMAXPROCS` derived by the runtime itself from a container CPU limit
does not count as explicit today; whether it should is listed under
Open issues.

Confinement is conservative in both directions. If the operator *has*
restricted the affinity mask, the runtime treats that as explicit
placement and does nothing. And if the preconditions later stop
holding, most importantly if `GOMAXPROCS` is raised at runtime above
the node's width, confinement stands down by restoring the original
affinity mask, permanently for the life of the process. There is no
oscillation: stand-down is one-way.

*Everything up to this point is phase 1. The layers below are phase 2.*

### P homes

Each P is assigned a *home node* at startup: the Ps are partitioned
across the usable nodes in proportion to each node's share of the
allowed CPUs, in contiguous blocks. On a two-node machine with 128 CPUs
per node and `GOMAXPROCS=256`, Ps 0 to 127 are homed to node 0 and Ps
128 to 255 to node 1. Homes are stable for the life of the process and
recomputed only if `GOMAXPROCS` changes.

A P's home is deliberately a *placement key, not a scheduling
constraint*. The scheduler's run queues, steal order, and wake
ordering are untouched by this proposal. Gs migrate freely between Ps
exactly as they do today. The home simply answers one question
wherever the runtime needs it: "when this P asks for memory, which
node should it come from?"

Two consequences of that choice are worth stating plainly. First, the
home says where a P's memory comes from, not where its thread runs:
allocation routing keys off the home (falling back to the faulting
thread's current node, read via `getcpu`, in contexts with no P at
hand), while the M holding the P is placed by the kernel. With no
thread affinity those two are uncorrelated. Measured on the shipped
tree, the thread holding a P is on the P's home node 47 to 58% of the
time, which is chance (see "Thread placement" below). The memory
layers do not need that correlation to function (the windows and
node-keyed recycling hold regardless), but the locality benefit a
thread actually sees does, and the Evaluation section is explicit
about which measured wins can be attributed to it.
Second, a G can still migrate across nodes, through the global run
queue or by being stolen by a P homed elsewhere, exactly as today.
When that happens, its existing working set becomes remote, and this
proposal does not chase it: the G's subsequent allocations are simply
local to its new node, so locality recovers at the allocation rate
rather than by page migration. A node-preferring steal order is the
natural answer to that residual, and it is precisely the scheduler
territory this proposal leaves to a future proposal (see Rationale).

### Per-node heap address ranges

To route memory by node, the runtime needs to control, and later
recognize, which node a given heap address belongs to. The proposal
partitions the heap's address space rather than tagging its pages.

Let `N` be a small compile-time capacity constant (proposed: 4), `W` a
per-node window size (proposed: 4 TiB), and `B` the heap's randomized
base address. The page allocator's address space is divided into
windows:

    window(n)  = [B + n*W, B + (n+1)*W)       for n in 0..N-1
    node(addr) = (addr - B) / W               for any heap address addr

Two mechanisms cooperate to keep allocations inside their window:

- *Per-node arena hint streams:* Today the runtime keeps one chain of
  arena hints, the preferred addresses at which to `mmap` new heap
  arenas. This proposal keeps one hint stream per node, each seeding
  its addresses inside that node's window, so heap growth on behalf of
  node `n` maps new arenas into `window(n)`.
- *Windowed page allocation:* An allocation on behalf of a P homed to
  node `n` runs a strictly bounded sequence. First, one search
  constrained to `window(n)`. On a miss, one attempt to grow the heap
  into `window(n)`, then one constrained retry. If that also misses,
  the allocation falls through to the stock unconstrained search over
  the whole heap, growing wherever it can, exactly as the allocator
  behaves today. "Bounded" means bounded work, not a loop: each step
  runs at most once per allocation, so the worst case is a fixed,
  small number of extra searches. The final unconstrained step is not
  the same as searching each window in turn: it ignores window
  boundaries entirely, so it can be satisfied by free memory in any
  window, by ranges that straddle a window edge, and by address space
  no window covers. None of this duplicates allocator state per node:
  the page allocator's bitmap and summaries remain one global
  structure, a window is two addresses plus a search cursor, and node
  membership is arithmetic on the address, so a free run that crosses
  a window edge is simply a run in the global bitmap that the windowed
  search declines and the unconstrained search may take. One ordering
  choice is deliberate: on a window
  miss the allocator prefers growing the home window over reusing free
  memory in other nodes' windows, because reuse-anywhere-first is
  precisely the cross-node consumption that defeats placement.
  Placement can degrade under address-space pressure; allocation can
  never fail because of it.

Note what this partitioning does and does not do. Each chunk grown
into `window(n)` is given a *soft* per-node policy,
`mbind(MPOL_PREFERRED, n)`: the kernel places its pages on node `n`
while node `n` has free memory and spills to any other allowed node
otherwise. Nothing is hard-bound, so the OOM-on-one-node failure modes
of `MPOL_BIND` to a single node are structurally impossible, and the
policy is still an explicit one, so the balancer exemption holds over
the window. This is what makes the address arithmetic trustworthy: a
page in `window(n)` is physically on node `n` regardless of which
thread happened to fault it, so a thread running on the wrong node
does not drag pages there, and the runtime never depends on first
touch landing where it hoped. Measured at full width on the evaluation
machine, unpinned, every resident page of each node's window was on
that node (Evaluation). And because `node(addr)` is computable from
the address alone in a few instructions, every later layer (span
recycling, diagnostics) can tell where memory lives with no per-page
metadata and no syscalls. Heap grown with no routing node at hand
(runtime-internal manual allocations such as goroutine stacks, and
anything mapped before topology discovery) carries the uniform
exemption policy alone; it was 3% of resident heap pages in that
measurement.

### Span routing and node-keyed span recycling

With the address space partitioned, the span lifecycle is made
node-aware at its two ends:

- *Refill:* When a P's mcache refills from a central list or grows the
  heap, the request carries the P's home node, and the pages come from
  that node's window.
- *Recycle:* Central free lists are keyed by node as well as span
  class. When a span is swept and returned, it goes back to the list
  for `node(span base address)`, and refills for a P prefer its home
  node's list, falling back to other nodes' lists before growing the
  heap.

Two fallback orders appear in this design and they differ on purpose.
The central lists hold *spans already carved for a size class*, many
of them partially full. A refill that finds nothing on its home node's
list takes a span from another node's list before growing the heap,
which preserves the stock allocator's space economics (a partially
used span is reused before new memory is mapped) and its bounded sweep
budget. The page allocator, one level down, deals in *free pages*;
there, a miss in the home window grows the home window before reusing
free pages in another node's window, because free pages consumed
across nodes become remote spans for their whole lifetime. Reusing a
remote span is a bounded, self-correcting cost: when that span is
freed it returns to its own node's list, not the borrower's.

The second half is what makes the first durable. Without node-keyed
recycling, spans drift across nodes through the shared central lists
in steady state: a span freed by a P on node 0 sits in a global list
and is next handed to whichever P asks, so after a few free/reuse
cycles the correspondence between where a span's pages live and who is
using them is gone, and refill locality decays back toward blind. With
node-keyed recycling, memory that starts local tends to stay local
through arbitrarily many recycle generations.

### Thread placement: measured, and deliberately omitted

Memory placement above is keyed by the P; the thread running that P is
placed by the kernel. At first glance the design needs thread affinity
to close that gap: pin each M to its P's home node, so the CPU touching
the memory is on the memory's node. The prototype
implemented exactly that -- a soft, node-wide `sched_setaffinity`
while an M ran a P, plus a calibrated wake-rate detector that stood
the affinity down in the one regime it hurt -- and measuring the whole
mechanism end to end is what removed it from this proposal:

- On the flagship workload (the 4 GiB / 256-proc GC benchmark in the
  Evaluation section), disabling thread affinity entirely cost only
  +2.4% wall time (p=0.002, n=10 interleaved) and showed no
  significant change on the user+sys metric (p=0.28).
- Span-refill locality as the runtime counts it (the span's window
  against the P's home) was unchanged without affinity: per-width
  medians 92.6-95.8%, against 92.6-96.3% with it, every width above
  the 90% bar in both configurations. That counter is bookkeeping, so
  the thread-to-memory question was measured directly on the shipped
  tree: refills classified against the node the thread was actually
  running on, read via `getcpu` at refill time, are local only 47 to
  58% of the time (per-width medians, Evaluation), which is chance.
- The pathology that affinity forces the runtime to manage is real
  and not confined to microbenchmarks. Narrowing wake-target CPU sets
  regressed wake-heavy scheduler microbenchmarks 15-19% in wall time
  (hardware counters and execution traces attribute it to off-CPU
  wake latency: runnable work waiting while the other node idles),
  and an ordinary short-request `net/http` server under keep-alive
  load sustains a wake rate that tripped the prototype's storm
  detector in 3 of 3 runs. On exactly the workloads most common in
  deployment, the detector immediately stood affinity down anyway:
  the machinery shipped only to disable itself.

What the ablation did not show is that locality survives without
affinity; measured directly, it does not. With no affinity, nothing
ties an M to its P's home node: the kernel places threads by its own
load balancing, and a thread holding a P homed to node `n` is on node
`n` about half the time. The memory side of the design still holds
without affinity (windows are resident on their nodes; routing and
recycling keep a P's spans in its window), but the thread side is
unmet, so the full-width win cannot be credited to thread-to-memory
locality. The ablation bounds that credit: thread affinity, which does
put the thread on its memory's node (the same measurement on the
prototype tree with affinity active reads 93 to 96% at four of the
five widths; Evaluation), was worth
2.4 points of wall time on the flagship workload and nothing
significant on user+sys. The remaining 6 to 7 points come from
elsewhere, most plausibly the exemption's removal of balancer work at
full width and the per-node sharding of the central lists and
page-allocator search state under 256-way contention; separating
those is listed under Open issues.

The decision stands on cost and benefit, not on locality being free.
Kernel affinity bought 2.4 points on one workload at the price of a
real pathology plus a calibrated detector, a cooldown, and a trip cap
to manage it, and this proposal ships none of it. Thread placement is
therefore the open half of NUMA locality. It belongs to a future
scheduler-level proposal in the direction CL 714801 already names ("a
more general affinity for specific Ms"): an M that prefers to run on
its P's home node, without a kernel mask, would give the memory layers
here the thread stability they were built to exploit.

The one remaining use of thread affinity in this design is
fill-one-socket confinement (above): a single narrowing at startup
with a one-way stand-down, no detector, applied only when the process
is sized to one node and unplaced.

### Observability

`GODEBUG=numa=1` reports the discovered topology and each feature's
enable-or-disable decision with its reason at startup; `numa=2` adds
verbose detail (window layout, per-node hint streams). These are
diagnostics, not configuration: the design has no behavioral knobs.

### What is deliberately out of scope

No public API of any kind. No non-Linux implementation (the design
isolates OS specifics behind per-OS files so other platforms can follow
later). No scheduler restructuring: run queues,
steal order, and wake paths are untouched. No thread affinity beyond
confinement's one-shot narrowing (implemented, measured, and dropped;
see "Thread placement" above). No hard memory binding of
any region to a single node. No page migration. No NUMA-aware channel
or goroutine placement: co-locating a channel's memory with the
goroutines communicating over it (#12298), or scheduling a goroutine
near the data it uses, requires exactly the scheduler integration this
proposal avoids (see the Rationale section); making allocation
NUMA-aware and homing Ps is the foundation such work would
build on, as a separate, later proposal. Default-on is explicitly a
separate, future decision with its own bar.

## Rationale and alternatives considered

*Keep relying on kernel NUMA balancing.* This is the status quo, and
the Background section describes why it cannot work well for a GC'd
heap: the balancer optimizes placement of pages whose usage the
runtime is constantly reassigning. It also charges for the attempt,
continuously, in minor faults.

*Hard binding: per-node `MPOL_BIND` on each window.* Rejected. Hard
binding turns one node's memory pressure into allocation failure or
swap while the other node has free memory, fights operator and
container placement, and is irreversible damage when the runtime
guesses wrong. A soft per-node preference plus homed routing achieves
the same placement with none of the failure modes; the allocation fallback
sequence means the worst case is remote memory, exactly what we have
everywhere today.

*The full NUMA scheduler (per-node run queues, node-preferring steal).*
Vyukov's 2014 design remains the natural end state, but it rebuilds the
scheduler's hottest paths and its cost/benefit is unproven. This
proposal takes its central insight, bind Ps to nodes and let memory
follow, while leaving the scheduler alone. Notably, a node-filtered
steal order was prototyped during the development of this design and
contributed nothing measurable on top of the memory layers, so it is
omitted. If the experiment proves out, scheduler work can be a later
proposal with this one as its foundation.

*A user-facing placement API.* Rejected, in agreement with the
expressed preference in #78044. An API would freeze NUMA concepts into
the standard library based on today's hardware, push a hard problem
onto users, and help only programs that are rewritten. The precedent of
#73193 is that the runtime should just do the right thing.

*Solve it in userland.* `numactl` handles the confined case (if the
operator remembers), and the `automaxprocs` lineage shows userland can
adapt `GOMAXPROCS`, but no userland tool can partition the heap by
node, key span recycling by node, or coordinate placement with the
scheduler. The interesting 80% of this proposal is reachable only from
inside the runtime.

## Compatibility

No API changes, no new public identifiers, and no behavior change of
any kind unless a program is built with `GOEXPERIMENT=numa` (set in
the environment at `go build` time, or baked in as a default when the
toolchain itself is built, as with any experiment).
With the experiment off the new code is compiled out; with it on, Go
programs' observable behavior is unchanged apart from performance and
the documented `GODEBUG` outputs. The proposal is Go 1 compatible.

## Evaluation

I have implemented this design in full as a prototype and measured it
on a two-socket Sapphire Rapids system (2 nodes, 256 logical CPUs),
with the kernel's NUMA balancing at its defaults, comparing against a
stock toolchain built from the same source. Method, throughout: every
gate pre-registered with a frozen bar before measurement; interleaved
A/B arms on an otherwise idle machine, one warmup round then 10
recorded rounds per arm unless noted; benchstat (Mann-Whitney) as the
sole authority on significance; raw archives retained for every number
below.

The workloads: the `golang.org/x/benchmarks` suite (`garbage` with a
4 GiB live set as the deliberately pathological full-width GC
workload; `json` as the realistic single-threaded proxy), the runtime
package's allocation and scheduler microbenchmarks, a span-refill
locality probe reading the runtime's local/remote refill counters
across widths, and a keep-alive `net/http` server under closed-loop
load for the real-server checks. Kernel-side effects were read from
`/proc/vmstat` NUMA counters and perf hardware counters around each
run.

- *Balancer exemption:* over the full-width GC workload, the kernel
  takes **2.78 million NUMA hint faults per run for stock Go and
  exactly 0** with the exemption in place (`/proc/vmstat` deltas over
  three sampled rounds of the full-width gate below; exemption-only
  arms of earlier sweeps read 0 against 57 thousand to 4.2 million).
  The exemption *alone*, with neither confinement nor placement, is
  not a throughput win: on `garbage` at half width (128 Ps, unpinned)
  it measured **+5.2% slower (p=0.001, n=15)** and, on a 4 GiB live
  set, **+8.6% slower (p=0.003, n=10)** against stock, because the
  balancer's migrations were doing useful work there; at full width
  on an 8 GiB live set it was within noise (-0.7%, n=3), and on a GC
  pause benchmark it cut STW p99 by 21% (median of 5). Remote-DRAM
  share does not fall under the exemption alone and on one real
  server (Prometheus) rose from 34% to 45%. Every speedup claimed in
  this document comes from the layers below, with the exemption as
  their precondition.
- *Placement:* span-refill locality on unpinned runs rises from
  **53-75% (varying with width) to 93-96% at every measured width**
  on the affinity-free tree (GOMAXPROCS 2, 8, 32, 128, 256; 5
  launches per width since the randomized heap base makes layout a
  per-launch property; per-width medians 92.7-96.3%, minimum launch
  92.4%). This counter classifies the span's window against the P's
  home, so it is bookkeeping by itself; the two physical facts it
  rests on were measured separately. First, *residency*: sampling
  `/proc/self/numa_maps` of a 256-P unpinned run, **100% of the
  resident pages of node 0's window were on node 0 (544,256 pages) and
  100% of node 1's on node 1 (647,548 pages)**; the 3% of heap pages
  carrying only the uniform policy were split between nodes. Second,
  *thread-to-memory locality*: with the counter re-classified against
  the node the thread was actually running on (`getcpu` at refill
  time, a one-line diagnostic patch), per-width medians read **55.4 /
  55.9 / 58.1 / 55.0 / 46.8% at GOMAXPROCS 2 / 8 / 32 / 128 / 256** (5
  launches per width, minimum launch 4.5%): chance level. Without
  thread affinity the thread holding a P is on the P's home node about
  half the time. The same measurement on the prototype tree with soft
  affinity active reads **95.8 / 94.7 / 49.5 / 93.3 / 95.5%**; the
  32-P reading is 49% in four launches of five and 69% in the fifth,
  consistent with the prototype's storm detector standing affinity
  down at that width, and was not chased since that mechanism is
  dropped. The 93-96% figure is
  therefore a statement about routing and recycling, not about where
  threads run, and the full-width win below is not attributed to
  thread-to-memory locality (see "Thread placement").
- *Pre-runtime threads:* the cgo program described under the balancer
  exemption reports `MPOL_BIND` from the runtime's threads and
  `MPOL_DEFAULT` from a thread started by a C constructor, in 3 of 3
  runs, while that thread allocates and touches 64 MiB of Go heap.
- *Full width:* on the pathological GC workload (garbage, 4 GiB live
  set, `GOMAXPROCS=256`, the regime confinement cannot help), the
  exact configuration this proposal describes, with no thread
  affinity, measures **9.1% faster wall time and 5.4% faster user+sys
  (both p=0.000, n=10)** against a stock build of the same source.
  Earlier gates on the prototype with thread affinity still present
  read 8-10% wall across recorded runs; the dedicated interleaved
  ablation (n=10 per arm) bounds affinity's contribution at 2.4
  points of wall (p=0.002) and none of user+sys (p=0.28), consistent
  with the affinity-free result. Since affinity is what puts a thread
  on its memory's node, 2.4 points is also the most of this win that
  thread-to-memory locality can explain; the rest is unattributed
  (Open issues).
- *Confined case:* a single-socket-sized process, unpinned, runs
  **30-33% faster** than stock, and within noise of the same binary
  under `numactl --cpunodebind --membind`.
- *Thread-affinity ablation, in full* (the evidence behind "Thread
  placement" above): disabling affinity changes refill locality by
  less than measurement noise at every width (per-width medians
  92.6-95.8% without vs 92.6-96.3% with); wake-heavy scheduler
  microbenchmarks regress 15-19% wall with affinity pinned on
  (attributed to off-CPU wake latency by perf counters, and confirmed
  by execution traces showing +56% median scheduler delay per op with
  no overlap across runs); and a plain `net/http` server at ~44k
  requests/s pays only -0.5% throughput under pinned affinity while
  tripping the prototype's storm detector in 3 of 3 runs.
- *Scheduler neutrality:* with no thread affinity there is no
  mechanism left by which this proposal changes scheduling; the four
  scheduler microbenchmarks were statistically indistinguishable from
  stock even in the prototype's affinity-plus-detector configuration,
  and the shipped configuration removes that machinery outright.

The costs, equally measured, because a proposal that hides them is not
worth reviewing:

- Single-threaded allocation microbenchmarks pay roughly **+3.5%
  geomean** with the experiment on at `N = 4`. This is a dispersed
  structural cost, approximately linear in the capacity constant
  (+1.9% at `N = 1`, +5.5% at `N = 8`), not attributable to any single
  hot path; a realistic single-threaded workload proxy reads +1.35%.
  Those numbers are from the two-node machine at `GOMAXPROCS=1`, where
  the runtime confines and the windowed allocator is active. On a
  literal single-node host, where every feature declines itself, the
  same microbenchmarks read **+1.0% geomean (Malloc8 unchanged,
  p=0.44; Malloc16 +1.85%, p=0.002; n=20 interleaved, spreads of 1-2%)**
  on a 16-core desktop part. That residual is the compile-time
  footprint of the `N`-sized structures, and it is the strongest
  argument for keeping `N` small.
- The balancer exemption alone costs **5 to 9%** on workloads that the
  kernel balancer happens to serve well (stable working sets, unpinned,
  spread across nodes with neither confinement nor placement engaged;
  the numbers and conditions are in the exemption bullet above). On
  the shipped tree both configurations in which that cost was measured
  now engage a placement layer and come out ahead: the same half-width
  process confines and runs 30% faster, and at full width placement
  reads 9% faster. On single-node hosts the exemption is never applied
  and the cost is zero. The residual exposure is a workload the
  balancer served well and placement does not; the full-width `json`
  gate, the one such workload measured, is statistically flat.
- Memory: resident set size is statistically flat against stock across
  the measured workloads. The design adds a fixed amount of runtime
  metadata, independent of heap size: the central free lists, arena
  hint chains, and page-allocator search state are replicated per node,
  a small constant footprint at `N = 4`. The address-space windows are
  layout, not reservations; pages are mapped only as the heap actually
  grows, as today, so a program's virtual and resident footprint do not
  grow with the window size. The one measured pressure point is the
  grow-before-reuse ordering in the windowed allocator, which can map
  memory in the home window while free memory exists in another node's
  window; the fallback sequence bounds this, and it did not move RSS in
  any measured workload.
- The off-build is byte-identical in every function to a stock build.

All numbers are from one machine so far; validating on a second
platform (a multi-node EPYC or arm64 system) is called out under Open
issues.

## Implementation

The work lands as two independently valuable phases, matching the
structure above:

1. *Topology, balancer exemption, confinement.* Small, no allocator
   changes, and carries the 30% confined-case win on its own. One
   caveat, drawn from the exemption-only measurements above: in a
   process that does not confine, phase 1 as currently implemented
   applies the exemption with nothing to win back its cost, a 5 to 9%
   loss on balancer-friendly workloads. Confinement does not need the
   exemption layer (its own task policy is already an explicit one),
   so the exemption's real customer is phase 2. How to resolve that
   is listed under Open issues.
2. *Placement.* P homes, per-node heap ranges, windowed page
   allocation, node-keyed span recycling. Carries the locality and
   full-width wins.

I have a complete implementation of the design and would send it as CL
series matching these phases, each CL individually building and
passing tests. I would do this work.

## Open issues

- *The capacity constant `N`.* The prototype uses `N = 4`: it covers
  two-socket systems and multi-die parts of up to four nodes, keeps the
  structural cost near its floor, and costs 4 windows x 4 TiB of
  address space.
  Machines with more CPU-bearing nodes than `N` disable the placement
  layer (phase 1 still applies). Whether 4 is the right number, and
  whether it should eventually become link-time configurable, is open.
- *Second-platform validation.* Before graduating beyond an
  experiment, the numbers should be reproduced on at least one
  additional topology (4-node x86 and/or multi-node arm64). The
  benchmark harness makes this mechanical; the hardware is the
  constraint.
- *Where the balancer exemption lands.* Measured alone it is a 5 to 9%
  loss on balancer-friendly unpinned workloads and it is only needed by
  placement. Two clean resolutions: move it from phase 1 to phase 2, or
  keep it in phase 1 but apply it only when confinement or placement
  is active. My recommendation is the second, which keeps phase 1 free
  of any measured regression without changing its scope; it is a small
  change to the prototype and I would make it before sending the CLs.
- *Container-derived `GOMAXPROCS`.* Confinement requires an explicit
  `GOMAXPROCS`. A process whose `GOMAXPROCS` the runtime derived from a
  container CPU limit (#73193) is not treated as explicit, so an
  8-CPU-limited container on a two-socket host gets placement across
  both nodes rather than confinement to one. Treating the
  container-derived value as explicit for this purpose is probably
  right and is a one-line change, but it widens the population
  confinement affects and deserves its own measurement.
- *Attribution of the full-width win.* The 9% wall improvement on the
  flagship workload is reproducible, but only 2.4 points of it are
  attributable to thread-to-memory locality (the affinity ablation),
  and the shipped tree's thread locality is at chance. The remainder
  needs a decomposition before this proposal is filed upstream: an
  exemption-only arm at full width on the same 4 GiB live set, and a
  placement arm with routing on but the per-node windows disabled,
  would separate balancer relief from sharding from placement. If the
  sharding explains most of it, part of this design's value is a
  contention fix that does not need NUMA at all, and the proposal
  should say so.
- *Phasing of review.* Whether phase 2 should wait for a release of
  experience with phase 1, or land in the same cycle, is a judgment
  call I leave to the review.
