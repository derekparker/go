# NUMA Runtime v2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **Revision 2026-08-19 (plan review):** correctness fixes applied (interleave test, both `mheap.grow` sysMap sites, mbind policy-replacement semantics, init-order guard, maxnode=65, deps_test, allowed-nodes source) and concerns annotated inline as **CONCERN:** — measure them during implementation, do not delete the sections.

> **Revision 2 (2026-08-19, independent-design cross-check, design §12):** merged the three-ingredient locality rule and post-kill alternatives (Task 10), off-binary/instruction-count gates (Task 4 Step 1c), pinned-thread routing validation + refill metrics (Task 11), affinity stand-down + soft-affinity note (Task 12), scavenger and vDSO notes (Tasks 3, 6), `numa_balancing=2` (Task 7).

**Goal:** Re-implement opt-in Linux NUMA support on **latest `master`**, one gated layer at a time, so #14406 balancer exemption can ship without locality tax, and locality work is only continued if it does not slow single-P allocation.

**Architecture:** Layer 0 is topology + `GOEXPERIMENT=numa` with empty hot paths. Layer 1 is product A: process BIND-all + heap-arena BIND-all `mbind` only (no span tags, no mcache flush, no PREFERRED, no steal). Layer 2 is product B first attempt: `mbind(MPOL_PREFERRED)` on arena growth only (`getcpu` once per arena, not per malloc). Layers 3–4 (mcentral sharding, steal, Green Tea mark) are **cancelled** unless Layer 2’s IMC gate passes. Do **not** copy `numa-dev` runtime sources; they include the rejected flush.

**Tech Stack:** Go runtime (`src/runtime`, `src/internal/runtime/numa`, `src/internal/goexperiment`), Linux `getcpu`/`mbind`/`set_mempolicy`, remote host `numa-dell`, `golang.org/x/benchmarks` json, optional Sweet/Prometheus promotion.

## Global Constraints

- **Start from latest `origin/master`**, not `numa-dev`. `numa-dev` is evidence only (`numa-design/RESULTS.md`, this plan, `numa-v2-design.md`).
- **Never land a change that makes single-P allocation slower** than the parent commit: json `GOMAXPROCS=1` median **ns/op** and **user+sys-ns/op** must be ≤ baseline within **+2%** (fail if either exceeds). A vmstat 0/0 win does **not** waive this.
- The json gate compares experiment-off vs experiment-on **at the same commit**; unconditional (non-`goexperiment`-gated) costs are invisible to it. **Once per layer**, also run the same protocol against the **parent-commit** build.
- Design §1 also mandates a **1P alloc micro** beside json at every layer gate (Task 4 Step 1b), so a `mallocgc` regression cannot hide behind json's allocation mix.
- **Never** call a 136-class mcache scan from `getMCache` or `acquirep`. `numaFlushForeignCachedSpans` is forbidden.
- **Never** revive STW-only `set_mempolicy` toggles.
- **Never** steal by logical `p.numaNode` P-index on this machine (even/odd CPU interleave). Steal (Layer 4) must use physical CPU/node or pinned Ms.
- Experiment off or single-node: behavior identical to stock Go.
- Linux only; other GOOS: stubs / no-ops.
- Do not set `GODEBUG=numa=1` during performance runs (arena logs pollute and cost).
- Do not enable Sweet `diagnostics = ["perf=..."]` (`perf record` hangs on ResetTimer SIGINT).
- Heap targets on `numa-dell`: never 32 GiB; json gate uses `-benchmem=512`; garbage if used is `-benchmem=8192`.
- `GOTOOLCHAIN=local` for any Sweet/etcd build.
- **Never** add `Co-authored-by` trailers. **Never** `--no-verify`.
- If a layer gate fails: **stop**, write the failure in `numa-design/RESULTS.md`, do not start the next layer.
- Parent of the CL is the performance baseline (rebuild both binaries from the same GOROOT).
- Commits: one layer (or one task) per commit when possible; message explains **why**.

**Spec:** `numa-design/numa-v2-design.md`. **Background:** `numa-design/numa-runtime-background.md`. **#14406 policy notes:** `numa-design/bind-all-policy.md` (inheritance; do not treat as a speed waiver). **v1 autopsy:** `numa-design/RESULTS.md`.

---

## File map (what this plan creates)

| Path | Layer | Role |
|------|-------|------|
| `src/internal/goexperiment/flags.go` (+ `go generate`) | 0 | `Numa bool` |
| `src/go/build/deps_test.go` | 0 | allow `internal/runtime/numa` in the dep graph (next to `internal/runtime/cgroup`) |
| `src/internal/runtime/numa/numa.go` | 0 | `Topology`, `Node`, `MaxNodes`, `ScratchSize` |
| `src/internal/runtime/numa/numa_linux.go` | 0 | sysfs read (`/sys/devices/system/node/...`) |
| `src/internal/runtime/numa/numa_stub.go` | 0 | non-Linux: 1 node |
| `src/internal/runtime/numa/numa_linux_test.go` | 0 | parser tests (can run anywhere with fixtures) |
| `src/runtime/sys_linux_amd64.s` / `sys_linux_arm64.s` | 0 | `getcpu` if missing |
| `src/runtime/os_linux.go` | 0 | `getcpu` declaration |
| `src/runtime/numa_linux.go` | 0–2 | glue: `numaInitTopology`, later BIND / PREFERRED |
| `src/runtime/stubs_nonlinux.go` | 0–2 | no-ops |
| `src/runtime/runtime1.go` | 0 | `debug.numa` + `dbgvars` (tracing only) |
| `src/runtime/proc.go` `schedinit` | 0 | call `numaSchedinit` after debug vars, after `mallocinit` |
| `src/runtime/runtime2.go` | 0 | **do not** add `p.numaNode` / `m.numaNode` until a later layer needs them |
| `src/runtime/export_numa_test.go` | 0–1 | test hooks |
| `src/runtime/numa_linux_test.go` | 0–1 | topology / mempolicy tests |
| `src/internal/runtime/syscall/linux/defs_linux_*.go` | 1 | `SYS_MBIND` / `SET_MEMPOLICY` / `GET_MEMPOLICY` if master lacks them |
| `src/runtime/mheap.go` `mheap.grow` | 1–2 | `numaBindArena` after **both** `sysMap` call sites |
| `numa-design/gate-json.sh` | 0+ | 1P / 256P json compare |
| `numa-design/gate-vmstat.sh` | 1 | hint-fault / migration deltas |
| `Makefile` (repo root) | 0 | remote push/build/test/gate |
| `numa-design/RESULTS.md` | all | append each layer’s numbers |

**Do not create:** per-node `mcentral`, `numaStealWork`, span `numaNode` flush, `p.numaNode` assignment by P index.

---

## Environment: NUMA test machine

