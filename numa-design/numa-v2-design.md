# NUMA runtime v2 — design from measured evidence

**Status:** Design approved for planning (2026-08-19). Implementation plan: [`2026-08-19-numa-v2-implementation-plan.md`](2026-08-19-numa-v2-implementation-plan.md).  
**Supersedes for future work:** [`numa-proposal.md`](numa-proposal.md) staging and [`IMPLEMENTATION_PLAN.md`](IMPLEMENTATION_PLAN.md) as a *backlog*. Those documents plus [`RESULTS.md`](RESULTS.md) remain the record of what `numa-dev` built and measured.  
**Does not supersede:** [`numa-runtime-background.md`](numa-runtime-background.md) (how stock Go and Linux NUMA work), [`bind-all-policy.md`](bind-all-policy.md) (what BIND-all actually does).

Author: Derek Parker. Test machine: 2-socket Xeon Platinum 8592+, 256 CPUs, ~30 GiB RAM, even/odd CPU↔node interleave, `kernel.numa_balancing=1`.

---

## 1. Decision locked before this document

**Never land a change that makes single-P allocation slower than the parent commit.**

Operational definition (layer invariant, NUMA machine):

- Workload: `golang.org/x/benchmarks` **json**, `GOMAXPROCS=1`, same binary flags otherwise (start with `-benchmem=512 -benchnum=3 -benchtime=3s`; raise mem only if 1P heap is too small to see GC).
- Metrics: median **ns/op** and **user+sys-ns/op**.
- Bar: numa **≤ baseline** within a stated noise band. Until we have a longer 1P series, use **+2%** as a fail (either metric). A balancer or locality win does **not** waive this bar.
- Same bar on a tiny `runtime` alloc micro (`BenchmarkNUMAAllocMixed` or a dedicated 1P `mallocgc` loop) so we do not depend only on json.

If 1P json regresses, the CL does not merge. Sweet, Prometheus, and 256P results are supporting evidence only at that point.

---

## 2. Why v2 exists

The v1 proposal assumed extra allocator and scheduler work would be paid back by locality (lower remote DRAM, better app ns/op). On this machine that did not happen.

What v1 actually showed ([`RESULTS.md`](RESULTS.md), json CPU profiles 2026-08-19):

| Claim | Evidence |
|-------|----------|
| #14406: stop kernel NUMA balancer on the heap | **True.** `GOEXPERIMENT=numa` binaries: 0 hint faults / 0 migrations, matching `numactl --membind=0,1`. |
| Apps get faster | **False on the programs we care about.** json +36%, http +23%, etcd put +36% / STM +42%, bleve-index +156%. garbage ~−6%. tile38 / Prometheus query rate ~flat. |
| Remote DRAM share drops ≥10% | **False.** App benches stayed ~45–49% remote. Prometheus bind-all **raised** remote share (~34% → ~46%) because pages stay on both nodes and the balancer cannot migrate hot pages home. |
| Extra machinery is cheap | **False.** `numaFlushForeignCachedSpans` is called from `getMCache` (every allocation) and `acquirep`. It always walks **136** span classes. CPU profile: **~11% of samples at `GOMAXPROCS=1`**, **~22% at 256 P**. 1P json already **+10%** ns/op with identical allocs/op. |
| Logical `p.numaNode` is the socket | **False on this host.** Even CPUs = node 0, odd = node 1. `numaAssignPNodes` splits by P index. Steal “local” is not physical local unless Ms are pinned. |

v1 also never shipped the 1b keystone (address-partitioned heap). What landed was BIND-all + PREFERRED arenas + per-node mcentral + two-tier steal + flush-on-every-malloc.

**Everything is back on the table**, including 1a. A layer may still be its own CL if it has standalone value *and* passes the 1P bar.

---

## 3. Two products, one stack

Holistic picture: Linux NUMA + Go’s P/M/mcache/mcentral/GC. Two user-visible products that must not smuggle costs into each other:

