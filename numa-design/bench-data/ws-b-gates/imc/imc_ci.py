import statistics, math, random
from itertools import combinations

baseline = [(19069494,17759955),(74177335,69903196),(83589700,78705634),(26330712,24900195),(83692288,77441054)]
numa =     [(57423456,52966335),(57164562,48390709),(62303648,53761260),(62896187,50573858),(70788953,61592956)]

def share(l,r): return r/(l+r)
bshares = [share(l,r) for l,r in baseline]
nshares = [share(l,r) for l,r in numa]

bmean, nmean = statistics.mean(bshares), statistics.mean(nshares)
bstd, nstd = statistics.stdev(bshares), statistics.stdev(nshares)
bn, nn = len(bshares), len(nshares)

# Welch's t-test CI on relative drop = (nmean-bmean)/bmean, via delta method /
# direct simulation-free approach: compute CI on absolute diff (nmean-bmean)
# via Welch t, then express as relative to bmean (holding bmean fixed as the
# audit's own convention implies "relative-drop CI").
se = math.sqrt(bstd**2/bn + nstd**2/nn)
# Welch-Satterthwaite df
df = (bstd**2/bn + nstd**2/nn)**2 / ((bstd**2/bn)**2/(bn-1) + (nstd**2/nn)**2/(nn-1))
t_table = {4:2.776,5:2.571,6:2.447,7:2.365,8:2.306}
# round df down for a conservative table lookup
import bisect
dfi = max(1, int(df))
t_crit = t_table.get(dfi, 2.776)
diff = nmean - bmean
ci_abs = (diff - t_crit*se, diff + t_crit*se)
ci_rel = (ci_abs[0]/bmean*100, ci_abs[1]/bmean*100)
print(f"Welch: bmean={bmean*100:.2f}% nmean={nmean*100:.2f}% diff={diff*100:.2f}pp se={se*100:.3f}pp df={df:.2f} t_crit={t_crit}")
print(f"Welch relative-drop 95% CI: [{ci_rel[0]:.2f}%, {ci_rel[1]:.2f}%]")

# Bootstrap ratio-of-medians CI (resample each arm with replacement, 200k reps)
random.seed(42)
N = 200000
rel_diffs = []
hit_minus10 = 0
for _ in range(N):
    bs = [random.choice(bshares) for _ in range(bn)]
    ns = [random.choice(nshares) for _ in range(nn)]
    bm, nm = statistics.median(bs), statistics.median(ns)
    rel = (nm - bm)/bm
    rel_diffs.append(rel)
    if rel <= -0.10:
        hit_minus10 += 1
rel_diffs.sort()
lo = rel_diffs[int(0.025*N)]
hi = rel_diffs[int(0.975*N)]
print(f"Bootstrap ratio-of-medians 95% CI: [{lo*100:.2f}%, {hi*100:.2f}%]  ({hit_minus10}/{N} resamples reach <= -10%)")

# exact MWU floor note
p_floor = 2 * 1 / (math.comb(bn+nn, bn))
print(f"exact MWU p floor at n=5,5 (complete separation): {p_floor:.5f}")

# volume-independence check: correlation between total (local+remote) and share, across all 10 runs
all_vols = [l+r for l,r in baseline+numa]
all_shares = bshares+nshares
meanv = statistics.mean(all_vols)
means = statistics.mean(all_shares)
cov = sum((v-meanv)*(s-means) for v,s in zip(all_vols,all_shares))
sdv = math.sqrt(sum((v-meanv)**2 for v in all_vols))
sds = math.sqrt(sum((s-means)**2 for s in all_shares))
r = cov/(sdv*sds)
print(f"volume range: {min(all_vols):,} to {max(all_vols):,} (spread {max(all_vols)/min(all_vols):.2f}x)")
print(f"Pearson r (volume vs share, all 10 runs pooled): {r:.3f}")
# within-arm too
def pearson(xs,ys):
    mx,my=statistics.mean(xs),statistics.mean(ys)
    cov=sum((x-mx)*(y-my) for x,y in zip(xs,ys))
    sx=math.sqrt(sum((x-mx)**2 for x in xs)); sy=math.sqrt(sum((y-my)**2 for y in ys))
    return cov/(sx*sy) if sx>0 and sy>0 else float('nan')
bvols=[l+r for l,r in baseline]; nvols=[l+r for l,r in numa]
print(f"Pearson r baseline only: {pearson(bvols,bshares):.3f}")
print(f"Pearson r numa only: {pearson(nvols,nshares):.3f}")
