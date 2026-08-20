#!/usr/bin/env bash
# Exact invocations for Workstream A gates 1-5, 8 (task-4-brief.md).
# Archived verbatim alongside the raw .out/.txt files in this directory.
# Not meant to be re-run as a single script (some steps require rebuilding
# a parent-commit toolchain in a separate worktree) — a record, not a driver.
set -euo pipefail

### Gate 1 -- 1P json, off vs on, same commit, BENCHNUM=10
ssh numa-dell 'cd /home/deparker/go-numa && GOROOT=$PWD GOMAXPROCS=1 BENCHNUM=10 OUT=/tmp/wsA-gate1 ./numa-design/gate-json.sh'
ssh numa-dell '/tmp/numa-tools/benchstat /tmp/wsA-gate1/baseline.out /tmp/wsA-gate1/numa.out'

### Gate 2 -- 1P alloc micro, -count=10
ssh numa-dell 'cd /home/deparker/go-numa && export GOROOT=$PWD PATH=$PWD/bin:$PATH GOTOOLCHAIN=local
GOMAXPROCS=1 go test runtime -run=NONE -bench="Malloc(8|16|Types)" -count=10 >/tmp/wsA-alloc-base.out
GOMAXPROCS=1 GOEXPERIMENT=numa go test runtime -run=NONE -bench="Malloc(8|16|Types)" -count=10 >/tmp/wsA-alloc-numa.out
/tmp/numa-tools/benchstat /tmp/wsA-alloc-base.out /tmp/wsA-alloc-numa.out'

### Gate 3 -- 256P json, 3 unconditional sessions x BENCHNUM=10, pooled n=30
for r in r1 r2 r3; do
  ssh numa-dell "cd /home/deparker/go-numa && GOROOT=\$PWD GOMAXPROCS=256 BENCHNUM=10 OUT=/tmp/wsA-gate3-$r ./numa-design/gate-json.sh"
done
ssh numa-dell 'cat /tmp/wsA-gate3-r1/baseline.out /tmp/wsA-gate3-r2/baseline.out /tmp/wsA-gate3-r3/baseline.out > /tmp/wsA-gate3-pooled-baseline.out
cat /tmp/wsA-gate3-r1/numa.out /tmp/wsA-gate3-r2/numa.out /tmp/wsA-gate3-r3/numa.out > /tmp/wsA-gate3-pooled-numa.out
/tmp/numa-tools/benchstat /tmp/wsA-gate3-pooled-baseline.out /tmp/wsA-gate3-pooled-numa.out'

### Gate 4 -- strace proof, three arms
# (a) 256P
ssh numa-dell 'cd /tmp/wsA-gate3-r1 && strace -f -e trace=sched_setaffinity,set_mempolicy,mbind -o /tmp/wsA-strace-256p.txt \
  env GOMAXPROCS=256 ./numa/json -benchmem=512 -benchnum=1 -benchtime=2s >/dev/null'
# (b) confined 1P
ssh numa-dell 'cd /tmp/wsA-gate3-r1 && strace -f -e trace=sched_setaffinity,set_mempolicy,mbind -o /tmp/wsA-strace-1p.txt \
  env GOMAXPROCS=1 ./numa/json -benchmem=512 -benchnum=1 -benchtime=2s >/dev/null'
# maps spot check
ssh numa-dell 'cd /tmp/wsA-gate3-r1 && env GOMAXPROCS=1 ./numa/json -benchmem=512 -benchnum=1 -benchtime=5s >/dev/null & PID=$!; sleep 1; wc -l /proc/$PID/maps; wait $PID'
# (c) stand-down, testprog
ssh numa-dell 'cd /home/deparker/go-numa && export GOROOT=$PWD PATH=$PWD/bin:$PATH GOTOOLCHAIN=local
GOEXPERIMENT=numa go build -o /tmp/wsA-testprog ./src/runtime/testdata/testprog
strace -f -e trace=sched_setaffinity,set_mempolicy -o /tmp/wsA-strace-standdown.txt \
  env GOMAXPROCS=1 /tmp/wsA-testprog NUMAStandDown'
# stand-down wall-time comparison vs stock build (n=3 each, uninstrumented)
ssh numa-dell 'cd /home/deparker/go-numa && export GOROOT=$PWD PATH=$PWD/bin:$PATH GOTOOLCHAIN=local
go build -o /tmp/wsA-testprog-stock ./src/runtime/testdata/testprog
for i in 1 2 3; do GOMAXPROCS=1 /tmp/wsA-testprog-stock NUMAStandDown; done
for i in 1 2 3; do GOMAXPROCS=1 /tmp/wsA-testprog NUMAStandDown; done'

### Gate 5 -- vmstat 0/0, confined arm, rerun once
ssh numa-dell 'cd /home/deparker/go-numa
./numa-design/gate-vmstat.sh snap /tmp/wsA-gate5.before
env GOMAXPROCS=1 /tmp/wsA-gate3-r1/numa/json -benchmem=512 -benchnum=1 -benchtime=3s >/dev/null
./numa-design/gate-vmstat.sh snap /tmp/wsA-gate5.after
./numa-design/gate-vmstat.sh diff /tmp/wsA-gate5.before /tmp/wsA-gate5.after'
ssh numa-dell 'cd /home/deparker/go-numa
./numa-design/gate-vmstat.sh snap /tmp/wsA-gate5b.before
env GOMAXPROCS=1 /tmp/wsA-gate3-r1/numa/json -benchmem=512 -benchnum=1 -benchtime=3s >/dev/null
./numa-design/gate-vmstat.sh snap /tmp/wsA-gate5b.after
./numa-design/gate-vmstat.sh diff /tmp/wsA-gate5b.before /tmp/wsA-gate5b.after'

### Gate 8 -- off-binary function census (local, not remote)
# printf 'package main\n\nfunc main() { println("census") }\n' > /tmp/census-canary.go
# GOROOT=<HEAD worktree> GOWORK=off GOTOOLCHAIN=local GOEXPERIMENT= go build -o /tmp/census-head /tmp/census-canary.go
# git worktree add /tmp/census-parent abe916018e
# (cd /tmp/census-parent/src && GOROOT_FINAL=/tmp/census-parent ./make.bash)
# GOROOT=/tmp/census-parent GOWORK=off GOTOOLCHAIN=local GOEXPERIMENT= go build -o /tmp/census-parent-bin /tmp/census-canary.go
# objdump -d /tmp/census-head       | sed 's/[0-9a-f]\{6,\}//g' >/tmp/census-head.txt
# objdump -d /tmp/census-parent-bin | sed 's/[0-9a-f]\{6,\}//g' >/tmp/census-parent.txt
# (per-function census done via a small python script -- see task-4-report.md for the exact
#  parsing logic: symbol-labeled objdump blocks, per-function instruction-line counts)
