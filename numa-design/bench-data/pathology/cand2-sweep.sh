#!/usr/bin/env bash
# Candidate 2 sweep: gc-pause-bench heavy profile, arms A/B/C, n=10 + 1 warmup round.
# Rotating arm order per round (ABC/BCA/CAB), vmstat snap around every run,
# node-0 free-RAM gate before every arm-A run.
set -euo pipefail

OUT=/tmp/pb/data
GATE=/home/deparker/go-numa/numa-design/gate-vmstat.sh
BASE=/tmp/pb/gcpause-base
NUMA=/tmp/pb/gcpause-numa
FLAGS="-ptrheap=true -toucher=false -heap=4096 -idle=1000000 -stacks=200 -warm=45 -gcgap=5s -discard=2 -n=8"
GOMAXPROCS=128
ROUNDS=10
MINFREE_MB=10000

mkdir -p "$OUT"
cd "$OUT"

log() { echo "[$(date -Iseconds)] $*" | tee -a sweep2.log; }

idle_check() {
	log "idle check"
	ps aux --sort=-%cpu | head -8 >>sweep2.log
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
	local vb="$OUT/cand2-arm${arm}-r${round}.vmstat.before"
	local va="$OUT/cand2-arm${arm}-r${round}.vmstat.after"
	local of="$OUT/cand2-arm${arm}-${tag}.out"

	if [ "$arm" = "A" ]; then
		gate_node0
	fi

	idle_check
	bash "$GATE" snap "$vb"

	case "$arm" in
	A)
		numactl --cpunodebind=0 --membind=0 env GOMAXPROCS=$GOMAXPROCS GODEBUG=gcshrinkstackoff=1 "$BASE" $FLAGS >>"$of" 2>>"$OUT/cand2-arm${arm}-${tag}.stderr"
		;;
	B)
		env GOMAXPROCS=$GOMAXPROCS GODEBUG=gcshrinkstackoff=1 "$BASE" $FLAGS >>"$of" 2>>"$OUT/cand2-arm${arm}-${tag}.stderr"
		;;
	C)
		env GOMAXPROCS=$GOMAXPROCS GODEBUG=gcshrinkstackoff=1 "$NUMA" $FLAGS >>"$of" 2>>"$OUT/cand2-arm${arm}-${tag}.stderr"
		;;
	esac

	bash "$GATE" snap "$va"
	local hint mig hint0 mig0
	hint=$(awk '/numa_hint_faults/{print $2}' "$va")
	mig=$(awk '/numa_pages_migrated/{print $2}' "$va")
	hint0=$(awk '/numa_hint_faults/{print $2}' "$vb")
	mig0=$(awk '/numa_pages_migrated/{print $2}' "$vb")
	log "arm=$arm round=$round(tag=$tag) hint_faults_delta=$((hint-hint0)) pages_migrated_delta=$((mig-mig0))"
	echo "arm=$arm round=$round tag=$tag hint_faults_delta=$((hint-hint0)) pages_migrated_delta=$((mig-mig0))" >>"$OUT/cand2-vmstat-summary.txt"
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

log "=== sweep2 complete ==="
