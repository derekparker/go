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