SSH: `local → fedorawork (VPN) → numa-dell` (`~/.ssh/config` `Host numa-dell` `ProxyJump fedorawork`). Remote tree: `/home/deparker/go-numa`. Bootstrap: `/usr/local/go-bootstrap`, `GOROOT_BOOTSTRAP` in remote `~/.bashrc`.

Hardware (verify with `make topo`): 256 CPUs, 2 nodes, **even CPUs = node 0, odd = node 1**, ~15 GiB DRAM/node, `kernel.numa_balancing=1`.

If VPN is down, stop; do not fake gates locally (laptop is not 2-node).

Layer-0 unit tests can run on any Linux; **json 1P / vmstat / IMC gates only on `numa-dell`.**

---

## Forbidden copy-paste from `numa-dev`

Do not port:

- `numaFlushForeignCachedSpans` / `getMCache` NUMA hook / `acquirep` flush
- `numaNodeHeap` per-node mcentral
- `numaStealWork` / `numaAssignPNodes`
- `mspan.numaNode` + mcache owner tracking
- PREFERRED **together with** BIND in Layer 1 (PREFERRED is Layer 2 only)
- Verbose arena logging on the alloc path

You **may** read `numa-dev` for nodemask-width handling (v2 corrects the value: pass `maxnode = numaNodemaskBits+1` = 65, never `MaxNodes+1` — see Task 6) and for sysfs parsing ideas. Re-type; do not git-merge the branch.

---

### Task 0: Workspace from master + remote Makefile + docs

**Files:**
- Create: `Makefile` (if missing on the master-based branch)
- Copy from evidence worktree into the new branch: `numa-design/numa-v2-design.md`, `numa-design/numa-runtime-background.md`, `numa-design/bind-all-policy.md`, `numa-design/RESULTS.md` (read-only history), `numa-design/gc-pause-bench/` (for Layer 1), this plan
- Modify: none of `src/` yet

**Interfaces:**
- Consumes: SSH `numa-dell`, git `origin/master`
- Produces: branch `numa-v2` (name may vary) tracking master; remote `/home/deparker/go-numa` updated to this branch

- [ ] **Step 1: Confirm you are not implementing on `numa-dev`**

```bash
git fetch origin
git checkout origin/master
git switch -c numa-v2
git log -1 --oneline
# Must NOT be a numa-dev flush/mcentral commit. src/runtime/numa_linux.go must not exist.
test ! -f src/runtime/numa_linux.go
```

Expected: `test` succeeds (file absent).

- [ ] **Step 2: Copy design docs into this branch**

From the evidence worktree (path may be `.../go/.claude/worktrees/numa-dev/numa-design/`), copy the files listed above into `numa-design/`. Commit docs only:

```bash
git add numa-design Makefile
git commit -m "$(cat <<'EOF'
numa-design: import v2 spec, evidence, and remote Makefile

Future NUMA work starts from master with gated layers; v1 numa-dev is reference only.
EOF
)"
```

- [ ] **Step 3: Write `Makefile` remote targets**

Use this content (adjust `REMOTE` if needed). `push` uses the **current branch name**, not hard-coded `numa-dev`:

```makefile
REMOTE      := numa-dell
REMOTE_DIR  := /home/deparker/go-numa
PKG         ?= runtime
RUN         ?= TestNUMA
BRANCH      := $(shell git rev-parse --abbrev-ref HEAD)

.PHONY: help push build test-numa topo shell gate-json-1p

help:
	@echo "make push | build | test-numa RUN=TestFoo | topo | shell | gate-json-1p"

push:
	git push $(REMOTE) HEAD:$(BRANCH)
	ssh $(REMOTE) "cd $(REMOTE_DIR) && git fetch && git checkout -f $(BRANCH)"

build:
	ssh $(REMOTE) "cd $(REMOTE_DIR)/src && ./make.bash"

test-numa:
	ssh $(REMOTE) "cd $(REMOTE_DIR) && GOROOT=$(REMOTE_DIR) GOEXPERIMENT=numa \
	    $(REMOTE_DIR)/bin/go test $(PKG) -run '$(RUN)' -count=1"

topo:
	ssh $(REMOTE) "lscpu | grep -E 'CPU|NUMA|Socket'; echo; numactl --hardware; sysctl kernel.numa_balancing"

shell:
	ssh -t $(REMOTE) "cd $(REMOTE_DIR) && exec \$$SHELL"

gate-json-1p:
	scp numa-design/gate-json.sh $(REMOTE):$(REMOTE_DIR)/numa-design/gate-json.sh
	ssh $(REMOTE) "chmod +x $(REMOTE_DIR)/numa-design/gate-json.sh && \
	    GOROOT=$(REMOTE_DIR) GOMAXPROCS=1 $(REMOTE_DIR)/numa-design/gate-json.sh"
```

`push` requires a **local git remote named `numa-dell`**; create it if absent:

```bash
git remote add numa-dell numa-dell:/home/deparker/go-numa
```

If remote `receive.denyCurrentBranch` is `updateInstead` and the working tree is dirty, stash on the remote first.

- [ ] **Step 4: Verify SSH and topology**

```bash
ssh -o ConnectTimeout=15 numa-dell 'echo ok; nproc; numactl --hardware | head -20'
make topo
```

Expected: 256 CPUs, 2 nodes, even/odd split, `kernel.numa_balancing` = 1.

- [ ] **Step 5: Point remote repo at this branch**

```bash
make push
make build
```

Expected: `./src/make.bash` succeeds; `~/go-numa/bin/go version` matches the new HEAD.

---

### Task 1: `GOEXPERIMENT=numa` plumbing

**Files:**
- Modify: `src/internal/goexperiment/flags.go` (add field at end of `Flags`)
- Generate: `src/internal/goexperiment/exp_numa_on.go`, `exp_numa_off.go` via `go generate`
- Check: `src/internal/buildcfg` — do **not** add Numa to default experiments

**Interfaces:**
- Consumes: existing `go generate` in `goexperiment`
- Produces: `goexperiment.Numa` bool constant; build tag `goexperiment.numa`

- [ ] **Step 1: Add the flag**

In `src/internal/goexperiment/flags.go`, append to `Flags`:

```go
	// Numa enables NUMA topology discovery and (later) memory policy.
	// Linux-only; other platforms no-op. Off by default.
	Numa bool
```

- [ ] **Step 2: Generate constants**

```bash
cd src/internal/goexperiment && go generate
git diff --stat
```

Expected: `exp_numa_on.go` / `exp_numa_off.go` appear (or update).

- [ ] **Step 3: Local compile check**

```bash
cd src && ./make.bash
```

If this machine cannot bootstrap Go, skip and use `make build` on `numa-dell` after commit.

- [ ] **Step 4: Commit**

```bash
git add src/internal/goexperiment
git commit -m "$(cat <<'EOF'
internal/goexperiment: add Numa experiment flag

Opt-in GOEXPERIMENT=numa with no runtime behavior yet.
EOF
)"
```

---

