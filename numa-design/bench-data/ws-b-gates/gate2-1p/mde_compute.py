import re, statistics, math, random
from itertools import combinations

def read_ns(path):
    vals=[]
    for line in open(path):
        m = re.search(r"Benchmark\S+\s+\d+\s+(\d+(?:\.\d+)?)\s+ns/op", line)
        if m: vals.append(float(m.group(1)))
    return vals

def cv(vals):
    return statistics.stdev(vals)/statistics.mean(vals)

def mde(pooled_cv, n, z_total=2.80):
    return z_total*pooled_cv*math.sqrt(2/n)

D = "/home/deparker/Code/golang/go/.claude/worktrees/numa-dev/.claude/worktrees/numa-v2-implementation-a6eb4c/numa-design/bench-data/ws-b-gates"
# --- Gate 2a MDE ---
b = read_ns(D+"/gate2-1p/baseline.out")
n = read_ns(D+"/gate2-1p/numa.out")
cvb, cvn = cv(b), cv(n)
pooled = math.sqrt((cvb**2+cvn**2)/2)
print(f"Gate2a: n(b)={len(b)} n(n)={len(n)} CV_b={cvb:.2%} CV_n={cvn:.2%} pooled={pooled:.2%} MDE(n=10)={mde(pooled,10):.2%}")

# --- Gate 2c pooled MDE ---
bp = read_ns(D+"/gate3-256p/wsB-gate3-pooled-baseline.out")
np_ = read_ns(D+"/gate3-256p/wsB-gate3-pooled-numa.out")
cvb2, cvn2 = cv(bp), cv(np_)
pooled2 = math.sqrt((cvb2**2+cvn2**2)/2)
print(f"Gate2c: n(b)={len(bp)} n(n)={len(np_)} CV_b={cvb2:.2%} CV_n={cvn2:.2%} pooled={pooled2:.2%} MDE(n=30)={mde(pooled2,30):.2%}")
medb, medn = statistics.median(bp), statistics.median(np_)
print(f"Gate2c sec/op medians: base={medb:.6g} numa={medn:.6g} rel={(medn-medb)/medb*100:+.2f}%")

