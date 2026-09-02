# Proposal: `GOEXPERIMENT=numa`: NUMA-aware scheduling and memory placement for Linux

Author: Derek Parker

Last updated: September 2026

## Abstract

I propose making the Go runtime aware of NUMA topology on Linux, behind
a new, off-by-default `GOEXPERIMENT=numa`. With the experiment enabled,
the runtime would discover the machine's node topology at startup,
partition Ps across nodes, grow the heap into per-node address ranges,
route span allocation and recycling by the allocating P's node, softly
bind threads to their node's CPUs, and opt the heap out of the kernel's
automatic NUMA balancing, which becomes redundant once the runtime
places memory itself. A process sized to a single socket would confine
itself to one node automatically, matching what operators achieve today
with `numactl`.

The design adds no public API. Configuration is limited to `GODEBUG`
knobs for diagnostics and overrides. With the experiment disabled the
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
(`/sys/devices/system/node`): the set of online nodes, each node's CPU
list, and whether each node has local memory. It intersects this with the
process's CPU affinity mask from `sched_getaffinity`, so a process
started under `numactl` or in a container with a restricted cpuset sees
only the nodes it may actually use. Nodes with CPUs but no local memory
and vice versa are handled by falling back to treating the machine as
uniform.

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
   it is mapped. This layer is the guarantee for the heap specifically:
   a task policy only applies to faults taken by threads that inherited
   it, so heap pages touched first by threads created before the Go
   runtime initialized, or by cgo threads carrying their own policies,
   would otherwise remain eligible for balancing. Binding the VMA
   itself closes that gap regardless of which thread faults the pages
   in.

Only heap memory is exempted. Balancing continues to apply to
everything else in the process (cgo allocations, mapped files), where
the kernel's heuristics remain the right tool.

This layer is independent of everything below it and is worth having
even without the placement machinery. The elimination is preventive,
not reactive: the balancer works by periodically write-protecting
sampled ranges so the next touch faults, and it skips
policy-covered memory when choosing what to sample, so over the exempt
heap those protections are never installed and the faults simply never
occur. There is nothing to absorb or handle more cheaply; the work
disappears. On the system measured in the Evaluation section, the same
GC-heavy benchmark run takes 2.78 million NUMA hint faults on stock Go
and exactly 0 with the exemption in place, while also running faster.

### Fill-one-socket confinement

One deployment shape deserves specific handling: a Go process sized to
a fraction of the machine, with `GOMAXPROCS` set (or container-limited)
to at most one node's worth of CPUs, on a box shared with other
processes. Today such
a process schedules across all sockets and takes remote-access
penalties for no benefit unless the operator remembers to pin it.

Under this proposal, when the runtime observes at startup that

- `GOMAXPROCS` is no larger than the CPU count of a single node, and
- the process's affinity mask is unrestricted (the operator has not
  placed it),

it confines itself to its boot node (the node the initial thread was
running on): thread affinity is set to that node's CPUs, and since all
faulting threads now run there, first-touch places the heap there too.
The effect is equivalent to `numactl --cpunodebind=B --membind=B`
without the operator having to know about it.

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
  no window covers. One ordering choice is deliberate: on a window
  miss the allocator prefers growing the home window over reusing free
  memory in other nodes' windows, because reuse-anywhere-first is
  precisely the cross-node consumption that defeats placement.
  Placement can degrade under address-space pressure; allocation can
  never fail because of it.

Note what this partitioning does and does not do. It does not bind any
page to any node; there is no per-node `mbind`, and the OOM-on-one-node
failure modes of hard binding are structurally impossible. Actual page
placement still happens by first touch. The window's job is to make
locality *composable*: because threads are softly affine to their
node's CPUs (below), the first touch of a page in `window(n)` almost
always happens on node `n`, and because `node(addr)` is computable from
the address alone in a few instructions, every later layer (span
recycling, diagnostics) can tell where memory lives with no per-page
metadata and no syscalls.

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

The second half is what makes the first durable. Without node-keyed
recycling, spans drift across nodes through the shared central lists
in steady state: a span freed by a P on node 0 sits in a global list
and is next handed to whichever P asks, so after a few free/reuse
cycles the correspondence between where a span's pages live and who is
using them is gone, and refill locality decays back toward blind. With
node-keyed recycling, memory that starts local tends to stay local
through arbitrarily many recycle generations.

### Soft thread affinity and adaptive stand-down

First-touch placement only lands pages on the right node if the
faulting thread is running there, so the last piece is thread
placement: an M running a P homed to node `n` sets its affinity
(`sched_setaffinity`) to node `n`'s CPUs. The affinity is *soft* in
every sense that matters. It is node-wide, not per-CPU, so the kernel
scheduler retains full freedom within the node; it is dropped when the
M releases its P; and it never constrains which P an M may acquire or
which G a P may run.

