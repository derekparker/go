#!/usr/bin/env bash
# Task 12 mechanism-analysis capture rounds: 3 designated rounds, both
# arms, WITH -numamaps enabled. Deliberately NOT part of the primary n=20
# set (run separately from pathology-sweep.sh's wsC-phase campaign) --
# see the Task 12 pre-registration in numa-design/RESULTS.md for the
# exclusion rationale (numa_maps snapshotting adds a few extra
# syscalls/file writes per phase that the primary-set runs don't do).
#
# Same protocol conventions as pathology-sweep.sh: idle check + vmstat
# snap before/after every run, rotating B/C order across the 3 rounds.
set -euo pipefail

OUT=/tmp/pb/wsC-phase-capture
GATE=/home/deparker/go-numa/numa-design/gate-vmstat.sh
BASE=/tmp/pb/phaseshift-base
NUMA=/tmp/pb/phaseshift-numa
FLAGS="-heap=6144 -readers=64 -phase=30 -phases=4"
GOMAXPROCS=256
ROUNDS=3

mkdir -p "$OUT"
cd "$OUT"

log() { echo "[$(date -Iseconds)] $*" | tee -a capture-sweep.log; }

idle_check() {
	local top
	top=$(ps -eo pcpu,comm --sort=-pcpu | awk 'NR>1 && $2 != "ps" && $2 != "awk" {print int($1); exit}')
	while [ "${top:-0}" -gt 50 ]; do
		log "idle_check: top process at ${top}% CPU; sleeping 30s"
		sleep 30
		top=$(ps -eo pcpu,comm --sort=-pcpu | awk 'NR>1 && $2 != "ps" && $2 != "awk" {print int($1); exit}')
	done
}

run_arm() {
	local arm="$1" round="$2"
	local vb="$OUT/wsC-phase-capture-arm${arm}-r${round}.vmstat.before"
	local va="$OUT/wsC-phase-capture-arm${arm}-r${round}.vmstat.after"
	local of="$OUT/wsC-phase-capture-arm${arm}-r${round}.out"
	local prefix="$OUT/wsC-phase-capture-arm${arm}-r${round}"

	idle_check
	bash "$GATE" snap "$vb"

	case "$arm" in
	B)
		env GOMAXPROCS=$GOMAXPROCS "$BASE" $FLAGS -numamaps="$prefix" \
			>>"$of" 2>>"$OUT/wsC-phase-capture-arm${arm}-r${round}.stderr"
		;;
	C)
		env GOMAXPROCS=$GOMAXPROCS "$NUMA" $FLAGS -numamaps="$prefix" \
			>>"$of" 2>>"$OUT/wsC-phase-capture-arm${arm}-r${round}.stderr"
		;;
	esac

	bash "$GATE" snap "$va"
	local hint mig hint0 mig0
	hint=$(awk '/numa_hint_faults/{print $2}' "$va")
	mig=$(awk '/numa_pages_migrated/{print $2}' "$va")
	hint0=$(awk '/numa_hint_faults/{print $2}' "$vb")
	mig0=$(awk '/numa_pages_migrated/{print $2}' "$vb")
	log "arm=$arm round=$round hint_faults_delta=$((hint-hint0)) pages_migrated_delta=$((mig-mig0))"
	echo "arm=$arm round=$round hint_faults_delta=$((hint-hint0)) pages_migrated_delta=$((mig-mig0))" \
		>>"$OUT/wsC-phase-capture-vmstat-summary.txt"
}

orders=(BC CB BC)

log "=== capture sweep: $ROUNDS rounds, both arms, -numamaps enabled ==="
for ((r = 1; r <= ROUNDS; r++)); do
	order=${orders[$((r - 1))]}
	log "--- capture round $r, arm order $order ---"
	for ((i = 0; i < 2; i++)); do
		arm=${order:$i:1}
		run_arm "$arm" "$r"
	done
done

log "=== capture sweep complete ==="
