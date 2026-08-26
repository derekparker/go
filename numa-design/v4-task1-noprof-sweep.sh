#!/usr/bin/env bash
# v4 Task 1: 4-arm single-session sweep, GOMAXPROCS=256.
# Arms: off-prof, numa-prof, off-noprof, numa-noprof (noprof = BENCH_DISABLE_CPUPROF=1,
# honored only by the patched harness copy — see v4 plan Task 1 Step 3).
# Rotating arm order per round; N rounds; raw benchfmt appended per arm.
# Pre-registered primary: benchstat off-noprof.out numa-noprof.out, user+sys-ns/op.
set -euo pipefail
BIN_OFF="${BIN_OFF:?path to patched-harness json binary, experiment OFF}"
BIN_NUMA="${BIN_NUMA:?path to patched-harness json binary, GOEXPERIMENT=numa}"
OUT="${OUT:-$HOME/v4-task1-out}"
N="${N:-10}"
PROCS=256 MEM=512 TIME=3s
mkdir -p "$OUT"
# idle check: fail if any foreign process is burning >50% of a CPU
busy=$(ps aux --sort=-%cpu | awk 'NR>1 && $3>50 {print $11}' | grep -v -e json -e ps || true)
[ -z "$busy" ] || { echo "machine not idle: $busy"; exit 1; }
[ "$(cat /proc/sys/kernel/numa_balancing)" = "1" ] || { echo "numa_balancing != 1"; exit 1; }
arms=(off-prof numa-prof off-noprof numa-noprof)
run_arm() {
  local arm="$1" bin env_no=""
  case "$arm" in
    off-*)  bin="$BIN_OFF" ;;
    numa-*) bin="$BIN_NUMA" ;;
  esac
  case "$arm" in *-noprof) env_no=1 ;; esac
  env GOMAXPROCS=$PROCS ${env_no:+BENCH_DISABLE_CPUPROF=1} \
    "$bin" -benchmem=$MEM -benchnum=1 -benchtime=$TIME >>"$OUT/$arm.out" 2>>"$OUT/$arm.log"
}
for a in "${arms[@]}"; do : >"$OUT/$a.out"; : >"$OUT/$a.log"; done
for round in $(seq 0 $((N-1))); do
  echo "== round $round =="
  for i in 0 1 2 3; do
    run_arm "${arms[$(( (round + i) % 4 ))]}"
  done
done
echo "done; raws in $OUT"
