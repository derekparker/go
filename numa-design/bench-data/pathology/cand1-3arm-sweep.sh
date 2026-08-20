#!/usr/bin/env bash
# Single-session three-arm candidate 1 sweep: B (stock unpinned), C-full
# (GOEXPERIMENT=numa at branch HEAD, Layer 0+1+2), C-L1 (scratch patch,
# Layer 0+1 only). All unpinned, GOMAXPROCS=128, -benchmem=4096. n=15 + 1
# warmup round, rotating BCL/CLB/LBC order, vmstat per run, idle checks.
set -euo pipefail

OUT=/tmp/pb/data
GATE=/home/deparker/go-numa/numa-design/gate-vmstat.sh
B_BIN=/tmp/pb/base/garbage
CFULL_BIN=/tmp/pb/numa/garbage
CL1_BIN=/tmp/pb/l1v2/garbage
FLAGS="-benchmem=4096 -benchnum=1"
GOMAXPROCS=128
ROUNDS=15

mkdir -p "$OUT"
cd "$OUT"

log() { echo "[$(date -Iseconds)] $*" | tee -a sweep3arm.log; }

idle_check() {
	log "idle check"
	ps aux --sort=-%cpu | head -8 >>sweep3arm.log
}

run_arm() {
	local arm="$1" round="$2" tag="$3"
	local vb="$OUT/cand1-3arm-arm${arm}-r${round}.vmstat.before"
	local va="$OUT/cand1-3arm-arm${arm}-r${round}.vmstat.after"
	local of="$OUT/cand1-3arm-arm${arm}-${tag}.out"

	idle_check
	bash "$GATE" snap "$vb"

	case "$arm" in
	B)
		env GOMAXPROCS=$GOMAXPROCS "$B_BIN" $FLAGS >>"$of" 2>>"$OUT/cand1-3arm-arm${arm}-${tag}.stderr"
		;;
	CFULL)
		env GOMAXPROCS=$GOMAXPROCS "$CFULL_BIN" $FLAGS >>"$of" 2>>"$OUT/cand1-3arm-arm${arm}-${tag}.stderr"
		;;
	CL1)
		env GOMAXPROCS=$GOMAXPROCS "$CL1_BIN" $FLAGS >>"$of" 2>>"$OUT/cand1-3arm-arm${arm}-${tag}.stderr"
		;;
	esac

	bash "$GATE" snap "$va"
	local hint mig hint0 mig0
	hint=$(awk '/numa_hint_faults/{print $2}' "$va")
	mig=$(awk '/numa_pages_migrated/{print $2}' "$va")
	hint0=$(awk '/numa_hint_faults/{print $2}' "$vb")
	mig0=$(awk '/numa_pages_migrated/{print $2}' "$vb")
	log "arm=$arm round=$round(tag=$tag) hint_faults_delta=$((hint-hint0)) pages_migrated_delta=$((mig-mig0))"
	echo "arm=$arm round=$round tag=$tag hint_faults_delta=$((hint-hint0)) pages_migrated_delta=$((mig-mig0))" >>"$OUT/cand1-3arm-vmstat-summary.txt"
}

orders=(BCL CLB LBC)

log "=== warmup round (unrecorded, discarded) ==="
warm_order=${orders[0]}
for ((i=0;i<3;i++)); do
	c=${warm_order:$i:1}
	case "$c" in
	B) arm=B ;;
	C) arm=CFULL ;;
	L) arm=CL1 ;;
	esac
	run_arm "$arm" 0 warmup
done

log "=== recorded sweep: $ROUNDS rounds ==="
for ((r=1;r<=ROUNDS;r++)); do
	order=${orders[$(( (r-1) % 3 ))]}
	log "--- round $r, arm order $order ---"
	for ((i=0;i<3;i++)); do
		c=${order:$i:1}
		case "$c" in
		B) arm=B ;;
		C) arm=CFULL ;;
		L) arm=CL1 ;;
		esac
		run_arm "$arm" "$r" recorded
	done
done

log "=== sweep3arm complete ==="
