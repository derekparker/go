# NUMA Runtime v3 Implementation Plan — Locality After the Layer-2 Kill

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver the locality win that Layer 2 (homing alone) could not, in three strictly gated workstreams: **A** — fill-one-socket-first confinement (design §12.2, the priority item and the cheapest real win); **B** — the full three-ingredient locality unit (homing + routing + thread stability as ONE gated change, design §12.3–§12.4), started only if A ships or A's gates show the remaining gap justifies it; **C** — enablers and diagnostics (phase-shift attribution rerun, rseq/vDSO getcpu, getcpu asm for remaining arches, cpuset-staleness documentation). Every gate is built on the pathology A/B/C harness (`pathology-bench-design.md`) whose corrections history (`RESULTS.md`) is the reason for this plan's statistical constraints.

**Architecture:** Workstream A leaves `numaSchedinit` (topology + Layer-1 BIND-all) **untouched at its current call site** (`proc.go:912`) and adds one new call, `numaConfineIfSmall(procs)`, between `unlock(&sched.lock)` (`proc.go:953`) and `worldStarted()` (`proc.go:956`) in `schedinit`: when the experiment is on, the machine is multi-node, GOMAXPROCS was **explicitly set** (`sched.customGOMAXPROCS`), the process has **no pre-existing narrowed CPU affinity** (operator placement always wins), and GOMAXPROCS ≤ one node's CPU count — narrow the process to the boot CPU's node (`sched_setaffinity` to that node's CPUs) and replace only the **task** policy with `set_mempolicy(MPOL_PREFERRED, node)`. Layer 1's arena BIND-all `mbind` keeps running while confined (`numaAllowedNodemask` stays published) — the uniform per-chunk policy merges VMAs and keeps heap VMAs balancer-exempt against *any* thread, including pre-existing cgo threads the task policy never reached. A one-way stand-down (trigger detected in `startTheWorldWithSema`, the single funnel through which every GOMAXPROCS change flows; per-thread convergence at each M's next park in `stopm`) reverts to exact Layer-1 behavior if GOMAXPROCS later exceeds the node. Workstream B implements design §12.3's address-partition sketch (per-node arena streams, `heapArena.node`), per-node mcentral spanSets with getcpu **at refill only**, and node-mask soft affinity applied from `schedule()` firing only on node change — validated pinned-first per §12.4. Workstream A also carries the task to **remove** the superseded Layer-2 PREFERRED-at-grow code once A's **hard** gates pass.

**Tech Stack:** Go runtime (`src/runtime`, `src/internal/runtime/numa`, `src/internal/runtime/syscall/linux`), Linux `sched_setaffinity`/`sched_getaffinity`/`set_mempolicy`/`mbind`/`getcpu`, remote host `numa-dell`, `golang.org/x/benchmarks` json/garbage, `numa-design/gc-pause-bench`, `numa-design/phase-shift`, benchstat.

**Spec:** `numa-design/numa-v2-design.md` §12.1–§12.4 (these ARE the specs for the three workstreams). **Harness:** `numa-design/pathology-bench-design.md`. **Evidence:** `numa-design/RESULTS.md` (Layer 2 verdict; pathology sections **including all corrections** — read the corrections; the constraints below exist because of them). **Policy forensics:** `numa-design/numa-runtime-background.md` §6.2/§8, `numa-design/bind-all-policy.md`.

---

## Design decisions locked (Workstream A)

Each decision is final for this plan; changing one requires editing this section first (pre-registration discipline).

1. **Memory policy while confined: task `set_mempolicy(MPOL_PREFERRED, confined-node)` — not single-node BIND — with Layer 1's arena BIND-all `mbind` still running.** PREFERRED spills gracefully when the node fills, avoiding the single-node-BIND OOM footgun `bind-all-policy.md` explicitly forbids ("Do not use MPOL_BIND with a single node for the process"). **Balancer exemption is carried by two mechanisms and both stay in force:** (a) per `numa-runtime-background.md` §6.2 (verified against v6.15 `task_numa_work()`), *any* explicit policy set by `set_mempolicy(2)` never carries `MPOL_F_MOF`, so the scanner skips policy-free VMAs *for threads that inherited the explicit task policy*; but `vma_policy_mof()` falls back to the **scanning task's** policy when the VMA has none — a pre-existing cgo/c-archive thread still running the implicit default (`MPOL_F_MOF|MPOL_F_MORON`) would re-open policy-free heap VMAs to the balancer. Therefore (b) the arena BIND-all `mbind` keeps stamping every heap chunk while confined: a VMA-own explicit policy exempts the range against **any** thread. Record in RESULTS.md: exemption = task-policy-lacks-`MPOL_F_MOF` for reached threads + VMA-own policy for the heap; gate 0/0 vmstat on confined arms confirms it empirically.
2. **Node choice at startup: the node of the booting CPU, via `getcpu`** (through the existing `numaGetCPUNode` wrapper, read by `numaCurrentNode`). One syscall, no sysfs meminfo parsing, and the kernel already first-touched the runtime's earliest pages on that node, so the boot node keeps init-time allocations local; most-free-memory could strand those pages remote and adds parsing. §12.2 requires only "one node-mask affinity choice at startup" — boot node is the boring choice. On arches without `getcpu` asm (`numaGetCPUNode` ok=false), confinement never engages (falls through to Layer 1).
3. **Stand-down trigger location: `startTheWorldWithSema`, immediately after `sched.lock` is released (after `procresize`), before `worldStarted()`.** This is the single funnel: `runtime.GOMAXPROCS`, `runtime.SetDefaultGOMAXPROCS`, and the `updatemaxprocs` cgroup goroutine all set `newprocs` and pass through this exact code (verified on this tree: `debug.go:102/141`, `proc.go:7166` → `proc.go:1786-1789`). **Honesty note:** at this hook point the world is restarting — `gcwaiting` is already cleared and `startTheWorldWithSema`'s own loop (`proc.go:1813`) can `newm`; clones CAN be in flight around the trigger. The trigger therefore only flips state and does best-effort eager restores; correctness is carried by per-thread convergence (decision 4). Stand-down is **one-way** (`numaStoodDown` latch): re-confinement after a later GOMAXPROCS decrease would require racefully re-narrowing arbitrary running threads and is not attempted.
4. **Stand-down semantics: correctness by per-thread convergence, eager walk as a latency optimization only.** `set_mempolicy` has no tid argument (per-calling-thread, `bind-all-policy.md`), and the allm walk cannot reach every thread anyway (`mcommoninit` publishes an M to `allm` **before** `newosproc` stores `procid` — the runtime's own walks spin on `atomic.Load64(&mp.procid)`, `os_linux.go:848-853`; plus clones in flight per decision 3). So: the trigger sets `numaStoodDown` (atomic), does an eager allm walk restoring affinity via `sched_setaffinity(tid)` for every M whose `procid` (atomic read) is nonzero — latency optimization — and reverts the calling thread's own policy. **Every M then converges itself in `numaFixThreadPlacement`, called from `stopm` (parked-M path — never malloc, never steal): it restores BOTH its affinity (self-call, tid 0, no tid needed) AND its task policy to BIND-all, and latches `m.numa` only after the syscalls succeed** (a failed syscall retries at the next park). Residual, stated honestly: an M that never parks after stand-down keeps the confined affinity and PREFERRED policy until it first parks — still balancer-exempt (explicit policy), still spill-capable; late-cloned threads inherit from their creator and likewise converge at first park. Documented, accepted.
5. **Interaction with Layer 1: Layer 1 runs first and stays running.** `numaSchedinit` (topology + `numaSetProcessBindAll`) is **untouched** at `proc.go:912`; confinement is a separate later call (`numaConfineIfSmall`) that narrows affinity and replaces only the **task** policy with PREFERRED. `numaAllowedNodemask` **stays published** and `numaBindArena`'s per-chunk BIND-all `mbind` **keeps running while confined**. Two reasons: (a) correctness — VMA-own policy is the only exemption that holds against threads the task policy never reached (decision 1); (b) the VMA-fragmentation fear was misdirected — RESULTS.md:1496-1515 shows the ~34× blowup came from **per-chunk different-node PREFERRED** policies that cannot merge; a **uniform** BIND-all policy on adjacent chunks merges fine (baseline-with-task-policy sat at ~34 maps lines). After Task 6 removes Layer 2, the confined heap gets exactly one uniform mbind per chunk. No "unstamped chunks" residual exists in this design.
6. **Confinement requires an explicitly chosen GOMAXPROCS (`sched.customGOMAXPROCS`).** Without it there is a real feedback loop: `defaultGOMAXPROCS` re-reads `sched_getaffinity` on every call and `sysmonUpdateGOMAXPROCS` runs ~1/sec moving `procs` in **both** directions unless `sched.customGOMAXPROCS` — a cgroup-limited default-GOMAXPROCS process (quota 4 on the 256-CPU box → confines) whose quota is later raised to 200 would get `min(affinity=128, quota=200) = 128` forever, and stand-down (strictly `procs > nodeCPUs`) never fires at `== 128`. Requiring an explicit GOMAXPROCS matches §12.2's framing (an operator/app that *chose* a small P count) and every benchmark arm in this plan (all set `GOMAXPROCS=` in the environment ⇒ `customGOMAXPROCS=true`). The rejected alternative — `getCPUCount` reading the saved pre-confinement mask while confined — is more invasive and still lies to in-process affinity readers. Document: while confined, anything reading the process affinity mask (in- or out-of-process) sees the narrowed mask.
7. **256P case on numa-dell (GOMAXPROCS=256 > 128/node): `numaConfineIfSmall` declines and the process runs exactly today's Layer-1 path — byte-identical trivially, since Layer 1 already ran unconditionally.** The gate requires strace-level proof (zero `sched_setaffinity` calls, exactly one `set_mempolicy(MPOL_BIND)`) plus 256P json unchanged. GOMAXPROCS **equal to** the node's CPU count (128 on numa-dell) **does** confine (condition is ≤) — candidate 1's GOMAXPROCS=128 arms exercise this deliberately.
8. **Layer 2 PREFERRED-at-grow is removed once Workstream A's HARD gates pass — its removal does not wait on A's decision gates** (the evidence for removal is independent of fill-one-socket's success: Layer 2's own IMC gate FAIL plus the three-arm sweep's inertness proof, C-full ≈ C-L1 primary p=0.838, direct comparison p=0.631). Revert the PREFERRED half of `numaBindArena` + `numaPreferredCalls` counter + `TestNUMAPreferredBindOnGrow`; keep `numaGetCPUNode` **and** `numaCurrentNode` — confinement's node choice uses them.

Design notes (not decisions, but verified reasoning to carry into review):

- **GOMAXPROCS/affinity interplay:** `numCPUStartup` is captured in `osinit` before confinement, so `runtime.NumCPU` and the startup default are unaffected. The dangerous interaction — sysmon's periodic `defaultGOMAXPROCS` recompute reading the narrowed mask — is neutralized by decision 6: confinement only engages under `sched.customGOMAXPROCS`, and custom GOMAXPROCS is never auto-updated.
- **Thread inheritance:** confinement runs in `schedinit` while m0 is the only runtime thread; every later M inherits both the affinity mask and the task policy via `clone`. cgo threads created *before* runtime init (c-archive/c-shared) keep wide affinity **and the implicit default mempolicy** — which is exactly why decision 5 keeps the arena mbind running (their scans would otherwise re-open policy-free heap VMAs to the balancer). Same class of gap as `bind-all-policy.md` inheritance notes; document, do not chase further.
- **Dynamic cpusets:** the affinity/allowed-node masks are read at startup and re-read only on stand-down (`numaSetProcessBindAll` re-queries `MPOL_F_MEMS_ALLOWED`). If the cpuset narrowed while confined, restoring the saved startup mask intersects with (or is rejected by) the current cpuset in the kernel; staleness is accepted and documented (Task 14).

---

## Global Constraints

Lessons paid for in v2 — do not soften any of these.

- **Statistics:** `benchstat` (Mann–Whitney U, α=0.05) is the **authoritative** comparator on `numa-dell` (timings are bimodal; medians at small n land on the wrong side by chance). n≥10 interleaved rounds per arm. **Never compare across sessions** — control-arm drift of +2.9% was observed between same-config sessions; every A/B claim comes from a **single-session sweep with rotating arm order** (ABC/BCA/CAB…).
- **One pre-declared primary metric per gate**; every other number is labeled exploratory and may motivate a new pre-registered sweep but never a claim. **No difference-of-significance inferences** ("X-vs-Z significant, Y-vs-Z not, therefore X≠Y" is the fallacy that produced the withdrawn candidate-1 conclusion — compare arms directly). Per-cycle samples within one process are **clustered** — report ICC and effective n (round-level analysis), never raw per-cycle n.
- **Archive raw `.out` files AND the sweep driver scripts under `numa-design/bench-data/<campaign>/` in the same commit as the results.** Commit the design/plan **before** execution so pre-registration is git-verifiable (the pathology design's after-the-fact commit is the counterexample).
- **Never attribute controller/dispatch guidance to the plan or brief in committed evidence records** — RESULTS.md entries describe what was measured and why in this plan's terms only.
- **1P json GOMAXPROCS=1 ≤ +2%** (ns/op AND user+sys-ns/op, benchstat) **and the 1P alloc micro** (`Malloc(8|16|Types)`, `-count=10`) at **every** gate. **Off-binary function census** (objdump function-level diff, experiment off, HEAD vs parent) once per workstream.
- **Experiment off, or single-node machine, or pre-existing narrowed affinity: behavior identical to stock Go.** All new call sites in shared runtime code are wrapped in `if goexperiment.Numa { ... }` so the off binary is bit-identical modulo build IDs.
- **Never single-node `MPOL_BIND` for the whole process** (OOM footgun, `bind-all-policy.md`). **`maxnode = 65` fixed-width nodemask convention everywhere** (`numaMaxNode`; see `numa_linux.go`'s doc comment for the 32-bit out-of-bounds-write rationale — never derive from `numaNodemaskBits` or `numa.MaxNodes`).
- **Machine hygiene:** never leave `kernel.numa_balancing` changed (verify `=1` at session end); heap targets respect ~15 GiB/node — arm A and confined arm C must fit one node (candidate 1 stays at `-benchmem=4096`, node-free ≥10 GB checked before each single-node run); `GOTOOLCHAIN=local` always; **no `GODEBUG=numa=1` in measured runs**; idle checks (`ps aux --sort=-%cpu`, not `uptime`) before every measurement block.
- **Layer 2 PREFERRED-at-grow remains in-tree at plan start but is superseded:** Workstream A and B code paths must not depend on it, and Task 6 removes it once Workstream A's **hard** gates pass (removal evidence is independent of the decision gates — see locked decision 8).
- **If a gate fails: stop, record the failure in `numa-design/RESULTS.md`, do not start the next workstream.**
- Forbidden-list carryover from v2 (see Forbidden section): no mcache flush, no per-malloc scans, no STW policy toggles, no P-index steal.
- **Never** add `Co-authored-by` trailers beyond the repo convention. **Never** `--no-verify`. Every commit compiles and passes the tests it touches.

---

## File map (what this plan creates or modifies)

| Path | WS | Role |
|------|----|------|
| `numa-design/pathology-sweep.sh` | 0 | Reusable single-session A/B/C sweep driver (rotating order, vmstat snaps, warmup round) |
| `src/internal/runtime/syscall/linux/defs_linux_amd64.go` | A | `SYS_SCHED_SETAFFINITY = 203` |
| `src/internal/runtime/syscall/linux/defs_linux_arm64.go` | A | `SYS_SCHED_SETAFFINITY = 122` |
| `src/runtime/numa_linux_affinity.go` | A | `numaSetThreadAffinity` (`linux && (amd64 \|\| arm64)`) |
| `src/runtime/numa_linux_affinity_other.go` | A | stub (`linux && !(amd64 \|\| arm64)`) — confinement inert there |
| `src/runtime/numa_linux.go` | A | confinement state + decision + confine + stand-down + convergence (`numaConfineIfSmall`, `numaStandDownIfNeeded`, `numaFixThreadPlacement`); `numaSchedinit` untouched; later Layer-2 removal |
| `src/runtime/proc.go` | A | `schedinit`: `numaConfineIfSmall(procs)` between `:953` (unlock) and `:956` (worldStarted); `startTheWorldWithSema`: stand-down trigger; `stopm`: placement-convergence check |
| `src/runtime/runtime2.go` | A | `numa mNUMAState` field in `m` (embedded, zero-size when experiment off — see C2 note in Task 3) |
| `src/runtime/numa_mstate_on.go` / `numa_mstate_off.go` | A | `mNUMAState` + accessors, `//go:build goexperiment.numa` / `!goexperiment.numa` |
| `src/runtime/stubs_nonlinux.go` | A | `numaConfineIfSmall` / `numaStandDownIfNeeded` / `numaFixThreadPlacement` no-ops |
| `src/runtime/export_numa_test.go` | A | `NumaConfinedForTest`, … — tag gains `&& goexperiment.numa` so off test binaries keep DCE |
| `src/runtime/testdata/testprog/numa.go` | A | subprocess placement probes (affinity popcount + mempolicy mode) |
| `src/runtime/numa_linux_test.go` | A | confinement / stand-down tests |
| `src/runtime/mheap.go` | B | `heapArena.node`; per-node arena hint chains / `curArena`; `grow(npage, node)` |
| `src/runtime/mcentral.go`, `mcache.go` | B | per-node spanSets; getcpu-at-refill routing |
| `src/runtime/metrics.go` (+ doc) | B | `/numa/span-refills:local`, `/numa/span-refills:remote` |
| `src/runtime/sys_linux_{386,arm,loong64,mipsx,mips64x,ppc64x,riscv64,s390x}.s` | C | `getcpu` wrappers |
| `src/runtime/numa_linux_getcpu.go` / `_other.go` | C | widen build tags as arches gain asm |
| `numa-design/phase-shift/main.go` | C | per-phase `/proc/self/numa_maps` capture |
| `numa-design/RESULTS.md` | all | one section per gate, SHA + commands + verdicts |

**Do not create:** anything on the v2 forbidden list; no `p.numaNode`-by-index; no flush.

---

## Environment: NUMA test machine

Unchanged from the v2 plan (see `2026-08-19-numa-v2-implementation-plan.md` "Environment"). Summary:

- SSH: `local → fedorawork (VPN) → numa-dell`; remote tree `/home/deparker/go-numa`; `Makefile` targets `push` / `build` / `test-numa` / `topo` / `gate-json-1p` already exist on this branch.
- Hardware (verify with `make topo`): 256 CPUs, 2 nodes, **even CPUs = node 0, odd = node 1**, ~15 GiB DRAM/node, `kernel.numa_balancing=1`, THP=always, kernel 6.12.
- Local: build/test with the tree toolchain from `src/` (`cd src && go test <pkg>`), `GOWORK=off` for any module-mode operation against the tree; `GOTOOLCHAIN=local` everywhere.
- Layer-0-style unit tests run on any Linux; **json/vmstat/IMC/pathology gates only on `numa-dell`**. If VPN is down, stop; never fake gates locally.
- x/benchmarks pin: record the resolved version (`go version -m`) in every RESULTS.md section; currently `v0.0.0-20260819172200-70693762b6a0`.
- benchstat: `/tmp/numa-tools/benchstat` on the remote.

---

## Forbidden (carried verbatim in force from v2)

- **Never** call a per-span-class mcache scan from `getMCache` or `acquirep`. `numaFlushForeignCachedSpans` and anything shaped like it is forbidden. No per-malloc scans of any kind.
- **Never** revive STW-only `set_mempolicy` toggles (wrong window, wrong scope — `bind-all-policy.md`).
- **Never** steal or route by logical `p.numaNode` P-index (even/odd CPU interleave makes P index meaningless on this machine). Node identity comes from `getcpu` (or a pinned mask), physically.
- **Experiment off = stock:** no unconditional code changes on any hot path; all call sites `goexperiment.Numa`-guarded; off-binary objdump census must show only build-ID diffs.
- No `getcpu` in `getMCache`/`mallocgc` fast paths — refill/grow/scheduler-pass frequency only. No syscalls under `sched.lock` or with `mp.locks != 0` (Task 10's hook placement rule).
- Do not enable Sweet perf diagnostics; do not compare Sweet results across invocations; Bleve N=1 / etcd p99 / one-shot STW p99 are never gates.
- Do not port anything from `numa-dev` v1 (flush, `numaNodeHeap`, `numaStealWork`, `numaAssignPNodes`, `mspan.numaNode`).

---

### Task 0: Harness prep — reusable pathology sweep driver + pre-registration commit

**Files:**
- Create: `numa-design/pathology-sweep.sh`
- Verify (no edits expected): `numa-design/gate-json.sh`, `numa-design/gate-vmstat.sh`, `numa-design/gc-pause-bench/`, `numa-design/phase-shift/`
- Commit: this plan file (pre-registration — the plan must be in git history **before** any Workstream A gate runs)

**Interfaces:**
- Consumes: `gate-vmstat.sh snap|diff`, `ps aux` idle protocol
- Produces: one script that runs any N-round, K-arm, rotating-order, vmstat-instrumented sweep so gates are never hand-rolled again (every prior sweep was a bespoke `cand*-sweep.sh`; the protocol is now fixed, so fix the tooling too)

- [ ] **Step 1: Verify the existing harness still runs**

```bash
ssh numa-dell 'cd /home/deparker/go-numa && ls numa-design/gate-vmstat.sh numa-design/gate-json.sh && bash -n numa-design/gate-vmstat.sh && bash -n numa-design/gate-json.sh && echo HARNESS-OK'
ssh numa-dell 'sysctl kernel.numa_balancing; cat /sys/kernel/mm/transparent_hugepage/enabled'
```

Expected: `HARNESS-OK`, `kernel.numa_balancing = 1`, THP `[always]`. If the scripts are missing on the remote, `make push` first.

- [ ] **Step 2: Write `numa-design/pathology-sweep.sh`**

```bash
#!/usr/bin/env bash
# Reusable single-session pathology sweep driver.
#
# Protocol (fixed; see pathology-bench-design.md): one unrecorded warmup
# round, then N recorded rounds; arm order rotates each round; vmstat
# (numa_hint_faults, numa_pages_migrated) snapped immediately before and
# after every individual run; idle check before every run. Raw outputs
# land in OUTDIR and MUST be archived under
# numa-design/bench-data/<campaign>/ in the same commit as the results.
#
# Usage:
#   pathology-sweep.sh CAMPAIGN N OUTDIR "ARM=cmd" "ARM=cmd" [...]
# Example (Workstream A candidate 1):
#   pathology-sweep.sh wsA-cand1 15 /tmp/pb/wsA-cand1 \
#     "A=numactl --cpunodebind=0 --membind=0 env GOMAXPROCS=128 /tmp/pb/base/garbage -benchmem=4096 -benchnum=1" \
#     "B=env GOMAXPROCS=128 /tmp/pb/base/garbage -benchmem=4096 -benchnum=1" \
#     "C=env GOMAXPROCS=128 /tmp/pb/numa/garbage -benchmem=4096 -benchnum=1"
set -euo pipefail
CAMPAIGN="$1"; N="$2"; OUT="$3"; shift 3
HERE="$(cd "$(dirname "$0")" && pwd)"
VMSTAT="$HERE/gate-vmstat.sh"
mkdir -p "$OUT"
declare -a NAMES CMDS
for spec in "$@"; do
    NAMES+=("${spec%%=*}")
    CMDS+=("${spec#*=}")
done
NARMS=${#NAMES[@]}

idle_check() {
    # ps, not uptime: load average decays for hours after 256P runs.
    local top
    top=$(ps aux --sort=-%cpu | awk 'NR==2 {print int($3)}')
    while [ "${top:-0}" -gt 50 ]; do
        echo "idle_check: top process at ${top}% CPU; sleeping 30s" >&2
        sleep 30
        top=$(ps aux --sort=-%cpu | awk 'NR==2 {print int($3)}')
    done
}

run_one() { # arm-index round recorded?
    local i="$1" round="$2" rec="$3"
    local name="${NAMES[$i]}"
    local kind=recorded; [ "$rec" = 0 ] && kind=warmup
    local tag="${CAMPAIGN}-arm${name}-r${round}"
    idle_check
    "$VMSTAT" snap "$OUT/$tag.vmstat.before"
    eval "${CMDS[$i]}" \
        >>"$OUT/${CAMPAIGN}-arm${name}-${kind}.out" \
        2>>"$OUT/${CAMPAIGN}-arm${name}-${kind}.out.stderr"
    "$VMSTAT" snap "$OUT/$tag.vmstat.after"
}

for round in $(seq 0 "$N"); do
    rec=1; [ "$round" -eq 0 ] && rec=0
    for k in $(seq 0 $((NARMS - 1))); do
        run_one $(( (k + round) % NARMS )) "$round" "$rec"
    done
done

: >"$OUT/${CAMPAIGN}-vmstat-summary.txt"
for f in "$OUT"/*.vmstat.before; do
    a="${f%.before}.after"
    d=$("$VMSTAT" diff "$f" "$a" 2>/dev/null || "$VMSTAT" diff "$f" "$a" || true)
    echo "$(basename "${f%.vmstat.before}"): $d" >>"$OUT/${CAMPAIGN}-vmstat-summary.txt"
done
echo "sweep complete: $OUT (archive under numa-design/bench-data/${CAMPAIGN}/)"
```

Note: `gate-vmstat.sh diff` exits 1 on nonzero deltas by design; the summary loop tolerates that (nonzero deltas are *expected* on arm B).

- [ ] **Step 3: Syntax-check and smoke-test the driver** (locally, with `true` arms)

```bash
bash -n numa-design/pathology-sweep.sh
bash numa-design/pathology-sweep.sh smoke 1 /tmp/sweep-smoke "X=true" "Y=true" && ls /tmp/sweep-smoke
```

Expected: warmup + 1 recorded round per arm, vmstat snaps present, summary file written.

- [ ] **Step 4: Commit plan + driver (pre-registration)**

```bash
chmod +x numa-design/pathology-sweep.sh
git add numa-design/2026-08-20-numa-v3-locality-plan.md numa-design/pathology-sweep.sh
git commit -m "$(cat <<'EOF'
numa-design: add v3 locality plan and reusable pathology sweep driver

Pre-registers the Workstream A/B/C gates before any measurement runs,
and promotes the per-campaign sweep scripts into one fixed driver so
gate protocol (rotating order, vmstat snaps, warmup, idle checks) is
never hand-rolled again.
EOF
)"
```

---

## Workstream A: fill-one-socket-first (design §12.2)

### Task 1: `sched_setaffinity` plumbing

**Files:**
- Modify: `src/internal/runtime/syscall/linux/defs_linux_amd64.go` — add `SYS_SCHED_SETAFFINITY = 203`
- Modify: `src/internal/runtime/syscall/linux/defs_linux_arm64.go` — add `SYS_SCHED_SETAFFINITY = 122`
- Create: `src/runtime/numa_linux_affinity.go` (`//go:build linux && (amd64 || arm64)`)
- Create: `src/runtime/numa_linux_affinity_other.go` (`//go:build linux && !(amd64 || arm64)`)
- Modify: `src/runtime/export_numa_test.go`, `src/runtime/numa_linux_test.go`

Only amd64/arm64 get the constant: confinement requires `numaGetCPUNode`, which only exists there (same link-safety split Layer 2 established — see the `numa_linux_getcpu.go` doc comment). Other Linux arches get an always-false stub so shared code links everywhere.

**Build-tag rule for test-only exports (C2, applies to this and every later task):** first verify the actual tags in the tree, then align `export_numa_test.go` and `numa_linux_test.go` on `linux && (amd64 || arm64) && goexperiment.numa` for everything confinement-related. The critical half is **`&& goexperiment.numa`**: the export file is a real `package runtime` file in test builds and is **not** experiment-gated today, so un-gated new exports would root the confinement code in the *experiment-off* test binary and defeat dead-code elimination — which is exactly what Gate 8's census would then flag. Test files that reference these exports must carry matching tags or they fail to build on stub arches.

**Interfaces:**
- Produces:

```go
// numaCPUMaskBytes: 8192 CPUs / 8, matching numa.Topology's CPUToNode capacity.
const numaCPUMaskBytes = 8192 / 8

// numaSetThreadAffinity sets the CPU affinity mask of thread tid
// (0 = calling thread). Reports success. Not nosplit: every caller
// (schedinit, the STW stand-down walk, stopm convergence) runs on a
// normal stack with growth available.
func numaSetThreadAffinity(tid int32, mask *[numaCPUMaskBytes]byte) bool
```

- Consumes: `linux.Syscall6`, existing `sched_getaffinity` asm (all Linux arches already have it — `os_linux.go:455`).

- [ ] **Step 1: Write the failing test**

In `src/runtime/numa_linux_test.go` — this test references an export that only builds on real-implementation arches, so it lives under the aligned tag (`linux && (amd64 || arm64) && goexperiment.numa`; split the file if some existing tests must keep a wider tag). No stub branch: on arches where `numaHasSetAffinity` is false the file simply does not build, so a "stub reported success" branch would be unreachable.

```go
func TestNUMASetThreadAffinitySelf(t *testing.T) {
	// Read the current mask and set it back unchanged: must succeed.
	if !runtime.NumaSetThreadAffinitySelfForTest() {
		t.Fatal("sched_setaffinity(self, current mask) failed")
	}
}
```

And in `src/runtime/export_numa_test.go` (tag per the rule above — MUST include `&& goexperiment.numa`):

```go
// NumaSetThreadAffinitySelfForTest re-applies the calling thread's own
// affinity mask.
func NumaSetThreadAffinitySelfForTest() bool {
	var buf [numaCPUMaskBytes]byte
	r := sched_getaffinity(0, uintptr(len(buf)), &buf[0])
	if r <= 0 {
		return false
	}
	return numaSetThreadAffinity(0, &buf)
}
```

- [ ] **Step 2: Run (expect FAIL: undefined `numaSetThreadAffinity` / `numaHasSetAffinity`)**

```bash
cd src && GOEXPERIMENT=numa go test runtime -run TestNUMASetThreadAffinitySelf -count=1
```

- [ ] **Step 3: Implement**

`numa_linux_affinity.go`:

```go
// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package runtime

import (
	"internal/runtime/syscall/linux"
	"unsafe"
)

// numaHasSetAffinity reports whether this platform implements
// numaSetThreadAffinity. Confinement (fill-one-socket-first) engages
// only where it is true; elsewhere numaShouldConfine already fails via
// numaGetCPUNode, and this constant keeps the stand-down path honest.
const numaHasSetAffinity = true

// numaSetThreadAffinity sets the CPU affinity mask of thread tid
// (0 = the calling thread) to *mask. It reports whether the kernel
// accepted the mask. Errors are not distinguished: a false return
// simply means confinement (or a stand-down restore for one thread)
// did not take effect, which callers treat as stand-down.
func numaSetThreadAffinity(tid int32, mask *[numaCPUMaskBytes]byte) bool {
	_, _, errno := linux.Syscall6(linux.SYS_SCHED_SETAFFINITY,
		uintptr(tid), numaCPUMaskBytes, uintptr(unsafe.Pointer(&mask[0])), 0, 0, 0)
	return errno == 0
}
```

`numa_linux_affinity_other.go`:

```go
//go:build linux && !(amd64 || arm64)

package runtime

const numaHasSetAffinity = false

func numaSetThreadAffinity(tid int32, mask *[numaCPUMaskBytes]byte) bool {
	return false
}
```

Put `const numaCPUMaskBytes = 8192 / 8` in `numa_linux.go` (shared).

- [ ] **Step 4: Tests pass locally, then on numa-dell**

```bash
cd src && GOEXPERIMENT=numa go test runtime -run TestNUMASetThreadAffinitySelf -count=1
make push && make build && make test-numa RUN=TestNUMASetThreadAffinitySelf
```

- [ ] **Step 5: Link sweep** (the Layer-2 lesson — reachability, not just compilation)

```bash
cd src && for arch in amd64 arm64 riscv64 arm 386 ppc64le s390x loong64; do
  for exp in "" numa; do
    GOOS=linux GOARCH=$arch GOEXPERIMENT=$exp go build -o /dev/null ../test/helloworld.go || echo "FAIL $arch $exp"
  done
done
```

(Use any trivial `package main`; all combinations must link.)

- [ ] **Step 6: Commit**

```bash
git add src/internal/runtime/syscall/linux src/runtime
git commit -m "$(cat <<'EOF'
runtime: add sched_setaffinity plumbing for NUMA confinement

amd64/arm64 only, matching the getcpu availability split; other Linux
architectures get an always-false stub so fill-one-socket-first is
inert there.
EOF
)"
```

---

### Task 2: Confinement decision and `numaConfineIfSmall` (red)

**Files:**
- Modify: `src/runtime/numa_linux.go` — state, `numaShouldConfine`, `numaConfine`, `numaConfineIfSmall`, `numaNodeCPUCount`. **`numaSchedinit` is NOT touched** — Layer 1 (topology + `numaSetProcessBindAll`) keeps running unconditionally at `proc.go:912`, byte-identical fallback (I8/C3).
- Modify: `src/runtime/stubs_nonlinux.go` — `numaConfineIfSmall` no-op
- Modify: `src/runtime/proc.go` `schedinit` — one new guarded call between `unlock(&sched.lock)` (`proc.go:953`) and `worldStarted()` (`proc.go:956`)
- Create: `src/runtime/testdata/testprog/numa.go`
- Modify: `src/runtime/export_numa_test.go`, `src/runtime/numa_linux_test.go` (build-tag rule from Task 1 applies)

**Interfaces:**

```go
// All in numa_linux.go (compiled for every GOOS=linux arch):
func numaConfineIfSmall(procs int32)                // confinement decision; from schedinit, AFTER Layer 1 ran
func numaShouldConfine(procs int32) (node int32, ok bool)
func numaConfine(node int32) bool
func numaNodeCPUCount(node int32) int32             // the ONLY source for a node's CPU count
```

State (all in `numa_linux.go`; `numaConfined`/`numaStoodDown` are atomic — read from `stopm` by every parking M (I4); the rest written only during single-threaded `schedinit` or under the stand-down trigger):

```go
var (
	// numaConfined is true while fill-one-socket-first confinement is
	// active. Set once in numaConfineIfSmall (m0 is the only runtime
	// thread); cleared only by numaStandDownIfNeeded.
	numaConfined atomic.Bool
	// numaStoodDown latches the one-way stand-down: once true, the
	// process never re-confines, and every M converges its own
	// affinity + task mempolicy at its next park
	// (numaFixThreadPlacement, Task 3).
	numaStoodDown atomic.Bool

	numaConfinedNode     int32
	numaConfinedNodeCPUs int32

	// numaSavedAffinity is the process's startup affinity mask, saved
	// by numaShouldConfine before any narrowing, and restored per
	// thread at/after stand-down.
	numaSavedAffinity    [numaCPUMaskBytes]byte
	numaSavedAffinityLen int32
)
```

**Semantics to implement (locked decisions 1–2, 5–6):**

```go
// numaConfineIfSmall applies fill-one-socket-first confinement (design
// §12.2) when the decision conditions hold. Called from schedinit after
// procresize, before any other runtime thread exists. Layer 1
// (numaSetProcessBindAll) has ALREADY run by this point and stays in
// force: confinement only narrows CPU affinity and replaces the task
// mempolicy; numaAllowedNodemask remains published and numaBindArena
// keeps stamping heap chunks with uniform BIND-all (locked decision 5).
// Experiment off: caller never invokes it (goexperiment.Numa-guarded
// call site).
func numaConfineIfSmall(procs int32) {
	if node, ok := numaShouldConfine(procs); ok && numaConfine(node) {
		if debug.numa > 0 {
			println("numa: confined to node", node, "cpus", numaConfinedNodeCPUs)
		}
	}
}

// numaShouldConfine reports whether fill-one-socket-first should engage
// (design §12.2): multi-node machine, representable topology, Layer 1
// actually engaged (published nodemask), an EXPLICITLY chosen GOMAXPROCS
// (locked decision 6 — without sched.customGOMAXPROCS, sysmon's ~1/sec
// defaultGOMAXPROCS recompute reads the narrowed mask and freezes procs
// at the node size, a real feedback loop), no pre-existing narrowed CPU
// affinity (operator placement always wins), a usable getcpu, and
// procs <= the boot node's CPU count.
// As a side effect it saves the startup affinity mask for stand-down.
// Every declined reason prints under GODEBUG=numa=1 (diagnosability).
func numaShouldConfine(procs int32) (int32, bool) {
	if numaTopology.NumNodes < 2 || numaTopology.TruncatedNodes || !numaHasSetAffinity {
		return 0, false
	}
	if numaAllowedNodemask.Load() == 0 {
		// Layer 1 declined or failed; do not build confinement on top.
		numaConfineDeclined("layer1 inactive")
		return 0, false
	}
	if !sched.customGOMAXPROCS {
		// Default GOMAXPROCS auto-updates from the affinity mask we are
		// about to narrow (locked decision 6): only confine a process
		// whose P count was chosen explicitly.
		numaConfineDeclined("GOMAXPROCS not explicitly set")
		return 0, false
	}
	r := sched_getaffinity(0, uintptr(numaCPUMaskBytes), &numaSavedAffinity[0])
	if r <= 0 {
		numaConfineDeclined("sched_getaffinity failed")
		return 0, false
	}
	numaSavedAffinityLen = int32(r)
	online := int32(0)
	for i := int32(0); i < numaTopology.NumNodes; i++ {
		online += numaTopology.Nodes[i].NumCPUs
	}
	pop := int32(0)
	for _, b := range numaSavedAffinity[:r] {
		for b != 0 {
			b &= b - 1
			pop++
		}
	}
	if pop != online {
		// taskset / narrowed cpuset: operator placement wins. NOTE:
		// offline CPUs can also make sysfs-online and the affinity
		// popcount disagree; the check then declines — conservative
		// (offline-CPU hosts simply do not confine). Record if seen.
		numaConfineDeclined("affinity narrower than online CPUs")
		return 0, false
	}
	node := numaCurrentNode() // boot CPU's node (locked decision 2)
	if node < 0 || node >= 64 {
		numaConfineDeclined("getcpu failed")
		return 0, false
	}
	ncpus := numaNodeCPUCount(node)
	if ncpus <= 0 || procs > ncpus {
		numaConfineDeclined("GOMAXPROCS exceeds node")
		return 0, false
	}
	return node, true
}

// numaConfineDeclined prints the decline reason under GODEBUG=numa=1.
func numaConfineDeclined(reason string) {
	if debug.numa > 0 {
		println("numa: confinement declined:", reason)
	}
}

// numaNodeCPUCount returns node's CPU count from the topology. It is
// the single source for both the <= decision in numaShouldConfine and
// the numaConfinedNodeCPUs threshold the stand-down trigger compares
// against.
func numaNodeCPUCount(node int32) int32 {
	for i := int32(0); i < numaTopology.NumNodes; i++ {
		if numaTopology.Nodes[i].ID == node {
			return numaTopology.Nodes[i].NumCPUs
		}
	}
	return 0
}

// numaConfine confines the process to node: CPU affinity to that node's
// CPUs plus a task MPOL_PREFERRED policy for its memory (locked
// decision 1: PREFERRED, never single-node BIND — the OOM footgun).
// The PREFERRED set_mempolicy REPLACES the Layer-1 BIND-all task policy
// for this thread and, by clone inheritance, every later M. Runs while
// m0 is the only runtime thread.
//
// Heap-VMA exemption does not depend on this task policy: numaBindArena
// keeps stamping every chunk with uniform BIND-all (numaAllowedNodemask
// stays published) — the VMA-own policy is what holds against threads
// the task policy never reached (pre-runtime cgo threads; locked
// decision 1), and uniform policies VMA-merge, so there is no Layer-2-
// style map blowup.
func numaConfine(node int32) bool {
	var cpumask [numaCPUMaskBytes]byte
	n := 0
	for cpu := 0; cpu < 8192; cpu++ {
		if numaTopology.NodeOfCPU(cpu) == node {
			cpumask[cpu/8] |= 1 << (uint(cpu) % 8)
			n++
		}
	}
	if n == 0 {
		return false
	}
	if !numaSetThreadAffinity(0, &cpumask) {
		return false
	}
	var pmask numaNodemask
	pmask[uintptr(node)/numaNodemaskBits] = 1 << (uintptr(node) % numaNodemaskBits)
	if _, _, errno := linux.Syscall6(linux.SYS_SET_MEMPOLICY,
		uintptr(_MPOL_PREFERRED), uintptr(unsafe.Pointer(&pmask[0])), numaMaxNode, 0, 0, 0); errno != 0 {
		// Policy failed: undo the affinity narrowing and stay on the
		// Layer-1 BIND-all policy that is already in force.
		numaSetThreadAffinity(0, &numaSavedAffinity)
		return false
	}
	numaConfined.Store(true)
	numaConfinedNode = node
	numaConfinedNodeCPUs = numaNodeCPUCount(node) // same source as the decision
	return true
}
```

`schedinit` edit (proc.go — insertion point is **between `unlock(&sched.lock)` at `:953` and `worldStarted()` at `:956`**; `numaSchedinit()` at `:912` is untouched):

```go
	if procresize(procs) != nil {
		throw("unknown runnable goroutine during bootstrap")
	}
	unlock(&sched.lock)

	if goexperiment.Numa {
		// Fill-one-socket-first needs the startup GOMAXPROCS value; no
		// other runtime thread exists yet, so affinity/mempolicy set
		// here is inherited by every future M. Layer 1 BIND-all already
		// ran in numaSchedinit above and stays the fallback.
		numaConfineIfSmall(procs)
	}
```

(`stubs_nonlinux.go` gains an empty `numaConfineIfSmall(procs int32)`.) Reading `sched.customGOMAXPROCS` without the lock here is safe: `schedinit` is still single-threaded at this point and the field was set a few lines above.

- [ ] **Step 1: Write the failing tests (testprog probe + tests)**

`src/runtime/testdata/testprog/numa.go`:

```go
// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package main

import (
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

func init() {
	register("NUMAPlacementInfo", NUMAPlacementInfo)
	register("NUMAStandDown", NUMAStandDown)
}

// getMempolicySyscall: get_mempolicy(2) numbers differ per arch.
var getMempolicySyscall = map[string]uintptr{
	"amd64": 239,
	"arm64": 236,
}[runtime.GOARCH]

const mpolModeFlags = 0xe000 // MPOL_F_* flag bits get_mempolicy may OR into mode

func placementLine(label string) {
	var buf [1024]byte
	n, _, errno := syscall.RawSyscall(syscall.SYS_SCHED_GETAFFINITY,
		0, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		fmt.Printf("%s ERR getaffinity errno=%d\n", label, errno)
		return
	}
	pop := 0
	for _, b := range buf[:n] {
		for b != 0 {
			b &= b - 1
			pop++
		}
	}
	var mode int32
	if getMempolicySyscall == 0 {
		fmt.Printf("%s SKIP no get_mempolicy number for %s\n", label, runtime.GOARCH)
		return
	}
	_, _, errno = syscall.RawSyscall6(getMempolicySyscall,
		uintptr(unsafe.Pointer(&mode)), 0, 0, 0, 0, 0)
	if errno != 0 {
		fmt.Printf("%s ERR get_mempolicy errno=%d\n", label, errno)
		return
	}
	fmt.Printf("%s affinity=%d mode=%d\n", label, pop, mode&^mpolModeFlags)
}

// NUMAPlacementInfo prints one line: "info affinity=<popcount> mode=<mempolicy mode>".
// Run with GOMAXPROCS set by the test to steer the confinement decision.
func NUMAPlacementInfo() {
	placementLine("info")
}

// NUMAStandDown confines at startup (small GOMAXPROCS via env), then
// raises GOMAXPROCS past one node and prints before/after placement.
// LockOSThread keeps the observing goroutine on the M that executes the
// GOMAXPROCS stop-the-world, so "after" deterministically reflects the
// stand-down thread's restored policy. The timed GOMAXPROCS call bounds
// the stand-down STW plus the allm affinity walk; the gate records this
// (and compares against a stock-build run of the same probe) so the
// walk's stop-the-world cost is on the record.
func NUMAStandDown() {
	runtime.LockOSThread()
	placementLine("before")
	start := time.Now()
	runtime.GOMAXPROCS(runtime.NumCPU())
	fmt.Printf("standdown-gomaxprocs-wall-ns=%d\n", time.Since(start).Nanoseconds())
	placementLine("after")
}
```

`src/runtime/numa_linux_test.go` additions (`//go:build linux && goexperiment.numa`):

```go
// mempolicy modes (numa_linux.go): 1 = MPOL_PREFERRED, 2 = MPOL_BIND.

func TestNUMAFillOneSocketConfined(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip("not multi-node")
	}
	if !runtime.NumaHasSetAffinityForTest() {
		t.Skip("no sched_setaffinity plumbing on this arch")
	}
	// GOMAXPROCS=1 <= every node's CPU count: the subprocess must confine.
	// NOTE: testprog is built by buildTestProg with the inherited
	// environment; run via `make test-numa` so GOEXPERIMENT=numa applies
	// to the subprocess build too.
	got := runTestProg(t, "testprog", "NUMAPlacementInfo", "GOMAXPROCS=1")
	aff, mode := parsePlacement(t, got, "info")
	if mode != 1 {
		t.Fatalf("confined process mode=%d want MPOL_PREFERRED(1); output %q", mode, got)
	}
	if !runtime.NumaIsNodeCPUCountForTest(aff) {
		t.Fatalf("confined affinity popcount %d matches no node's CPU count; output %q", aff, got)
	}
}

func TestNUMAConfineSkipsNarrowedAffinity(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip("not multi-node")
	}
	testenv.MustHaveExecPath(t, "taskset")
	// Operator placement wins: under taskset, confinement never engages
	// and the narrowed 2-CPU mask is left untouched. Layer-1 BIND-all is
	// affinity-independent and still applies, so the task policy is
	// MPOL_BIND (mode=2).
	exe, err := buildTestProg(t, "testprog")
	if err != nil {
		t.Fatal(err)
	}
	cmd := testenv.Command(t, "taskset", "-c", "0,1", exe, "NUMAPlacementInfo")
	cmd.Env = append(os.Environ(), "GOMAXPROCS=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	aff, mode := parsePlacement(t, string(out), "info")
	if aff != 2 {
		t.Fatalf("affinity=%d, want the operator's 2 CPUs untouched", aff)
	}
	if mode != 2 {
		t.Fatalf("mode=%d want MPOL_BIND(2) (Layer 1 BIND-all)", mode)
	}
}

func parsePlacement(t *testing.T, out, label string) (aff int, mode int) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, label+" ") {
			if strings.Contains(line, "ERR") || strings.Contains(line, "SKIP") {
				t.Fatalf("probe failed: %q", line)
			}
			if _, err := fmt.Sscanf(line, label+" affinity=%d mode=%d", &aff, &mode); err != nil {
				t.Fatalf("bad probe line %q: %v", line, err)
			}
			return aff, mode
		}
	}
	t.Fatalf("no %q line in output %q", label, out)
	return 0, 0
}
```

`export_numa_test.go` additions:

```go
func NumaHasSetAffinityForTest() bool { return numaHasSetAffinity }

// NumaIsNodeCPUCountForTest reports whether n equals some node's CPU count.
func NumaIsNodeCPUCountForTest(n int) bool {
	for i := int32(0); i < numaTopology.NumNodes; i++ {
		if int(numaTopology.Nodes[i].NumCPUs) == n {
			return true
		}
	}
	return false
}

func NumaConfinedForTest() bool { return numaConfined.Load() }
```

Also add a `numaConfined` guard to the existing `TestNUMABindAllTaskPolicy` (skip when confined — it asserts BIND, which is correct only on the unconfined path; a plain `go test` run has no GOMAXPROCS env, so `sched.customGOMAXPROCS` is false and confinement never engages — locked decision 6 — but do not depend on that):

```go
	if runtime.NumaConfinedForTest() {
		t.Skip("process is socket-confined; BIND-all not in effect by design")
	}
```

- [ ] **Step 2: Run tests remotely — expect FAIL (undefined symbols, then wrong placement until Task 2 Step 3 lands)**

```bash
make push && make build && make test-numa RUN='TestNUMAFillOneSocket|TestNUMAConfineSkips'
```

- [ ] **Step 3: Commit red**

```bash
git add src/runtime
git commit -m "$(cat <<'EOF'
runtime: add TestNUMAFillOneSocketConfined (red)

Fill-one-socket-first (design 12.2): subprocess placement probes assert
node-sized affinity + MPOL_PREFERRED when GOMAXPROCS fits one node, and
operator-narrowed affinity is left untouched.
EOF
)"
```

- [ ] **Step 4: Implement** the state, `numaShouldConfine`, `numaConfine`, `numaConfineIfSmall`, `numaNodeCPUCount`, the `schedinit` insertion (between `:953` and `:956`; `numaSchedinit` untouched), and the `stubs_nonlinux.go` stub — exactly as specified in the Interfaces/Semantics block above.

- [ ] **Step 5: `go vet` + full local build + remote green**

```bash
cd src && go vet runtime && ./make.bash
make push && make build
make test-numa RUN='TestNUMAFillOneSocket|TestNUMAConfineSkips|TestNUMABindAllTaskPolicy|TestNUMATopologyDiscovery|TestNUMAGetcpu'
```

Expected: all PASS (BindAll test unconfined at default GOMAXPROCS=256 asserts mode=2 as before).

- [ ] **Step 6: Commit green**

```bash
git add src/runtime
git commit -m "$(cat <<'EOF'
runtime: confine small-GOMAXPROCS processes to one NUMA node

Fill-one-socket-first (design 12.2): when GOEXPERIMENT=numa is on, the
machine is multi-node, GOMAXPROCS was explicitly set (a default
GOMAXPROCS would be auto-recomputed from the mask we narrow), the
process has no operator-narrowed affinity, and startup GOMAXPROCS fits
one node, narrow to the boot CPU's node with sched_setaffinity and
replace the task policy with MPOL_PREFERRED. PREFERRED spills under
pressure (never the single-node-BIND OOM footgun) and, being an
explicit task policy, still suppresses the NUMA balancer (no
MPOL_F_MOF). Layer 1 is untouched and keeps running: numaSchedinit's
BIND-all and numaBindArena's uniform per-chunk mbind stay in force
while confined -- the VMA-own policy is what exempts heap ranges
against threads the task policy never reached, and uniform policies
VMA-merge, so there is no Layer-2-style map blowup. When confinement
declines, behavior is byte-identical to Layer 1 alone.
EOF
)"
```

---

### Task 3: Stand-down on GOMAXPROCS growth

**Files:**
- Modify: `src/runtime/numa_linux.go` — `numaStandDownIfNeeded`, `numaFixThreadPlacement`
- Modify: `src/runtime/proc.go` — `startTheWorldWithSema` trigger; `stopm` convergence check
- Modify: `src/runtime/runtime2.go` — `numa mNUMAState` field in `m`
- Create: `src/runtime/numa_mstate_on.go` (`//go:build goexperiment.numa`), `src/runtime/numa_mstate_off.go` (`//go:build !goexperiment.numa`)
- Modify: `src/runtime/stubs_nonlinux.go` — stubs
- Modify: `src/runtime/numa_linux_test.go`

**The `m` field, done without breaking the off binary (C2):** a plain new field in `m` shifts every later field's offset — compiled offsets (`0x1a8(AX)`-style immediates) survive Gate 8's address-stripping sed, so the census would fail, and worse, `m` sits near the 2048-byte size-class boundary that `lockVerifyMSize` throws on at boot (background.md §4.5). So the state is an **embedded struct that is empty when the experiment is off**:

```go
// runtime2.go, in type m struct (place at the END of the struct so no
// existing field's offset moves even when the experiment is on):
	numa mNUMAState // NUMA stand-down convergence state; zero-size when goexperiment.numa is off

// numa_mstate_on.go  (//go:build goexperiment.numa):
type mNUMAState struct {
	bindAllDone bool // this thread's placement converged after stand-down
}
func (s *mNUMAState) placementDone() bool { return s.bindAllDone }
func (s *mNUMAState) setPlacementDone()   { s.bindAllDone = true }

// numa_mstate_off.go (//go:build !goexperiment.numa):
type mNUMAState struct{}
func (s *mNUMAState) placementDone() bool { return false }
func (s *mNUMAState) setPlacementDone()   {}
```

Accessor methods (not direct field access) let `numa_linux.go` — which compiles in both experiment states — touch the state without build errors when the struct is empty. **Record `sizeof(m)` experiment-off before/after (must be identical) and experiment-on (must not cross a size class):** build a trivial canary and check with `gdb -batch -ex 'ptype /o struct runtime.m' ./canary` for both builds; put the numbers in the commit message.

**Interfaces:**

```go
// numaStandDownIfNeeded triggers stand-down if the new GOMAXPROCS
// exceeds the confined node's CPU count (locked decisions 3-4). Called
// from startTheWorldWithSema after sched.lock is released. One-way:
// sets numaStoodDown. NOTE it does NOT guarantee every thread is
// restored on return: the world is already restarting at this point
// (gcwaiting cleared; startTheWorldWithSema's own loop can newm,
// proc.go:1813), and Ms can be on allm before their procid is stored
// (mcommoninit runs before newosproc). The eager walk below is a
// latency optimization; per-thread correctness is numaFixThreadPlacement.
func numaStandDownIfNeeded(procs int32)

// numaFixThreadPlacement converges the calling M's placement after
// stand-down: restores BOTH its CPU affinity (self-call, no tid
// needed) AND its task mempolicy to Layer-1 BIND-all (set_mempolicy is
// per-thread and cannot be applied cross-thread). Called from stopm --
// parked-M path, never malloc, never steal; one atomic load when the
// experiment is on and stand-down has not happened, nothing when off.
// This is the correctness path: every M -- including ones the eager
// walk missed (unset procid, late clones) -- converges at its first
// park after stand-down.
func numaFixThreadPlacement()
```

Implementation:

```go
func numaStandDownIfNeeded(procs int32) {
	if !numaConfined.Load() || procs <= numaConfinedNodeCPUs {
		return
	}
	numaConfined.Store(false)
	numaStoodDown.Store(true)
	// Eager affinity restore for every M whose tid is visible: latency
	// optimization only. procid is read atomically -- mcommoninit
	// publishes an M to allm BEFORE newosproc stores procid (the
	// runtime's own walks spin on this, os_linux.go:848-853); a zero
	// procid here just means that M converges at its first park.
	for mp := allm; mp != nil; mp = mp.alllink {
		if tid := atomic.Load64(&mp.procid); tid != 0 {
			numaSetThreadAffinity(int32(tid), &numaSavedAffinity)
		}
	}
	// Revert this thread's own task policy to Layer-1 BIND-all.
	// numaSetProcessBindAll re-issues set_mempolicy and re-Stores the
	// already-published numaAllowedNodemask: deliberately idempotent --
	// the re-issue costs one syscall once per process lifetime, and its
	// get_mempolicy(MPOL_F_MEMS_ALLOWED) re-read is the documented
	// "re-read the mask on stand-down triggers" point for dynamic
	// cpusets (Task 14).
	numaSetProcessBindAll()
	getg().m.numa.setPlacementDone()
	if debug.numa > 0 {
		println("numa: confinement stood down, GOMAXPROCS", procs, ">", numaConfinedNodeCPUs)
	}
}

func numaFixThreadPlacement() {
	if !numaStoodDown.Load() {
		return
	}
	mp := getg().m
	if mp.numa.placementDone() {
		return
	}
	// Affinity first (idempotent if the eager walk already got us).
	if !numaSetThreadAffinity(0, &numaSavedAffinity) {
		return // retry at next park
	}
	w0 := numaAllowedNodemask.Load()
	if w0 == 0 {
		return
	}
	var mask numaNodemask
	mask[0] = w0
	if _, _, errno := linux.Syscall6(linux.SYS_SET_MEMPOLICY,
		uintptr(_MPOL_BIND), uintptr(unsafe.Pointer(&mask[0])), numaMaxNode, 0, 0, 0); errno != 0 {
		return // retry at next park
	}
	// Latch only after every syscall succeeded (I4): latching first
	// would permanently strand a thread that raced a transient failure.
	mp.numa.setPlacementDone()
}
```

`proc.go` hooks:

```go
	// startTheWorldWithSema, after unlock(&sched.lock), before worldStarted():
	if goexperiment.Numa {
		numaStandDownIfNeeded(procs)
	}
```

```go
	// stopm, at function entry (M is parking; never a malloc or steal path):
	if goexperiment.Numa {
		numaFixThreadPlacement()
	}
```

- [ ] **Step 1: Failing test**

```go
func TestNUMAStandDownOnGOMAXPROCSGrowth(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip("not multi-node")
	}
	if !runtime.NumaHasSetAffinityForTest() {
		t.Skip("no sched_setaffinity plumbing on this arch")
	}
	got := runTestProg(t, "testprog", "NUMAStandDown", "GOMAXPROCS=1")
	baff, bmode := parsePlacement(t, got, "before")
	aaff, amode := parsePlacement(t, got, "after")
	if bmode != 1 || !runtime.NumaIsNodeCPUCountForTest(baff) {
		t.Fatalf("before stand-down: affinity=%d mode=%d, want node-sized+PREFERRED; %q", baff, bmode, got)
	}
	if amode != 2 {
		t.Fatalf("after stand-down: mode=%d want MPOL_BIND(2); %q", amode, got)
	}
	if aaff <= baff {
		t.Fatalf("after stand-down: affinity=%d not restored past confined %d; %q", aaff, baff, got)
	}
}
```

- [ ] **Step 2: Run remotely, expect FAIL; commit red**

```bash
make push && make build && make test-numa RUN=TestNUMAStandDownOnGOMAXPROCSGrowth
git add src/runtime && git commit -m "runtime: add TestNUMAStandDownOnGOMAXPROCSGrowth (red)"
```

- [ ] **Step 3: Implement** per the block above. `go vet runtime`, `./make.bash`. Then record `sizeof(m)` per the C2 note (gdb `ptype /o` on canaries built with and without the experiment; off must be byte-identical to the parent, on must not cross the `m` size class — `lockVerifyMSize`), and paste the numbers into the Step 5 commit message.

- [ ] **Step 4: Green + full NUMA suite + one full `runtime -short` with the experiment on**

```bash
make push && make build
make test-numa RUN='TestNUMA'
ssh numa-dell 'GOEXPERIMENT=numa GOROOT=/home/deparker/go-numa GOTOOLCHAIN=local /home/deparker/go-numa/bin/go test -short runtime -count=1'
```

Known-flaky exceptions (e.g. `TestCgoNoEscape`) are re-run in isolation before judging, per the Layer-2 precedent.

- [ ] **Step 5: Commit green**

```bash
git add src/runtime
git commit -m "$(cat <<'EOF'
runtime: stand down NUMA confinement when GOMAXPROCS outgrows the node

One-way stand-down triggered in startTheWorldWithSema -- the single
funnel every GOMAXPROCS change (runtime.GOMAXPROCS,
SetDefaultGOMAXPROCS, updatemaxprocs) flows through via newprocs. The
trigger flips atomic state, does a best-effort eager affinity restore
over allm (atomic procid reads; Ms without a published procid, and
clones in flight while the world restarts, are expected misses), and
reverts its own task policy to Layer-1 BIND-all. Correctness is
carried by numaFixThreadPlacement in stopm: every M restores its own
affinity AND mempolicy at its next park, latching only after the
syscalls succeed. An M that never parks keeps the confined placement
until it first does: still balancer-exempt, bounded skew, documented.
The per-M latch lives in an embedded mNUMAState that is empty when the
experiment is off, so sizeof(m) and all field offsets are unchanged in
off builds (sizes recorded below).
EOF
)"
```

---

### Task 4: Workstream A gate battery (numa-dell; STOP on any hard-gate failure)

**Files:** none in `src/`. Results → `numa-design/RESULTS.md` "Workstream A gate (v3)"; raws + driver invocations → `numa-design/bench-data/wsA-*/`, committed **with** the results.

Pre-declared metrics (this section is the pre-registration; do not add claims post hoc):

| Gate | Primary metric | Comparisons (all on the primary metric) | Pass |
|------|----------------|------------------------------------------|------|
| 1. 1P json (hard) | median ns/op + user+sys-ns/op | off vs on, same commit, BENCHNUM=10 | benchstat ≤ +2% / not significant |
| 2. 1P alloc micro (hard) | ns/op geomean | off vs on, `-count=10` | ≤ +2% |
| 3. 256P json (hard) | sec/op | off vs on, **unconditionally 3 sessions × BENCHNUM=10, pooled n=30** (pre-declared — no data-dependent stopping) | pooled benchstat not significantly worse; a pooled significant regression beyond +2% = FAIL (stop) |
| 4. Stand-down proof (hard) | syscall traces, three arms | (a) 256P: 0 `sched_setaffinity`, exactly 1 `set_mempolicy` (BIND); (b) confined 1P: 1 `sched_setaffinity`, 2 `set_mempolicy` (BIND then PREFERRED), `mbind` present; (c) stand-down run: ~1 `sched_setaffinity` per live M + further `set_mempolicy(BIND)` as Ms park | all three shapes as declared |
| 5. vmstat (hard) | hint faults / migrations | confined 1P json + confined candidate arms | 0/0 |
| 6. Candidate 1 (decision) | ns/op `Garbage/benchmem-MB=4096-128` | **C vs B** significantly better (superiority) AND **C vs A** non-inferior: 95% CI upper bound of the C/A ratio ≤ +10% | C-vs-B closes most of the ~60% B-vs-A gap; C-vs-A CI bound holds |
| 7. Candidate 2 (decision) | round-median GC cycle wall ns (n=10 rounds, ICC-honest) | C vs B significantly better AND C vs A non-inferior (95% CI upper bound ≤ +10%) | same shape (~25% gap); MDE ≈5.6% at n=10 stated with any null |
| 8. Off-binary census | objdump function diff on a NON-test binary, experiment off | HEAD vs Task-0 parent | build-ID-only |

Non-inferiority note (I1): "C within +5% of A" or "C ~ A" would be claims **below the design's resolving power** — the prior three-arm sweep's MDE was ≈9.05% at n=15 (pooled CV 8.84%, RESULTS.md:2311-2313), and a TOST equivalence test at ±5% would need n≈50. The pre-declared C-vs-A criterion is therefore **non-inferiority with an explicit confidence bound** (bootstrap or log-ratio CI on the n=15 round pairs): PASS iff the 95% CI upper bound of C/A − 1 is ≤ +10%. Report the point estimate and full CI either way; never report "C ≈ A" without the bound. The same criterion and phrasing is used in "What done means".

- [ ] **Step 1: Push, build, idle-check**

```bash
make push && make build
ssh numa-dell 'ps aux --sort=-%cpu | head -5; sysctl kernel.numa_balancing'
```

- [ ] **Step 2: Gate 1 — 1P json** (note: at GOMAXPROCS=1 the numa arm **is confined** — this gate now also measures confinement's 1P cost, which must still clear the band)

```bash
ssh numa-dell 'cd /home/deparker/go-numa && GOROOT=$PWD GOMAXPROCS=1 BENCHNUM=10 OUT=/tmp/wsA-gate1 ./numa-design/gate-json.sh'
ssh numa-dell '/tmp/numa-tools/benchstat /tmp/wsA-gate1/baseline.out /tmp/wsA-gate1/numa.out'
```

- [ ] **Step 3: Gate 2 — 1P alloc micro**

```bash
ssh numa-dell 'cd /home/deparker/go-numa && export GOROOT=$PWD PATH=$PWD/bin:$PATH GOTOOLCHAIN=local
GOMAXPROCS=1 go test runtime -run=NONE -bench="Malloc(8|16|Types)" -count=10 >/tmp/wsA-alloc-base.out
GOMAXPROCS=1 GOEXPERIMENT=numa go test runtime -run=NONE -bench="Malloc(8|16|Types)" -count=10 >/tmp/wsA-alloc-numa.out
/tmp/numa-tools/benchstat /tmp/wsA-alloc-base.out /tmp/wsA-alloc-numa.out'
```

- [ ] **Step 4: Gate 3 — 256P json** (numa arm must stand down at startup: GOMAXPROCS=256 > 128)

```bash
# Three sessions, unconditionally (idle re-verified between each):
ssh numa-dell 'cd /home/deparker/go-numa && GOROOT=$PWD GOMAXPROCS=256 BENCHNUM=10 OUT=/tmp/wsA-gate3-r1 ./numa-design/gate-json.sh'
ssh numa-dell 'cd /home/deparker/go-numa && GOROOT=$PWD GOMAXPROCS=256 BENCHNUM=10 OUT=/tmp/wsA-gate3-r2 ./numa-design/gate-json.sh'
ssh numa-dell 'cd /home/deparker/go-numa && GOROOT=$PWD GOMAXPROCS=256 BENCHNUM=10 OUT=/tmp/wsA-gate3-r3 ./numa-design/gate-json.sh'
```

**Pre-declared (I2, replacing the Layer-2 era's data-dependent "escalate if the signal is mixed"):** all three replicates run unconditionally regardless of what the first shows — a "replicate only if bad" rule is a one-sided stopping rule that biases the pooled estimate. Verdict comes from pooled benchstat (n=30). This benchmark's heap autoscaling produces spurious sign-flipping significance at n=10 (Layer-2 Gate 3: +39%, ~, −21%); pooling is the pre-registered remedy. **A pooled significant regression beyond +2% is a hard FAIL: stop, find the cost (suspects: the goexperiment-guarded checks in `startTheWorldWithSema`/`stopm`), fix or revert.**

- [ ] **Step 5: Gate 4 — strace proof, three arms** (separate from timing runs; strace has overhead)

```bash
# Arm (a) — 256P, never confines:
ssh numa-dell 'cd /tmp/wsA-gate3-r1 && strace -f -e trace=sched_setaffinity,set_mempolicy,mbind -o /tmp/wsA-strace-256p.txt \
  env GOMAXPROCS=256 ./numa/json -benchmem=512 -benchnum=1 -benchtime=2s >/dev/null
grep -c "sched_setaffinity" /tmp/wsA-strace-256p.txt || true
grep set_mempolicy /tmp/wsA-strace-256p.txt'
# Arm (b) — confined 1P:
ssh numa-dell 'cd /tmp/wsA-gate3-r1 && strace -f -e trace=sched_setaffinity,set_mempolicy,mbind -o /tmp/wsA-strace-1p.txt \
  env GOMAXPROCS=1 ./numa/json -benchmem=512 -benchnum=1 -benchtime=2s >/dev/null'
# Arm (c) — actual stand-down exercised (I7): the NUMAStandDown testprog,
# built with the experiment, started confined and raised past the node:
ssh numa-dell 'cd /home/deparker/go-numa && export GOROOT=$PWD PATH=$PWD/bin:$PATH GOTOOLCHAIN=local
GOEXPERIMENT=numa go build -o /tmp/wsA-testprog ./src/runtime/testdata/testprog
strace -f -e trace=sched_setaffinity,set_mempolicy -o /tmp/wsA-strace-standdown.txt \
  env GOMAXPROCS=1 /tmp/wsA-testprog NUMAStandDown'
```

Expected shapes (all archived):

- **(a) 256P:** `sched_setaffinity` count **0**; exactly one `set_mempolicy(0x2, ...)` (MPOL_BIND); `mbind` calls **present** (Layer 1 stamping, unchanged).
- **(b) confined 1P:** exactly one `sched_setaffinity`; **two** `set_mempolicy` — first `0x2` (Layer-1 BIND-all from `numaSchedinit`), then `0x1` (PREFERRED from `numaConfineIfSmall`); `mbind` calls **present** (locked decision 5: arena BIND-all keeps running while confined). Also sample `wc -l /proc/PID/maps` during one confined run: expect the low, merged-VMA count (~tens of lines, like the Layer-1 baseline at RESULTS.md:1496-1515), NOT Layer 2's ~1172 — empirical confirmation that uniform BIND-all merges.
- **(c) stand-down:** the confined prologue from (b), then at the GOMAXPROCS raise approximately one `sched_setaffinity` per live M (the eager walk) plus a further `set_mempolicy(0x2, ...)` on the STW thread, and additional per-thread `set_mempolicy(0x2, ...)` as Ms park (`numaFixThreadPlacement`). Also record the probe's `standdown-gomaxprocs-wall-ns` line here and compare it against the same probe on the stock build — this puts the stand-down walk's stop-the-world cost on the record (the wall-clock bound stands in for a `/sched/pauses/total/other:seconds` delta: that metric is a histogram and the single stwGOMAXPROCS pause lands in one bucket — additionally note which bucket shifted if convenient).

- [ ] **Step 6: Gates 6+7 — pathology candidates 1 and 2, single-session three-arm sweeps via the Task-0 driver**

Build once (record `go version -m` pins):

```bash
ssh numa-dell 'export GOROOT=/home/deparker/go-numa PATH=/home/deparker/go-numa/bin:$PATH GOTOOLCHAIN=local
GOBIN=/tmp/pb/base go install golang.org/x/benchmarks/garbage@v0.0.0-20260819172200-70693762b6a0
GOBIN=/tmp/pb/numa GOEXPERIMENT=numa go install golang.org/x/benchmarks/garbage@v0.0.0-20260819172200-70693762b6a0
go build -o /tmp/pb/gcpause-base ./numa-design/gc-pause-bench
GOEXPERIMENT=numa go build -o /tmp/pb/gcpause-numa ./numa-design/gc-pause-bench'
```

Candidate 1 (n=15, matching the three-arm precedent; ~1 h). Node-0 free ≥10 GB checked by protocol before A; the **C arm confines to its boot node** — check that node too:

```bash
ssh numa-dell 'cd /home/deparker/go-numa && numactl --hardware | grep free
./numa-design/pathology-sweep.sh wsA-cand1 15 /tmp/pb/wsA-cand1 \
  "A=numactl --cpunodebind=0 --membind=0 env GOMAXPROCS=128 /tmp/pb/base/garbage -benchmem=4096 -benchnum=1" \
  "B=env GOMAXPROCS=128 /tmp/pb/base/garbage -benchmem=4096 -benchnum=1" \
  "C=env GOMAXPROCS=128 /tmp/pb/numa/garbage -benchmem=4096 -benchnum=1"'
```

Sanity mid-sweep expectations: arm C vmstat **0/0** every round; arm B hint faults ≫ 0 every round (else setup fault — investigate, don't average in); one C round's `/proc/PID/status` `Cpus_allowed_list` shows one node's CPUs (spot-check that confinement actually fired in the benchmark child).

Analysis (pre-declared): `benchstat B.out C.out` (primary; PASS = C significantly faster), C-vs-A **non-inferiority** per the Gate-6 criterion above (95% CI upper bound of the C/A ratio ≤ +10%, computed from this session's n=15 round pairs; benchstat's table reported alongside but the CI bound is the criterion), `benchstat A.out B.out` (context only — the pinning gap, ~+60% in prior sessions). All three from **this session only**; never reuse prior-session arms.

Candidate 2 (heavy gc-pause profile; ~90 min):

```bash
ssh numa-dell 'cd /home/deparker/go-numa
FLAGS="-ptrheap=true -toucher=false -heap=4096 -idle=1000000 -stacks=200 -warm=45 -gcgap=5s -discard=2 -n=8"
./numa-design/pathology-sweep.sh wsA-cand2 10 /tmp/pb/wsA-cand2 \
  "A=numactl --cpunodebind=0 --membind=0 env GOMAXPROCS=128 GODEBUG=gcshrinkstackoff=1 /tmp/pb/gcpause-base $FLAGS" \
  "B=env GOMAXPROCS=128 GODEBUG=gcshrinkstackoff=1 /tmp/pb/gcpause-base $FLAGS" \
  "C=env GOMAXPROCS=128 GODEBUG=gcshrinkstackoff=1 /tmp/pb/gcpause-numa $FLAGS"'
```

Analysis (pre-declared): **round-level** — median of the 8 cycles per round, n=10 per arm, exact Mann–Whitney U (cycles within a round are clustered, ICC ≈ 0.6–1.0 measured previously; report ICC and effective n; the raw n=80 cycle-level benchstat may be shown but is never the verdict). C vs B primary (superiority); C vs A co-required as **non-inferiority** (95% CI upper bound of the C/A round-median ratio ≤ +10%). State the achieved MDE numerically with any null — the prior candidate-2 round-level noise floor was **≈5.6% at n=10** (pooled round-level CV 4.45%, RESULTS.md candidate 2 correction) — so a "~" verdict is always reported as "no effect ≥ ~5.6% detectable", never as "no effect".

- [ ] **Step 7: Gate 5 — vmstat 0/0 confined arms** — already collected per-round by the driver; additionally one confined 1P json run wrapped in `gate-vmstat.sh snap/diff`. Expected 0/0 (rerun once before declaring FAIL — counters are machine-global).

- [ ] **Step 8: Gate 8 — off-binary function census (NON-test binary)**

A **test** binary is the wrong census subject (C2): `export_numa_test.go` is compiled into it, and any test-only export that is not `goexperiment.numa`-gated roots the confinement code even with the experiment off, producing census diffs that say nothing about production binaries. Census a plain program instead (the Task-1 build-tag rule keeps the test binary clean too, but the gate must not depend on that):

```bash
cd src
printf 'package main\n\nfunc main() { println("census") }\n' > /tmp/census-canary.go
GOEXPERIMENT= go build -o /tmp/census-head /tmp/census-canary.go
git worktree add /tmp/census-parent <task0-parent-sha>
(cd /tmp/census-parent/src && GOEXPERIMENT= go build -o /tmp/census-parent-bin /tmp/census-canary.go)
objdump -d /tmp/census-head       | sed 's/[0-9a-f]\{6,\}//g' >/tmp/census-head.txt
objdump -d /tmp/census-parent-bin | sed 's/[0-9a-f]\{6,\}//g' >/tmp/census-parent.txt
diff -q /tmp/census-head.txt /tmp/census-parent.txt || diff /tmp/census-head.txt /tmp/census-parent.txt | head -50
git worktree remove /tmp/census-parent
```

Pass bar: function-level differences limited to build IDs. **Caveat the sed does not fix (C2):** stripping hex constants does NOT hide struct-offset shifts — a changed `m` field offset appears as a different displacement immediate (e.g. `0x1a8(AX)` → `0x1b0(AX)`) and would rightly fail the diff; the Task-3 `mNUMAState` empty-when-off embedding exists precisely so no off-build offset moves. This workstream also adds a guarded call in `startTheWorldWithSema` and `stopm`; with `goexperiment.Numa` false these are const-folded empty — the census verifies exactly that.

- [ ] **Step 9: Record + archive + verdict**

Append "Workstream A gate (v3)" to RESULTS.md: SHA, `go version`, kernel, x/benchmarks pin, every gate's numbers and verdicts, ICC/effective-n for candidate 2, MDEs for any null, `kernel.numa_balancing` confirmed 1 at session end. Copy `/tmp/pb/wsA-*` raws AND the driver invocation lines into `numa-design/bench-data/wsA-cand1/`, `wsA-cand2/`, `wsA-gates/`. One commit for results + raws:

```bash
git add numa-design/RESULTS.md numa-design/bench-data/wsA-*
git commit -m "numa-design: record Workstream A (fill-one-socket) gate results"
```

**Stop rule:** hard gates 1–5/8 FAIL → fix or revert; do not proceed to anything, including Task 6. Decision gates 6–7 FAIL (C not better than B, or C-vs-A non-inferiority bound violated) → Workstream A does not ship as a *win*; record honestly and evaluate whether the measured residual gap justifies Workstream B (Task 7's decision record). **Task 6's Layer-2 removal proceeds on hard-gate pass alone** (I9, locked decision 8): its justification — Layer 2's own IMC FAIL and the three-arm inertness proof — is independent of whether fill-one-socket wins its candidates.

---

### Task 5: Documentation for review (small)

**Files:**
- Modify: `numa-design/bind-all-policy.md` — two changes:
  1. Add a "Fill-one-socket-first (v3)" section: confinement policy table row (task PREFERRED while confined, arena BIND-all mbind still running; what inherits; the until-first-park convergence residual after stand-down; the pre-runtime cgo-thread rationale for keeping the arena mbind), one paragraph each.
  2. **Fix the stale maxnode claim at `bind-all-policy.md:21-23`** ("`maxnode` is the width of one nodemask word (64)…"): the shipped convention is `numaMaxNode = 65` with a fixed 8-byte mask buffer on every arch — see `numa_linux.go`'s `numaMaxNode` doc comment for the 32-bit out-of-bounds rationale. The doc currently contradicts the code.

- [ ] **Step 1:** Write the section and the maxnode correction (mirror the existing table's style; cite locked decisions 1, 4, 5 by content, not by "the plan said").
- [ ] **Step 2:** `git add numa-design/bind-all-policy.md && git commit -m "numa-design: document fill-one-socket confinement; fix stale maxnode=64 claim"`

---

### Task 6: Remove superseded Layer 2 PREFERRED-at-grow (contingent on Task 4's HARD gates only — I9)

**Entry condition:** Task 4 hard gates 1–5 and 8 PASS. The decision gates (6–7, candidate outcomes) do **not** gate this task: removal is justified by independent evidence regardless of how fill-one-socket performs. File-conflict note: this task touches only `numaBindArena` and Layer-2 test scaffolding — no overlap with Workstream B's Tasks 8–10 surfaces beyond `numa_linux.go` housekeeping — so it may proceed in parallel with a Workstream B decision without rebase pain.

**Files:**
- Modify: `src/runtime/numa_linux.go` — delete the PREFERRED half of `numaBindArena` (keep BIND-all; keep `numaGetCPUNode` **and** `numaCurrentNode` — confinement's node choice uses them), delete `numaPreferredCalls`
- Modify: `src/runtime/export_numa_test.go` — delete `NumaPreferredBindCalls`
- Modify: `src/runtime/numa_linux_test.go` — delete `TestNUMAPreferredBindOnGrow`

Evidence basis (cite in the commit): Layer-2 IMC gate FAIL (+0.10%/−0.07% vs required ≥10% drop); three-arm sweep C-full ≈ C-L1 (primary p=0.838, direct p=0.631) — the code is inert for good and bad alike; and its per-chunk different-node PREFERRED policies were the sole cause of the ~34× VMA blowup (uniform BIND-all merges — confirmed again by Gate 4 arm (b)'s maps count).

- [ ] **Step 1:** Apply the deletion — the shipped-tree equivalent of `bench-data/pathology/l1-only.patch`: `numaBindArena` ends after the `MPOL_BIND` `mbind` call. Update `numaBindArena`'s doc comment (drop the Layer-2/policy-replacement paragraphs; keep init-order guard, scavenger note, maxnode rationale).
- [ ] **Step 2:** `cd src && go vet runtime && ./make.bash && GOEXPERIMENT=numa go test runtime -run 'TestNUMA' -count=1` (locally), then `make push && make build && make test-numa RUN='TestNUMA'`.
- [ ] **Step 3:** Re-run Gate 1 + Gate 2 (1P json BENCHNUM=10; alloc micro) on the removal commit — cheap insurance that the deletion is clean. Record a short RESULTS.md note.
- [ ] **Step 4: Commit**

```bash
git add src/runtime numa-design/RESULTS.md
git commit -m "$(cat <<'EOF'
runtime: drop Layer 2 PREFERRED-at-grow arena homing

Superseded: its IMC locality gate failed (remote-DRAM share unchanged,
+0.10%/-0.07% vs a required >=10% drop) and the single-session
three-arm sweep proved it behaviorally inert (C-full vs C-L1 p=0.838).
Fill-one-socket-first supersedes it for small processes; BIND-all
(Layer 1, #14406) arena stamping is unchanged. Also removes the ~34x
per-chunk VMA fragmentation the homing calls caused.
EOF
)"
```

---

## Workstream B: the full locality unit (design §12.3–§12.4)

**Entry condition (Task 7 records it):** Workstream A shipped its gates, or A's candidate results show a remaining C-vs-A/C-vs-B gap for node-exceeding processes that justifies the cost. Workstream B is **one gated unit** — homing + routing + thread stability land and are measured together (design §12.1: any proper subset measures ~zero; Layer 2 was the experiment that proved it). Tasks 8–10 build the three ingredients; nothing merges until Task 11's combined gates pass.

### Task 7: Go/no-go decision record

- [ ] **Step 1:** Append "Workstream B decision" to RESULTS.md: quote Workstream A's gate table, state which entry condition holds (or that neither does → Workstream B cancelled; mark Tasks 8–11 cancelled in this plan), and pre-register Task 11's primary metric (IMC remote share) before any Workstream B code lands. Commit.

### Task 8: Per-node heap growth streams (ingredient a — homing, design §12.3)

**Files:** `src/runtime/mheap.go`, `src/runtime/malloc.go` (arena hints), `src/runtime/numa_linux.go`.

**Interfaces (exact; implementation may refine internals but not shapes):**

```go
// heapArena gains the home node (one byte; pointer->node is then the
// same two loads spanOf already does — an mspan never crosses arenas).
type heapArena struct {
	// ...
	node uint8 // NUMA home node of this arena's address range
}

// mheap: per-node hint chains and per-node current arena. With the
// experiment off (or numaHeapNodes()==1) index 0 is the only stream and
// the code collapses to today's shape (node argument is the constant 0).
arenaHints [numaMaxHeapNodes]*arenaHint // hint addresses >= 1 TiB apart per node
curArena   [numaMaxHeapNodes]struct{ base, end uintptr }

func (h *mheap) grow(npage uintptr, node int32) (uintptr, bool)
func numaArenaNode(p uintptr) int32 // heapArena.node lookup; 0 when experiment off

// Homing enforcement at map time: mbind(MPOL_PREFERRED, node) at sysMap
// on the node's stream (VMA policy survives the scavenger — MADV_FREE/
// DONTNEED refaults re-home under the surviving policy; no re-bind).
```

`numaMaxHeapNodes` **must be build-tagged (I5)** — `goexperiment.numa` file: `const numaMaxHeapNodes = 8`; `!goexperiment.numa` file: `const numaMaxHeapNodes = 1` — because it multiplies static arrays (`mheap.arenaHints`/`curArena` here, and Task 9's per-node `mcentral` spanSets: mcentral is ~168 B today × 136 span classes; an unconditional ×8 is ~180 KB of extra BSS in every Go binary with the experiment off). With the constant 1 in off builds, all indexed shapes collapse to today's layout. Nodes beyond `numaMaxHeapNodes`: **not** `node % N` sharing — stand down: hosts with more nodes than streams use stream 0 for all (correctness first; record). Tight-VA arches (39-bit): fall back to shared hints, keep `heapArena.node` correctness, lose address-monotonicity (design §12.3 caveat).

**`mheap.sysAlloc` identity check (I6, must be in the interface work, not discovered mid-implementation):** `sysAlloc` decides whether it is allocating heap (vs a user-arena) by pointer identity — `hintList == &h.arenaHints` (`malloc.go:741-757`). Turning `arenaHints` into a per-node array silently breaks that check (`&h.arenaHints[node] != &h.arenaHints` in the old shape's sense). The interface change must replace the identity test with an explicit membership/range test over the per-node array (or a caller-supplied `register bool`), with a test covering both the heap and user-arena paths.

- [ ] Steps: failing unit test that two allocations forced from different (pinned) nodes land in arenas with different `heapArena.node`; implement; `grow` callers pass `numaGrowNode()` (getcpu **at grow only** — the existing `numaGetCPUNode` frequency); off-path collapse verified by the off-binary census; red/green commits per branch convention. Commit message: `runtime: per-node heap arena streams for NUMA homing`.

### Task 9: Refill-time routing + metrics (ingredient b)

**Files:** `src/runtime/mcentral.go`, `mcache.go`, `mheap.go` (central), `metrics.go`, `metrics/doc.go`.

**Interfaces:**

```go
// mcentral partial/full spanSets become per-node:
//   partial [2][numaMaxHeapNodes]spanSet   (or an equivalent indexed shape)
// mcache.refill / mcentral.cacheSpan consult the current node ONCE per
// refill via numaRefillNode() (getcpu; NEVER called from getMCache or
// mallocgc fast paths — the v2 forbidden list binds). A span taken from
// a foreign node's set (fallback when the local set is empty) is
// counted remote; no flush, no reassignment, no per-malloc scans.

// runtime/metrics:
//   /numa/span-refills:local   (uint64 counter)
//   /numa/span-refills:remote  (uint64 counter)
// incremented ONLY in refill paths — the in-vivo locality proxy
// between perf-counter runs (design §12.4).
```

Lock-rank note (I5): `mcentral.init` must `lockInit` the spine lock of **every** per-node spanSet — 2 partial + 2 full sets × `numaMaxHeapNodes` = 4×N spine locks per span class, not just the four the current shape has. Missing inits trip `staticlockranking` builds; run one `GOEXPERIMENT=numa,staticlockranking` test pass in this task.

- [ ] Steps: failing metrics test (`TestNUMASpanRefillMetrics`: counters exist, sum > 0 after allocation churn, never decrease); implement per-node spanSets + routing + counters; `mheap.grow` requests pages for the refilling node (ties Task 8 to Task 9); 1P local sanity: with one thread the remote counter stays ~0. Commit: `runtime: route mcentral span refills by physical NUMA node`.

### Task 10: Node-mask soft affinity from the scheduler (ingredient c)

**Files:** `src/runtime/proc.go` (`schedule()` → wireup hook), `src/runtime/numa_linux.go`, the Task-3 `mNUMAState` (add `lastNode int8`).

**Hook placement (I3 — NOT `acquirep`):** `acquirep` runs with locks held on several of its call paths — `procresize` asserts `sched.lock` is held, and `allocm`'s path holds `allocmLock` plus `acquirem` — so a syscall there is a lock-ordering/latency hazard. The hook goes in **`schedule()`**, after `findRunnable` returns and before `execute`, where the M is about to run user code and `mp.locks == 0`; guard with an explicit `if mp.locks != 0 { return }` (debug builds may `throw`). Frequency is per scheduler pass — same order as acquirep, never malloc.

**Interfaces:**

```go
// numaNoteSchedule applies node-mask soft affinity to the current M:
// read the node (getcpu — scheduler-pass frequency, not malloc), and
// ONLY if it differs from m.numa.lastNode, sched_setaffinity the M to
// that node's CPU mask (kernel keeps full freedom within the socket).
// Fires on node CHANGE only — steady state is two loads. Called from
// schedule() with mp.locks == 0 (asserted); never from acquirep.
//
// Stand-down rule (design §12.4, same detection as Workstream A): if
// the process started with a narrowed affinity mask, soft affinity
// never engages — operator placement wins. Also never engages while
// fill-one-socket confinement is active (already single-node), after
// a confinement stand-down (numaStoodDown — the operator/API raised
// GOMAXPROCS; do not re-narrow threads), or on arches without
// numaSetThreadAffinity.
func numaNoteSchedule()
```

- [ ] Steps: failing test (subprocess: after allocation churn at GOMAXPROCS=8 on the 2-node box, each M's `Cpus_allowed_list` is single-node — testprog probe walks `/proc/self/task/*/status`); implement; verify the scheduler pass is not measurably hotter (1P gates). Commit: `runtime: soft node affinity for Ms from schedule`.

### Task 11: Workstream B validation and gates (the unit's single verdict)

Validation order is mandatory (design §12.4): **prove routing correctness with externally pinned threads FIRST**, then measure unpinned.

- [x] **Step 1 — pinned routing proof:** run a DRAM-heavy alloc/read workload as two halves, `numactl --cpunodebind=0` and `--cpunodebind=1` (soft affinity irrelevant under external pinning): `/numa/span-refills:local / (local+remote)` must be ≥95% in each half, and sampled `heapArena.node` distribution must track the pinned node. This isolates routing from placement stability. FAIL → fix routing before any unpinned measurement.
- [x] **Step 2 — hard gates:** 1P json (BENCHNUM=10) ≤ +2%; 1P alloc micro ≤ +2%; **256P json ≤ +2%**; RSS not systematically fatter (per-node spanSets can strand memory — watch `peak-RSS-bytes`, pooled n≥30 if noisy); vmstat 0/0; off-binary census (workstream's once).
- [x] **Step 3 — IMC decision gate (pre-registered primary):** the metric that killed Layer 2, now with all three ingredients present. Same events, same protocol, ≥3 interleaved runs per arm, medians:

```bash
perf stat -x, -e mem_load_l3_miss_retired.local_dram,mem_load_l3_miss_retired.remote_dram -- \
  env GOMAXPROCS=256 ./json -benchmem=512 -benchnum=1 -benchtime=10s
```

**Pass:** ≥10% **relative** drop in `remote/(local+remote)` vs the experiment-off arm (e.g. 0.48 → ≤0.432). Corroborate with the `/numa/span-refills` ratio from the same runs. **Fail:** stop; RESULTS.md verdict must say the three-ingredient unit was actually measured (unlike Layer 2) and what the refill-local ratio was — that distinguishes "routing broken" from "routing works, hardware can't show it".
- [x] **Step 4 — pathology candidates 1–2 rerun** (three arms, single session, Task-0 driver) as supporting evidence; record.
- [x] **Step 5:** RESULTS.md + bench-data archive + verdict commit. Merge the unit only on a full pass.

---

## Workstream C: enablers and diagnostics

Independent of A/B except where noted; Tasks 12 and 13 may run in parallel with Workstream A (different files); Task 14's implementation half is contingent on Workstream B profiles.

### Task 12: Phase-shift attribution rerun (settles bandwidth-spread vs fault-tax)

**Files:** `numa-design/phase-shift/main.go` (additive flag), RESULTS.md, bench-data.

Candidate 3's C-vs-B win replicated across two sweeps (Fisher p=0.0056) but its *mechanism* is contested: dual-memory-controller bandwidth (~31 GB/s single-controller ceiling vs 36–58 GB/s spread arms) vs balancer fault-tax (~0.3 GB/s migration traffic — two orders too small). This task decides it with placement data.

- [ ] **Step 1 — pre-register in RESULTS.md (commit before running):** primary = ns/read, B vs C, n=20, exact MWU + Wilcoxon + sign test + leave-one-out all reported (the full robustness battery, not just MWU). Attribution analysis: per round and per phase, compute the C arm's heap page split across nodes from `/proc/self/numa_maps` (sum `N0=`/`N1=` fields over heap VMAs). **H-bandwidth predicts:** across C rounds, ns/read correlates negatively with node-balance (min(N0,N1)/(N0+N1)) — fast rounds are spread rounds, and the 2-in-10 slow C rounds show ≥80/20 skew. **H-faulttax predicts:** no such correlation; C's advantage tracks B's per-round migration volume instead. Spearman rank correlation, pre-declared α=0.05.
- [ ] **Step 2:** Add `-numamaps` flag to phase-shift: at each phase boundary, copy `/proc/self/numa_maps` to `<out>-phase<k>.numamaps` (stdlib file copy in the coordinator goroutine between phases — never in the reader loop). Commit code before the sweep.
- [ ] **Step 3:** Sweep on the post-Task-6 toolchain (HEAD's `GOEXPERIMENT=numa` is then genuinely L1-only — no scratch patch needed, unlike the previous L1-only reruns): `pathology-sweep.sh wsC-phase 20 ... "B=..." "C=..."` with GOMAXPROCS=128 both arms, same flags as the original candidate 3.
- [ ] **Step 4:** Analysis per the pre-registration; RESULTS.md + bench-data commit. Either outcome is a deliverable: H-bandwidth confirmed reframes the upstream story ("BIND-all preserves dual-controller spread that balancer churn disrupts"); H-faulttax confirmed restores the fault-tax narrative.

### Task 13: `getcpu` assembly for remaining Linux arches

**Files:** `src/runtime/sys_linux_{386,arm,loong64,mipsx,mips64x,ppc64x,riscv64,s390x}.s`, `src/runtime/numa_linux_getcpu.go` (widen tag), `src/runtime/numa_linux_getcpu_other.go` (shrink tag).

Follow the amd64/arm64 pattern (`#define SYS_getcpu`, NOSPLIT wrapper matching `func getcpu(cpu, node *uint32) int32`; third argument NULL). Syscall numbers (verify each against the arch's `unistd.h` table before use — do not trust this table blind):

| GOARCH | `SYS_getcpu` |
|--------|--------------|
| 386 | 318 |
| arm | 345 |
| riscv64 | 168 (generic) |
| loong64 | 168 (generic) |
| ppc64 / ppc64le | 302 |
| s390x | 311 |
| mips / mipsle (o32) | 4312 |
| mips64 / mips64le (n64) | 5271 |

- [ ] **Step 1:** One arch per commit or one commit for all — but each arch verified: asm added, `numa_linux_getcpu.go` build tag extended (`amd64 || arm64 || riscv64 || ...`), `_other.go` complement shrunk.
- [ ] **Step 2:** Link sweep across all `GOOS=linux` arches × {off, numa} (trivial main + `go test -c runtime` for at least riscv64), matching the Layer-2 22-combination precedent. `TestNUMAGetcpu` runs on whatever native hardware is available (numa-dell covers amd64 only; the rest are link+vet-verified).
- [ ] **Step 3:** Commit: `runtime: add getcpu wrappers for remaining linux architectures`. Note: this also widens where fill-one-socket confinement *could* engage — but `numaHasSetAffinity` still gates it to amd64/arm64 until `SYS_SCHED_SETAFFINITY` constants are added per-arch (do that only with a machine to test on; record as future work).

### Task 14: rseq / vDSO getcpu (research-then-implement, profile-gated)

**DEFERRED (profile gate not met at Task 11)** — Task 11's own CPU-cost
attribution (E2) measured the NUMA syscall/routing-decision path
(`getcpu` et al.) at 0.02–0.05% of cycles across every capture, never
approaching the ≥1% adoption bar in Step 2 below. Per Step 2's own
stated fallback, the raw syscall stays; see RESULTS.md "Workstream B
verdict (Task 11 decision)" for the full record.

The refill/scheduler-pass paths added by Workstream B call raw-syscall `getcpu` (~50 ns). Alternatives: amd64 `__vdso_getcpu` (few ns; runtime has vDSO plumbing in `vdso_linux_amd64.go`; arm64 has **no** vDSO getcpu) and rseq `cpu_id` (~1 ns userspace read; kernel ≥4.18; needs runtime-owned per-thread registration and a coexistence story with glibc/cgo rseq registration — `EBUSY` on double registration; see background.md §7).

- [ ] **Step 1 (research, anytime):** one-page note in `numa-design/` covering: vDSO getcpu wiring cost on amd64; rseq registration design (register in `minit`, handle cgo threads, `CONFIG_RSEQ` absence fallback); what glibc versions pre-register rseq and how `MFD`/`RSEQ_FLAG_UNREGISTER` interacts. Commit the note.
- [ ] **Step 2 (adoption gate — do not implement without it):** a CPU profile from Workstream B's Task 11 Step 1 pinned run (or any gate run) showing `getcpu` ≥1% of cycles attributable to refill/scheduler-pass paths. Below that, the raw syscall stays (design §12.4: "at grow/refill frequency the raw syscall alone is also acceptable").
- [ ] **Step 3 (only if gated in):** implement vDSO path on amd64 first (smallest change), re-profile, only then consider rseq. 1P gates re-run on any change.

### Task 15: Dynamic cpuset handling note

- [ ] **Step 1:** Add to `numa-design/bind-all-policy.md` (same commit as Task 5's section or separate): affinity and allowed-node masks are read at startup and re-read **only on stand-down triggers** (`numaStandDownIfNeeded` → `numaSetProcessBindAll` re-queries `MPOL_F_MEMS_ALLOWED`); a cpuset narrowed *after* startup is not detected until then; restoring the saved mask intersects with the live cpuset in the kernel (or fails per-thread, leaving that thread on its current mask). Staleness is **accepted** — containers that repartition cpusets mid-run get correct-but-stale placement, never a crash. Add a matching sentence to `numaShouldConfine`'s doc comment.

---

## Runtime test commands (cheat sheet)

```bash
make push && make build
make test-numa RUN='TestNUMAFillOneSocketConfined'
make test-numa RUN='TestNUMAStandDownOnGOMAXPROCSGrowth'
make test-numa RUN='TestNUMA'                # full NUMA suite
cd src && GOEXPERIMENT=numa go test runtime -run 'TestNUMA' -count=1   # local (1-node: most tests skip)
ssh numa-dell 'cd /home/deparker/go-numa && ./numa-design/pathology-sweep.sh <campaign> <n> <out> "A=..." "B=..." "C=..."'
```

---

## Self-review vs design §12.1–§12.4

| Spec item | Plan location |
|-----------|---------------|
| §12.1 three-ingredient rule (subset ⇒ ~zero) | Workstream B is ONE gated unit (Tasks 8–11); Task 7 records the framing; Task 11 verdict language |
| §12.2 fill-one-socket-first | Workstream A entire (Tasks 1–5); locked decisions 1–7 |
| §12.2 "one node-mask choice at startup" | `numaConfine` from `numaConfineIfSmall` (Task 2) |
| §12.3 per-node arena streams / heapArena.node / grow(node) / PREFERRED at sysMap / scavenger survival / tight-VA fallback | Task 8 (incl. `sysAlloc` hintList identity fix, build-tagged `numaMaxHeapNodes`) |
| §12.4 soft affinity, fires on node change | Task 10 (hook in `schedule()`, not `acquirep` — lock-safety) |
| §12.4 stand-down rule (narrowed mask ⇒ operator wins) | `numaShouldConfine` (Task 2), Task 10 interface, tested in `TestNUMAConfineSkipsNarrowedAffinity` |
| §12.4 pinned-first validation order | Task 11 Step 1 |
| §12.4 refill metrics `/numa/span-refills:*` | Task 9 |
| §12.4 getcpu vDSO/rseq | Task 14 (profile-gated) |
| §12.4 off-binary / instruction gates | Task 4 Gate 8 (non-test canary); Task 11 Step 2 |
| Balancer-exemption mechanism recorded | Locked decision 1 (task policy + VMA-own policy; `vma_policy_mof` fallback semantics, background §6.2) |
| BIND-all OOM footgun avoided | Locked decision 1 (PREFERRED); Global Constraints |
| Layer 2 removal | Task 6 (contingent on Task 4 **hard** gates) |
| Harness/corrections lessons (single-session, rotating, benchstat, ICC, MDE, no diff-of-significance, no one-sided stopping, non-inferiority CIs, pre-registration) | Global Constraints; Task 0; Task 4 Gates 3/6/7; Task 12 Step 1 |

Self-review re-run after the review-findings revision (all 4 Critical, 9 Important, 12 Minor applied): spec-coverage table above re-checked against §12.2–§12.4 line by line; placeholder scan clean (no TODO/TBD/`...`-as-content outside deliberately-compressed Workstream B/C step lists — the one broken snippet introduced mid-edit was caught and replaced); type/state consistency pass across snippets (`numaCPUMaskBytes` shared const; `numaSetThreadAffinity(tid int32, *[numaCPUMaskBytes]byte) bool` used identically in Tasks 1–3 with no nosplit; `numaConfined`/`numaStoodDown` consistently `atomic.Bool` incl. the test export; per-M state consistently accessed via `mNUMAState` accessors; `numaNodemask`/`numaMaxNode=65` conventions preserved; testprog probe output format matches `parsePlacement`; strace expectations consistent with decision 5's still-active arena mbind).

---

## What done means

1. **Task 0:** plan + `pathology-sweep.sh` committed **before** any gate ran (pre-registration git-verifiable).
2. **Workstream A merged** with: confinement + stand-down tests green on numa-dell; 1P json ≤ +2%; 1P alloc micro ≤ +2%; 256P json unchanged (3-session pooled n=30, pre-declared) with three-arm strace proof (never-fired at 256P, correct confined shape, correct stand-down shape); vmstat 0/0 on confined arms; candidate 1 C significantly better than B **and** C-vs-A non-inferior (95% CI upper bound of C/A ≤ +10% — same criterion as Gate 6; "within noise" is not a claim this design can make at n=15); candidate 2 same shape (~25% gap, MDE ≈5.6% stated with any null); off-binary census clean on a non-test canary; all raws + driver scripts archived under `numa-design/bench-data/wsA-*/` in the results commit.
3. **Layer 2 PREFERRED-at-grow removed** (Task 6, contingent on the hard gates only) with a passing 1P re-gate — the upstream series carries BIND-all + confinement only.
4. **Workstream B** either: merged as one unit with pinned routing proof ≥95% local, all hard gates, and the **IMC ≥10% relative remote-share drop** that killed Layer 2 — or explicitly not started / killed in a RESULTS.md decision record with numbers (Task 7 / Task 11).
5. **Workstream C:** phase-shift attribution question answered with a pre-registered analysis; getcpu asm landed (or per-arch blockers recorded); rseq/vDSO note written and its adoption gate recorded; cpuset staleness documented.
6. Experiment off, single-node, or operator-narrowed affinity: bit-identical to stock (census-proven), at every merge point.
7. `kernel.numa_balancing=1` confirmed unchanged at the end of every numa-dell session; no gate skipped, no gate result claimed without its raw data in the same commit.
