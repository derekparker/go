#!/usr/bin/env python3
"""Paired sign test for the C-vs-A comparison (round-paired, since A and C
ran in the same rotating-order rounds within one session). Complements
noninf_ci.py's log-ratio CI: the CI answers "is C bounded within +10% of
A"; the sign test answers "does C tend to run on the same side of A round
after round, even if the aggregate ratio is small and not significant."

Usage: sign_test.py A_valfile C_valfile
"""
import sys
from math import comb

def main():
    a = [float(x) for x in open(sys.argv[1])]
    c = [float(x) for x in open(sys.argv[2])]
    n = len(a)
    c_slower = sum(1 for i in range(n) if c[i] > a[i])
    c_faster = sum(1 for i in range(n) if c[i] < a[i])
    ties = n - c_slower - c_faster
    print(f"n={n} C slower than A in {c_slower} rounds, C faster in {c_faster} rounds, ties={ties}")
    k = max(c_slower, c_faster)
    p = min(1.0, sum(comb(n, i) for i in range(k, n + 1)) * 2 / (2 ** n))
    print(f"exact two-sided sign test p = {p:.4f}")

if __name__ == "__main__":
    main()
