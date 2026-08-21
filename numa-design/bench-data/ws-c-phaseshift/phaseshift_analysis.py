#!/usr/bin/env python3
"""Task 12 analysis: phase-shift mechanism study.

Primary: exact Mann-Whitney U (full enumeration), exact Wilcoxon
signed-rank (paired by round index), exact paired sign test, and
leave-one-out exact Mann-Whitney U (each round dropped in turn) on the
n=20 B-vs-C ns/read samples.

Mechanism: parses /proc/self/numa_maps snapshots from the designated
capture rounds and computes node-balance = min(N0,N1)/(N0+N1) summed
over all anonymous VMAs, per (arm, round, phase, tag) snapshot. Reports
the pre-registered round-level Spearman correlation (arm C, n=3,
disclosed as underpowered), the descriptive start-vs-end movement per
arm, and the migration-traffic-vs-bandwidth budget check (n=20, arm B).

No scipy/numpy dependency (matches numa-design/bench-data/wsA-cand1,
wsA-cand2's existing analysis scripts on this host, which has no
scipy). Pure stdlib.
"""
import sys
import os
import re
import glob
import math
import statistics
from math import comb, erf
from functools import lru_cache


# --- primary stats -----------------------------------------------------

def read_ns_per_read(path):
    """Extract the ns/op values (one per recorded run) from a
    pathology-sweep.sh accumulated .out file: each run appends one
    'BenchmarkPhaseChase 1 <ns> ns/op' line, in round order."""
    vals = []
    with open(path) as f:
        for line in f:
            m = re.match(r"BenchmarkPhaseChase\s+1\s+([0-9.eE+-]+)\s+ns/op", line)
            if m:
                vals.append(float(m.group(1)))
    return vals


def exact_mwu_full_enum(a, b):
    """Exact two-sided Mann-Whitney U p-value via full enumeration of the
    null U distribution (assumes no ties)."""
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
    count.cache_clear()
    return ua, ub, p_two


def sign_test(a, b):
    """Exact two-sided paired sign test. a, b same length, paired by index."""
    n = len(a)
    b_lt_a = sum(1 for i in range(n) if b[i] < a[i])
    b_gt_a = sum(1 for i in range(n) if b[i] > a[i])
    ties = n - b_lt_a - b_gt_a
    k = max(b_lt_a, b_gt_a)
    neff = b_lt_a + b_gt_a
    if neff == 0:
        return b_lt_a, b_gt_a, ties, 1.0
    p = min(1.0, sum(comb(neff, i) for i in range(k, neff + 1)) * 2 / (2 ** neff))
    return b_lt_a, b_gt_a, ties, p


def wilcoxon_exact(a, b):
    """Exact two-sided Wilcoxon signed-rank test, paired by index.
    Ranks |diff| (average ranks for ties in magnitude, zeros dropped),
    then computes the exact null distribution of W+ (sum of ranks of
    positive diffs) via generating-function DP: product over ranks r of
    (1 + x^r), counting subsets summing to each possible total."""
    diffs = [b[i] - a[i] for i in range(len(a))]
    nz = [d for d in diffs if d != 0]
    n = len(nz)
    absd = sorted(range(n), key=lambda i: abs(nz[i]))
    # average ranks for ties in |diff|
    mags = sorted(abs(nz[i]) for i in range(n))
    rank_of_mag = {}
    idx = 0
    while idx < n:
        j = idx
        while j < n and mags[j] == mags[idx]:
            j += 1
        avg_rank = (idx + 1 + j) / 2.0
        rank_of_mag[mags[idx]] = avg_rank  # last-write is fine: ties share the same avg rank
        idx = j
    # assign ranks to each diff by magnitude (duplicates get the shared avg rank)
    ranks = [rank_of_mag[abs(d)] for d in nz]
    wplus = sum(r for r, d in zip(ranks, nz) if d > 0)

    # DP over achievable sums; ranks may be non-integer (tie averages), so
    # scale by 2 to keep everything integral (average ranks are always
    # multiples of 0.5).
    scaled = [round(r * 2) for r in ranks]
    maxsum = sum(scaled)
    dp = [0] * (maxsum + 1)
    dp[0] = 1
    for r in scaled:
        for s in range(maxsum, r - 1, -1):
            dp[s] += dp[s - r]
    total = 2 ** n if n <= 30 else sum(dp)
    wplus_scaled = round(wplus * 2)
    wminus_scaled = maxsum - wplus_scaled
    wmin_scaled = min(wplus_scaled, wminus_scaled)
    cum = sum(dp[: wmin_scaled + 1])
    p_two = min(1.0, 2 * cum / total)
    return wplus, maxsum / 2.0 - wplus, p_two, n


