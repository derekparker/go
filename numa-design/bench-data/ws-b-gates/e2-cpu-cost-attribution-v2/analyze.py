import re

def load(path):
    entries = []
    for line in open(path):
        m = re.match(r"\s*([\d.]+)%\s+\[([^\]]*)\]\s+(\S+)", line)
        if not m: continue
        pct = float(m.group(1)); mode = m.group(2); sym = m.group(3)
        entries.append((pct, mode, sym))
    return entries

on = load("perf-on-full.txt")
off = load("perf-off-full.txt")

groups = {
    "spanSet/mcentral/sweep (WS-B routing path)": ["(*spanSet)", "(*mcentral).cacheSpan", "(*mcentral).uncacheSpan", "nextSpanForSweep", "spanSetScans", "spanSetBlockAlloc"],
    "numa syscall/routing-decision path": ["getcpu", "numaGetCPUNode", "numaNoteSchedule", "numaCurrentNode", "numaSetThreadAffinity", "numaBindGrowth", "numaBindArenaHome", "numaGrowNode", "numaWiden", "numaRefillNode", "numaArenaNode"],
    "kernel spinlock contention (native_queued_spin_lock_slowpath)": ["native_queued_spin_lock_slowpath"],
    "other kernel-mode samples ([k], excl. spinlock)": None,
}

def group_totals(entries):
    totals = {k: 0.0 for k in groups}
    for pct, mode, sym in entries:
        if mode == "k" and "native_queued_spin_lock_slowpath" not in sym:
            totals["other kernel-mode samples ([k], excl. spinlock)"] += pct
            continue
        for g, keys in groups.items():
            if keys is None: continue
            if any(k in sym for k in keys):
                totals[g] += pct
                break
    return totals

on_t, off_t = group_totals(on), group_totals(off)
print(f"{'group':60s} {'ON%':>8s} {'OFF%':>8s} {'delta':>10s}")
tot = 0.0
for g in groups:
    d = on_t[g]-off_t[g]; tot += d
    print(f"{g:60s} {on_t[g]:8.3f} {off_t[g]:8.3f} {d:10.3f}")
print(f"\nsum of measured group deltas (ON-OFF): {tot:.3f} pct pts of ON-arm cycles")

on_k = sum(p for p,m,s in on if m=="k"); off_k = sum(p for p,m,s in off if m=="k")
on_u = sum(p for p,m,s in on if m=="."); off_u = sum(p for p,m,s in off if m==".")
print(f"total kernel-mode: ON={on_k:.3f}% OFF={off_k:.3f}% delta={on_k-off_k:.3f}")
print(f"total user-mode:   ON={on_u:.3f}% OFF={off_u:.3f}% delta={on_u-off_u:.3f}")

# The +19.96% user+sys-sec/op delta, expressed as a fraction of ON-arm's own total
rel = 0.1996
target_pct_of_on = rel/(1+rel)*100
print(f"\n+19.96% user+sys increase = {target_pct_of_on:.2f}% of ON-arm cycles (target to explain)")
print(f"measured net group delta explains: {tot:.2f} / {target_pct_of_on:.2f} = {tot/target_pct_of_on*100:.1f}% of the target")
spinlock_delta = on_t["kernel spinlock contention (native_queued_spin_lock_slowpath)"] - off_t["kernel spinlock contention (native_queued_spin_lock_slowpath)"]
print(f"spinlock contention alone explains: {spinlock_delta:.2f} / {target_pct_of_on:.2f} = {spinlock_delta/target_pct_of_on*100:.1f}% of the target")
unattributed = target_pct_of_on - tot
print(f"unattributed remainder: {target_pct_of_on:.2f} - {tot:.2f} = {unattributed:.2f} pct pts ({unattributed/target_pct_of_on*100:.1f}% of the target)")
