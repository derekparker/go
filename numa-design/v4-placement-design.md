# v4 Stage 2 — P/goroutine node placement: design

Status: REVIEWED — APPROVED-WITH-CHANGES applied (verdict at bottom; C1/M1/M2/m1–m4
folded into the sections below).
Scope: locked decisions P1–P7 in `2026-08-26-numa-v4-placement-plan.md`; this doc
turns them into code-level design. Line references are to the tree at `873dc29245`.

## Problem recap (one paragraph)

WS-B homes heap memory per node and routes mcentral refills by getcpu, and it is
provably correct when pinned (100%/99.54% local). Unpinned it fails its payoff gate
because the *consumer* moves: 25–47% of refills are remote at every GOMAXPROCS, and
even local-at-refill spans go remote when the kernel migrates the consuming thread.
getcpu-keyed soft affinity (4ms throttle) sticks Ms to whatever node they were last
*observed* on — it stabilizes threads but does not partition the process, so both
nodes' worth of Ms converge onto whatever the kernel happened to do. Stage 2 replaces
the observation key with an *assignment*: each P gets a home node; the M running a P
converges to that P's node; refills and growth are keyed by the same assignment.
One mechanism, three consumers, all behind one predicate.

## 1. Per-P state: `pNUMAState`

New files `src/runtime/numa_pstate_on.go` / `numa_pstate_off.go`, exactly mirroring
the `mNUMAState` pattern (on: real struct; off: `struct{}` + no-op methods so the
off build is byte-identical; both compile on every GOOS).

```go
//go:build goexperiment.numa

package runtime

// pNUMAState is the per-P NUMA placement state, embedded as p.numa
// (runtime2.go). homeNode stores (node id + 1); 0 (the zero value) means
// "no home assigned" — placement inactive, or this P not yet assigned by
// procresize. Same +1 zero-value idiom as mNUMAState.lastNode.
//
// Written only by numaAssignPHomes (procresize: sched.lock held, world
// stopped). Read without synchronization from schedule()/stealWork/
// numaGrowNode — safe because writes happen only while the world is
// stopped and readers run only in a started world.
type pNUMAState struct {
	homeNode int8
}

func (s *pNUMAState) home() (node int8, ok bool) {
	if s.homeNode == 0 {
		return 0, false
	}
	return s.homeNode - 1, true
}
func (s *pNUMAState) setHome(node int8) { s.homeNode = node + 1 }
func (s *pNUMAState) clearHome()        { s.homeNode = 0 }
```

Embed point: in `p` in `runtime2.go`, **immediately before `xRegs` — never as the
last field** (review m2: a zero-size field placed last triggers trailing-zero-size
padding and grows the struct in the off build; `m.numa` is deliberately placed
before `self` for exactly this reason, and `xRegs` is itself zero-size on some
platforms so `p.numa` must not be the final field either). Off-build p size
unchanged — the census gate covers function-level identity only, so the
implementation adds an experiment-on size/offset guard test analogous to the
m-side check (review m2).

## 2. Engagement predicate: `numaPlacementActive`

