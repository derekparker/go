#!/usr/bin/env bash
# Candidate 3 sweep: phase-shift, arms A/B/C, n=10 + 1 warmup round.
# Rotating arm order per round (ABC/BCA/CAB), vmstat snap around every run,
# node-0 free-RAM gate before every arm-A run.
set -euo pipefail

OUT=/tmp/pb/data
GATE=/home/deparker/go-numa/numa-design/gate-vmstat.sh
BASE=/tmp/pb/phaseshift-base
NUMA=/tmp/pb/phaseshift-numa
FLAGS="-heap=6144 -readers=64 -phase=30 -phases=4"
GOMAXPROCS=128
ROUNDS=10
MINFREE_MB=10000

mkdir -p "$OUT"
cd "$OUT"

log() { echo "[$(date -Iseconds)] $*" | tee -a sweep3.log; }

idle_check() {
	log "idle check"
	ps aux --sort=-%cpu | head -8 >>sweep3.log
}

node0_free_mb() {
	numactl --hardware | awk '/^node 0 free:/{print $4}'
}

gate_node0() {
	local free
	free=$(node0_free_mb)
	log "node0 free = ${free} MB (need >= ${MINFREE_MB})"
	local tries=0
	while [ "$free" -lt "$MINFREE_MB" ]; do
		tries=$((tries+1))
		if [ "$tries" -gt 6 ]; then
			log "ABORT: node0 free RAM below gate after ${tries} waits"
			exit 3
		fi
		log "node0 free RAM below gate, waiting 30s (try $tries)"
		sleep 30
		free=$(node0_free_mb)
		log "node0 free = ${free} MB"
	done
}

run_arm() {
	local arm="$1" round="$2" tag="$3"
	local vb="$OUT/cand3-arm${arm}-r${round}.vmstat.before"
	local va="$OUT/cand3-arm${arm}-r${round}.vmstat.after"
	local of="$OUT/cand3-arm${arm}-${tag}.out"

	if [ "$arm" = "A" ]; then
		gate_node0
	fi

	idle_check
	bash "$GATE" snap "$vb"

	case "$arm" in
	A)
		numactl --cpunodebind=0 --membind=0 env GOMAXPROCS=$GOMAXPROCS "$BASE" $FLAGS >>"$of" 2>>"$OUT/cand3-arm${arm}-${tag}.stderr"
		;;
	B)
		env GOMAXPROCS=$GOMAXPROCS "$BASE" $FLAGS >>"$of" 2>>"$OUT/cand3-arm${arm}-${tag}.stderr"
		;;
	C)
		env GOMAXPROCS=$GOMAXPROCS "$NUMA" $FLAGS >>"$of" 2>>"$OUT/cand3-arm${arm}-${tag}.stderr"
		;;
	esac

	bash "$GATE" snap "$va"
	local hint mig hint0 mig0
	hint=$(awk '/numa_hint_faults/{print $2}' "$va")
	mig=$(awk '/numa_pages_migrated/{print $2}' "$va")
	hint0=$(awk '/numa_hint_faults/{print $2}' "$vb")
	mig0=$(awk '/numa_pages_migrated/{print $2}' "$vb")
	log "arm=$arm round=$round(tag=$tag) hint_faults_delta=$((hint-hint0)) pages_migrated_delta=$((mig-mig0))"
	echo "arm=$arm round=$round tag=$tag hint_faults_delta=$((hint-hint0)) pages_migrated_delta=$((mig-mig0))" >>"$OUT/cand3-vmstat-summary.txt"
}

orders=(ABC BCA CAB)

log "=== warmup round (unrecorded, discarded) ==="
warm_order=${orders[0]}
for ((i=0;i<3;i++)); do
	arm=${warm_order:$i:1}
	run_arm "$arm" 0 warmup
done

log "=== recorded sweep: $ROUNDS rounds ==="
for ((r=1;r<=ROUNDS;r++)); do
	order=${orders[$(( (r-1) % 3 ))]}
	log "--- round $r, arm order $order ---"
	for ((i=0;i<3;i++)); do
		arm=${order:$i:1}
		run_arm "$arm" "$r" recorded
	done
done

log "=== sweep3 complete ==="