### Task 2: `internal/runtime/numa` topology types + Linux sysfs

**Files:**
- Create: `src/internal/runtime/numa/numa.go`
- Create: `src/internal/runtime/numa/numa_linux.go`
- Create: `src/internal/runtime/numa/numa_stub.go` (`//go:build !linux`)
- Test: `src/internal/runtime/numa/parse_test.go` (pure parsing; no root)

**Interfaces:**
- Consumes: `internal/runtime/syscall/linux` `Open`/`Read`/`Close` (same as `internal/runtime/cgroup`)
- Produces:

```go
package numa

const MaxNodes = 64
const ScratchSize = 8192

type Node struct {
	ID      int32
	NumCPUs int32
}

type Topology struct {
	NumNodes        int32
	NumAllowedNodes int32
	Nodes           [MaxNodes]Node
	Distance        [MaxNodes][MaxNodes]uint8
	CPUToNode       [8192]int8 // CPU id → node id; -1 unknown
}

func ReadTopology(t *Topology, scratch []byte) error // fills t in place; Topology is ~13 KiB — never return it by value
func (t *Topology) NodeOfCPU(cpu int) int32
func (t *Topology) NodeAllowed(node int32) bool
func (t *Topology) AllowedNode(i int32) int32 // i in [0, NumAllowedNodes)
```

Notes (from plan review):

- **Allowed nodes must include CPU-less memory nodes** (CXL/HBM). "Nodes with memory + CPUs" would silently shrink the BIND-all mask and cut usable RAM. Layer 0: treat every online node as allowed. Layer 1 replaces this with one syscall — `get_mempolicy` with `MPOL_F_MEMS_ALLOWED` returns the kernel's own allowed mask (Task 6) — prefer that over any cpuset parsing.
- Errors: this package compiles as runtime code — return preallocated sentinel errors (or booleans), never `fmt.Errorf`.
- **CONCERN (simplicity):** `Distance` has no consumer until Layer 3, which is presumptively cancelled. Keep the field if removing it churns the API, but deferring distance *parsing* until a layer needs it is simpler.
- `CPUToNode` is diagnostic/test-only: `getcpu` returns the node directly, so no hot path ever does a CPU→node lookup. The flat array is the right boring structure; just don't grow its role.
- Parsers must bounds-check: skip node IDs ≥ `MaxNodes` and CPU IDs ≥ 8192 (never write out of range); if `scratch` is too small for a sysfs file, fail cleanly (a huge non-contiguous cpulist can exceed 8 KiB).

- [ ] **Step 1: Write failing parser tests**

Create `src/internal/runtime/numa/parse.go` with `ParseNodeList`, `ParseCPUList`, `ParseDistance`, split from I/O — **required** so tests do not need sysfs. Parsers fill **caller-provided buffers** and return a count — no `make`, which keeps the "scratch + stack only" rule consistent for runtime and test callers alike (the original slice-returning API contradicted it):

```go
func ParseNodeList(dst []int32, data []byte) (int, error) // "0-1\n" → dst[0]=0, dst[1]=1, n=2
func ParseCPUList(dst []int32, data []byte) (int, error)
func ParseDistance(dst []uint8, data []byte) (int, error)
```

```go
func TestParseNodeList(t *testing.T) {
	var buf [MaxNodes]int32
	n, err := ParseNodeList(buf[:], []byte("0-1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || buf[0] != 0 || buf[1] != 1 {
		t.Fatalf("n=%d buf=%v", n, buf[:n])
	}
}

func TestParseCPUListInterleave(t *testing.T) {
	// Shape of numa-dell node0: even CPUs. Use a short fixture.
	var buf [16]int32
	n, err := ParseCPUList(buf[:], []byte("0,2,4,6\n"))
	if err != nil || n != 4 || buf[1] != 2 {
		t.Fatalf("%v %v", buf[:n], err)
	}
}
```

- [ ] **Step 2: Run tests (expect fail)**

```bash
cd src && go test internal/runtime/numa -count=1
```

