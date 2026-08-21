# Workstream B gate battery — PARTIAL archive (paused mid-task)

Session paused by the user after Gate 1 (pinned routing proof) completed
cleanly and before Gate 2 was started. This directory holds only Gate 1's
evidence; Gates 2-6 have not run yet.

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

## To resume

Continue from Task 11 Step 2 (hard gates: 1P json, 1P alloc micro, 256P json,
RSS, vmstat 0/0, off-binary census) using the established `numa-design/gate-json.sh`
/ `numa-design/pathology-sweep.sh` protocol (BENCHNUM=10, single-session,
rotating order). Then Step 3 (IMC decision gate, ≥5 interleaved runs/arm,
pre-registered in RESULTS.md's "Workstream B go/no-go (Task 7)" section).
Then Step 4 (pathology candidates A/B/C rerun, WS-A protocol, n=10-15).
See `.superpowers/sdd/2026-08-20-numa-v3-locality-plan/task-11-brief.md` and
`task-11-report.md` for the full remaining checklist.
