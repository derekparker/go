#!/usr/bin/env bash
# Prometheus scrape + query_range load: baseline vs GOEXPERIMENT=numa vs membind.
# Avalanche serves ~50k gauge series; Prometheus scrapes them; a curl loop
# issues query_range while we record RSS, CPU, vmstat, and IMC DRAM events.
set -euo pipefail

GOROOT="${GOROOT:-/home/deparker/go-numa}"
export GOROOT PATH="$GOROOT/bin:$PATH" GOMAXPROCS="${GOMAXPROCS:-256}" GOTOOLCHAIN=local
BIN="${BIN:-$GOROOT/x-benchmarks/prom-bin}"
OUT="${OUT:-/tmp/prom-numa-bench}"
EVENTS="${PERF_IMC_EVENTS:-mem_load_l3_miss_retired.local_dram,mem_load_l3_miss_retired.remote_dram}"
WARM="${WARM_SEC:-20}"
RUN="${RUN_SEC:-60}"
QPS="${QUERY_CONCURRENCY:-8}"

PROM_PORT=19090
AVA_PORT=19001

mkdir -p "$OUT"
cd "$OUT"

{
	echo "date=$(date -Iseconds)"
	echo "host=$(hostname)"
	echo "HEAD=$(git -C "$GOROOT" rev-parse --short HEAD)"
	echo "go=$("$GOROOT/bin/go" version)"
	echo "GOMAXPROCS=$GOMAXPROCS"
	echo "numa_balancing=$(sysctl -n kernel.numa_balancing)"
	echo "prometheus=$("$BIN/prometheus-baseline" --version 2>&1 | head -1 || true)"
} | tee "$OUT/meta.txt"

vmstat_snap() { grep -E '^(numa_|pgmigrate)' /proc/vmstat >"$1"; }

vmstat_delta() {
	python3 - "$1" "$2" <<'PY'
import sys
def parse(p):
    d = {}
    with open(p) as f:
        for line in f:
            k, v = line.split()
            d[k] = int(v)
    return d
b, a = parse(sys.argv[1]), parse(sys.argv[2])
for k in ("numa_hint_faults", "numa_hint_faults_local", "numa_pages_migrated"):
    print(f"{k}={a.get(k,0)-b.get(k,0)}")
PY
}

write_prom_yml() {
	cat >"$OUT/prometheus.yml" <<EOF
global:
  scrape_interval: 1s
  evaluation_interval: 1s
scrape_configs:
  - job_name: avalanche
    static_configs:
      - targets: ["127.0.0.1:${AVA_PORT}"]
  - job_name: prometheus
    static_configs:
      - targets: ["127.0.0.1:${PROM_PORT}"]
EOF
}

kill_port() {
	local p="$1"
	local pids
	pids=$(ss -lptn "sport = :$p" 2>/dev/null | sed -n 's/.*pid=\([0-9]*\).*/\1/p' | sort -u)
	if [[ -n "${pids:-}" ]]; then
		kill $pids 2>/dev/null || true
		sleep 1
		kill -9 $pids 2>/dev/null || true
	fi
}

rss_kib() {
	awk '/VmRSS:/ {print $2}' "/proc/$1/status" 2>/dev/null || echo 0
}

run_queries() {
	local dur="$1" log="$2"
	local end=$((SECONDS + dur))
	local ok=0 fail=0
	while (( SECONDS < end )); do
		local now
		now=$(date +%s)
		local start=$((now - 30))
		if curl -sS -m 5 -G "http://127.0.0.1:${PROM_PORT}/api/v1/query_range" \
			--data-urlencode 'query=prometheus_tsdb_head_series' \
			--data-urlencode "start=${start}" \
			--data-urlencode "end=${now}" \
			--data-urlencode 'step=1' \
			-o /dev/null; then
			ok=$((ok + 1))
		else
			fail=$((fail + 1))
		fi
	done
	echo "queries_ok=$ok queries_fail=$fail" | tee "$log"
}

run_arm() {
	local label="$1" prom="$2" ava="$3"
	local prefix="$OUT/$label"
	echo "==> $label"

	kill_port "$PROM_PORT"
	kill_port "$AVA_PORT"
	rm -rf "$prefix.data"
	mkdir -p "$prefix.data"

	write_prom_yml

	"$ava" \
		--port="$AVA_PORT" \
		--gauge-metric-count=200 \
		--series-count=50 \
		--label-count=8 \
		--value-interval=5 \
		--series-interval=0 \
		--metric-interval=0 \
		>"$prefix.avalanche.log" 2>&1 &
	local ava_pid=$!

	local wrap=()
	if [[ "$label" == membind ]]; then
		wrap=(numactl --membind=0,1)
	fi
	"${wrap[@]}" "$prom" \
		--config.file="$OUT/prometheus.yml" \
		--storage.tsdb.path="$prefix.data" \
		--web.listen-address="127.0.0.1:${PROM_PORT}" \
		--storage.tsdb.retention.time=2h \
		>"$prefix.prometheus.log" 2>&1 &
	local prom_pid=$!

	local ready=0
	for _ in $(seq 1 30); do
		if curl -sf "http://127.0.0.1:${PROM_PORT}/-/ready" >/dev/null 2>&1; then
			ready=1
			break
		fi
		sleep 1
	done
	sleep "$WARM"
	if [[ "$ready" != 1 ]] || ! kill -0 "$prom_pid" 2>/dev/null; then
		echo "prometheus died" >&2
		tail -40 "$prefix.prometheus.log" >&2
		kill "$ava_pid" 2>/dev/null || true
		return 1
	fi

	vmstat_snap "$prefix.vmstat.before"
	local rss_before
	rss_before=$(rss_kib "$prom_pid")

	perf stat -x, -e "$EVENTS" -p "$prom_pid" -- sleep "$RUN" \
		>"$prefix.perf" 2>&1 &
	local perf_pid=$!

	# query load in parallel with the IMC window
	run_queries "$RUN" "$prefix.queries" &
	local q_pid=$!
	wait "$q_pid" || true
	wait "$perf_pid" || true

	local rss_after
	rss_after=$(rss_kib "$prom_pid")
	vmstat_snap "$prefix.vmstat.after"

	{
		echo "--- $label ---"
		echo "prom_pid=$prom_pid ava_pid=$ava_pid"
		echo "rss_kib_before=$rss_before rss_kib_after=$rss_after"
		echo "vmstat:"
		vmstat_delta "$prefix.vmstat.before" "$prefix.vmstat.after"
		echo "perf:"
		grep -E 'local_dram|remote_dram' "$prefix.perf" || cat "$prefix.perf"
		echo "queries:"
		cat "$prefix.queries"
		echo
	} | tee -a "$OUT/summary.txt"

	kill "$prom_pid" "$ava_pid" 2>/dev/null || true
	wait "$prom_pid" "$ava_pid" 2>/dev/null || true
	sleep 2
}

: >"$OUT/summary.txt"
run_arm baseline "$BIN/prometheus-baseline" "$BIN/avalanche-baseline"
run_arm numa "$BIN/prometheus-numa" "$BIN/avalanche-numa"
run_arm membind "$BIN/prometheus-baseline" "$BIN/avalanche-baseline"

echo "==> done. $OUT/summary.txt"
cat "$OUT/summary.txt"