(Std packages are tested from within `src/` with the tree's toolchain; a `./src/...` pattern from the repo root is not a valid module context.)

Expected: fail (undefined `ParseNodeList`).

- [ ] **Step 3: Implement parsers + `ReadTopology`**

Read `/sys/devices/system/node/online`, each `nodeN/cpulist`, `nodeN/distance`. Intersect with `/sys/devices/system/node/possible` if needed. Fill `CPUToNode[cpu]=nodeID`. `NumAllowedNodes`: for Layer 0 **treat all online nodes as allowed** — including nodes with memory but no CPUs (see Notes above); Layer 1 replaces this with the `get_mempolicy(MPOL_F_MEMS_ALLOWED)` mask (Task 6). Do not parse cpusets. Use only `scratch` + stack; no `make`.

Stub `ReadTopology` on `!linux`: `NumNodes=1`, `NumAllowedNodes=1`.

- [ ] **Step 4: Tests pass**

```bash
cd src && go test internal/runtime/numa -count=1
```

- [ ] **Step 4b: Register the package in the dependency graph**

Add `internal/runtime/numa` to `src/go/build/deps_test.go`, next to `internal/runtime/cgroup` in the group that feeds `runtime`. `all.bash` fails without this.

- [ ] **Step 5: Commit**

```bash
git add src/internal/runtime/numa src/go/build/deps_test.go
git commit -m "$(cat <<'EOF'
internal/runtime/numa: add topology types and sysfs reader

Layer 0: discover nodes without changing the allocator or scheduler.
EOF
)"
```

---

### Task 3: Runtime glue — `getcpu`, `numaSchedinit`, GODEBUG tracing, tests

**Files:**
- Modify: `src/runtime/os_linux.go` — declare `func getcpu(cpu, node *uint32) int32`
- Modify: `src/runtime/sys_linux_amd64.s` — `SYS_getcpu` 309 wrapper if missing (arm64: 168)
- Create: `src/runtime/numa_linux.go` — `numaSchedinit`, `numaInitTopology`, `numaCurrentNode`
- Create: `src/runtime/stubs_nonlinux.go` — empty `numaSchedinit`
- Modify: `src/runtime/runtime1.go` — `debug.numa int32` and dbgvars `{name: "numa", value: &debug.numa}` (alphabetical)
- Modify: `src/runtime/proc.go` `schedinit` — after `finishDebugVarsSetup()`, `numaSchedinit()`
- Create: `src/runtime/export_numa_test.go`
- Create: `src/runtime/numa_linux_test.go` with `//go:build linux && goexperiment.numa`

**Interfaces:**
- Consumes: `numa.ReadTopology`
- Produces:

```go
func numaSchedinit()
func numaCurrentNode() int32 // getcpu; -1 on failure; 0 if experiment off or 1 node
func NumaNumNodes() int32    // test export
```

**Must not:** touch `getMCache`, `stealWork`, `mspan`, `mheap.grow`.

- [ ] **Step 1: Write failing tests**

`src/runtime/export_numa_test.go` — **must carry `//go:build linux`**: `numaTopology` lives in `numa_linux.go`, so an unconstrained export file breaks every non-Linux build of the runtime test archive:

```go
//go:build linux

package runtime

func NumaNumNodes() int32 { return numaTopology.NumNodes }
func NumaNumAllowedNodes() int32 { return numaTopology.NumAllowedNodes }
func NumaNodeOfCPU(cpu int) int32 { return numaTopology.NodeOfCPU(cpu) }
func NumaCurrentNodeForTest() int32 { return numaCurrentNode() }
```

`src/runtime/numa_linux_test.go`:

```go
//go:build linux && goexperiment.numa

package runtime_test

func TestNUMATopologyDiscovery(t *testing.T) {
	n := runtime.NumaNumNodes()
	if n < 1 {
		t.Fatalf("NumNodes=%d", n)
	}
	if runtime.NumaNumAllowedNodes() < 1 {
		t.Fatal("no allowed nodes")
	}
	// Portable across CPU numbering schemes. numa-dell interleaves
	// even/odd, but block numbering (0-63 = node 0, 64-127 = node 1)
	// is the common case — asserting NodeOfCPU(0) != NodeOfCPU(1)
	// would fail on most real 2-node machines. Instead assert the
	// CPU map spans >= 2 distinct nodes when NumNodes >= 2.
	if n >= 2 {
		seen := map[int32]bool{}
		for cpu := 0; cpu < 8192; cpu++ {
			if nd := runtime.NumaNodeOfCPU(cpu); nd >= 0 {
				seen[nd] = true
			}
		}
		if len(seen) < 2 {
			t.Fatalf("NumNodes=%d but CPUs map to %d node(s)", n, len(seen))
		}
	}
}

func TestNUMAGetcpu(t *testing.T) {
	node := runtime.NumaCurrentNodeForTest()
	if node < 0 {
		t.Fatal("getcpu failed")
	}
}
```

Use `import "runtime"` and exported names only; or `package runtime` tests if that matches neighboring files. Prefer `package runtime` + export helpers to avoid exporting widely.

- [ ] **Step 2: Implement `getcpu` assembly**

`sys_linux_amd64.s` pattern:

```
#define SYS_getcpu 309
TEXT runtime·getcpu(SB),NOSPLIT,$0-24
	MOVQ	cpu+0(FP), DI
	MOVQ	node+8(FP), SI
	MOVQ	$0, DX
	MOVL	$SYS_getcpu, AX
	SYSCALL
	MOVL	AX, ret+16(FP)
	RET
```

(Adjust FP offsets to match `func getcpu(cpu, node *uint32) int32`.)

Prefer the vDSO where trivially available: amd64 exposes `__vdso_getcpu` and the runtime already has vDSO lookup plumbing (`vdso_linux_amd64.go`); keep the raw syscall as the arm64 path and fallback. At Layer 0–2 call frequency (init + heap growth) the raw syscall alone is also fine — do not block on this (design §12.4).

- [ ] **Step 3: `numaSchedinit`**

```go
func numaSchedinit() {
	if !goexperiment.Numa {
		return
	}
	numaInitTopology()
	if debug.numa > 0 {
		println("numa: nodes", numaTopology.NumNodes, "allowed", numaTopology.NumAllowedNodes)
	}
}
```

`numaInitTopology` fills `var numaTopology numa.Topology` and `var numaScratch [numa.ScratchSize]byte`.

- [ ] **Step 4: Run tests on numa-dell**

```bash
make push && make build
make test-numa PKG=runtime RUN='TestNUMATopologyDiscovery|TestNUMAGetcpu'
```

Expected: PASS. On a 1-node Linux laptop with experiment on, topology test should skip the interleave assertion when `NumNodes==1`.

- [ ] **Step 5: Commit**

```bash
git commit -m "$(cat <<'EOF'
runtime: discover NUMA topology under GOEXPERIMENT=numa

Layer 0: sysfs + getcpu + GODEBUG=numa tracing. Allocator unchanged.
EOF
)"
```

---

### Task 4: `gate-json.sh` — Layer invariant harness

**Files:**
- Create: `numa-design/gate-json.sh`

**Interfaces:**
- Consumes: `$GOROOT/bin/go`, json via `go install golang.org/x/benchmarks/json@latest`
- Produces: exit 0 if numa 1P (or 256P) medians ≤ baseline+2%; prints table

- [ ] **Step 1: Write the script**

```bash
#!/usr/bin/env bash
# Compare json baseline vs GOEXPERIMENT=numa. Fail if numa is >2% slower
# on ns/op OR user+sys-ns/op (median of NUM interleaved runs).
set -euo pipefail
GOROOT="${GOROOT:-$PWD}"
export PATH="$GOROOT/bin:$PATH" GOTOOLCHAIN=local
PROCS="${GOMAXPROCS:-1}"
MEM="${BENCHMEM:-512}"
NUM="${BENCHNUM:-3}"
TIME="${BENCHTIME:-3s}"
OUT="${OUT:-/tmp/numa-gate-json}"
BAND="${BAND:-0.02}"
# Pin x/benchmarks (record the resolved version in RESULTS.md);
# @latest is a moving target and a network dependency per run.
JSON_PKG="golang.org/x/benchmarks/json@${BENCH_REV:-latest}"

mkdir -p "$OUT/baseline" "$OUT/numa"
export GOROOT
GOBIN="$OUT/baseline" go install "$JSON_PKG"
GOBIN="$OUT/numa" GOEXPERIMENT=numa go install "$JSON_PKG"

run() {
	local bin="$1" file="$2"
	env GOMAXPROCS="$PROCS" "$bin" -benchmem="$MEM" -benchnum=1 -benchtime="$TIME" >>"$file"
}

: >"$OUT/baseline.out"; : >"$OUT/numa.out"
# Interleave the arms (A/B, A/B, ...) so thermal / frequency / cache
# drift hits both equally instead of biasing the second arm.
for _ in $(seq "$NUM"); do
	run "$OUT/baseline/json" "$OUT/baseline.out"
	run "$OUT/numa/json" "$OUT/numa.out"
done
echo "=== baseline ==="; cat "$OUT/baseline.out"
echo "=== numa ==="; cat "$OUT/numa.out"

python3 - "$OUT/baseline.out" "$OUT/numa.out" "$BAND" <<'PY'
import re, sys, statistics
NUMV = r"(\d+(?:\.\d+)?)"  # benchfmt values are not always integers
def metrics(path):
    ns, usys = [], []
    for line in open(path):
        if not line.startswith("Benchmark"):
            continue
        m = re.search(r"Benchmark\S+\s+\d+\s+" + NUMV + r"\s+ns/op", line)
        if m:
            ns.append(float(m.group(1)))
        u = re.search(NUMV + r"\s+user\+sys-ns/op", line)
        if u:
            usys.append(float(u.group(1)))
    return ns, usys

band = float(sys.argv[3])
bns, bus = metrics(sys.argv[1])
nns, nus = metrics(sys.argv[2])
if not bns or not nns:
    sys.exit("missing Benchmark lines")
def chk(name, b, n):
    mb, mn = statistics.median(b), statistics.median(n)
    rel = (mn - mb) / mb
    print(f"{name}: baseline {mb} numa {mn} rel {rel:+.1%}")
    if rel > band:
        print(f"FAIL {name} exceeds +{band:.0%}")
        return 1
    return 0
rc = chk("ns/op", bns, nns)
if bus and nus:
    rc |= chk("user+sys-ns/op", bus, nus)
sys.exit(rc)
PY
```

Notes:

- Better than the hand-rolled comparator: `benchstat baseline.out numa.out` (Mann–Whitney U significance). Prefer it for any decision near the band.
- **CONCERN:** a median of `NUM=3` against a 2% band is noise-level. For any PASS/FAIL that lands within ±1% of the band, rerun with `BENCHNUM=10` before deciding.
- This script compares off-vs-on at the **same commit**. Per Global Constraints, once per layer run the same protocol against the parent-commit build to catch unconditional costs.

- [ ] **Step 1b: 1P alloc micro (design §1's second gate — do not skip)**

```bash
# On numa-dell, same GOROOT, off vs on:
GOMAXPROCS=1 go test runtime -run=NONE -bench='Malloc(8|16|Types)' -count=10 >/tmp/alloc-base.out
GOMAXPROCS=1 GOEXPERIMENT=numa go test runtime -run=NONE -bench='Malloc(8|16|Types)' -count=10 >/tmp/alloc-numa.out
benchstat /tmp/alloc-base.out /tmp/alloc-numa.out   # same +2% bar
```

Run this beside `gate-json-1p` at every layer gate (Tasks 5, 7, 9).

- [ ] **Step 1c: Off-binary identity + instruction-count gate (design §12.4)**

Two cheap, strong checks to run at each layer gate:

1. **Off = stock, proven:** build the same canary (e.g. `GOEXPERIMENT= go test -c runtime`) from the **parent commit** and from HEAD, both without the experiment; `objdump -d` output must differ only in build IDs. This turns "bit-identical when off" from an assertion into a gate and catches unconditional code changes the same-commit json gate cannot see.
2. **1P instruction flatness:** run a fixed-work `GOMAXPROCS=1` alloc loop under `perf stat -e instructions`, experiment off vs on. The instruction count must be exactly flat — achievable because no fast-path code changes, and immune to timing noise (a stronger claim than the ±2% band).

- [ ] **Step 2: Commit the script**

```bash
chmod +x numa-design/gate-json.sh
git add numa-design/gate-json.sh Makefile
git commit -m "$(cat <<'EOF'
numa-design: add json 1P/256P performance gate script

Fails the layer if GOEXPERIMENT=numa is more than 2% slower than stock.
EOF
)"
```

---

### Task 5: Layer 0 remote performance gate

**Files:** none in `src/` (measurement only)

- [ ] **Step 1: Push, rebuild, run 1P json gate vs this HEAD**

```bash
make push && make build
make gate-json-1p
```

Expected: **PASS** (Layer 0 must be noise). If FAIL, Layer 0 put work on the alloc path — find it (tracing, accidental `getcpu` in malloc) and fix before continuing.

Also run the 1P alloc micro (Task 4 Step 1b) and, per Global Constraints, one parent-commit comparison for this layer.

- [ ] **Step 2: Record in RESULTS.md**

Append a “Layer 0” subsection: commit SHA, `go version`, medians, PASS.

- [ ] **Step 3: Optional 256P span check** (not a fail for Layer 0 except huge regressions)

```bash
ssh numa-dell 'cd /home/deparker/go-numa && GOROOT=$PWD GOMAXPROCS=256 numa-design/gate-json.sh'
```

If 256P is >2% slower, still **stop** — topology init must not tax json.

- [ ] **Step 4: Commit RESULTS only if you edited it**

---

### Task 6: Layer 1 — BIND-all task policy + arena BIND-all (no PREFERRED)

**Files:**
- Modify: `src/internal/runtime/syscall/linux/defs_linux_amd64.go` (and other GOARCH files **if constants are missing on master**). amd64:

```go
SYS_MBIND         = 237
SYS_GET_MEMPOLICY = 239
SYS_SET_MEMPOLICY = 238
```

- Modify: `src/runtime/numa_linux.go` — `numaSetProcessBindAll`, `numaBindArena` (**BIND-all only**)
- Modify: `src/runtime/mheap.go` — `mheap.grow` has **two** `sysMap` call sites and **both** must bind: the main transition (`sysMap(unsafe.Pointer(v), nBase-v, ...)` → `numaBindArena(unsafe.Pointer(v), nBase-v)`) **and** the leftover-`curArena` transition taken when the new reservation is discontiguous (`sysMap(unsafe.Pointer(h.curArena.base), size, ...)`). Missing the second leaves heap ranges with no VMA policy, covered only by the task policy.
- Modify: `numaSchedinit` to call `numaSetProcessBindAll()` **after** topology init (after `mallocinit` already ran in `schedinit`)

**Interfaces:**
- Consumes: `linux.Syscall6`
- Produces:

```go
func numaSetProcessBindAll()
func numaBindArena(addr unsafe.Pointer, size uintptr) // nosplit
```

`maxnode` argument to the kernel is **`numaNodemaskBits+1` (65)**, never derived from `MaxNodes`. The kernel's `get_nodes` decrements `maxnode` and copies `maxnode-1` bits, so 64 covers only nodes 0–62 and **silently drops bit 63**; 65 covers all 64 bits and still copies exactly one word (`BITS_TO_LONGS(64)=1` — no over-read; the v1 over-read came from a much larger value, not from 65).

Constants:

```go
const (
	_MPOL_BIND           = 2
	_MPOL_F_MEMS_ALLOWED = 4 // get_mempolicy flag: return the kernel's allowed-node mask
	numaNodemaskBits     = 8 * unsafe.Sizeof(uintptr(0))
)
```

Nodemask: bit `1<<physicalNodeID` for each allowed node, in one `uintptr` (Layer 1 assumes node IDs < 64, true on the test machine).

`numaBindArena` Layer 1 body: **only** `mbind(addr, size, MPOL_BIND, &allMask, numaNodemaskBits+1, 0)`. Ignore errors (deliberate; optionally count failures behind `debug.numa` for diagnosis). **No** second `MPOL_PREFERRED` call. **No** `getcpu`. **No** span fields.

**Init-order guard (required):** `mheap.grow` runs **before** `numaSchedinit` — `goargs`/`goenvs` allocate before `finishDebugVarsSetup()` in `schedinit`. `numaBindArena` must therefore no-op until topology init has published the mask; otherwise Layer 1 issues `mbind` with an empty nodemask (EINVAL, silently swallowed). The early-grown chunks then carry no VMA policy and are balancer-exempt **only via the task policy** — one more reason `numaSetProcessBindAll` is load-bearing. Optionally `numaSchedinit` may catch up by `mbind`ing already-`sysMap`'d `curArena` ranges; if skipped, record that in RESULTS.md.

`numaBindArena` is called with `h.lock` held: acceptable (plain `mbind` without `MPOL_MF_MOVE` only sets VMA policy — no page migration), but treat it as a reviewed decision and add nothing slower under that lock.

Scavenger interaction (design §12.3): VMA policies **survive** `sysUnused` (`MADV_FREE`/`MADV_DONTNEED`) — released pages that refault are re-placed under the surviving policy. No re-`mbind` is needed when scavenged ranges are reused.

`numaSetProcessBindAll`: first fill the allowed mask from the kernel itself — `get_mempolicy(nil, &allMask, numaNodemaskBits+1, nil, MPOL_F_MEMS_ALLOWED)` — which includes CPU-less memory nodes and respects cpusets with zero parsing; then `set_mempolicy(MPOL_BIND, &allMask, numaNodemaskBits+1)` once. Do **not** STW-toggle. Do **not** skip this in favor of arena mbind only unless vmstat later proves task policy unused — v1 needed both, and the init-order guard above makes early heap chunks depend on it.

**CONCERN (kernel-version behavior):** multi-node `MPOL_BIND` allocation order is kernel-dependent — old kernels fill the lowest-numbered node first (silent capacity/locality skew toward node 0); recent kernels prefer the local node. Record numa-dell's `uname -r` in RESULTS.md; document the dependence if this ships.

**CONCERN (subprocess inheritance):** task mempolicy survives `fork`+`execve` (that is how `numactl` works), so `os/exec` children inherit BIND-all until they change it themselves. User-visible side effect; flag in review (noted in `bind-all-policy.md` Inheritance).

**CONCERN (dynamic cpusets):** `cpuset.mems` can change at runtime in containers; the mask is read once and can go stale. Non-blocking for Layers 0–2; note it.

- [ ] **Step 1: Tests first**

In `numa_linux_test.go` (`linux && goexperiment.numa`), skip if `NumAllowedNodes<=1`:

```go
func TestNUMABindAllTaskPolicy(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip("not multi-node")
	}
	mode := runtime.NumaTaskMemPolicyModeForTest()
	if mode != 2 { // MPOL_BIND
		t.Fatalf("mempolicy mode=%d want BIND(2)", mode)
	}
}
```

Export `NumaTaskMemPolicyModeForTest` wrapping `get_mempolicy`.

- [ ] **Step 2: Run test (fail until implemented)**

```bash
make push && make build
make test-numa PKG=runtime RUN=TestNUMABindAllTaskPolicy
```

- [ ] **Step 3: Implement syscalls + BIND-all**

Call `numaBindArena` only from `mheap.grow`, after **each** of its two `sysMap` sites (MAP_FIXED resets VMA policy; the leftover-`curArena` site fires when the new reservation is discontiguous).

- [ ] **Step 4: Tests pass**

```bash
make test-numa PKG=runtime RUN=TestNUMABindAllTaskPolicy
```

- [ ] **Step 5: Commit**

```bash
git commit -m "$(cat <<'EOF'
runtime: BIND-all mempolicy to suppress NUMA balancer (#14406)

Layer 1: task set_mempolicy + arena mbind MPOL_BIND over all allowed
nodes. No PREFERRED, no span tags, no mcache flush.
EOF
)"
```

---

### Task 7: Layer 1 — vmstat gate + 1P/256P json

**Files:**
- Create: `numa-design/gate-vmstat.sh`
- Modify: `numa-design/gc-pause-bench/` must exist (copy from evidence if missing)

- [ ] **Step 1: vmstat helper**

```bash
#!/usr/bin/env bash
# Usage: gate-vmstat.sh snap FILE          (immediately before and after the run)
#        gate-vmstat.sh diff BEFORE AFTER  (exit 1 if either delta != 0)
set -euo pipefail
case "${1:-}" in
snap)
	grep -E '^(numa_hint_faults |numa_pages_migrated )' /proc/vmstat >"$2"
	;;
diff)
	python3 - "$2" "$3" <<'PY'
import sys
def p(path):
    d={}
    for line in open(path):
        k,v=line.split()
        d[k]=int(v)
    return d
b,a=p(sys.argv[1]),p(sys.argv[2])
h=a['numa_hint_faults']-b['numa_hint_faults']
m=a['numa_pages_migrated']-b['numa_pages_migrated']
print(f"hint_faults={h} pages_migrated={m}")
if h!=0 or m!=0:
    raise SystemExit(1)
PY
	;;
*)
	echo "usage: $0 snap FILE | diff BEFORE AFTER" >&2; exit 2
	;;
esac
```

Use `snap` immediately before and after the benchmark, then `diff`. **numa** and **membind** arms expect 0/0; baseline is allowed nonzero.

**CONCERN (system-wide counters):** `/proc/vmstat` is machine-global. With `kernel.numa_balancing=1`, *any* other process on numa-dell during the window can bump these counters, so a strict 0/0 gate can false-fail. Verify the machine is otherwise idle for the window and rerun once before declaring FAIL. There is no per-PID hint-fault counter to substitute (per-task scan stats need `CONFIG_SCHED_DEBUG` `/proc/PID/sched`).

- [ ] **Step 2: 1P json gate (hard)**

```bash
make push && make build
make gate-json-1p
```

Expected: PASS. If FAIL, **revert Layer 1** or find the alloc-path cost. Do not proceed.

Run the 1P alloc micro (Task 4 Step 1b) and one parent-commit comparison as well.

- [ ] **Step 3: 256P json gate**

```bash
ssh numa-dell 'cd /home/deparker/go-numa && GOROOT=$PWD GOMAXPROCS=256 ./numa-design/gate-json.sh'
```

Expected: PASS (+2% band). During one numa json run, sample threads:

```bash
# while json runs:
python3 -c '...'  # even vs odd processor field → both nodes occupied
```

- [ ] **Step 4: #14406 three-way vmstat**

Build `numa-design/gc-pause-bench` twice; run baseline, numa, `numactl --membind=0,1` baseline. Snap `/proc/vmstat` before/after `-n=100` (heap 4096 if RAM allows; else 2048).

Expected: numa and membind **0/0**; baseline ≫ 0.

Record `uname -r` with the results. If the kernel supports the tristate, run one numa arm with `kernel.numa_balancing=2` as well (design §12.4); restore the setting afterwards.

Alternatively wrap json `-benchnum=1 -benchmem=512 GOMAXPROCS=256` with vmstat; same 0/0 requirement for numa.

- [ ] **Step 5: Record RESULTS.md and commit**

IMC remote share: **do not fail** Layer 1 if it rises.

---

### Task 8: Layer 1 promotion (optional, not merge-blocking if 1P+vmstat passed)

**Files:** none required in runtime

- [ ] **Step 1: garbage vmstat (8 GiB)**

```bash
ssh numa-dell 'export GOROOT=/home/deparker/go-numa PATH=/home/deparker/go-numa/bin:$PATH GOTOOLCHAIN=local
mkdir -p /tmp/g
GOBIN=/tmp/g/b go install golang.org/x/benchmarks/garbage@latest
GOBIN=/tmp/g/n GOEXPERIMENT=numa go install golang.org/x/benchmarks/garbage@latest
# snap vmstat; run each -benchmem=8192 -benchnum=1; delta
'
```

Expected: numa 0/0; baseline not. Throughput is **not** a Layer 1 fail.

- [ ] **Step 2: Do not run full Sweet yet unless you want extra evidence.** If you do: `GOTOOLCHAIN=local`, `envbuild` includes it, **no** perf diagnostics, `-count 3 -run etcd,tile38,bleve-index`, one `sweet run`, two configs. Bleve N=1 and etcd p99 are **not** pass/fail.

- [ ] **Step 3: Commit RESULTS notes only**

Layer 1 is **done** when Tasks 6–7 pass. This is a **shippable CL** even if Layer 2 never happens.

---

### Task 9: Layer 2 — PREFERRED at arena growth only (product B, first attempt)

**Stop if Task 7 failed.**

**Chosen approach (do not also implement pinning or address partitioning in this task):** after the BIND-all mbind, a **second** `mbind(MPOL_PREFERRED, 1<<node)` on the same range. `node` comes from **`getcpu` in `numaBindArena` only**. Note the true granularity: `mheap.grow` grows in **palloc chunks (~4 MiB)**, not 64 MiB arenas — this fires per ~4 MiB of heap growth. Still rare relative to malloc, but say "heap growth", not "arena growth", when describing frequency. Still **no** span tags, **no** `getMCache` hook, **no** `m.numaNode` cache required (if you add `m.numaNode`, you may only set it in `mstart`, never refresh on malloc).

**Kernel semantics (get this right in review):** the second `mbind` **replaces** the VMA policy for the range (`vma_replace_policy`) — the chunk ends up PREFERRED-only. BIND and PREFERRED do **not** stack. #14406 still holds anyway, because (a) a plain-`mbind` policy carries no `MPOL_F_MOF`, so the balancer skips the VMA regardless of mode, and (b) the task policy covers every unbound VMA. Keep the BIND call first **only** as a fallback so a chunk is never left with no VMA policy when the PREFERRED call is skipped (`getcpu` failure, `node >= 64`).

**CONCERN (VMA growth):** adjacent chunks PREFERRED to different nodes cannot VMA-merge; worst case ≈ heap/4 MiB VMAs (~7.5k at 30 GiB — under the default `vm.max_map_count` 65530, but not free: kernel memory plus fault-path cost). Record `wc -l /proc/PID/maps` for the numa arm in RESULTS.md.

**Files:**
- Modify: `src/runtime/numa_linux.go` `numaBindArena` — BIND-all then PREFERRED
- Test: `TestArenaPreferredNode` — observational: more arenas on the node that grew them is **not** a strict assert on interleaved CPUs; prefer a test that `get_mempolicy`/`move_pages` is too heavy. Minimum: PREFERRED syscall is attempted (counter `numaPreferredCalls` exported for tests).

**Forbidden:** flush, mcentral sharding, steal.

- [ ] **Step 1: Test counter**

```go
func TestNUMAPreferredBindOnGrow(t *testing.T) {
	if runtime.NumaNumAllowedNodes() <= 1 {
		t.Skip()
	}
	before := runtime.NumaPreferredBindCalls()
	runtime.GC()
	var a []byte
	// ~256 MiB: crosses many 4 MiB grow chunks. The original 1<<20
	// (4 GiB + append-copy peak) is pointless on a shared ~30 GiB box.
	for i := 0; i < 1<<16; i++ {
		a = append(a, make([]byte, 4096)...)
	}
	runtime.KeepAlive(a)
	if runtime.NumaPreferredBindCalls() <= before {
		t.Fatal("expected mbind PREFERRED on heap growth")
	}
}
```

- [ ] **Step 2: Implement PREFERRED half of `numaBindArena`**

```go
const _MPOL_PREFERRED = 1

var cpu, node uint32
getcpu(&cpu, &node)
if node < 64 {
    mask := uintptr(1) << node
    linux.Syscall6(linux.SYS_MBIND, uintptr(addr), size, uintptr(_MPOL_PREFERRED),
        uintptr(unsafe.Pointer(&mask)), numaNodemaskBits+1, 0)
    atomic.Xadd(&numaPreferredCalls, 1) // numaPreferredCalls is a uint32
}
```

Keep the BIND-all call first as the no-VMA-policy fallback (see the approach note: it is **replaced**, not stacked, when PREFERRED succeeds). The v1 datum "PREFERRED alone did not zero vmstat" was about running **without the task policy** — non-heap VMAs faulted — a different mechanism, not evidence of BIND/PREFERRED layering.

- [ ] **Step 3: 1P json gate (hard)**

```bash
make push && make build
make gate-json-1p
```

FAIL → revert PREFERRED; Layer 1 remains the tip. Include the 1P alloc micro (Task 4 Step 1b).

- [ ] **Step 4: 256P json gate**

Same +2% band; RSS must not jump like v1 json 5→9.6 GiB. If RSS doubles, **kill Layer 2**.

- [ ] **Step 5: vmstat still 0/0** on numa json/gc-pause

- [ ] **Step 6: IMC locality gate**

```bash
# On a DRAM-heavy bench, not AllocMixed at b.N cap.
# Use golang.org/x/benchmarks json GOMAXPROCS=256 -benchmem=512 -benchnum=1
# wrapped in:
perf stat -x, -e mem_load_l3_miss_retired.local_dram,mem_load_l3_miss_retired.remote_dram -- \
  ./json ...
```

Compute `remote/(local+remote)` for baseline (or Layer 1 binary) vs Layer 2. **Run each arm ≥3 times and compare medians** — this step can kill the whole product line; a single `-benchnum=1` sample is not enough evidence for that. (Prior expectation on this box: FAIL — a whole chunk gets the node of whichever M happened to grow the heap, then 256 Ps on both nodes allocate from it. The gate is the honest answer; just don't let one noisy run give it.)

**Pass:** ≥10% **relative** drop in remote share (e.g. 0.48 → ≤0.432).  
**Fail:** no drop — **stop product B**. Write RESULTS: “PREFERRED-at-grow did not move IMC on interleaved 2P; do not add mcentral/steal.” Layer 1 remains shippable.

- [ ] **Step 7: Commit only if 1P+256P+vmstat passed.** If IMC failed, still commit the measurement and **do not** add Layers 3–4.

---

### Task 10: Stop / kill decision (required)

- [x] **Step 1: Write an explicit RESULTS.md “Layer 2 verdict”**

Either:

- **Continue:** IMC passed; Layers 3–4 allowed, or
- **Kill product B for this hardware:** only Layer 0+1 will be proposed to Go.

Frame the verdict with the **three-ingredient rule** (design §12.1): locality = homing + routing + thread stability, and Layer 2 is homing alone — an IMC FAIL is the *predicted* outcome and proves only that a proper subset measures ~zero, not that locality is impossible. If killed, record the next candidates (a future plan, not this one), in order of expected value per unit cost:

1. **Fill-one-socket-first** at GOMAXPROCS ≤ CPUs/node (design §12.2) — near-free; makes everything local for processes that fit on one socket.
2. **Homing + routing + affinity as one gated unit** — per-node arena streams (design §12.3), per-node mcentral spanSets, node-mask soft affinity with the stand-down rule (design §12.4) — the shape every NUMA-successful allocator converges on.

- [x] **Step 2: If killed, skip Tasks 11–12.** Mark them cancelled in this plan’s checkboxes.

---

### Task 11: **CANCELLED (Task 9 IMC gate failed)** — Layer 3 — per-node mcentral (ONLY if Task 9 IMC passed)

**If Task 9 IMC failed: cancel this task.**

**Files (only then):** `src/runtime/mheap.go` / `mcentral.go` / `mcache.go` — node-local `cacheSpan` using **physical** node from `getcpu` **only inside `refill`/`cacheSpan`** (rare), not `getMCache` every alloc.

**Still forbidden:** scanning all span classes on every malloc. If a P moves nodes, refill naturally pulls a new span when the current one is empty; do not flush the world.

**Modern lookup note:** if per-refill `getcpu` (~50 ns as a raw syscall) ever shows in profiles, the alternatives are the amd64 vDSO `__vdso_getcpu` (a few ns; arm64 has no vDSO getcpu) and rseq `cpu_id` (zero-cost userspace read, kernel ≥ 4.18, needs runtime-owned rseq registration). Research item, not a Layer 3 requirement.

**Gates:** 1P json; 256P json; vmstat 0/0; IMC not worse than Layer 2.

**Validation order (design §12.4):** first prove routing correctness with *externally pinned* threads (`numactl --cpunodebind` per half), so routing is not confounded by threads drifting between sockets; only then measure unpinned. Add `/numa/span-refills:local` / `/numa/span-refills:remote` `runtime/metrics` counters — incremented only in `refill`/`cacheSpan`, never on malloc — as the in-vivo locality proxy between perf-counter runs.

---

### Task 12: **CANCELLED (Task 9 IMC gate failed)** — Layer 4 — steal / GC mark (ONLY if Task 11 passed)

**If Task 9 or 11 failed: cancel this task.**

Steal victims by **`getcpu` of this M** or pinned CPU set, **never** by `allp[i].numaNode` P-index. Do not add a 2× full `allp` scan before the existing steal loop unless a 256P json profile shows steal is cheap.

From the cross-check (design §12.1, §12.4): node-mask **soft affinity** on M at `acquirep` (fires only on node *change*; kernel keeps full freedom within the socket) is the load-bearing alternative to steal-order changes — thread stability, not steal preference, is what NUMA-successful allocators depend on. Consider it before (or instead of) two-tier steal, behind the same gates. Any affinity mechanism must **stand down if the process started with a non-default affinity mask** (taskset, narrowed cpuset): the operator's placement wins.

**Gates:** 1P json (steal idle); 256P json/http not worse.

Green Tea NUMA mark: out of scope until steal exists and 1P still passes.

---

### Task 13: Promotion Sweet / Prometheus (after a shippable slice)

Run after Layer 1 (product A) and again after Layer 2 **only if it survived**.

**Sweet:**

```bash
# from x/benchmarks/sweet, one invocation, two configs
[[config]]
  name = "baseline"
  goroot = "/home/deparker/go-numa"
  envbuild = ["GOTOOLCHAIN=local"]
[[config]]
  name = "numa"
  goroot = "/home/deparker/go-numa"
  envbuild = ["GOEXPERIMENT=numa", "GOTOOLCHAIN=local"]
# NO diagnostics perf
./sweet run -count 3 -run etcd,tile38,bleve-index -stop-on-error config.toml
```

Do not compare across two `sweet run`s. Bleve N=1 and etcd p99 are not gates.

**Prometheus:** build twice; Avalanche ~10k series; system-wide `/proc/vmstat` delta 0/0 **while only the numa Prometheus runs** (there is no per-PID hint-fault counter — see Task 7 CONCERN); QPS is supporting only. Script shape: evidence `numa-design/run-prometheus-bench.sh` (copy, drop if Layer 2-only).

---

## Runtime test commands (cheat sheet)

```bash
make push && make build
make test-numa PKG=runtime RUN=TestNUMATopologyDiscovery
make test-numa PKG=runtime RUN=TestNUMABindAllTaskPolicy
# Full runtime with experiment (slow):
ssh numa-dell 'cd /home/deparker/go-numa && GOROOT=$PWD GOEXPERIMENT=numa ./bin/go test runtime -count=1'
# Never use make test-numa RUN=. with -v by default (too large).
make gate-json-1p
```

---

## Self-review vs `numa-v2-design.md`

| Spec section | Plan task |
|--------------|-----------|
| §1 1P never slower | Tasks 4–5, 7, 9 |
| §1 1P alloc micro | Task 4 Step 1b; run in Tasks 5, 7, 9 |
| §3 product A vs B | Tasks 6–8 vs 9–10 |
| §5 reject flush / STW toggle / P-index steal | Global constraints, Tasks 11–12 |
| §6 layers 0–4 | Tasks 1–3, 6, 9, 11, 12 |
| §8 Sweet/Prometheus pyramid | Tasks 5, 7, 8, 13 |
| §9 start from master | Task 0 |
| BIND-all nodemask (maxnode=65, one word) | Task 6 |
| Interleaved CPUs | Task 3 topology test; steal forbidden by index |
| §12 cross-check adoptions | Tasks 3, 4 (Step 1c), 6, 7, 10, 11, 12 |

**Gaps closed:** Layer 2 approach locked to arena PREFERRED + getcpu-on-grow; address partition / M pinning deferred to a future plan if IMC fails. Layer 3–4 are conditional.

---

## What “done” means for a fresh agent

1. Branch from **master**, docs + Makefile, remote build works.
2. Layer 0 merged with **1P json PASS**.
3. Layer 1 merged with **1P json PASS**, **256P json PASS**, **vmstat 0/0**.
4. Layer 2 either merged with IMC win **or** explicitly killed with numbers.
5. No flush function exists in the tree.
6. `RESULTS.md` has a section per layer with SHA and commands.
