#!/usr/bin/env python3
"""Round-level analysis for Workstream A Gate 7 (candidate 2, gc-pause-bench).

Cycles within a round are clustered (same process, same warmed-up state);
the plan requires round-level analysis: median of the 8 cycles per round,
n=10 rounds per arm, exact Mann-Whitney U, report ICC and effective n.
"""
import sys, math, statistics
from itertools import combinations

def read_cycles(path):
    with open(path) as f:
        return [float(line.strip()) for line in f if line.strip()]

def round_medians(cycles, k=8):
    n = len(cycles)
    assert n % k == 0, f"{n} not divisible by {k}"
    rounds = [cycles[i:i+k] for i in range(0, n, k)]
    return [statistics.median(r) for r in rounds], rounds

def icc1(rounds):
    """One-way random-effects ICC(1) from clusters of equal size k."""
    k = len(rounds[0])
    n = len(rounds)
    all_vals = [v for r in rounds for v in r]
    grand_mean = statistics.mean(all_vals)
    ss_between = k * sum((statistics.mean(r) - grand_mean) ** 2 for r in rounds)
    ss_within = sum((v - statistics.mean(r)) ** 2 for r in rounds for v in r)
    df_between = n - 1
    df_within = n * (k - 1)
    ms_between = ss_between / df_between
    ms_within = ss_within / df_within if df_within > 0 else 0.0
    if ms_between + (k - 1) * ms_within == 0:
        return 0.0, ms_between, ms_within
    icc = (ms_between - ms_within) / (ms_between + (k - 1) * ms_within)
    return icc, ms_between, ms_within

def effective_n(n_rounds, k, icc):
    design_effect = 1 + (k - 1) * max(icc, 0)
    return (n_rounds * k) / design_effect

def normal_approx_mwu(a, b):
    """Mann-Whitney U with a NORMAL APPROXIMATION (tie-corrected variance,
    continuity correction), NOT an exact permutation p-value despite this
    function's earlier name (`exact_mwu`) in prior revisions of this
    script -- that name was wrong and is corrected here. This estimator is
    conservative (reports a larger p than the true exact test) for a
    fully-separated small-n comparison; see exact_mwu_full_enum() below
    for the true exact p, used to cross-check the primary B-vs-C result
    in RESULTS.md."""
    all_vals = sorted(a + b)
    ranks = {}
    # average ranks for ties
    i = 0
    n = len(all_vals)
    rank_of = [0.0]*n
    idx = 0
    while idx < n:
        j = idx
        while j < n and all_vals[j] == all_vals[idx]:
            j += 1
        avg_rank = (idx + 1 + j) / 2.0
        for m in range(idx, j):
            rank_of[m] = avg_rank
        idx = j
    val_to_rank = {}
    for v, r in zip(all_vals, rank_of):
        val_to_rank.setdefault(v, []).append(r)
    # assign ranks respecting duplicates via a pool
    pools = {v: list(rs) for v, rs in val_to_rank.items()}
    def take_rank(v):
        return pools[v].pop(0)
    ra = sum(take_rank(v) for v in a)
    na, nb = len(a), len(b)
    u1 = ra - na*(na+1)/2.0
    u2 = na*nb - u1
    u = min(u1, u2)
    # normal approximation with tie correction (standard, reliable for n=10/10)
    mu = na*nb/2.0
    # tie correction
    from collections import Counter
    tie_term = 0
    cnt = Counter(all_vals)
    for t in cnt.values():
        tie_term += t**3 - t
    sigma2 = (na*nb/12.0) * ((na+nb+1) - tie_term/((na+nb)*(na+nb-1)))
    sigma = math.sqrt(sigma2) if sigma2 > 0 else 1e-9
    z = (u - mu) / sigma
    # two-sided p from normal approx (continuity correction)
    if u < mu:
        z = (u + 0.5 - mu) / sigma
    else:
        z = (u - 0.5 - mu) / sigma
    from math import erf
    p = 2 * (1 - 0.5*(1+erf(abs(z)/math.sqrt(2))))
    return u1, u2, p

def exact_mwu_full_enum(a, b):
    """True exact two-sided Mann-Whitney U p-value via full enumeration of
    the null U distribution (assumes no ties, true for this session's
    round-median data at nanosecond precision). Uses the standard
    recursive count of arrangements achieving each U value; exact and
    correct for n,m up to a few dozen."""
    from functools import lru_cache
    from math import comb
    n, m = len(a), len(b)
    @lru_cache(maxsize=None)
    def count(u, nn, mm):
        if u < 0:
            return 0
        if nn == 0 or mm == 0:
            return 1 if u == 0 else 0
        return count(u - mm, nn - 1, mm) + count(u, nn, mm - 1)
    total = comb(n + m, n)
    combined = sorted(a + b)
    ranks = {v: i + 1 for i, v in enumerate(combined)}
    ra = sum(ranks[v] for v in a)
    ua = ra - n * (n + 1) / 2
    ub = n * m - ua
    umin = min(ua, ub)
    cum = sum(count(u, n, m) for u in range(0, int(umin) + 1))
    p_two = min(1.0, 2 * cum / total)
    return ua, ub, p_two

