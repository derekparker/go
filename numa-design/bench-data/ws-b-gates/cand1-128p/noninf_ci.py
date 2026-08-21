#!/usr/bin/env python3
"""Non-inferiority CI for the C-vs-A ratio, computed round-by-round (paired
by round index within the single-session rotating-order sweep), per the
task-4 brief's I1 criterion: PASS iff the 95% CI upper bound of C/A - 1 is
<= +10%.

Method: log-ratio + t-distribution CI (paired per round). For each round r
(recorded rounds only, excludes warmup round 0), extract the arm's ns/op
value (single -benchnum=1 sample per round per arm) or, for round-level
data (candidate 2), the round median. Compute log(C_r / A_r) per round,
then a two-sided 95% CI on the mean log-ratio via the t-distribution,
exponentiate back.

Usage: noninf_ci.py A_valfile C_valfile
  where each file has one numeric value per line, in matching round order.
"""
import sys, math
import statistics

def read_vals(path):
    vals = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            vals.append(float(line))
    return vals

def t_crit_95(df):
    # Two-sided 95% critical t-values for small df (table lookup;
    # sufficient for n=10..15 rounds => df=9..14).
    table = {
        5: 2.571, 6: 2.447, 7: 2.365, 8: 2.306, 9: 2.262,
        10: 2.228, 11: 2.201, 12: 2.179, 13: 2.160, 14: 2.145,
        15: 2.131, 16: 2.120, 17: 2.110, 18: 2.101, 19: 2.093,
        20: 2.086,
    }
    if df in table:
        return table[df]
    # fallback: normal approx for larger df
    return 1.960

def main():
    a_path, c_path = sys.argv[1], sys.argv[2]
    a = read_vals(a_path)
    c = read_vals(c_path)
    if len(a) != len(c):
        sys.exit(f"length mismatch: A={len(a)} C={len(c)}")
    n = len(a)
    log_ratios = [math.log(c[i] / a[i]) for i in range(n)]
    mean_lr = statistics.mean(log_ratios)
    sd_lr = statistics.stdev(log_ratios)  # sample stdev, ddof=1
    se = sd_lr / math.sqrt(n)
    df = n - 1
    tcrit = t_crit_95(df)
    lo = mean_lr - tcrit * se
    hi = mean_lr + tcrit * se
    point = math.exp(mean_lr) - 1
    ci_lo = math.exp(lo) - 1
    ci_hi = math.exp(hi) - 1
    print(f"n={n} point_estimate_C/A-1={point:+.2%} 95%CI=[{ci_lo:+.2%}, {ci_hi:+.2%}]")
    print(f"Non-inferiority (upper bound <= +10%): {'PASS' if ci_hi <= 0.10 else 'FAIL'}")

if __name__ == "__main__":
    main()
