# Current-State Reference: Go Runtime and Linux NUMA Mechanics

Companion document to `numa-proposal.md`. This document describes how the
Go runtime and the Linux kernel behave **today** — no proposed changes
appear here. Line references are against Go master @ 2e82d2ce86 (July
2026); kernel references are against Linux v6.15 source.

Contents:

1. [Heap arenas and heap growth](#1-heap-arenas-and-heap-growth)
2. [mcache, mcentral, and refill](#2-mcache-mcentral-and-refill)
3. [Goroutine stack allocation](#3-goroutine-stack-allocation)
4. [Scheduler: work stealing and run-queue mixing](#4-scheduler-work-stealing-and-run-queue-mixing)
5. [GC mark: Green Tea work distribution](#5-gc-mark-green-tea-work-distribution)
6. [Linux kernel NUMA mechanics](#6-linux-kernel-numa-mechanics)
7. [Node identity primitives: getcpu, vDSO, rseq](#7-node-identity-primitives-getcpu-vdso-rseq)
8. [Issue #14406 forensics](#8-issue-14406-forensics)
9. [Prior art in detail](#9-prior-art-in-detail)

---

## 1. Heap arenas and heap growth

Go's heap is organized into *arenas*: contiguous regions of 64 MB (on
64-bit Linux) reserved via `mmap(MAP_ANON|MAP_PRIVATE)`. Each arena
covers 8,192 pages of 8 KB and is tracked by a `heapArena` metadata
struct (`runtime/mheap.go`) containing a `spans` array mapping each page
to its owning `mspan`, plus per-page bitmaps for in-use, mark, and
special tracking. With the Green Tea GC's `pageUseSpanInlineMarkBits`
addition (mheap.go:321), the metadata is ~70 KB per 64 MB arena (~0.1%
overhead), allocated off-heap to avoid circularity.

Arenas are addressed through a two-level arena map on `mheap`: an L1
index selects an L2 array, which points to the `heapArena` for that
address range. This supports a sparse 48-bit address space without
allocating metadata for unused regions.

**Reservation.** When the heap needs address space, `mheap.sysAlloc()`
(`runtime/malloc.go`) first attempts to extend the heap at a *hint
address* — the runtime maintains a linked list of `arenaHint` structs
pointing to addresses where the heap previously grew, so consecutive
arenas are contiguous in virtual memory. If the hint succeeds
(`sysReserve` returns the requested address), the arena is placed there.
If all hints fail, `sysReserveAligned` asks the kernel for any suitably
aligned region, and new hints are created pointing both upward and
downward from the returned address.

**Commit — and a VMA landmine.** After reservation, `sysMap` transitions
the memory from Reserved to Prepared. On Linux, `sysMapOS`
(mem_linux.go:172-173) re-mmaps the region with `MAP_FIXED`. This
**replaces the VMA**: any attribute attached to the VMA at reserve time
— including a NUMA memory policy set with `mbind(2)` — is destroyed by
the `MAP_FIXED` remap. Any future policy-setting code must therefore run
strictly *after* `sysMap`, not after `sysReserve`.

**Growth is a single shared bump region.** `mheap.grow()`
(`runtime/mheap.go`) requests memory in 4 MB chunks (`pallocChunkPages`
× page size) and carves them from `h.curArena` (mheap.go:202) — a
single `{base, end}` bump pointer for the entire process, advanced
linearly under `mheap.lock` (mheap.go:1544). There is exactly one
`curArena`; it is not partitioned by P, by size class, or by anything
else. Whichever P triggers heap growth advances the shared pointer, and
the resulting pages then serve span allocations for **all** Ps until
exhausted. After growth, `mheap.pages.grow()` registers the pages with
the page allocator and the `heapArena` metadata is installed in the
arena map.

**NUMA involvement: none.** The `mmap` calls in `sysReserve` and
`sysMap` pass no memory policy flags, and no `mbind(2)` call exists
anywhere in the runtime (grep-confirmed: no code in `src/runtime` reads
NUMA topology, calls `mbind`/`set_mempolicy`/`getcpu`, or partitions any
structure by node). Physical page placement is governed entirely by the
kernel's default policy — first-touch, plus automatic NUMA balancing
(§6).

---

## 2. mcache, mcentral, and refill

Go's small-object allocator is a three-level hierarchy:
`mcache` → `mcentral` → `mheap`.

An `mcache` (`runtime/mcache.go`) is a per-P cache of spans: one active
`mspan` per span class, 136 span classes total. The 136 comes from
`gc.NumSizeClasses = 68` size classes, doubled for scan/noscan variants
(`numSpanClasses = 68 << 1`) to separate pointer-bearing objects from
pointer-free ones. Class 0 is the large-object/no-size class; the sized
classes 1–67 cover 8 bytes to 32 KB. Because each P owns its mcache, the
allocation fast path takes no locks: `mallocgc` maps the requested size
to a span class, checks `mcache.alloc[spanClass]` for a free slot, and
bumps the allocation index. mcaches themselves are allocated from a
fixalloc pool (`allocmcache`), off-heap.

When a span's free slots are exhausted, `mcache.refill()` swaps in a
fresh span:

1. The exhausted span returns to its mcentral via
   `mheap_.central[spc].mcentral.uncacheSpan(s)`, landing on the full
   list for its span class.
2. A replacement comes from the same mcentral via
   `mheap_.central[spc].mcentral.cacheSpan()`, which tries in order:
   partial swept spans, partial unswept spans (swept on demand), full
   unswept spans (swept on demand, may reveal free slots), and finally
   `mcentral.grow()` — a fresh span from `mheap`.
3. The new span is installed in `mcache.alloc[spc]` with a sweep
   generation preventing reclamation while cached.

**The mcentral is global and flat.** `mheap.central` is an array of 136
entries — one per span class — where each entry is an *anonymous inline
struct* holding an `mcentral` plus cache-line padding to prevent false
sharing (mheap.go:211-214; there is no named `paddedMcentral` type).
Each `mcentral` holds a `spanClass` and four `spanSet`s (two partial,
two full, indexed by sweep generation parity). The array is shared by
all Ps regardless of NUMA node: when a P on node 0 refills, it may
receive a span whose physical pages were first touched on node 2 by a P
that later returned the span to the same global pool.

**Where fresh spans come from.** The full chain when all mcentral lists
are empty: `mcache.refill` → `mcentral.cacheSpan` → `mcentral.grow` →
`mheap_.alloc` → `mheap.allocSpan` → (if the page allocator is empty)
`mheap.grow` — into the single shared `curArena` (§1). There is no
per-node anything at any level of this chain.

---

## 3. Goroutine stack allocation

Goroutine stacks **do not flow through mcentral**. They have a parallel
allocation path (`runtime/stack.go`):

- **Small stacks** (up to `_FixedStack << (_NumStackOrders-1)`) come
  from the per-P `mcache.stackcache[order]` free lists (stack.go:46-53)
  — a set of order-sized free lists cached on the mcache, distinct from
  `mcache.alloc`.
- When a stackcache order runs dry, `stackcacherefill` (stack.go:279)
  pulls stacks from the **global `stackpool`** (stack.go:153) — an
  array of order-indexed span lists shared by all Ps, protected by
  per-order locks.
- When `stackpool` itself is empty, it grows via
  `mheap_.allocManual(..., spanAllocStack)` (stack.go:200) — a *manual*
  span carved directly from heap-arena pages by the page allocator,
  bypassing mcache/mcentral entirely.
- **Large stacks** come from the global `stackLarge` free lists or
  direct `allocManual` calls (stack.go:79, 165).

Two consequences worth stating explicitly:

1. Any locality property engineered into mcentral does **not** transfer
   to stacks. A per-node mcentral design leaves stack sources — global
   `stackpool`, global `stackLarge` — untouched.
2. Stack *growth* (`morestack` → `newstack` → `copystack`) allocates
   the new stack from the same global pools. Growing a stack does not
   move it toward the current P's node under the current design or
   under mcentral-only sharding.

Because manual spans are still `mspan`s carved from heap arenas, they
live in the same address space and arena map as heap spans — any
arena-level memory policy (or address-based node scheme) covers them.

The initial goroutine stack is 2 KB (`stackMin`, stack.go). Most
goroutines that do nontrivial work grow their stack at least once.

---

## 4. Scheduler: work stealing and run-queue mixing

### 4.1 Work stealing

When a P's local run queue is empty, `findRunnable()` calls
`stealWork()` (proc.go:3843-3852). The algorithm makes `stealTries = 4`
passes over all Ps. Each pass enumerates every P exactly once in
pseudo-random order: `randomOrder` (proc.go:8044-8084) precomputes the
integers coprime with `GOMAXPROCS`, and `randomEnum` picks a random
start and a random coprime increment from `cheaprand()`, stepping
`pos = (pos + inc) % count`. Every P is visited exactly once per pass,
in a different permutation each pass. On the final pass
(`i == stealTries-1`), the algorithm also checks each victim's timers
and may steal from `runnext` (the victim's high-priority slot).

The steal itself, `runqsteal()` → `runqgrab()` (proc.go:7713), takes
half the victim's queue with a lock-free protocol: atomically read
`runqhead`, compute the batch, copy, and CAS the head to claim it.

Total examinations per idle scheduling round: `4 × GOMAXPROCS` — 512 on
a 128-core machine. The enumeration is completely NUMA-unaware: a P on
node 0 is exactly as likely to steal from node 3 as from node 0.

### 4.2 Global run queue

`sched.runq` is a `gQueue` with an internal `size` field, protected by
`sched.lock`. `findRunnable()` checks it every 61 scheduling ticks
(`pp.schedtick%61 == 0`) to prevent starvation, and as a fallback when
stealing fails. Two dequeue paths (proc.go:7332-7351):

- `globrunqget()` pops exactly **one** g.
- `globrunqgetbatch(n)` claims `n = min(n, size, size/gomaxprocs + 1)`
  gs at once.

The queue is a single FIFO with no node awareness of any kind.

### 4.3 How run queues mix across nodes

A P's local run queue routinely contains goroutines whose stacks and
heap data live on other nodes, via four independent paths:

1. **Stealing itself.** `runqgrab()` moves half a remote P's queue
   wholesale. If P₁ on node 0 steals from P₂ on node 2, P₁'s queue now
   holds node-2 goroutines.
2. **`goready()` uses the current P.** An unblocked goroutine (channel,
   mutex, I/O) is placed on the *waker's* P via `runqput(pp, gp,
   next=true)` — wherever the waker happens to run, not where the wakee
   was born.
3. **The global queue mixes everything.** Any P on any node enqueues
   into and dequeues from the single FIFO.
4. **Syscall handoff moves the P.** When an M blocks in a syscall
   (`entersyscallblock()` → `releasep()` → `handoffp()`,
   proc.go:3146), the P passes to `startm()`, which pops an *arbitrary*
   idle M via `mget()` — potentially on any node. The P's queued
   goroutines are now served from a different node. On syscall exit,
   `exitsyscallTryGetP()` (proc.go:5072) tries to reacquire the old P
   (helped by `setBlockOnExitSyscall`, proc.go:6801), falling back to
   `pidleget()` with no node preference.

So a realistic run queue is `[node0, node0, node2, node2, node3, ...]`
in terms of where each goroutine's data physically resides.

### 4.4 P↔M affinity after Go 1.26

Four CLs under issue #65694 (all by Michael Pratt) landed for Go 1.26:

| CL | What it does |
|----|-------------|
| [714800](https://go-review.googlesource.com/c/go/+/714800) | Converts `sched.midle` to a doubly-linked list (`listHeadManual`), enabling O(1) removal of a *specific* M via `mgetSpecific()`. |
| [714801](https://go-review.googlesource.com/c/go/+/714801) | After STW, Ps prefer to restart on their previous M. Mechanism: `p.oldm` holds an `mWeakPointer` to the last M; the M side is `m.self` (a weak self-pointer); the preference is applied in `procresize` (proc.go:6212). Reduced STW-induced P↔M migration from 99.8% to 1.9%. |
| [721001](https://go-review.googlesource.com/c/go/+/721001) | Splits `findRunnableGCWorker` into `assignWaitingGCWorker` + `findRunnableGCWorker`. |
| [721002](https://go-review.googlesource.com/c/go/+/721002) | Pre-assigns GC mark workers to idle Ps during start-the-world, preventing displacement of user Gs and preserving the CL 714801 affinity. |

Before these, any per-node placement would have been scrambled at every
GC STW. With them, a P that stays on one M stays on one OS thread —
and, to the extent the kernel keeps that thread on one node, on one
node. The ~2% residual migration comes from `wakep` stealing during
`startTheWorldWithSema` and goroutine `ready` during stack scanning.

### 4.5 The M struct size constraint

`m` must fit the 2048-byte size class: `mPadded` (runtime2.go:733)
enforces this at compile time, and `allocm` allocates via
`&new(mPadded).m`. Small field additions (an `int32`) fit; large
per-node state cannot live on `m`. The `p` struct (allocated via
`new(p)`) has no such constraint.

---

## 5. GC mark: Green Tea work distribution

**Green Tea is the default GC in this tree**: `GreenTeaGC: true` in the
baseline experiment configuration (`internal/buildcfg/exp.go:86`).
Descriptions of GC work distribution that predate Green Tea describe a
path that still exists but no longer carries most of the scan work.

### 5.1 Phase structure (what runs inside STW and what doesn't)

A GC cycle has two brief STW windows with the entire mark phase running
concurrently *between* them:

1. **Sweep-termination STW** — ends when `startTheWorldWithSema` is
   called at mgc.go:930.
2. **Concurrent mark** — dedicated and fractional `gcBgMarkWorker`s
   (plus idle workers and mutator assists) scan the reachable heap
   *while mutators run*. Background utilization target: 25%
   (`gcBackgroundUtilization`). This is where nearly all page access —
   and therefore all interaction with kernel NUMA balancing — happens.
3. **Mark-termination STW** — begins at mgc.go:1066, typically well
   under a millisecond when healthy.

Any mechanism that is active "only during STW" is inactive during
concurrent mark, i.e. during the phase that scans the heap.

### 5.2 Work sources and priority

Mark workers call `gcDrain()` (`runtime/mgcmark.go`), which pulls work
from five sources in priority order (documented at mgcwork.go:57-61):

1. `gcw.tryGetObjFast()` — P-local object workbufs, no synchronization.
2. `gcw.tryGetSpanFast()` — the P-local **span queue** (`gcw.spanq`),
   Green Tea's unit of distribution.
3. `gcw.tryGetObj()` — the global pool of full object workbufs
   (`work.full`), atomic access.
4. `gcw.tryGetSpan()` — the global span queue.
5. `gcw.tryStealSpan()` — steal spans from *other Ps'* span queues.

Under Green Tea, small-object scanning is batched by span: pointers to
small objects enqueue the containing span, and `scanSpan` scans
accumulated objects within it together, improving density and cache
behavior. The object-workbuf path (sources 1 and 3) still handles large
objects and various roots, but the span path carries the bulk of
small-object work — and small objects dominate typical Go heaps.

Object-address-to-metadata lookup on the mark path is
`findObject` → `spanOf` → `arenaIndex` (mbitmap.go:1363, mheap.go:687):
compute the arena index from the pointer, index the two-level arena
map, index `heapArena.spans` by page. This lookup is already paid for
on every marked object.

**Workbuf memory.** Object workbufs are 2048-byte structs
(`_WorkbufSize`) carved from 32 KB chunks (`workbufAlloc`) obtained via
`mheap_.allocManual(..., spanAllocWorkBuf)` and tracked in
`work.wbufSpans` (mgcwork.go:450) — heap-arena memory, not separate OS
mappings. The per-P `gcWork` struct is embedded in the P itself
(`pp.gcw`).

### 5.3 Where the cross-node traffic is

Nothing in mark considers where an object's physical page lives. A
worker on node 0 can drain a span queue entry whose span sits on node
2, scanning it with every load crossing the interconnect; the global
span queue and workbuf pool mix work from all nodes; `tryStealSpan`
picks victims with no distance preference. The scan (many loads per
object) is the expensive part; the mark-bit write is a single atomic.

---

## 6. Linux kernel NUMA mechanics

All kernel references verified against Linux v6.15 source
(`mm/mempolicy.c`, `kernel/sched/fair.c`) unless noted.

### 6.1 Two levels of memory policy

Every VMA can carry its own memory policy, set via `mbind(2)`.
Separately, each **thread** has a default policy, set via
`set_mempolicy(2)` — the syscall writes `current->mempolicy`, so it
affects the calling thread only; other threads of the process are
untouched. Children inherit the policy across fork/exec, which is how
`numactl` applies a policy to every thread of a program: it sets the
policy *before exec*, and all threads created afterward inherit it.

On a page fault, the kernel resolves the effective policy with
`get_vma_policy()`: the VMA's policy wins if present; otherwise the
faulting *thread's* policy applies; if neither is set, the default
policy is used.

Policy modes relevant here:

- `MPOL_DEFAULT` — first-touch: each page is allocated on the node of
  the thread that first faults it, plus automatic NUMA balancing (§6.2).
- `MPOL_BIND` — allocate only within the nodemask. With the local node
  in the mask, allocation still prefers local; with *all* nodes in the
  mask, placement behaves like first-touch.
- `MPOL_PREFERRED` — prefer a **single** node (the man page is
  explicit: one node only), falling back to any node on shortage.
  Overrides first-touch: pages faulted by a thread on node 3 land on
  the preferred node anyway. `MPOL_PREFERRED_MANY` (Linux 5.15+)
  generalizes to a set of preferred nodes.
- `MPOL_INTERLEAVE` — round-robin pages across the nodemask.
  `MPOL_WEIGHTED_INTERLEAVE` (Linux 6.9+) allocates proportionally by
  per-node weights, aimed at CXL/HBM tiered memory.

### 6.2 Automatic NUMA balancing — and what it skips

With `/proc/sys/kernel/numa_balancing` enabled (default on RHEL and
most distro kernels), the kernel periodically samples page access:
`task_numa_work()` (kernel/sched/fair.c) walks the process's VMAs and
installs `PROT_NONE` "hinting" PTEs; the next access takes a minor
fault, `task_numa_fault()` records which node the accessing thread ran
on, and `mpol_misplaced()` decides whether to migrate the page toward
the accessor via `migrate_misplaced_folio()`.

**The scanner skips VMAs whose policy lacks `MPOL_F_MOF`.** Verified in
v6.15 `task_numa_work()`:

```c
if (!vma_migratable(vma) || !vma_policy_mof(vma) ||
    is_vm_hugetlb_page(vma) || (vma->vm_flags & VM_MIXEDMAP)) {
        trace_sched_skip_vma_numa(mm, vma, NUMAB_SKIP_UNSUITABLE);
        continue;
}
```

`vma_policy_mof()` (mm/mempolicy.c) returns true only when the
governing policy carries the `MPOL_F_MOF` flag ("migrate on fault").
Which policies carry it:

- The **implicit default** (no `set_mempolicy`, no `mbind`): when a
  task has no explicit policy, `get_task_policy()` falls back to
  `preferred_node_policy[nid]`, which the kernel initializes with
  `.flags = MPOL_F_MOF | MPOL_F_MORON` in `numa_policy_init()`. This is
  the only common case where balancing is active — and it is exactly
  the state of an unmodified Go process.
- An **explicit policy set by `mbind(2)`** never gets `MPOL_F_MOF`. The
  historical opt-in flag (`MPOL_MF_LAZY`) has been removed from modern
  kernels; no `MPOL_MF_*` flag in v6.15 sets it.
- An **explicit policy set by `set_mempolicy(2)`** likewise never gets
  `MPOL_F_MOF`.

Consequences:

1. **Any VMA with any explicit `mbind` policy is permanently exempt
   from NUMA-balancing scans** — no hinting faults are installed on it,
   for any thread, regardless of mode or nodemask.
2. `numactl --membind=<all-nodes>` works primarily by this mechanism,
   one level up: the inherited explicit task policy lacks `MPOL_F_MOF`,
   so `vma_policy_mof()` returns false for every VMA without its own
   policy and the scanner skips the whole address space. The
   `mpol_misplaced()` short-circuit (for `MPOL_BIND`, a page whose node
   is in the mask returns "correctly placed") is real — verified —
   but it is a second-order backstop that is rarely even reached,
   because the hinting faults are never installed in the first place.

### 6.3 CFS thread migration across nodes

Memory policy does not pin *threads*. The kernel's load balancer
(`sched_balance_rq` in kernel/sched/fair.c) walks the `sched_domain`
hierarchy; at `SD_NUMA` domains it migrates threads when group
imbalance exceeds `imbalance_pct` (default 117). Cache-hot tasks get
`cache_nice_tries = 2` grace periods. For inter-node distances above
`RECLAIM_DISTANCE` (default 30), the kernel strips `SD_BALANCE_EXEC`,
`SD_BALANCE_FORK`, and `SD_WAKE_AFFINE`, making cross-node moves less
aggressive — but typical dual-socket SLIT distance is ~20–32, and at
≤30 wake-affinity remains enabled: a waker on node A can pull a wakee's
thread to node A.

Additionally, NUMA balancing itself tracks per-task fault statistics
and sets `task_struct.numa_preferred_nid`, which CFS uses as a
placement hint — the kernel forms its own opinion of where each thread
should run and may move it there.

**No notification exists** when the kernel migrates a thread: no
signal, no fd event. Detection options are polling `getcpu(2)` (§7),
the `sched:sched_move_numa` tracepoint (external tooling only), or
`/proc/self/stat` field 39 (file I/O; unusable at runtime frequency).

### 6.4 The distance matrix (SLIT)

The kernel exposes inter-node distances at
`/sys/devices/system/node/nodeN/distance` — one space-separated row per
node, sourced from the firmware's ACPI SLIT table. 10 means local;
higher is proportionally more expensive. Example, dual-socket AMD EPYC,
4 nodes (2 per socket):

```
$ cat /sys/devices/system/node/node0/distance
10 12 32 32
```

Nodes 0–1 share a socket (distance 12 over on-die fabric); nodes 2–3
are cross-socket (32 over the inter-socket link). The diagonal is
always 10. **Symmetry is not guaranteed** — SLIT permits
`d(A→B) ≠ d(B→A)` and rare platforms ship asymmetric tables; consumers
should read the full matrix rather than assume symmetry.

Allowed nodes are constrained by cpusets: cgroup v2 `cpuset.mems`
(Kubernetes Topology Manager, systemd `AllowedMemoryNodes=`) is
enforced by the kernel — `mbind`/`set_mempolicy` nodemasks are
intersected with the cpuset. A process can read its allowed set via
`get_mempolicy(MPOL_F_MEMS_ALLOWED)` or `/proc/self/status`
(`Mems_allowed_list`). Note the Go runtime has no precedent for parsing
`/proc/self/status`; the syscall route matches existing runtime style
(the cgroup package reads `/proc/self/cgroup` + `mountinfo`; auxv is
consumed raw).

### 6.5 Sub-NUMA topologies

Modern single-socket parts expose multiple NUMA nodes: Intel calls the
feature Sub-NUMA Clustering (SNC); AMD calls it NPS ("NUMA per
socket", NPS1/2/4). A 2-socket AMD EPYC in NPS4 presents 8 nodes. These
intra-socket distances are smaller than cross-socket ones, but the
node count means NUMA-blind software leaves locality on the table even
in single-socket servers.

---

## 7. Node identity primitives: getcpu, vDSO, rseq

**`getcpu(2)`** returns the calling thread's current CPU and NUMA node
— two integers, valid only at the instant of the call (the kernel may
migrate the thread immediately after). It answers "where am I right
now," which is a different question from `sched_getaffinity` (used by
the runtime's `getCPUCount()` in `os_linux.go` at startup: "how many
CPUs may I use").

**vDSO status in Go.** Go currently binds only these vDSO symbols:
`__vdso_gettimeofday`, `__vdso_clock_gettime`, `__vdso_getrandom` on
amd64 (vdso_linux_amd64.go:17-20) and `__kernel_clock_gettime`,
`__kernel_getrandom` on arm64 (vdso_linux_arm64.go:16-18). So:

- On **amd64**, the kernel exports `__vdso_getcpu`, but Go does not
  bind it today — using it requires a new `vdsoSymbolKey` entry plus an
  assembly stub. With that work, ~5–20 ns per call.
- On **arm64**, the kernel vDSO **does not export getcpu at all**
  (verified against v6.15 `arch/arm64/kernel/vdso/vdso.lds.S`, which
  exports only sigreturn, the time functions, and getrandom). Every
  `getcpu` is a full syscall — hundreds of nanoseconds — on exactly the
  high-core-count arm64 servers (Graviton, Ampere) that are prime NUMA
  targets.

**`rseq(2)`** is the fast alternative. Since Linux 6.3, the per-thread
rseq area contains a `node_id` field (verified in the v6.3 UAPI header:
`__u32 node_id`, "Contains the current NUMA node ID", updated by the
kernel with single-copy atomicity). Reading the current node becomes a
plain load from per-thread memory — ~1 ns, no syscall, works
identically on arm64. glibc's `sched_getcpu` already uses rseq. Caveats
for Go: the runtime does not register rseq today; registration is
per-thread (one-time in thread start), and coexistence with cgo code
that also registers rseq needs a story (rseq registration returns
`EBUSY` if an area is already registered; glibc registers it for
threads it creates).

---

## 8. Issue #14406 forensics

**Symptom (2016, Rick Hudson):** GC mark-termination STW inflated from
~10 ms to **1010 ms** on a multi-node machine, reproduced with a
workload of 100 stack-growing goroutines plus 1M idle goroutines.

**Mechanism, in current terms:** An unmodified Go process runs under
the kernel's implicit default policy — the only policy that carries
`MPOL_F_MOF` (§6.2), so NUMA balancing actively samples its pages.
During concurrent mark, `gcBgMarkWorker`s scan the entire reachable
heap from whatever nodes their Ms happen to occupy. The scan generates
hinting faults across all arena pages; `task_numa_fault()` sees workers
on node N touching pages homed on node M and schedules migrations,
which copy pages across the interconnect while holding page locks.
Mutators and mark workers stall on those locks; the mark phase
lengthens and STW windows balloon.

**The workaround that became the production recommendation:**
`numactl --membind=<all-nodes>`. Per §6.2, the explicit inherited task
policy lacks `MPOL_F_MOF`, so the balancer skips the entire address
space; pages stay put; placement still behaves first-touch because
every node is in the `MPOL_BIND` mask. The equivalent per-VMA form —
`mbind(MPOL_BIND, all_allowed_nodes)` on each heap arena — achieves the
same exemption for arena memory from inside the process, with no
per-thread action required, because the exemption is a property of the
VMA's policy, not of any thread.

No runtime fix was ever merged; the external workaround remains the
recommendation as of 2026.

---

## 9. Prior art in detail

**JVM (JEP 345, JDK 14).** G1 GC gained NUMA-aware heap region
allocation: each G1 region is bound on first use to the NUMA node of
the mutator thread triggering the allocation, via `mbind(MPOL_BIND)`;
the collector then prefers same-node regions for allocation and keeps
survivor copies on-node. Sangheon Kim reported **+20.64% Max-jOPS** and
+9.52% Critical-jOPS on SPECjbb2015 (4 nodes, 512 GB heap).

**.NET (Maoni Stephens, .NET Framework 4.5).** Server GC creates
per-NUMA-node managed heaps; allocation balancing prefers heaps within
the local node before considering cross-node balancing.

**TCMalloc.** Optional NUMA awareness partitions the *address space*:
each node gets its own virtual address range, so the owning node of any
pointer is computable from the address alone; per-CPU caches then draw
from the local node's range. This is the "address-partitioned heap"
model the proposal's Stage 1b/2 design adopts.

**jemalloc.** Binds arenas to nodes at the `mmap` level via
`extent_hooks`; per-CPU `tcache`s align with NUMA when threads are
pinned.

**NumaGiC (Gidra et al., ASPLOS 2015).** NUMA-aware GC for big-data
heaps (160–350 GB): GC workers scan mostly-local memory and push
remote references to remote-node inboxes, falling back to work stealing
to preserve progress. Up to **94%** application throughput improvement
vs. NUMA-unaware Parallel Scavenge. The "remote inbox" structure maps
naturally onto per-node global span queues in Green Tea terms.

**Vyukov design (2014).** Proposed a NUMA-aware Go scheduler with
per-node P partitioning, per-node M pools, and per-node global state.
Never implemented; Intel's Maria Bulatova prototyped pieces in 2016 and
surfaced the cross-process oversubscription problem (multiple
independent Go processes converging on node 0). The proposal's design
principles — opt-in, soft hints, allocator-first — are shaped largely
by that history.
