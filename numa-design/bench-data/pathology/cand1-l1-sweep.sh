#!/usr/bin/env bash
# L1-only re-run of candidate 1: stock unpinned (B) vs GOEXPERIMENT=numa
# built from a scratch L1-only patch (C-L1, Layer 2's MPOL_PREFERRED
# removed, BIND-all only). n=10 + 1 warmup round, rotating BC/CB order,
# vmstat snap per run.
set -euo pipefail

OUT=/tmp/pb/data
GATE=/home/deparker/go-numa/numa-design/gate-vmstat.sh
BASE=/tmp/pb/base/garbage
L1=/tmp/pb/l1/garbage
FLAGS="-benchmem=4096 -benchnum=1"
GOMAXPROCS=128
ROUNDS=10

mkdir -p "$OUT"
cd "$OUT"

log() { echo "[$(date -Iseconds)] $*" | tee -a sweepL1.log; }

idle_check() {
	log "idle check"
	ps aux --sort=-%cpu | head -8 >>sweepL1.log
}

run_arm() {
	local arm="$1" round="$2" tag="$3"
	local vb="$OUT/l1only-arm${arm}-r${round}.vmstat.before"
	local va="$OUT/l1only-arm${arm}-r${round}.vmstat.after"
	local of="$OUT/l1only-arm${arm}-${tag}.out"

	idle_check
	bash "$GATE" snap "$vb"

	case "$arm" in
	B)
		env GOMAXPROCS=$GOMAXPROCS "$BASE" $FLAGS >>"$of" 2>>"$OUT/l1only-arm${arm}-${tag}.stderr"
		;;
	CL1)
		env GOMAXPROCS=$GOMAXPROCS "$L1" $FLAGS >>"$of" 2>>"$OUT/l1only-arm${arm}-${tag}.stderr"
		;;
	esac

	bash "$GATE" snap "$va"
	local hint mig hint0 mig0
	hint=$(awk '/numa_hint_faults/{print $2}' "$va")
	mig=$(awk '/numa_pages_migrated/{print $2}' "$va")
	hint0=$(awk '/numa_hint_faults/{print $2}' "$vb")
	mig0=$(awk '/numa_pages_migrated/{print $2}' "$vb")
	log "arm=$arm round=$round(tag=$tag) hint_faults_delta=$((hint-hint0)) pages_migrated_delta=$((mig-mig0))"
	echo "arm=$arm round=$round tag=$tag hint_faults_delta=$((hint-hint0)) pages_migrated_delta=$((mig-mig0))" >>"$OUT/l1only-vmstat-summary.txt"
}

orders=(BCL1 CL1B)

log "=== warmup round (unrecorded, discarded) ==="
for arm in B CL1; do
	run_arm "$arm" 0 warmup
done

log "=== recorded sweep: $ROUNDS rounds ==="
for ((r=1;r<=ROUNDS;r++)); do
	if (( (r-1) % 2 == 0 )); then
		order=(B CL1)
	else
		order=(CL1 B)
	fi
	log "--- round $r, arm order ${order[*]} ---"
	for arm in "${order[@]}"; do
		run_arm "$arm" "$r" recorded
	done
done

log "=== sweepL1 complete ==="