def leave_one_out(a, b):
    """Exact MWU p-value with each paired round dropped in turn."""
    n = len(a)
    ps = []
    for i in range(n):
        a2 = a[:i] + a[i + 1 :]
        b2 = b[:i] + b[i + 1 :]
        _, _, p = exact_mwu_full_enum(a2, b2)
        ps.append(p)
    return ps


# --- mechanism analysis --------------------------------------------------

def parse_numamaps(path):
    """Sum N0/N1 (etc.) over all anonymous VMAs in a numa_maps snapshot.
    Returns dict {node_index: pages}."""
    totals = {}
    with open(path) as f:
        for line in f:
            if "anon=" not in line:
                continue
            for m in re.finditer(r"\bN(\d+)=(\d+)\b", line):
                node = int(m.group(1))
                pages = int(m.group(2))
                totals[node] = totals.get(node, 0) + pages
    return totals


def node_balance(totals):
    if not totals:
        return None
    vals = list(totals.values())
    total = sum(vals)
    if total == 0:
        return None
    return min(vals) / total if len(vals) > 1 else 0.0


def spearman(xs, ys):
    n = len(xs)
    if n < 2:
        return None, None

    def ranks(vals):
        order = sorted(range(len(vals)), key=lambda i: vals[i])
        r = [0.0] * len(vals)
        i = 0
        while i < len(order):
            j = i
            while j < len(order) and vals[order[j]] == vals[order[i]]:
                j += 1
            avg = (i + 1 + j) / 2.0
            for k in range(i, j):
                r[order[k]] = avg
            i = j
        return r

    rx, ry = ranks(xs), ranks(ys)
    d2 = sum((rx[i] - ry[i]) ** 2 for i in range(n))
    rho = 1 - (6 * d2) / (n * (n * n - 1)) if n > 1 else 0.0

    if n <= 9:
        # Exact two-sided p via full permutation enumeration of the null
        # (every ranking of ys against a fixed ranking of xs equally
        # likely). Small n only (n! grows fast); this is the honest choice
        # at n=3 -- the Fisher z-transform normal approximation used below
        # is invalid there (its own derivation assumes n large enough for
        # asymptotic normality).
        from itertools import permutations

        base = tuple(rx)
        obs = abs(rho)
        total = 0
        as_extreme = 0
        for perm in permutations(ry):
            total += 1
            d2p = sum((base[i] - perm[i]) ** 2 for i in range(n))
            rho_p = 1 - (6 * d2p) / (n * (n * n - 1))
            if abs(rho_p) >= obs - 1e-9:
                as_extreme += 1
        p_exact = as_extreme / total
        return rho, p_exact

    z = math.atanh(max(min(rho, 0.999999), -0.999999)) * math.sqrt(n - 3)
    p = 2 * (1 - 0.5 * (1 + erf(abs(z) / math.sqrt(2))))
    return rho, p


def gbps(ns_per_read):
    return 64.0 / ns_per_read


def read_vmstat_summary(path):
    """Parse a pathology-sweep.sh vmstat-summary.txt line format:
    '<tag>: hint_faults=<h> pages_migrated=<m>' -> {tag: (h, m)}"""
    out = {}
    with open(path) as f:
        for line in f:
            m = re.match(r"(\S+):\s+hint_faults=(-?\d+)\s+pages_migrated=(-?\d+)", line)
            if m:
                out[m.group(1)] = (int(m.group(2)), int(m.group(3)))
    return out


