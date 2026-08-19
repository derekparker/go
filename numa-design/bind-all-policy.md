# Stage 1a task policy: `MPOL_BIND` over all allowed nodes

Status: current `GOEXPERIMENT=numa` behavior (2026-08-14).
Audience: review / Go team. Not a user-facing API.

## What we set

After topology init (still **after** `mallocinit`), `numaSetProcessBindAll` calls
`set_mempolicy(MPOL_BIND, mask_of_all_allowed_nodes)` on the init thread.

Each later heap arena, after `sysMap` replaces the VMA, gets:

1. `mbind(MPOL_BIND, all allowed nodes)`
2. `mbind(MPOL_PREFERRED, local node)`

`MPOL_BIND` here is **not** “pin this process to one node.” The nodemask is
every node `cpuset.mems` allows, so allocation can still land on any allowed
node. The point is the kernel NUMA balancer skips VMAs whose policy has no
`MPOL_F_MOF` — the same reason `numactl --membind=<all-nodes>` fixes #14406.

`maxnode` is the width of one nodemask word (64), not `MaxNodes+1`. A larger
`maxnode` made the kernel read past our stack slot.

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
| Heap arenas | `mbind` BIND-all then PREFERRED local. Balancer-exempt. First-touch biased to the allocating M’s node. Shortage can spill (`PREFERRED`). |
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
runtime init, plus extra PREFERRED on Go heap arenas.

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

- `TestNUMAProcessBindAll`: `get_mempolicy` mode is `MPOL_BIND` (2) on
  multi-node.
- Evidence pack: 5× gc-pause + `/proc/vmstat` vs `numactl --membind=0,1`.