def cv(vals):
    m = statistics.mean(vals)
    s = statistics.stdev(vals)
    return s/m

def main():
    a_path, b_path, c_path = sys.argv[1], sys.argv[2], sys.argv[3]
    a_cyc = read_cycles(a_path)
    b_cyc = read_cycles(b_path)
    c_cyc = read_cycles(c_path)

    a_med, a_rounds = round_medians(a_cyc)
    b_med, b_rounds = round_medians(b_cyc)
    c_med, c_rounds = round_medians(c_cyc)

    print(f"n rounds: A={len(a_med)} B={len(b_med)} C={len(c_med)}")
    print(f"round medians (ns) A: {[f'{v/1e6:.1f}ms' for v in a_med]}")
    print(f"round medians (ns) B: {[f'{v/1e6:.1f}ms' for v in b_med]}")
    print(f"round medians (ns) C: {[f'{v/1e6:.1f}ms' for v in c_med]}")

    for name, rounds in [("A", a_rounds), ("B", b_rounds), ("C", c_rounds)]:
        icc, msb, msw = icc1(rounds)
        eff_n = effective_n(len(rounds), len(rounds[0]), icc)
        print(f"arm {name}: ICC={icc:.3f} (MSB={msb:.3e} MSW={msw:.3e}), effective_n={eff_n:.1f} (raw n_cycles={len(rounds)*len(rounds[0])}, n_rounds={len(rounds)})")

    print()
    print("=== Primary: B vs C (round medians, exact/normal-approx Mann-Whitney U) ===")
    u1, u2, p = normal_approx_mwu(b_med, c_med)
    mb, mc = statistics.median(b_med), statistics.median(c_med)
    print(f"B median-of-round-medians={mb/1e6:.2f}ms  C median-of-round-medians={mc/1e6:.2f}ms  rel={(mc-mb)/mb:+.2%}")
    print(f"normal-approx: U1={u1} U2={u2} p={p:.4f}  {'SIGNIFICANT' if p<0.05 else 'not significant'}")
    ue1, ue2, pe = exact_mwu_full_enum(b_med, c_med)
    print(f"exact (full enumeration): U1={ue1} U2={ue2} p={pe:.3e}  {'SIGNIFICANT' if pe<0.05 else 'not significant'}  (cross-check; normal-approx is conservative when fully separated)")
    print(f"CV: B={cv(b_med):.2%} C={cv(c_med):.2%}")

    print()
    print("=== Secondary/context: A vs B ===")
    u1, u2, p = normal_approx_mwu(a_med, b_med)
    ma = statistics.median(a_med)
    print(f"A median={ma/1e6:.2f}ms  B median={mb/1e6:.2f}ms  rel(B vs A)={(mb-ma)/ma:+.2%}")
    print(f"U1={u1} U2={u2} p={p:.4f}  {'SIGNIFICANT' if p<0.05 else 'not significant'}")

    print()
    print("=== C vs A (raw MWU, context) ===")
    u1, u2, p = normal_approx_mwu(a_med, c_med)
    print(f"A median={ma/1e6:.2f}ms  C median={mc/1e6:.2f}ms  rel(C vs A)={(mc-ma)/ma:+.2%}")
    print(f"U1={u1} U2={u2} p={p:.4f}  {'SIGNIFICANT' if p<0.05 else 'not significant'}")
    print(f"CV: A={cv(a_med):.2%}")

    # Achieved MDE from pooled CV (B vs C), n=10/arm, alpha=0.05, power=0.80
    # two-sample MDE (relative, %) approx: MDE = z_total * pooled_CV * sqrt(2/n)
    # z_total (alpha=0.05 two-sided, power=0.80) = 1.96+0.84 = 2.80
    pooled_cv = math.sqrt((cv(b_med)**2 + cv(c_med)**2)/2)
    n = len(b_med)
    z_total = 2.80
    mde = z_total * pooled_cv * math.sqrt(2/n)
    print()
    print(f"Pooled round-level CV (B,C): {pooled_cv:.2%}; achieved MDE at n={n}, alpha=0.05, power=0.80: {mde:.2%}")

if __name__ == "__main__":
    main()
