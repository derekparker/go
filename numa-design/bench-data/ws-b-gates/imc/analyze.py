#!/usr/bin/env python3
"""Task 11 Step 3 IMC decision gate: remote/(local+remote) share per run,
medians per arm, relative change. Reads perf stat -x, CSV files
(mem_load_l3_miss_retired.local_dram, .remote_dram)."""
import sys, glob, re, statistics

def read_csv(path):
    local = remote = None
    for line in open(path):
        line = line.strip()
        if not line or line.startswith('#'):
            continue
        fields = line.split(',')
        count = int(fields[0])
        event = fields[2]
        if 'local_dram' in event:
            local = count
        elif 'remote_dram' in event:
            remote = count
    return local, remote

def shares(pattern):
    files = sorted(glob.glob(pattern))
    out = []
    for f in files:
        l, r = read_csv(f)
        out.append((f, l, r, r/(l+r)))
    return out

base = shares('baseline-run*.csv')
numa = shares('numa-run*.csv')
print("baseline runs:")
for f, l, r, s in base:
    print(f"  {f}: local={l} remote={r} share={s*100:.2f}%")
print("numa runs:")
for f, l, r, s in numa:
    print(f"  {f}: local={l} remote={r} share={s*100:.2f}%")

bmed = statistics.median(s for _,_,_,s in base)
nmed = statistics.median(s for _,_,_,s in numa)
rel = (nmed - bmed) / bmed
print(f"\nbaseline median remote share: {bmed*100:.2f}%")
print(f"numa median remote share:     {nmed*100:.2f}%")
print(f"relative change: {rel*100:+.2f}%  (pass bar: <= -10.00%)")
print(f"verdict: {'PASS' if rel <= -0.10 else 'FAIL'}")

from math import comb
bs = sorted(s for _,_,_,s in base)
ns = sorted(s for _,_,_,s in numa)
if max(ns) < min(bs) or max(bs) < min(ns):
    p = 2 * 1 / comb(len(bs)+len(ns), len(bs))
    print(f"\ncomplete separation between arms; exact two-sided MWU p = {p:.5f}")