| Product | User-visible win | Allowed cost |
|---------|------------------|--------------|
| **A. Balancer exemption (#14406)** | During GC (and in general), the kernel does not migrate heap pages. vmstat: 0 hint faults / 0 migrations vs tens of thousands–millions on stock Go. STW may get cheaper; throughput need not. | **Zero** 1P alloc regression. 256P json must not get *slower* as the price of 0/0. |
| **B. Allocation and scheduling locality** | Objects and workers stay on the node that allocated / woke them. Measured as remote DRAM share and/or app ns/op. | **Zero** 1P alloc regression. If remote share does not move, the layer is killed even if 1P is flat. |

A is not allowed to include span tagging, per-node mcentral, or mcache flush “because we will need it for B.” B is not allowed to weaken A’s vmstat win without an explicit, measured trade.

Stock Go remains the baseline. `GOEXPERIMENT=numa` off is bit-identical in behavior to today (v1 already aimed at this; v2 keeps it).

---

## 4. Hardware and scheduling facts the design must name

Test machine (`numa-dell`):

- 2 nodes, 128 CPUs per node, **interleaved numbering**.
- ~15 GiB DRAM per node, ~30 GiB total. Do not use 32 GiB heap targets.
- Default cpuset is all CPUs and both memory nodes. We do **not** currently `sched_setaffinity`.
- `GOMAXPROCS=256` json: both nodes occupied (~half of threads on even CPUs, half on odd).
- json’s `-affinity` exists in `x/benchmarks` and was **not** used. v2 tests must say whether affinity is on.

Implications:

- “Span both NUMA nodes” is the default as soon as two threads run. A 1P test is the *absence* of spanning, which is exactly why it is the merge bar.
- Any steal or P→node map that uses **P index** without pinning Ms is not a locality feature on this box.
- BIND-all (`membind=0,1`) **cannot** cut remote share the way pinning to one node can. IMC “remote %” is the wrong success metric for product A. It is optional for product B.

---

## 5. Mechanisms under review (not a backlog)

Keep, redesign, or drop — decided per layer by gates, not by v1 checkboxes.

| v1 mechanism | Standing |
|--------------|----------|
| `GOEXPERIMENT=numa` / topology / `getcpu` | Likely keep as layer 0 if 1P delta is **zero**. Tracing must not be on the alloc path (`GODEBUG=numa=1` is diagnostic only). |
| Process BIND-all + arena BIND then PREFERRED | Candidate for product A **alone**, with no span tags. Re-prove 1P json. Inheritance/cgo notes in `bind-all-policy.md` stay a review issue, not a free pass on speed. |
| STW-only `set_mempolicy` toggle | **Rejected** (wrong window, per-thread). Do not revive. |
| `m.numaNode` frozen at `mstart` | Stale after the kernel migrates the M. Either refresh at a safe point, pin the M, or do not use it to choose span homes. |
| Span `numaNode` + flush all mcache classes on every `getMCache` / `acquirep` | **Rejected as designed.** This is the measured 1P tax. v2 may not walk 136 classes per alloc. If homes are required, movement of a P to another node must be O(1) or rare (see §7). |
| Per-node mcentral | Only after placement is real and 1P is clean. Sharding without placement is extra pools for the same interleaved first-touch. |
| Two-tier `numaStealWork` (scan all Ps twice, then the normal 4× steal) | Only after 1P is clean (steal is idle at 1P) **and** “local” means physical node. Extra full-`allp` scans are suspected overhead at 256 P; they were not the json CPU top, flush was. |
| Address-partitioned heap | Still the only way to make “node of this pointer” O(1) without per-span tags. Unshipped in v1. Reconsider as the core of product B, not as a later nice-to-have. |
| Green Tea NUMA mark | Last, if ever. No dedicated mark interconnect win in v1 proxies. |
| CPU affinity / `P` pinned to a node | On the table. It is the honest way to make `p.numaNode` true. It is also a behavior change (less OS freedom). Must pass 1P (no-op) and not regress 256P json vs stock. |

---

## 6. Layering (holistic stack, separable CLs)

Each layer is a CL-sized slice. **Stop** if its gates fail. Do not start layer *n+1* to “make up for” layer *n*.

### Layer 0 — Experiment + topology only

**In the picture:** the runtime can *see* nodes. Hot paths still look like stock Go.

**Standalone value:** tests, `GODEBUG` topology dump, future layers.

**Must not include:** `mbind`, `set_mempolicy`, span tags, mcache flush, steal, per-node mcentral.

**Gates:**

- 1P json: **no slower** (§1).
- Single-node or experiment off: no behavior change (existing tests).
- Optional: print topology under `GODEBUG=numa=1` only.

### Layer 1 — Product A: balancer exemption only

**In the picture:** #14406 without locality machinery.

**Candidate (re-prove, do not copy blindly):** task `set_mempolicy(MPOL_BIND, all allowed nodes)` after `mallocinit`, plus arena `mbind` BIND-all. **No** PREFERRED, **no** span `numaNode`, **no** flush. PREFERRED is a product-B placement hint; mixing it into A is how v1 coupled the two.

If BIND-all alone fails vmstat, add the **minimum** extra VMA policy that restores 0/0 **without** touching `getMCache`.

**Standalone value:** operators can run “stock allocator, no balancer storms.” Ship even if product B never happens.

**Gates:**

- 1P json: **no slower**.
- 256P json: ns/op **not worse** than parent (same +2% band); peak RSS not a large unexplained jump.
- #14406: three-way gc-pause or json **vmstat** — baseline noisy, layer-1 and `numactl --membind=0,1` at **0 / 0**.
- IMC remote share: **do not fail the layer** if it stays flat or rises (expected for bind-all-nodes).

**Promotion (not a merge gate):** one Prometheus or garbage run for vmstat 0/0 on a real process.

### Layer 2 — Product B: placement without per-alloc mcache walks

**In the picture:** new pages prefer the **physical** node of the allocating M, and that preference is not implemented by scanning `mcache.alloc`.

Design work (pick one approach in the implementation plan; do not implement two):

1. **Address-partitioned arenas** (v1 1b): node is a function of pointer. No span-home flush.
2. **Pin M (or P+M) to a node** so a P’s mcache never changes node. Flush becomes rare or unused.
3. **Node-local mcache not owned by a migratable P** (bigger runtime change).

**Forbidden:** `numaFlushForeignCachedSpans` on the `getMCache` fast path.

**Gates:**

- 1P json: **no slower**.
- 256P json: ns/op not worse; RSS not systematically fatter than layer 1.
- IMC: remote share **drops** vs layer 1 on a DRAM-heavy bench (pointer chase or json at large heap), or the layer is killed. Suggested bar: ≥10% relative drop in `remote/(local+remote)` on the same events as `RESULTS.md`, or document why this hardware cannot show it (interleave + bind-all).

**Prediction (§12.1):** this gate is expected to FAIL — layer 2 is the homing ingredient alone. Run it anyway; a kill write-up must say "a proper subset of the three ingredients measured ~zero", not "locality impossible".

### Layer 3 — Sharding (mcentral, optional stacks)

**In the picture:** less cross-node contention on the central free lists, *after* homes are meaningful.

**Gates:** 1P json no slower; 256P json not worse; no RSS explosion. Steal still off.

### Layer 4 — Scheduling (steal) and optional GC mark

**In the picture:** prefer runnable Gs (and later mark work) on the same **physical** node.

**Gates:** 1P json no slower (steal idle). 256P json/http not worse. Steal victims must be selected by **M/CPU node**, not P index, unless layer 2 pinned Ps.

---

## 7. Why the v1 flush existed — and what v2 must do instead

P’s mcache can be used by a different M after `acquirep`. If spans are tagged with the allocating node, a foreign M would keep handing out remote objects until the span emptied. v1 flushed **every** alloc because an `numaOwnerNode` early-out missed that case (test flakes).

That is a real invariant. The **implementation** (O(span classes) on every malloc) is unacceptable under §1.

v2 may only preserve the invariant by making foreign spans **rare** (affinity / node-stable P) or **free to detect** (address partition: the span’s node is the pointer, and the mcache is filled only from the current node). A full scan of 136 classes is not a allowed “correctness tax.”

---

## 8. Testing plan

### 8.1 Pyramid

| Role | Tests | When |
|------|--------|------|
| **Layer invariant** | 1P json + 1P alloc micro vs **parent commit**, same GOROOT, ±experiment. | Every layer, every CL. Fail = stop. |
| **Off-binary identity** | `objdump -d` parent-vs-HEAD with experiment **off** differs only in build IDs; 1P `perf stat -e instructions` flat with experiment on (§12.4). | Every layer. |
| **Span check** | 256P json; sample even/odd running CPUs (or `Cpus_allowed_list` + task `processor` field) so both nodes are used when we claim 256P. | After 1P passes. Fail on ns/op or RSS blow-up. |
| **Product A proof** | vmstat around gc-pause or json: 0/0 vs membind oracle. | Layer 1. |
| **Product B proof** | `perf stat` IMC local/remote DRAM on a long enough DRAM bench (not `AllocMixed` at `b.N` cap). | Layer 2+. |
| **Promotion** | Sweet `etcd,tile38,bleve-index` (`GOTOOLCHAIN=local`, one `sweet run`, two configs); Prometheus+Avalanche harness. | After a **shippable slice** (layer 1, or 1+2, …), not after topology-only. |
| **Never a pass/fail gate** | Bleve `N=1`, etcd p99, single Prometheus QPS, one-shot STW p99. Record in `RESULTS.md`. | Always. |

### 8.2 Sweet and Prometheus

**Valid as promotion / integration.** Invalid as the definition of a layer.

They mix allocator, scheduler, GC, kernel balancer, and (Sweet) `GOMAXPROCS` splits (etcd 64×4, tile38 192/64). They would not have caught the 1P flush tax as a *layer* failure; they would have shown “apps got slower” after several layers.

Rules:

- Do not enable Sweet `perf record` diagnostics (driver `ResetTimer` SIGINT+Wait hung on PEBS).
- Always `GOTOOLCHAIN=local` (etcd pins `toolchain go1.23.x`, which rejects `GOEXPERIMENT=numa`).
- Prometheus: vmstat 0/0 is a fair product-A check; query QPS is not a locality metric at ~230 MiB RSS.

### 8.3 Comparison protocol

- Same GOROOT, two builds: default vs `GOEXPERIMENT=numa` (and later `GODEBUG` policy if split).
- Do not compare Sweet results across separate `sweet run` invocations.
- Rebuild the toolchain on `numa-dell` after checkout (`make build`).
- Parent of the CL is the performance baseline, not “stock Go from last month.”

### 8.4 `GOMAXPROCS=1` is mandatory even when the feature is multi-node

Multi-node behavior is tested at 256P. 1P answers: **did we put work on `mallocgc`?** That question is independent of spanning sockets. v1’s 1P json +10% is the existence proof.

---

## 9. What to do with `numa-dev`

- Keep the branch and `RESULTS.md` as **evidence**, not as the default merge direction.
- Do not land further locality patches on top of the flush.
- v2 implementation should restart from **upstream master** (or a branch with only reviewed, gated layers), porting BIND-all **only after** layer 0 passes 1P, and only if layer 1 still passes 1P without flush/mcentral/steal.
- Optional: a “v1 autopsy” appendix in review mail linking this file + `RESULTS.md` + the json pprof (`numaFlushForeignCachedSpans` 11% / 22%).

---

## 10. Success for the program (not a single CL)

The work is successful if:

1. Every merged CL passed §1.
2. Product A exists as a CL that an operator can enable for #14406 without taking B’s machinery.
3. Product B either (a) shows a locality win without 1P or 256P json loss, or (b) is explicitly abandoned in writing with measurements.
4. No stage is “complete” because the code landed; it is complete when its gates passed on `numa-dell`.

---

## 11. Open questions (do not block writing the implementation plan)

1. Exact 1P noise band after a 10-run json series (2% is a starting fail line).
2. Whether product A should remain `GOEXPERIMENT` or become a `GODEBUG` policy on an experiment-enabled toolchain.
3. Whether M pinning is acceptable in Go 1.N (OS scheduler vs runtime).
4. Whether address partitioning is feasible in the current arena allocator without a multi-month heap rewrite.
5. 4-node EPYC (#78044) is out of scope for the first v2 slice; the 1P bar still applies there later.
6. Whether **fill-one-socket-first** at small GOMAXPROCS (§12.2) should be measured before any general locality mechanism — it may be the cheapest real win.

---

## 12. Cross-check: independent design (2026-08-19)

A second design was produced from the same evidence by an agent with **no access to this document or the plan** (only the problem statement, the measured facts, and stock master source). It is archived as [`2026-08-19-numa-design-fresh.md`](2026-08-19-numa-design-fresh.md). It independently converged on this document's spine — two products, the 1P bar, no per-alloc flush, the `MPOL_F_MOF` exemption mechanism, refill-time `getcpu` — which is meaningful validation. The items below are adopted from it.

### 12.1 Locality needs three ingredients

Locality = (a) memory **homed** per node + (b) refill-time **routing** so a thread is fed spans homed where it runs + (c) **threads that stay put** long enough for (b) to still hold at use time. **Any proper subset is expected to measure ~zero.** v1's arena PREFERRED was (a) alone — measured ~45–49% remote, unchanged. Plan Layer 2 is also (a) alone; its IMC gate failing is the *predicted* outcome (see §6 Layer 2). Every NUMA-successful allocator implements all three: TCMalloc's NUMA mode partitions the routing layer per node; HotSpot `+UseNUMA` carves TLABs from the eden region of the node the thread runs on at refill time; jemalloc/mimalloc first-touch locality is downstream of OS thread stability. Go uniquely lacks (c) because Ms float freely — which is why sharding or homing alone cannot show a win.

### 12.2 Fill-one-socket-first (possibly the cheapest real win)

When GOMAXPROCS ≤ CPUs-per-node, keeping the entire process on one socket (one node-mask affinity choice at startup) makes *everything* local — no homing, no routing, no per-P machinery, and the balancer exemption still applies. Neither v1 nor this document previously considered it. Measure it before (or instead of) any general Layer 3–4 mechanism; it is also the honest story for small processes on big NUMA boxes.

### 12.3 Concrete address-partition sketch (deferred product-B core)

The "address-partitioned heap" (§5) has a CL-sized shape: per-node `arenaHints` chains and per-node `curArena{base, end}` with hint addresses ≥1 TB apart, plus one `node uint8` field in `heapArena`. Pointer→node is then the same two loads `spanOf` already does (an mspan never crosses arenas, so span home = arena home). `mheap.grow(npage, node)`; with the experiment off the node argument is a constant and the code collapses to today's shape. Homing enforcement is `mbind(MPOL_PREFERRED, node)` at `sysMap`. VMA policy **survives the scavenger** — `MADV_FREE`/`MADV_DONTNEED` drop pages, and refault re-homes them under the surviving policy — so no re-bind is needed. Caveat: tight-VA architectures (39-bit) may not afford per-node hint spacing; fall back to shared hints, keeping `heapArena.node` correctness and losing only address-monotonicity. Record this sketch as the starting point if product B is re-attempted after a Layer 2 kill.

### 12.4 Smaller adoptions

- **Off-binary proof:** `objdump -d` diff of parent-vs-HEAD builds with the experiment **off** (must differ only in build IDs) turns "bit-identical when off" from an assertion into a gate. Complement the 1P timing band with `perf stat -e instructions` on a fixed-work 1P alloc loop, experiment off vs on — the count must be exactly flat, which is achievable because no fast-path code changes, and is immune to timing noise.
- **Routing validation trick:** validate Layer 3 refill routing with *externally pinned* threads first (`numactl --cpunodebind` per half), isolating routing correctness from placement stability; only then measure unpinned.
- **In-vivo locality proxy:** `/numa/span-refills:local` / `/numa/span-refills:remote` `runtime/metrics` counters, incremented only in refill paths, never on malloc — a cheap continuous signal between perf-counter runs.
- **Affinity stand-down rule:** any layer that sets thread affinity must detect a non-default initial mask (taskset, narrowed cpuset) and stand down entirely — the operator's placement wins.
- **Kernel matrix:** also test `kernel.numa_balancing=2` where supported, and record `uname -r` with every result; multi-node `MPOL_BIND` allocation order is kernel-version-dependent.
- **getcpu:** prefer vDSO `__vdso_getcpu` on amd64 (the runtime already has vDSO plumbing) with the raw syscall as the arm64/fallback path; at grow/refill frequency the raw syscall alone is also acceptable.