def main():
    if len(sys.argv) < 4:
        print("usage: phaseshift_analysis.py OUTDIR B_RECORDED_OUT C_RECORDED_OUT "
              "[--numamaps CAPTURE_DIR]", file=sys.stderr)
        sys.exit(2)
    outdir, b_path, c_path = sys.argv[1:4]

    b = read_ns_per_read(b_path)
    c = read_ns_per_read(c_path)
    n = len(b)
    assert len(c) == n, f"arm sample count mismatch: B={len(b)} C={len(c)}"
    print(f"=== Primary: ns/read, B vs C, n={n} ===")
    mb, mc = statistics.median(b), statistics.median(c)
    meanb, meanc = statistics.mean(b), statistics.mean(c)
    print(f"B: median={mb:.4f} mean={meanb:.4f} min={min(b):.4f} max={max(b):.4f}")
    print(f"C: median={mc:.4f} mean={meanc:.4f} min={min(c):.4f} max={max(c):.4f}")
    print(f"relative (median, C vs B): {(mc-mb)/mb:+.2%}")
    print(f"relative (mean, C vs B):   {(meanc-meanb)/meanb:+.2%}")

    ua, ub, p_mwu = exact_mwu_full_enum(b, c)
    print(f"\nExact Mann-Whitney U: U(B)={ua} U(C)={ub} p={p_mwu:.4g}  "
          f"{'SIGNIFICANT' if p_mwu < 0.05 else 'not significant'} (alpha=0.05)")

    wplus, wminus, p_wil, nz = wilcoxon_exact(b, c)
    print(f"Exact Wilcoxon signed-rank (paired by round): W+={wplus} W-={wminus} "
          f"n(nonzero diffs)={nz} p={p_wil:.4g}  "
          f"{'SIGNIFICANT' if p_wil < 0.05 else 'not significant'}")

    lt, gt, ties, p_sign = sign_test(b, c)
    print(f"Exact paired sign test: C<B in {lt} rounds, C>B in {gt} rounds, ties={ties} "
          f"p={p_sign:.4g}  {'SIGNIFICANT' if p_sign < 0.05 else 'not significant'}")

    loo = leave_one_out(b, c)
    print(f"Leave-one-out exact MWU (n-1={n-1} each): p range [{min(loo):.4g}, {max(loo):.4g}], "
          f"{sum(1 for p in loo if p < 0.05)}/{n} subsets significant at alpha=0.05")

    print(f"\nImplied bandwidth (GB/s = 64/ns_per_read):")
    bgb = [gbps(v) for v in b]
    cgb = [gbps(v) for v in c]
    print(f"B: min={min(bgb):.1f} max={max(bgb):.1f} mean={statistics.mean(bgb):.1f}")
    print(f"C: min={min(cgb):.1f} max={max(cgb):.1f} mean={statistics.mean(cgb):.1f}")
    print(f"Reference single-controller ceiling (arm A, original candidate 3 pilot): ~31.0 GB/s")
    print(f"B rounds at/below 31.5 GB/s (near-ceiling): {sum(1 for v in bgb if v <= 31.5)}/{n}")
    print(f"C rounds at/below 31.5 GB/s (near-ceiling): {sum(1 for v in cgb if v <= 31.5)}/{n}")

    # migration-traffic-vs-bandwidth budget, arm B, using vmstat summary if given
    vsum_path = os.path.join(outdir, "wsC-phase-vmstat-summary.txt")
    if os.path.exists(vsum_path):
        summ = read_vmstat_summary(vsum_path)
        # collect all armB recorded rounds (r1..rN, not r0/warmup)
        armB_recorded = []
        for tag, (h, m) in summ.items():
            mm = re.search(r"armB-r(\d+)$", tag)
            if mm and int(mm.group(1)) >= 1:
                armB_recorded.append((int(mm.group(1)), h, m))
        armB_recorded.sort()
        if armB_recorded:
            mig_vals = [m for _, _, m in armB_recorded]
            mean_mig = statistics.mean(mig_vals)
            phase_total_s = 4 * 30.0  # -phase=30 -phases=4
            mig_bw_gbs = mean_mig * 4096 * 2 / phase_total_s / 1e9
            print(f"\nArm B migration-traffic budget: mean pages_migrated/round={mean_mig:.0f} "
                  f"-> {mig_bw_gbs:.3f} GB/s two-way migration traffic "
                  f"({mig_bw_gbs/statistics.mean(bgb):.2%} of B's own mean demand bandwidth)")
        else:
            print("\n(no armB recorded-round vmstat entries found for migration budget)")
    else:
        print(f"\n(vmstat summary not found at {vsum_path}; skipping migration budget)")

    # mechanism: numa_maps capture rounds
    if len(sys.argv) >= 6 and sys.argv[4] == "--numamaps":
        capture_dir = sys.argv[5]
        analyze_capture(capture_dir)


def read_capture_ns(capture_dir, arm, rnd):
    path = os.path.join(capture_dir, f"wsC-phase-capture-arm{arm}-r{rnd}.out")
    if not os.path.exists(path):
        return None
    vals = read_ns_per_read(path)
    return vals[0] if vals else None


