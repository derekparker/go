# Workstream B gate battery — archive index

**Status: COMPLETE.** This file originally documented a mid-task pause
after Gate 1 only; the battery was resumed and completed in a second
sitting. Kept as historical record of the pause per the coordinator's
instruction. See `../../RESULTS.md`'s "Workstream B gate battery" section
for the full verdict (overall: Workstream B does not ship as-is — two hard
gates fail, the IMC decision gate falls short of its ≥10% bar despite a
real, corroborated effect) and `task-11-report.md` (gitignored SDD ledger)
for the complete per-gate report.

Subdirectories added in the second sitting: `gate2-1p/`, `gate3-256p/`,
`census/`, `imc/`, `cand1-128p/`, `cand2-gcpause/`, `cand1-256p-exploratory/`,
plus `gate1-numamaps-followup.txt` and `go-version-m-all-binaries.txt` at
this directory's top level.

---

## Original pause note (first sitting, historical)

Session paused by the user after Gate 1 (pinned routing proof) completed
cleanly and before Gate 2 was started. This directory held only Gate 1's
evidence at that point.

## What's archived here

- `routing-probe.go` — the Task 11 Step 1 harness (source). Not part of the
  module tree; built directly with `go build -o probe routing-probe.go`
  (single-file main package, no go.mod needed), same technique as the
  Workstream A gate battery's off-binary census canary.
- `node0.out` / `node1.out` — raw stdout of the two pinned halves.
- `probe-on-govm.txt` — `go version -m` on the built probe binary, confirming
  SHA and `X:numa` tag.

## Remote state (numa-dell) at pause time

- Tree in sync at `ce4b564d8f` (matches local HEAD), already built
  (`./bin/go version` confirms `go1.28-devel_ce4b564d8f`) — no rebuild needed
  to resume.
- `kernel.numa_balancing = 1` (verified at pause).
- No orphaned processes left running (verified: no `probe-on`/`probe-off`
  processes on the remote).
- Built binaries still present on numa-dell (not yet needed again, but not
  cleaned up either): `/tmp/pb/routing-probe/probe-on`, `/tmp/pb/routing-probe/probe-off`.
- Pre-existing untracked files on the remote tree (`numa-design/run-x-benchmarks.sh`,
  `x-benchmarks/`) predate this task, unrelated, left as-is (matches the state
  recorded in the Workstream A gate battery's own post-hoc verification notes).

## Command used for Gate 1

```bash
scp routing-probe.go numa-dell:/tmp/routing-probe.go
ssh numa-dell '
  cd /home/deparker/go-numa
  export GOROOT=$PWD PATH=$PWD/bin:$PATH GOTOOLCHAIN=local
  GOEXPERIMENT=numa go build -o /tmp/pb/routing-probe/probe-on /tmp/routing-probe.go
  go build -o /tmp/pb/routing-probe/probe-off /tmp/routing-probe.go
'
ssh numa-dell '
  numactl --cpunodebind=0 env GOMAXPROCS=128 /tmp/pb/routing-probe/probe-on -totalmb=2048
  numactl --cpunodebind=1 env GOMAXPROCS=128 /tmp/pb/routing-probe/probe-on -totalmb=2048
'
```

## Gate 1 result (see task-11-report.md and RESULTS.md for the full verdict)

- node0 half: `local=93329 remote=0 total=93329 local_share=100.0000%`
- node1 half: `local=94346 remote=432 total=94778 local_share=99.5442%`
- Both ≥95% — **PASS**.

## Resume outcome (second sitting)

Steps 2-5 all ran to completion. Headline: Gate 2b (1P alloc micro)
geomean +3.73% FAIL, Gate 2c (256P json) user+sys-sec/op +19.96% FAIL,
Gate 3/Step 3 (IMC decision gate) −4.49% relative FAIL (short of the ≥10%
bar, but a real, statistically clean, corroborated effect — not a null
result like Layer 2). Both pathology candidates at GOMAXPROCS=128 (Step 4)
PASS, and are tighter than Workstream A's own gate battery on the same
workloads. Overall verdict: **Workstream B does not ship as-is** — see
RESULTS.md for the full three-ingredient candidate analysis of what's
still missing (leading candidate: thread-stability insufficiency at the
goroutine level, not the OS-thread level soft affinity actually built).
