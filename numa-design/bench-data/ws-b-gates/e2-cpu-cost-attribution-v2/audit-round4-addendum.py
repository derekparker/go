#!/usr/bin/env python3
"""Audit round 4 addendum: absolute-cycle conversion, futex-symbol sum,
and the on-json.out/off-json.out capture-pair disclosure. Run from this
directory (needs perf-on-full.txt, perf-off-full.txt, on-json.out,
off-json.out already present)."""
import re

ON_TOTAL = 631060713114
OFF_TOTAL = 650593805987

groups_pct = {
    "spanSet/mcentral/sweep": (1.850, 5.180),
    "kernel spinlock (native_queued_spin_lock_slowpath)": (35.710, 23.640),
}
for name, (on_pct, off_pct) in groups_pct.items():
    on_abs = on_pct/100*ON_TOTAL
    off_abs = off_pct/100*OFF_TOTAL
    print(f"{name}: ON={on_abs:.3e} OFF={off_abs:.3e} delta={on_abs-off_abs:+.3e} "
          f"({(on_abs-off_abs)/off_abs*100:+.1f}% relative to OFF)")

def futex_sum(path):
    total = 0.0
    for line in open(path):
        if "futex" in line.lower():
            m = re.match(r"\s*([\d.]+)%", line)
            if m:
                total += float(m.group(1))
    return total

print(f"\nfutex symbols (all): ON={futex_sum('perf-on-full.txt'):.2f}% OFF={futex_sum('perf-off-full.txt'):.2f}%")

print(f"\ncycle totals: ON={ON_TOTAL:,} OFF={OFF_TOTAL:,} delta={(ON_TOTAL-OFF_TOTAL)/OFF_TOTAL*100:+.2f}%")

# capture-pair disclosure, read directly from on-json.out/off-json.out
def parse_bench_line(path):
    for line in open(path):
        if line.startswith("BenchmarkJSON"):
            fields = line.split()
            d = {}
            for i in range(1, len(fields)-1, 2):
                try:
                    val = float(fields[i])
                except ValueError:
                    continue
                unit = fields[i+1]
                d[unit] = val
            return d
    return {}

on_b = parse_bench_line("on-json.out")
off_b = parse_bench_line("off-json.out")
print("\ncapture-pair benchmark line comparison:")
for k in ("ns/op", "user+sys-ns/op", "allocs/op", "bytes-from-system", "peak-RSS-bytes"):
    if k in on_b and k in off_b:
        rel = (on_b[k]-off_b[k])/off_b[k]*100
        print(f"  {k}: ON={on_b[k]:.0f} OFF={off_b[k]:.0f} rel={rel:+.2f}%")
