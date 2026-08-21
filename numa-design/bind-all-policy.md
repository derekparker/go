# Stage 1a task policy: `MPOL_BIND` over all allowed nodes

Status: current `GOEXPERIMENT=numa` behavior (2026-08-14).
Audience: review / Go team. Not a user-facing API.

## What we set

After topology init (still **after** `mallocinit`), `numaSetProcessBindAll` calls
`set_mempolicy(MPOL_BIND, mask_of_all_allowed_nodes)` on the init thread.

Each later heap arena, after `sysMap` replaces the VMA, gets:

1. `mbind(MPOL_BIND, all allowed nodes)`
2. `mbind(MPOL_PREFERRED, local node)` — **superseded.** Its own IMC
   locality gate failed (remote-DRAM share unchanged: +0.10%/-0.07% vs a
   required ≥10% drop) and a three-arm sweep proved it behaviorally inert
   (C-full vs C-L1, primary p=0.838, direct p=0.631): the call runs but
   changes nothing measurable, good host or bad. Task 6 removes it from
   `numaBindArena`, keeping only the `MPOL_BIND` call above. See
   "Fill-one-socket-first (v3)" below for what actually narrows placement
   for small processes now.

`MPOL_BIND` here is **not** “pin this process to one node.” The nodemask is
every node `cpuset.mems` allows, so allocation can still land on any allowed
node. The point is the kernel NUMA balancer skips VMAs whose policy has no
`MPOL_F_MOF` — the same reason `numactl --membind=<all-nodes>` fixes #14406.

`maxnode` is a fixed **65** (`numaMaxNode` in `numa_linux.go`) on every
architecture — never derived from the nodemask word width (32 bits on the
32-bit arches, 64 on the rest) and never `numa.MaxNodes` (64) or
`MaxNodes+1`. The kernel's `get_nodes()`/`copy_nodes_to_user()` decrements
`maxnode` and sizes its destination write as `BITS_TO_LONGS(maxnode-1)`
kernel `ulong`s; for 65 that's `BITS_TO_LONGS(64)` = 8 bytes, regardless of
the calling process's own word size. Passing `numaNodemaskBits+1` (33, not
65) on a 32-bit arch would still make the kernel round its copy up to that
same 8-byte length while our destination buffer was a single 4-byte
`uintptr` — a 4-byte out-of-bounds kernel write. `numaNodemask`'s buffer
(`numaNodemaskWords`: 1 native-uintptr word on 64-bit, 2 on 32-bit) is
sized to match that 8-byte requirement on every arch; see `numaMaxNode`'s
doc comment for the full derivation.

Single-node machines and `GOEXPERIMENT` off never call this.

## Why this is not a STW toggle

The old plan (`IMPLEMENTATION_PLAN` §1.4) set BIND at mark STW and restored
`MPOL_DEFAULT` at start-the-world. Do **not** revive that:

1. **Wrong window.** Concurrent mark (where #14406 hint faults happen) runs
   with the world running. A policy that exists only inside STW misses the
   storm.
2. **Wrong scope.** `set_mempolicy(2)` is per **thread**. One call from the
   STW thread does not change other Ms. `numactl --membind` works because it
   is in effect before exec, and every later thread inherits it.
3. **Wrong tool for heap.** Arena `mbind` is per-VMA and applies to every
   thread that touches those pages. That is the proposal’s 1a mechanism.

We still keep **process/task** BIND-all because arena `MPOL_PREFERRED` alone
did **not** zero hint faults on this host (2026-08-13: ~12k hint faults, ~4k
migrations). BIND-all task policy + BIND-then-PREFERRED arenas did (2026-08-14:
0 / 0, matching membind). See `RESULTS.md`.

Re-reading this with the Layer 2 finding above: this 2-point comparison never
isolated "task BIND-all + arena BIND-only" as its own arm, so it cannot by
itself distinguish which half did the work. Task 4's later three-arm sweep
did isolate them and found arena `MPOL_PREFERRED` behaviorally inert on its
own (C-full ≈ C-L1) — consistent with the zero here tracking to the
task-level policy, not the arena refinement. This entry does not contradict
Task 6's removal; it just predates the isolation that justified it.

## Inheritance

`set_mempolicy` applies to the calling thread. Linux `clone` copies mempolicy
to new tasks, so Ms created after `numaSchedinit` inherit BIND-all. `execve`
preserves the task mempolicy as well (this is how `numactl` works), so children
started via `os/exec` inherit BIND-all until they set their own policy.

Gaps:

- Mappings created **before** `numaSetProcessBindAll` keep whatever policy
  they had unless we `mbind` them later. Early `mallocinit` reservations are
  `PROT_NONE`; `sysMap` + `numaBindArena` then stamps heap VMAs.
- A thread that calls `set_mempolicy` / `numa_set_membind` itself overrides
  the runtime for that thread.
- We do not walk existing Ms and set policy on each; extra Ms are created
  after this call.

## What this does to non-heap mappings

Task policy is the default for **new** VMAs from that thread when the caller
does not pass an explicit policy (`mbind` after `mmap` still wins).

| Mapping | Effect with `GOEXPERIMENT=numa` on a multi-node box |
|---------|-----------------------------------------------------|
| Heap arenas | `mbind` BIND-all only. Balancer-exempt. (Layer 2's PREFERRED-local refinement was removed in Task 6 — its IMC gate failed and a three-arm sweep proved it inert; see "What we set" above.) |
| Goroutine stacks / workbufs | Manual spans from those arenas. Covered by arena `mbind`, not by a separate stack policy. |
| OS thread stacks (`clone` / glibc `mmap`) | Created after BIND-all → inherit task policy → balancer-exempt, first-touch among **all** allowed nodes (no local PREFERRED unless something `mbind`s them). |
| Runtime `mmap` (`sysAllocOS`, other `MAP_ANON`) after init | Same as task policy: BIND-all, no MOF. |
| `syscall.Mmap` / `unix.Mmap` from Go | Same: the M’s task policy. Callers who `mbind` afterwards keep their VMA policy. |
| cgo `malloc` / `mmap` on a Go-created thread | Same inheritance. libc `malloc` is not heap-arena `mbind`’d; it is BIND-all first-touch. |
| cgo threads created by `pthread_create` from those threads | Inherit BIND-all. |
| Pre-existing C threads / `LD_PRELOAD` allocators that set their own policy | Unchanged. |
| `MAP_SHARED` file maps | Task policy still applies unless `mbind`d; file pages are a different NUMA story. We do not special-case them. |

BIND-all does **not** OOM when one node is full: every allowed node is in the
mask. It **does** turn off auto-balancing for those VMAs, including C heaps
and user `mmap`s that never call `mbind`.

That is the product question: the experiment currently behaves like wrapping
the process in `numactl --membind=<allowed-nodes>` for **new** mappings after
runtime init.

## Fill-one-socket-first (v3)

Confinement (design §12.2, `numaConfineIfSmall`/`numaShouldConfine`/
`numaConfine` in `numa_linux.go`) narrows a small, explicitly-sized process
to one NUMA node's CPUs and its task mempolicy, decided once from
`schedinit` while m0 is still the only runtime thread — strictly *after*
Layer 1 (`numaSetProcessBindAll`) has already run and published the
allowed-node mask. It narrows on top of Layer 1; nothing here replaces it.

| Mapping | Effect while confined |
|---------|------------------------|
| Task-default policy (new allocations on the confined M with no explicit `mbind`) | `MPOL_PREFERRED` to the confined node, **replacing** the Layer-1 BIND-all task policy on that thread (`numaConfine`'s `set_mempolicy` call). Locked decision 1: PREFERRED, never single-node `MPOL_BIND` — BIND has no fallback node, so pinning the whole process to one node is exactly the OOM footgun this doc's Recommendation section below forbids. |
| Heap arenas | Unchanged and still running: `numaBindArena` keeps stamping every new chunk `MPOL_BIND` over the **full** allowed-node mask, not narrowed to the confined node (locked decision 5). Confinement changes where the task-default policy points; it does not touch the arena VMA policy. |

Gate 4's confined-1P strace confirms this empirically: `mbind`=22 (11
heap-chunk grows, each still issuing the BIND-then-PREFERRED pair that was
active in that gate run, predating Task 6's removal above), distinct from
confinement's own setup calls — `sched_setaffinity`=1,
`set_mempolicy`=2 in order `MPOL_BIND` (Layer 1, from `numaSchedinit`) then
`MPOL_PREFERRED` (confinement, from `numaConfineIfSmall`).

**Inheritance.** `numaConfine`'s `set_mempolicy(MPOL_PREFERRED)` call is
per-thread, exactly like Layer 1's. It runs while m0 is the only runtime
thread, so `clone` copies it to every M created afterward, which inherit
PREFERRED-to-node until stand-down; a thread that sets its own policy
overrides the runtime, same gap as Layer 1's inheritance section above.

**The until-first-park residual after stand-down.** Stand-down detection
(`numaStandDownIfNeeded`) runs under `sched.lock`/`mp.locks != 0` and must
not make syscalls there, so it only flips a one-way latch (`numaStoodDown`).
The actual widening is two-part: `numaStandDownWiden`'s best-effort eager
walk over `allm` (a latency optimization only — it can miss an M mid-exit,
or, rarely, hit a tid the kernel already recycled for something unrelated),
and `numaFixThreadPlacement`, called from every M's own next `stopm` park,
which restores that M's saved affinity and Layer-1 BIND-all task policy.
The second path is where correctness actually lives: an M cloned in the
window between the eager walk being dispatched and every live M having
converged, or one `allocm` creates after stand-down, inherits confined
placement from its creating M and keeps it until it first parks (locked
decision 4). This is documented, accepted residual behavior (`numaStandDownWiden`'s
doc comment; RESULTS.md's N1 note), not a bug — such an M stays
balancer-exempt throughout via its explicit task policy, just delayed in
reaching the fully-stood-down state, never incorrectly or unsafely placed.

**Why the arena `mbind` still runs while confined.** The confined task
policy is per-thread and reaches only Ms the runtime itself `clone`s after
confinement takes effect. It never reaches threads that already existed
before `numaSchedinit` ran, or cgo threads a C library spawns via
`pthread_create` outside Go's clone path — locked decision 1's
"pre-runtime cgo threads." `numaBindArena`'s VMA-level `MPOL_BIND` is what
still holds against those: a VMA policy governs whichever thread touches
its pages, task policy or not. Keeping that arena policy uniform BIND-all,
rather than narrowing it to the confined node, also lets adjacent chunks
VMA-merge — Gate 4 measured a 32-line `/proc/PID/maps` for the confined
arm, matching the Layer-1 baseline character, not the ~1172 VMAs Layer 2's
per-chunk different-node `MPOL_PREFERRED` calls produced (the same
fragmentation Task 6 cites as one reason to remove that call).

## Dynamic cpuset staleness (v3)

`cpuset.mems` and `cpuset.cpus` can change at runtime under a container
(a scheduler resizing the container's cgroup, an operator running
`docker update --cpuset-mems`, etc.). This section is the accepted model
for how — and how late — the runtime notices.

**What is a startup snapshot.** Both masks the experiment builds its
decisions from are read once, not tracked live:

- `numaAllowedNodemask`, the allowed-node mask `numaSetProcessBindAll`
  publishes from `get_mempolicy(MPOL_F_MEMS_ALLOWED)`.
- `numaSavedAffinity`, the process's startup CPU affinity mask
  `numaShouldConfine` saves via `sched_getaffinity` before any narrowing.

**The only re-read point.** `numaSetProcessBindAll` has exactly two call
sites: once from `numaSchedinit` at startup, and once from
`numaStandDownWiden`, which runs after a stand-down trigger fires
(`numaStandDownIfNeeded` — GOMAXPROCS grown past the confined node, or a
GOMAXPROCS-control transition back to automatic). Stand-down is a
consequence of a GOMAXPROCS change, not of a cpuset change — there is no
code path that watches `cpuset.mems`/`cpuset.cpus` directly. So a cpuset
that narrows while the process is running, with GOMAXPROCS untouched, is
not observed until the *next* stand-down trigger or a process restart —
whichever comes first.

**What happens meanwhile.** Nothing crashes; placement just goes stale in
the two ways below, both intersect-or-fail against the kernel's live
policy rather than corrupt anything:

- Heap arena `mbind` calls (`numaBindArena`, on the heap-growth path)
  keep using the last-read `numaAllowedNodemask`. If the kernel's actual
  allowed set has shrunk since, `mbind` targets nodes the cpuset no
  longer permits; the kernel `mbind` call itself will reject or clip a
  target outside the current cpuset (mbind errors are already ignored
  here by design — see above).
- The stand-down restore path (`numaFixThreadPlacement`,
  `numaStandDownWiden`'s eager walk) restores each thread to the startup
  `numaSavedAffinity` via `sched_setaffinity`. If that startup mask is no
  longer a subset of the process's current cpuset, Linux's
  `sched_setaffinity` either narrows the request to its intersection with
  the live cpuset, or — if the intersection is empty — fails the call
  outright (`numaSetThreadAffinity` returns `false`); a failure here is
  read as "retry at next park" (`numaFixThreadPlacement`'s early return),
  never treated as fatal.

**Why this is accepted, not fixed.** Re-reading the cpuset on every heap
grow (`numaBindArena` runs under `h.lock`, nosplit, once per ~4 MiB) would
put a `get_mempolicy`/`set_mempolicy` syscall pair on the heap-growth
path — exactly the cost this design goes out of its way to avoid
elsewhere. And there is no portable notification that `cpuset.mems` or
`cpuset.cpus` changed for the runtime to block on instead of polling;
polling on a timer would reintroduce the same cost on a different clock.
Containers that repartition cpusets mid-run therefore get
correct-but-stale placement until the next stand-down trigger or restart
— never a crash, never memory placed outside the mask that was live at
the time each policy call was actually issued.

## Recommendation

- **Do not add a STW-only `set_mempolicy` toggle.** It is the wrong window and
  the wrong scope.
- **Keep task BIND-all for the experiment** until we prove a smaller surface
  still zeros hint faults. The smaller alternative is: drop
  `numaSetProcessBindAll`, and `mbind` every runtime-owned VMA (arenas already;
  also OS stacks / other `sysAllocOS` if they still show up in vmstat). If
  `numa_hint_faults` stays 0, drop the task policy so cgo and `syscall.Mmap`
  stay at kernel default.
- **Do not** use `MPOL_BIND` with a **single** node for the process. That is
  the OOM footgun the proposal forbids.
- If this ever ships, document that `GOEXPERIMENT=numa` changes default
  mempolicy for the process (like `numactl --membind`), and that libraries
  which call `set_mempolicy` themselves still win on that thread.

## Checks we already have

- `TestNUMABindAllTaskPolicy`: `get_mempolicy` mode is `MPOL_BIND` (2) on
  multi-node, unconfined path. The test skips when fill-one-socket
  confinement is active, since confined mode is `MPOL_PREFERRED`, not
  `MPOL_BIND` — see "Fill-one-socket-first (v3)" above.
- Evidence pack: 5× gc-pause + `/proc/vmstat` vs `numactl --membind=0,1`.
