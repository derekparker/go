#!/usr/bin/env bash
# G2-locality (v4 plan): unpinned local-refill share via locality-probe at
# GOMAXPROCS in {2,8,32,128,256}, R launches per width (launch-to-launch
# variance matters: randomizeHeapBase can trim one node's window per launch --
# see v4-pagealloc-design.md C1). Bar: >=90% at every width (per-width median
# across launches; every individual launch recorded).
#
# Runs ON numa-dell. Build first:
#   cd /home/deparker/go-numa/numa-design/locality-probe
#   GOWORK=off GOEXPERIMENT=numa /home/deparker/go-numa/bin/go build -o /tmp/g2/locality-probe .
set -euo pipefail
PROBE="${PROBE:-/tmp/g2/locality-probe}"
OUT="${OUT:-$HOME/v4-g2-locality}"
R="${R:-5}"
mkdir -p "$OUT"
[ "$(cat /proc/sys/kernel/numa_balancing)" = "1" ] || { echo "numa_balancing != 1"; exit 1; }
: >"$OUT/locality.out"
for r in $(seq "$R"); do
	for p in 2 8 32 128 256; do
		"$PROBE" -procs "$p" -secs 3 >>"$OUT/locality.out"
	done
done
echo "== raw =="; cat "$OUT/locality.out"
echo "== per-width medians =="
awk '{
	match($0, /procs=([0-9]+)/, m); match($0, /share=([0-9.]+)%/, s)
	w[m[1]] = w[m[1]] " " s[1]
} END {
	for (p in w) {
		n = split(w[p], a, " "); asort(a)
		mid = (n % 2) ? a[(n+1)/2] : (a[n/2] + a[n/2+1]) / 2
		printf "procs=%s median=%.2f%% n=%d\n", p, mid, n
	}
}' "$OUT/locality.out"
