#!/usr/bin/env bash
# Compare json baseline vs GOEXPERIMENT=numa. Fail if numa is >2% slower
# on ns/op OR user+sys-ns/op (median of NUM interleaved runs).
set -euo pipefail
GOROOT="${GOROOT:-$PWD}"
export PATH="$GOROOT/bin:$PATH" GOTOOLCHAIN=local
PROCS="${GOMAXPROCS:-1}"
MEM="${BENCHMEM:-512}"
NUM="${BENCHNUM:-3}"
TIME="${BENCHTIME:-3s}"
OUT="${OUT:-/tmp/numa-gate-json}"
BAND="${BAND:-0.02}"
# Pin x/benchmarks (record the resolved version in RESULTS.md);
# @latest is a moving target and a network dependency per run.
JSON_PKG="golang.org/x/benchmarks/json@${BENCH_REV:-latest}"

mkdir -p "$OUT/baseline" "$OUT/numa"
export GOROOT
GOBIN="$OUT/baseline" go install "$JSON_PKG"
GOBIN="$OUT/numa" GOEXPERIMENT=numa go install "$JSON_PKG"

run() {
	local bin="$1" file="$2"
	env GOMAXPROCS="$PROCS" "$bin" -benchmem="$MEM" -benchnum=1 -benchtime="$TIME" >>"$file"
}

: >"$OUT/baseline.out"; : >"$OUT/numa.out"
# Interleave the arms (A/B, A/B, ...) so thermal / frequency / cache
# drift hits both equally instead of biasing the second arm.
for _ in $(seq "$NUM"); do
	run "$OUT/baseline/json" "$OUT/baseline.out"
	run "$OUT/numa/json" "$OUT/numa.out"
done
echo "=== baseline ==="; cat "$OUT/baseline.out"
echo "=== numa ==="; cat "$OUT/numa.out"

python3 - "$OUT/baseline.out" "$OUT/numa.out" "$BAND" <<'PY'
import re, sys, statistics
NUMV = r"(\d+(?:\.\d+)?)"  # benchfmt values are not always integers
def metrics(path):
    ns, usys = [], []
    for line in open(path):
        if not line.startswith("Benchmark"):
            continue
        m = re.search(r"Benchmark\S+\s+\d+\s+" + NUMV + r"\s+ns/op", line)
        if m:
            ns.append(float(m.group(1)))
        u = re.search(NUMV + r"\s+user\+sys-ns/op", line)
        if u:
            usys.append(float(u.group(1)))
    return ns, usys

band = float(sys.argv[3])
bns, bus = metrics(sys.argv[1])
nns, nus = metrics(sys.argv[2])
if not bns or not nns:
    sys.exit("missing Benchmark lines")
def chk(name, b, n):
    mb, mn = statistics.median(b), statistics.median(n)
    rel = (mn - mb) / mb
    print(f"{name}: baseline {mb} numa {mn} rel {rel:+.1%}")
    if rel > band:
        print(f"FAIL {name} exceeds +{band:.0%}")
        return 1
    return 0
rc = chk("ns/op", bns, nns)
if bus and nus:
    rc |= chk("user+sys-ns/op", bus, nus)
sys.exit(rc)
PY