def analyze_capture(capture_dir):
    print(f"\n=== Mechanism: numa_maps capture rounds ({capture_dir}) ===")
    files = sorted(glob.glob(os.path.join(capture_dir, "*.numamaps")))
    if not files:
        print("no .numamaps files found")
        return
    # filename convention: <prefix>-armX-rN-phaseP-tag.numamaps
    pat = re.compile(r"arm(?P<arm>[BC])-r(?P<round>\d+)-phase(?P<phase>\d+)-(?P<tag>start|mid|end)\.numamaps$")
    rows = []
    for path in files:
        m = pat.search(os.path.basename(path))
        if not m:
            print(f"(skipping unrecognized filename: {path})")
            continue
        totals = parse_numamaps(path)
        nb = node_balance(totals)
        rows.append((m.group("arm"), int(m.group("round")), int(m.group("phase")), m.group("tag"), totals, nb))

    print(f"{'arm':<4}{'round':<7}{'phase':<7}{'tag':<7}{'N0':<10}{'N1':<10}{'balance':<10}")
    for arm, rnd, phase, tag, totals, nb in sorted(rows):
        n0 = totals.get(0, 0)
        n1 = totals.get(1, 0)
        print(f"{arm:<4}{rnd:<7}{phase:<7}{tag:<7}{n0:<10}{n1:<10}{'' if nb is None else f'{nb:.4f}'}")

    # descriptive: start-vs-end movement per arm per phase
    print("\n--- descriptive: start-vs-end node-balance movement per arm/phase ---")
    by_key = {}
    for arm, rnd, phase, tag, totals, nb in rows:
        by_key.setdefault((arm, rnd, phase), {})[tag] = nb
    for arm in ("B", "C"):
        deltas = []
        for (a, rnd, phase), tags in sorted(by_key.items()):
            if a != arm:
                continue
            if "start" in tags and "end" in tags and tags["start"] is not None and tags["end"] is not None:
                d = tags["end"] - tags["start"]
                deltas.append(d)
                print(f"  arm={arm} round={rnd} phase={phase}: start={tags['start']:.4f} "
                      f"end={tags['end']:.4f} delta={d:+.4f}")
        if deltas:
            toward_skew = sum(1 for d in deltas if d < 0)
            toward_spread = sum(1 for d in deltas if d > 0)
            print(f"  arm={arm}: {toward_skew}/{len(deltas)} phases moved toward consolidation "
                  f"(end < start), {toward_spread}/{len(deltas)} moved toward spread, "
                  f"mean delta={statistics.mean(deltas):+.4f}")

    # descriptive: overall node-balance distribution, all snapshots and the
    # pre-registered phases-1-3-only subset (phase 0's "start" snapshot is
    # pre-touch, before any pinned reader has run -- excluded from the
    # steady-state reading per the pre-registration)
    print("\n--- descriptive: node-balance distribution ---")
    for label, keep in (("all phases (0-3)", lambda p: True), ("phases 1-3 only (pre-registered)", lambda p: p in (1, 2, 3))):
        for arm in ("B", "C"):
            vals = [nb for a, rnd, phase, tag, totals, nb in rows if a == arm and keep(phase) and nb is not None]
            if vals:
                print(f"  [{label}] arm={arm}: n={len(vals)} mean={statistics.mean(vals):.4f} "
                      f"min={min(vals):.4f} max={max(vals):.4f} "
                      f"stdev={statistics.stdev(vals) if len(vals) > 1 else 0.0:.4f}")

    # pre-registered primary attribution test: arm C only, round-level mean
    # node-balance (phases 1-3, all snapshots) vs that round's overall
    # ns/read, n=3 (one point per capture round).
    print("\n--- pre-registered primary attribution test (arm C, round-level, n=3) ---")
    round_points = []
    for rnd in sorted({r for a, r, p, t, tot, nb in rows if a == "C"}):
        vals = [nb for a, r2, p, t, tot, nb in rows if a == "C" and r2 == rnd and p in (1, 2, 3) and nb is not None]
        ns = read_capture_ns(capture_dir, "C", rnd)
        if vals and ns is not None:
            mean_bal = statistics.mean(vals)
            round_points.append((rnd, mean_bal, ns))
            print(f"  round={rnd}: mean node-balance (phases 1-3)={mean_bal:.4f}  ns/read={ns:.4f}")
    if len(round_points) >= 2:
        bals = [p[1] for p in round_points]
        nss = [p[2] for p in round_points]
        rho, p_exact = spearman(bals, nss)
        print(f"  Spearman(node-balance, ns/read): rho={rho:+.4f}  "
              f"exact p={'n/a' if p_exact is None else f'{p_exact:.4g}'}  n={len(round_points)}")
        print(f"  H-bandwidth predicts rho<0 (higher balance -> lower ns/read, i.e. faster). "
              f"Observed rho={rho:+.4f}: "
              f"{'consistent with H-bandwidth direction' if rho < 0 else 'NOT consistent with H-bandwidth direction'}.")
        print(f"  Disclosed limitation (pre-registered): n={len(round_points)} gives essentially no power "
              f"(a two-tailed exact test at n=3 cannot reach p<0.05 even at |rho|=1); reported as pre-declared, "
              f"not read as decisive on its own.")
    else:
        print("  (fewer than 2 capture rounds with both placement and ns/read data -- cannot correlate)")


if __name__ == "__main__":
    main()