`numa_linux.go` (plus stubs in `stubs_nonlinux.go` — review m4: not just
`numaPlacementActive() bool { return false }` but also a no-op
`numaAssignPHomes(nprocs int32)` (procresize compiles on every GOOS) and a no-op
`numaPlacementInit()` (the schedinit eligibility computation — the real one lives
in Linux-only `numa_linux.go` while schedinit is portable, so the call goes
through a named function with a stub, matching the `numaConfineIfSmall` pattern):

```go
// numaPlacementEligible is computed ONCE, in schedinit, immediately after
// numaConfineIfSmall has made the confinement decision (single-threaded,
// m0 only — same window as every other numa startup decision). It is the
// startup half of numaPlacementActive.
var numaPlacementEligible bool

// numaPlacementActive reports whether P-home placement is consumed
// ANYWHERE. Every consumer (numaNoteSchedule, stealWork, numaGrowNode,
// numaAssignPHomes) checks THIS predicate — never p.numa.home() alone.
// That is the pairing rule from the v4 plan's forbidden-list amendment:
// assignment may only be consumed while enforcement is active, and both
// are gated here. The numaStoodDown load makes stand-down disable
// placement everywhere at once even though P home fields remain set
// (they are never consumed once this returns false; the next procresize
// clears them).
func numaPlacementActive() bool {
	return numaPlacementEligible && !numaStoodDown.Load()
}
```

Eligibility (computed in schedinit, in this order, mirroring
`numaShouldConfine`'s decline diagnostics under GODEBUG=numa=1):
experiment on (call site DCE) AND `numaTopology.NumNodes >= 2` AND
`!numaTopology.TruncatedNodes` AND `numaHeapStreamsEnabled` (placement's payoff is
routing+homing; without streams there is nothing to key) AND **every CPU-bearing
node id < `numaMaxHeapNodes`** (review C1: the per-node spanSet/arenaHints/curArena
arrays are sized `numaMaxHeapNodes`=8; a CPU-bearing node id in [8, 64) —
sparse/CXL ids, >8-node boxes, the exact case the I5 comment near
`numa_linux.go:866` warns about — must make the whole feature decline, or Ps homed
to it would index out of bounds at every refill/grow) AND thread affinity
available on this arch AND startup affinity spans all online CPUs (operator
placement wins — reuse `numaStartupFullAffinity`) AND `!numaConfined.Load()`.

Immediately after computing eligibility, schedinit calls
`numaAssignPHomes(procs)` itself (review M1): the bootstrap `procresize`
(`proc.go:951`) runs BEFORE the confinement decision (`proc.go:960`), so waiting
for "the next procresize" would leave every P home unassigned until the first STW
(first GC or GOMAXPROCS call) — exactly the ramp-up phase in which the heap gets
laid out and homed. Single-threaded m0-only window, same invariant as every other
numa startup step.

Notes:
- Confined process ⇒ ineligible (mutually exclusive by construction; the confined
  process is single-node, placement is moot).
- A process that starts confined and later stands down does NOT gain placement
  (numaStoodDown is in the predicate). One-way latch semantics identical to soft
  affinity's. Moreover (review m1/A): because `numaStandDownIfNeeded` returns
  false unless `numaConfined` — and a confined process is permanently
  placement-ineligible — **numaStoodDown can never latch in a placement-eligible
  process**. The predicate's numaStoodDown term is defense-in-depth, not a live
  code path. Post-stand-down regime is explicitly out of scope for v4.
- `nprocs <= one node's CPUs` does NOT disable placement: the partition is
  proportional regardless (a 64-P process on 2×128 gets 32/32). Rationale
  (probe area 5, closed by review G): this case arises mainly with
  `customGOMAXPROCS == false` (cgroup-quota-derived defaults), where v3's locked
  decision 6 declines confinement precisely because sysmon's `defaultGOMAXPROCS`
  recompute reads the affinity mask; grouping all Ps on one node would narrow
  every M to that node and recreate the same feedback surface without
  confinement's explicit-GOMAXPROCS opt-in. Proportional is the conservative
  default.

## 3. Assignment: `numaAssignPHomes` in procresize

Call site: `procresize` (`proc.go:6198`), immediately after the "initialize new P's"
loop (`proc.go:6241-6248`), before any P is released — `sched.lock` held, world
stopped, so plain stores are race-free (see pNUMAState doc comment).

```go
// numaAssignPHomes (re)assigns contiguous home-node ranges to
// allp[:nprocs]. Called from procresize under sched.lock with the world
// stopped — including on every GOMAXPROCS change, so quotas always match
// the current nprocs. When placement is inactive it clears every home,
// so stale assignments can never be consumed after (e.g.) stand-down's
// own GOMAXPROCS-raise procresize.
func numaAssignPHomes(nprocs int32) {
	if !numaPlacementActive() {
		for i := int32(0); i < nprocs; i++ {
			allp[i].numa.clearHome()
		}
		return
	}
	// Largest-remainder proportional quotas over nodes that have CPUs,
	// in node-id order; every node with CPUs gets >=1 P while Ps remain.
	// Worked examples (numa-dell, 128 CPUs/node, 2 nodes):
	//   nprocs=256 -> 128/128;  200 -> 100/100;  3 -> 2/1;  1 -> 1/0.
	...
	// Contiguous assignment: Ps [0, q0) -> node 0, [q0, q0+q1) -> node 1, ...
}
```

Quota algorithm (integer-only, no floats in the runtime): for each node i with
`cpus[i] > 0`, `q[i] = nprocs * cpus[i] / totalCPUs`; distribute the remainder
`nprocs - Σq[i]` one P at a time to nodes in decreasing order of
`nprocs*cpus[i] % totalCPUs` (ties: lower node id first, determinism); then, while
any CPU-bearing node has `q[i] == 0` and some node has `q[j] > 1`, move one P from
the largest-quota node (the ≥1 rule). Node CPU counts come from the same topology
helper `numaShouldConfine` uses for its node-CPU threshold.

Contiguity is deliberate: it makes the assignment trivially inspectable (P id →
node is a range check), keeps same-node Ps adjacent for stealWork's enumeration,
and has no fairness cost since `stealOrder` already randomizes enumeration.

## 4. Enforcement: re-keying `numaNoteSchedule`

Call site unchanged (`proc.go:4323-4325`, after findRunnable, before execute,
mp.locks==0). The function gains a placement fast path *before* the
getcpu-throttled path; the getcpu path remains verbatim as the fallback for
placement-inactive processes (today's WS-B behavior):

```go
func numaNoteSchedule() {
	mp := getg().m
	if mp.locks != 0 {
		return // defensive, unchanged
	}
	if !numaSoftAffinityEligible() || numaConfined.Load() || numaStoodDown.Load() {
		return
	}
	if numaPlacementActive() {
		// Placement path: the key is the P's assigned home — no getcpu,
		// and in steady state (home already applied) no nanotime either:
		// two byte loads and a compare, cheaper than the throttled
		// getcpu path. nanotime + the existing nextCheck throttle guard
		// only the APPLY (review M2): numaApplySoftAffinity records
		// lastNode only on success, so an unthrottled `last != home`
		// condition would re-fire a FAILING sched_setaffinity (e.g.
		// cpuset narrowed mid-run, EINVAL — reachable under the
		// accepted-staleness model) on every schedule() pass; arming
		// nextCheck on every attempt bounds that at one syscall per 4ms
		// per M, the same envelope as the getcpu path.
		if pp := mp.p.ptr(); pp != nil {
			if home, ok := pp.numa.home(); ok {
				if last, applied := mp.numa.softAffinityNode(); applied && last == home {
					return // steady state
				}
				now := nanotime()
				if !mp.numa.softAffinityCheckDue(now) {
					return // bounded retry after a failed apply
				}
				mp.numa.armSoftAffinityCheck(now + numaSoftAffinityCheckInterval)
				numaApplySoftAffinity(mp, int32(home))
				return
			}
		}
		// P has no home (should not happen while active — schedinit and
		// every procresize assign before Ps run; review M1 defense):
		// fall through to the getcpu path rather than losing soft
		// affinity entirely.
	}
	// ... existing getcpu-keyed throttled path, unchanged ...
}
```

Cross-home churn honesty (review M2): the "stopm/park frequency" claim
undercounts — an M that loses its P at syscall exit re-enters via
`exitsyscall0 → schedule()` and can pick up a differently-homed P at syscall
frequency; each such pickup is one (throttle-bounded) syscall PLUS a forced
cross-node thread migration. G2-sched-micros will not exercise this regime (no
syscalls, no cross-home P churn); the 256P json G2-cost arm is the gate most
likely to catch it — recorded here so a G2-cost regression looks there first.

- `numaApplySoftAffinity` (mask build + `sched_setaffinity` + `setSoftAffinityNode`)
  is reused verbatim — same slow path, same GODEBUG diagnostics.
- `numaWidenBeforeClone` / fork / exec widening is UNCHANGED and applies identically:
  it clears `lastNode`/`nextCheck`, so the next schedule() pass re-applies the P
  home. The C1-class inheritance bugs stay fixed by the same mechanism.
- The `nextCheck` throttle around the apply is pre-wired (review M2 upgraded it
  from "post-hoc fallback" to required): steady state pays no nanotime at all;
  only a pending or failing apply pays nanotime, and the syscall itself is
  bounded to one per 4ms per M — the same envelope as the getcpu path.
- `numaSoftAffinityEligible`'s existing checks (multi-node, arch support, startup
  full affinity) are supersets of placement eligibility's overlapping terms — the
  placement branch adds only the predicate load.

## 5. Routing + homing: re-keying `numaGrowNode`

`numaGrowNode` (`numa_linux.go:876`) is the single node-key source for BOTH
mcentral refill routing (`numaRefillNode` forwards to it, `mcentral.go:169-171`)
and heap-growth homing. One change re-keys both consistently:

```go
func numaGrowNode() (stream int32, homed bool) {
	if !numaHeapStreamsEnabled {
		return 0, false
	}
	if numaPlacementActive() {
		// Deterministic key: the current P's assigned home. Requires no
		// syscall, and it is the node enforcement (numaNoteSchedule) is
		// converging this M to — routing memory to where the consumer
		// is KEPT, rather than where it happened to be observed.
		gp := getg()
		if gp != nil && gp.m != nil && gp.m.p != 0 {
			if home, ok := gp.m.p.ptr().numa.home(); ok && int32(home) < numaMaxHeapNodes {
				// The home < numaMaxHeapNodes check is defensive
				// (review C1): eligibility already declines when any
				// CPU-bearing node id >= numaMaxHeapNodes, but this
				// function's contract — "node is always a valid index
				// into the per-node spanSet/arenaHints/curArena
				// arrays" (mcentral.go, mheap.go) — is enforced HERE
				// for the getcpu path (the node >= numaMaxHeapNodes
				// test below) and must be enforced for the placement
				// key too, not inherited from a predicate computed
				// once at startup.
				return int32(home), true
			}
		}
		// No P (rare: allocation off-P) — fall through to getcpu.
	}
	node := numaCurrentNode()
	if node < 0 || node >= numaMaxHeapNodes {
		return 0, false
	}
	return node, true
}
```

- Removes the per-refill getcpu syscall entirely when placement is active.
- The "actual span decides local/remote" metrics ruling (review I1, mcentral.go)
  is unaffected — `/numa/span-refills/*` keeps measuring truth, now against the
  P-home key. G2-locality reads exactly this.
- nosplit/stack discipline: `numaGrowNode` is called with h.lock held in the grow
  path today; the new branch adds pointer loads only, no new locks, no allocation.

## 6. Locality-aware stealing: `stealWork` first-pass filter

`stealWork` (`proc.go:3910`) runs `stealTries = 4` passes over a randomized victim
enumeration; timers/runnext are last-pass-only. Change: pass 0 skips victims homed
on a different node; passes 1–3 are exactly today's unrestricted order.

```go
	// stealWork entry, after pp := getg().m.p.ptr():
	var stealHome int8 = -1
	if goexperiment.Numa && numaPlacementActive() {
		if home, ok := pp.numa.home(); ok {
			stealHome = home
		}
	}
	...
	// inner loop, immediately after `if pp == p2 { continue }`:
	if goexperiment.Numa && i == 0 && stealHome >= 0 {
		if vHome, ok := p2.numa.home(); ok && vHome != stealHome {
			continue // pass 0: same-node victims only
		}
	}
```

Work conservation argument: pass 0 only *skips* victims — it never blocks, spins,
or adds tries; cross-node victims are reachable in the same `stealWork` invocation
on pass 1 (and 2, 3). The timer-steal pass (`stealTimersOrRunNextG`, i==3) is
untouched, so cross-P timer handling is byte-for-byte today's. Worst case added
latency: one filtered enumeration (≤ nprocs pointer loads + byte compares) before
the first unrestricted pass — no syscalls, no locks. `gcwaiting` is checked inside
the enumeration exactly as today, so GC preemption latency is unchanged.
The predicate is hoisted to one load per `stealWork` call (not per victim).
Net semantic delta, stated for reviewers (review B): a cross-node victim gets 3
unrestricted probes instead of today's 4 — still work-conserving. Victims with no
assigned home are never skipped.

`runqsteal`/`checkRunqsNoP`/globrunq are untouched (P6 scope control).

## 7. Races and lifecycle (P7 probe areas, argued)

1. **Assignment vs in-flight refills:** none — `numaAssignPHomes` runs only inside
   procresize (world stopped; no M is inside cacheSpan/grow). After restart,
   readers see the new homes (happens-before via world restart).
2. **Reassignment leaves memory homed under the old partition:** stale but safe.
   Per-node spanSets are keyed by the SPAN's actual home (`numaArenaNode(s.base())`),
   never by P — a span homed to node 1 sits in node 1's set regardless of any P
   reassignment. A P whose home changed simply starts refilling from its new node's
   sets; old-node spans are found by the existing remote fallback (HWM-bounded) and
   counted honestly as remote by the I1 metrics rule. No invariant anywhere assumes
   "spans a P allocated are homed to that P's node".
3. **Stand-down: unreachable for a placement-eligible process (review m1 — the
   primary guarantee is mutual exclusivity, not ordering).** `numaStandDownIfNeeded`
   returns false unless `numaConfined` is set; `numaConfined` is set only by
   `numaConfine` ← `numaConfineIfSmall` ← schedinit (its sole caller); and
   placement eligibility requires `!numaConfined` at that same schedinit point.
   So `numaStoodDown` can never latch in a process where placement is eligible —
   there is no ordering race to argue about. (For the record, the ordering is
   also NOT airtight on its own: `numaStoodDown.Store(true)` in
   `numaStandDownIfNeeded` runs after `sched.lock` is released in
   `startTheWorldWithSema`, and a syscall-exiting M can acquire an idle P and run
   consumers in that window — which is why the mutual-exclusivity argument, not
   the ordering, carries the guarantee.) The predicate's `numaStoodDown` term and
   `numaAssignPHomes`'s clear-on-inactive branch remain as defense-in-depth.
4. **P id 0 special cases:** procresize's own M continues on its current P or
   acquires allp[0]; assignment covers allp[:nprocs] uniformly before release, so
   there is no window where a released P has a stale home from a previous, larger
   partition (clearHome on the inactive branch covers shrink + stand-down; shrink
   with placement active reassigns all surviving Ps).
5. **nprocs ≤ node CPUs** (see §2 note): proportional split stands; review may
   overrule toward "group all Ps on one node when they fit" — if so, that node is
   the boot node (consistency with confinement's decision 2) and the change is
   confined to the quota function.

## 8. Diagnostics

- `GODEBUG=numa=1`: `numaAssignPHomes` prints the partition once per procresize
  (`numa: P homes: node0=[0,128) node1=[128,256)` style, bounded to 8 nodes);
  decline reasons print via the existing `numaConfineDeclined`-style helper.
- Existing `/numa/span-refills/{local,remote}:spans` metrics are the in-vivo
  locality proxy for G2-locality — unchanged semantics, now keyed by P home.

## 9. Test list (implementation tasks carry the code)

Unit (any Linux, no hardware dependency):
- `TestNUMAPlacementQuota` — table-driven over (nprocs, per-node CPU counts) →
  expected contiguous ranges: (256, [128,128])→128/128; (200,[128,128])→100/100;
  (3,[128,128])→2/1 (tie → lower id); (1,[128,128])→1/0; (5,[64,128])→2/3
  (largest remainder); (4,[1000,1,1,1])→1/1/1/1 (the ≥1 rule genuinely fires:
  remainder pass alone gives 4/0/0/0 — review m3's replacement for the bogus
  (2,[128,128,128,128]) case, where largest-remainder already yields 1/1/0/0 and
  the rule is a no-op); (2,[128,128,128,128])→1/1/0/0 kept as a
  rule-must-NOT-fire case. Products `nprocs*cpus[i]` computed in int64 (m3).
  Via export_test hook calling the quota function directly (pure function of
  inputs — no world manipulation).
- `TestNUMAPlacementActivePredicate` — eligibility combos via export hooks
  (confined ⇒ false; stood-down latch ⇒ false; streams disabled ⇒ false).
- `TestNUMAPlacementProcresize` — GOMAXPROCS churn (1→256→2→128) in-process,
  assert homes recomputed and contiguous each time; run under `-race` in the
  existing race gate.

Hardware (numa-dell, skip guards like existing TestNUMA*):
- `testprog/numa.go` new probe `NUMAPlacementSpread`: multi-node + GOMAXPROCS =
  all CPUs ⇒ after a parallel workload, every worker thread's
  `Cpus_allowed_list` equals exactly one node's CPU list, and both nodes are
  represented (the anti-collapse assertion — the C1-regression shape).
- Refill locality: extend the existing metrics-based test to assert unpinned
  local share ≥ 90% on numa-dell under a parallel allocation workload (the
  in-tree analog of G2-locality; generous threshold vs the gate's own sweep).

Gates: G2-* as pre-registered in the plan (primary: pathology garbage 256P ≥5%;
locality ≥90% at {2,8,32,128,256}; IMC ≥10% relative; sched micros ≤+2%; standing
hard gates incl. 1P alloc micro vs stock and 256P json user+sys under the Task-1
noprof harness).

## 10. What this deliberately does not do (P6)

No GC mark-worker placement, no netpoll/timer wakeup preference, no global-runq
sharding, no g-struct fields, no mcache flush, no per-malloc anything, no new
syscalls in scheduler paths (placement strictly REMOVES the refill getcpu and
makes the schedule()-path syscall rarer).

---

## Review verdict

Adversarial review (fresh reviewer, self-contained prompt, probe areas A–H per
plan P7), 2026-08-26: **APPROVED-WITH-CHANGES** — one Critical, two Major, four
Minor, all applied above:

- **C1 (Critical):** `numaGrowNode` placement branch dropped the
  `numaMaxHeapNodes` bound → OOB spanSet/arenaHints indexing on hosts with
  CPU-bearing node ids ≥ 8. Fixed twice over: eligibility declines when any
  CPU-bearing node id ≥ `numaMaxHeapNodes` (§2), plus a defensive bound in the
  branch (§5).
- **M1 (Major):** homes were never assigned until the first STW (bootstrap
  procresize precedes the eligibility computation in schedinit), and the
  placement branch returned early even with no home — losing both placement AND
  getcpu soft affinity during heap ramp-up. Fixed: schedinit calls
  `numaAssignPHomes` right after computing eligibility (§2); no-home falls
  through to the getcpu path (§4).
- **M2 (Major):** unthrottled placement path would retry a *failing*
  `sched_setaffinity` every schedule() pass (reachable via mid-run cpuset
  narrowing). Fixed: the existing `nextCheck` throttle is pre-wired around the
  apply; steady state still pays no nanotime (§4). Cross-home churn at syscall
  frequency (exitsyscall0 P pickup) recorded as the regime G2-cost watches (§4).
- **m1:** §7.3 rewritten around the real guarantee — stand-down requires
  confinement, confinement permanently excludes placement eligibility, so the
  latch can never fire in a placement-eligible process; the ordering argument
  alone was not airtight (consumers can run after sched.lock release, before the
  latch store).
- **m2:** embed point corrected to "immediately before `xRegs`, never last"
  (trailing zero-size padding would grow p off-build); p-size guard test added to
  the test list (the census checks functions, not struct sizes).
- **m3:** quota test table corrected — (4,[1000,1,1,1]) genuinely exercises the
  ≥1 rule; (2,[128,128,128,128]) kept as a must-not-fire case; int64 products.
- **m4:** non-Linux stubs enumerated: `numaPlacementActive`, `numaAssignPHomes`,
  `numaPlacementInit`.

Probe areas B (stealWork semantics), C (procresize lifecycle incl. destroy/
retake/checkRunqsNoP), D (numaNoteSchedule call-site validity and
widen-before-clone interplay), E (numaGrowNode caller contexts and metrics
honesty under the new key), F (quota examples recomputed), G (proportional
partition upheld, rationale recorded in §2), and the
mallocinit-before-schedinit ordering of `numaHeapStreamsEnabled` all verified
clean against the tree at 873dc29245.
