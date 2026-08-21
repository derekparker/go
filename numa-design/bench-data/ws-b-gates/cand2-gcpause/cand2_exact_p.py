import statistics
D = "/home/deparker/Code/golang/go/.claude/worktrees/numa-dev/.claude/worktrees/numa-v2-implementation-a6eb4c/numa-design/bench-data/ws-b-gates"
from itertools import combinations

def read_cycles(path):
    with open(path) as f:
        return [float(l.strip()) for l in f if l.strip()]

def round_medians(cycles, k=8):
    rounds = [cycles[i:i+k] for i in range(0, len(cycles), k)]
    return [statistics.median(r) for r in rounds]

def exact_mwu_full_enum(a, b):
    na, nb = len(a), len(b)
    combined = a + b
    idx = list(range(len(combined)))
    obs_ra = sum(sorted(combined).index(v)+1 for v in a)  # not tie-safe but fine here (unlikely exact ties)
    # proper: use rank via sorted combined with average ranks for ties
    sorted_vals = sorted(combined)
    ranks = {}
    i = 0
    n = len(sorted_vals)
    while i < n:
        j = i
        while j < n and sorted_vals[j] == sorted_vals[i]:
            j += 1
        avg_rank = (i+1+j)/2.0
        for k in range(i,j):
            ranks.setdefault(sorted_vals[k], []).append(avg_rank)
        i = j
    pools = {v: list(r) for v, r in ranks.items()}
    def take(v):
        return pools[v].pop(0)
    ra = sum(take(v) for v in a)
    u1 = ra - na*(na+1)/2.0
    total = 0
    count_le = 0
    for comb_idx in combinations(range(na+nb), na):
        total += 1
    # full enumeration over which values go to group a (by position in combined value list)
    # simpler: enumerate all subsets of size na from combined VALUES (index-based to handle duplicates)
    all_u1 = []
    for comb_idx in combinations(range(na+nb), na):
        group_a = [combined[i] for i in comb_idx]
        group_b = [combined[i] for i in range(na+nb) if i not in comb_idx]
        # ranks already computed above for full set; recompute rank sum via position trick
        # easier: use rank list aligned to combined by original index computed once
        pass
    return u1, total

a = round_medians(read_cycles(D+"/cand2-gcpause/A-cycles.txt"))
c = round_medians(read_cycles(D+"/cand2-gcpause/C-cycles.txt"))
print("n(a)=",len(a),"n(c)=",len(c))

# Full enumeration exact MWU p-value (two-sided), tie-aware via ranks computed once.
def exact_mwu_p(a, b):
    na, nb = len(a), len(b)
    combined = list(a) + list(b)
    n = na+nb
    sorted_vals = sorted(combined)
    # assign average ranks
    rank_of_index = [0.0]*n  # rank per position in `combined` after we map values->ranks with duplicate handling via multiset
    # Build rank per VALUE occurrence using a stable assignment
    from collections import defaultdict
    value_positions = defaultdict(list)
    for idx, v in enumerate(sorted_vals):
        value_positions[v].append(idx)
    ranks_by_value = {}
    i = 0
    while i < n:
        j = i
        while j < n and sorted_vals[j] == sorted_vals[i]:
            j += 1
        avg_rank = (i+1+j)/2.0
        ranks_by_value[sorted_vals[i]] = avg_rank
        i = j
    # observed U1
    obs_ra = sum(ranks_by_value[v] for v in a)
    obs_u1 = obs_ra - na*(na+1)/2.0
    obs_u2 = na*nb - obs_u1
    obs_u = min(obs_u1, obs_u2)

    # enumerate all combinations of na indices (into combined list, 0..n-1) as "group a"
    count_extreme = 0
    total = 0
    idxs = list(range(n))
    for comb_idx in combinations(idxs, na):
        total += 1
        ra = sum(ranks_by_value[combined[i]] for i in comb_idx)
        u1 = ra - na*(na+1)/2.0
        u2 = na*nb - u1
        u = min(u1, u2)
        if u <= obs_u + 1e-9:
            count_extreme += 1
    p = count_extreme/total
    return obs_u1, obs_u2, p, total

u1, u2, p, total = exact_mwu_p(a, c)
print(f"exact MWU (full enumeration): U1={u1} U2={u2} p={p:.4f}  total_combos={total}")
