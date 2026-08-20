#!/usr/bin/env bash
# Usage: gate-vmstat.sh snap FILE          (immediately before and after the run)
#        gate-vmstat.sh diff BEFORE AFTER  (exit 1 if either delta != 0)
set -euo pipefail
case "${1:-}" in
snap)
	grep -E '^(numa_hint_faults |numa_pages_migrated )' /proc/vmstat >"$2"
	;;
diff)
	python3 - "$2" "$3" <<'PY'
import sys
def p(path):
    d={}
    for line in open(path):
        k,v=line.split()
        d[k]=int(v)
    return d
b,a=p(sys.argv[1]),p(sys.argv[2])
h=a['numa_hint_faults']-b['numa_hint_faults']
m=a['numa_pages_migrated']-b['numa_pages_migrated']
print(f"hint_faults={h} pages_migrated={m}")
if h!=0 or m!=0:
    raise SystemExit(1)
PY
	;;
*)
	echo "usage: $0 snap FILE | diff BEFORE AFTER" >&2; exit 2
	;;
esac