One workload shape is genuinely hurt by even this much: programs
dominated by sustained high-frequency thread sleep/wake cycles (heavy
timer or network wake storms at very high `GOMAXPROCS`), where
narrowing the wake-target CPU set adds latency to every wakeup. The
runtime softens this pathology itself rather than shipping a tuning
knob: it detects the regime and removes its own enforcement. A
detector in sysmon watches the process-wide thread wake rate, and when
the rate holds at or above a calibrated threshold (proposed: 2048
wakes per second across 8 consecutive 100 ms windows, thresholds
chosen so that GC-induced wake bursts, which are taller but die out
quickly, do not trip it), enforcement stands down: every thread's
kernel affinity mask is widened back to the full allowed set, and Ms
stop narrowing. From that point the program's threads schedule exactly
as they do without the experiment, so the cost of the pathological
interaction is limited to the detection window rather than paid for
the life of the process.

Stand-down is not necessarily permanent. After ten seconds of
accumulated below-threshold time, the detector re-arms and enforcement
resumes, with threads re-narrowing lazily as they schedule. Two limits
keep this from oscillating: consecutive transitions are separated by
at least the ten-second cooldown, and after eight stand-downs over the
life of the process the latch becomes permanent. So enforcement can
adapt to a program whose behavior changes phase, but the total number
of flips is small and bounded. A `GODEBUG=numaenforce` setting pins
the decision in either direction (always enforce, or never), for
debugging.

### Observability

`GODEBUG=numa=1` reports the discovered topology and each feature's
enable-or-disable decision with its reason at startup; `numa=2` adds
verbose detail (window layout, per-node hint streams, enforcement
transitions). These are diagnostics, not configuration: the only
behavioral knob is the `numaenforce` override above.

### What is deliberately out of scope

No public API of any kind. No non-Linux implementation (the design
isolates OS specifics behind per-OS files so other platforms can follow
later). No scheduler restructuring: run queues,
steal order, and wake paths are untouched. No hard memory binding of
any region to a single node. No page migration. No NUMA-aware channel
or goroutine placement: co-locating a channel's memory with the
goroutines communicating over it (#12298), or scheduling a goroutine
near the data it uses, requires exactly the scheduler integration this
proposal avoids (see the Rationale section); making allocation
NUMA-aware and homing Ps and threads is the foundation such work would
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
guesses wrong. First touch plus affinity achieves nearly the same
placement with none of the failure modes; the allocation fallback
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
stock toolchain built from the same source. Results that a decision
should rest on, all from pre-registered gates with benchstat
(Mann-Whitney) as the authority:

- *Balancer exemption:* a GC-heavy benchmark run takes **2.78 million
  NUMA hint faults on stock Go and exactly 0** with the exemption in
  place, while running faster.
- *Placement:* span-refill locality on unpinned runs rises from
  **53-75% (varying with width) to 93-97% at every width**.
- *Full-width wall time:* on a deliberately pathological GC workload
  (4 GiB live set, `GOMAXPROCS=256`, the regime confinement cannot
  help), **8-10% faster (p <= 0.005)**.
- *Confined case:* a single-socket-sized process, unpinned, runs
  **30-33% faster** than stock, and within noise of the same binary
  under `numactl`.
- *Stand-down:* on a synthetic wake-storm workload the detector stands
  enforcement down as designed, and all scheduler microbenchmarks are
  statistically indistinguishable from stock.

The costs, equally measured, because a proposal that hides them is not
worth reviewing:

- Single-threaded allocation microbenchmarks pay roughly **+3.5%
  geomean** with the experiment on at `N = 4`. This is a dispersed
  structural cost, approximately linear in the capacity constant
  (+1.9% at `N = 1`, +5.5% at `N = 8`), not attributable to any single
  hot path; a realistic single-threaded workload proxy reads +1.35%.
  This is the price of the experiment being *on*, paid even on
  single-node machines, and is the strongest argument for keeping `N`
  small.
- The balancer exemption alone costs about **5%** on workloads that the
  kernel balancer happens to serve well (stable working sets, unpinned,
  no confinement). This is the deliberate trade of the whole design;
  the placement layers are what win it back.
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
   changes, and carries the exemption and the 30% confined-case win on
   its own. This phase is a reasonable stopping point if phase 2
   stalls in review.
2. *Placement.* P homes, per-node heap ranges, windowed page
   allocation, node-keyed span recycling, soft affinity with the
   adaptive stand-down. Carries the locality and full-width wins.

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
- *Phasing of review.* Whether phase 2 should wait for a release of
  experience with phase 1, or land in the same cycle, is a judgment
  call I leave to the review.
