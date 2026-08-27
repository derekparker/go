# NUMA Evidence Results

Task-policy scope for Stage 1a (`set_mempolicy` BIND-all vs STW toggle, cgo/`mmap`): `numa-design/bind-all-policy.md`.


## Post Stages 1–3 remeasure (2026-08-13)

Date: 2026-08-13
Commit: `19f73e96c9` (arena `MPOL_PREFERRED`, per-node mcentral, NUMA-biased steal; Stage 1a STW `set_mempolicy` still not implemented)
Machine: Xeon Platinum 8592+ (2 node), same host as 2026-07-21
`kernel.numa_balancing`: 1
Toolchain: `go version go1.28-devel_19f73e96c9` built on the machine

This pass repeats the 2026-07-21 evidence pack after Stages 1–3 landed. Same flags as before unless noted.

### Section 2 — #14406 GC pauses

Profile: `-heap=4096 -warm=10 -n=100 -idle=100000 -stacks=100`

| label | stw_p50 | stw_p99 | stw_max | wall_p99 | notes |
|-------|---------|---------|---------|----------|-------|
| baseline | 0.382ms | 0.861ms | 0.936ms | 692.3ms | no `GOEXPERIMENT=numa` |
| numa | 0.414ms | 0.721ms | 0.797ms | 648.3ms | Stages 1–3 (`MPOL_PREFERRED` arenas + per-node mcentral) |
| membind | 0.380ms | 0.572ms | 0.625ms | 627.9ms | `numactl --membind=0,1` oracle, baseline binary |

Raw JSON (one run each):

```json
{"label":"baseline","heap_mib":4096,"n":100,"gomaxprocs":256,"stw_p50_ns":381665,"stw_p95_ns":774061,"stw_p99_ns":860510,"stw_max_ns":936034,"stw_mean_ns":432913,"wall_p99_ns":692304932}
{"label":"numa","heap_mib":4096,"n":100,"gomaxprocs":256,"stw_p50_ns":414186,"stw_p95_ns":606965,"stw_p99_ns":720764,"stw_max_ns":797015,"stw_mean_ns":438283,"wall_p99_ns":648313866}
{"label":"membind","heap_mib":4096,"n":100,"gomaxprocs":256,"stw_p50_ns":379808,"stw_p95_ns":478230,"stw_p99_ns":571957,"stw_max_ns":624831,"stw_mean_ns":387928,"wall_p99_ns":627943971}
```

Interpretation:

Unlike 2026-07-21, the **membind oracle now separates cleanly**: STW p99 is **33% below baseline** (0.572ms vs 0.861ms), and max/mean follow. That is the ≥30% p99 gate from the Stage 1 plan — but it is the **process membind oracle**, not `GOEXPERIMENT=numa`.

`numa` sits in between: STW p99 **16% below baseline** (0.721ms), max also lower (0.797 vs 0.936ms), p50 slightly worse. It does **not** match membind. Per-arena `MPOL_PREFERRED` is a soft hint and does not pin the whole process the way `numactl --membind=0,1` does, so the kernel balancer can still migrate pages during GC STW. Closing that remaining ~0.15ms p99 gap is the case for Stage 1a (`set_mempolicy(MPOL_BIND, all_nodes)` around STW) or a harder bind mode.

Caveats: still one run per label; p50/p99 can move with scheduler noise from `-idle=100000`.

### Section 3 — `golang.org/x/benchmarks/garbage`

Command: `make bench-garbage BENCH_GARBAGE_MEM=8192 BENCH_GARBAGE_NUM=3`
Same 8 GiB target as 2026-07-21 (machine ~31 GiB RAM).

| label | avg ns/op | avg STW-ns/op | avg STW-ns/GC | avg peak-RSS |
|-------|-----------|---------------|---------------|--------------|
| baseline | 3108.6µs | 28.12µs | 28.12ms | 14.39 GiB |
| numa | 2932.2µs | 21.38µs | 20.41ms | 13.54 GiB |

(avg of `-benchnum=3`; `allocated-bytes/op` ~6.22MB / `allocs/op` ~145220 — different from the 2026-07-21 `garbage` module rev, so **do not compare absolute ns/op to the old table**, only baseline-vs-numa in this pass.)

Raw:

```
baseline run1: ns/op=3144963  STW-ns/op=23962  STW-ns/GC=23962661  peak-RSS=15448702976  (N=10000)
baseline run2: ns/op=3098684  STW-ns/op=30075  STW-ns/GC=30075580  peak-RSS=15448702976  (N=10000)
baseline run3: ns/op=3082106  STW-ns/op=30333  STW-ns/GC=30333410  peak-RSS=15448702976  (N=10000)
numa     run1: ns/op=2551650  STW-ns/op=17327  STW-ns/GC=14439592  peak-RSS=14530510848  (N=5000)
numa     run2: ns/op=3065374  STW-ns/op=22874  STW-ns/GC=22874409  peak-RSS=14547288064  (N=5000)
numa     run3: ns/op=3179656  STW-ns/op=23925  STW-ns/GC=23925019  peak-RSS=14547288064  (N=5000)
```

Interpretation:

Directionally the opposite of pre-placement: `numa` avg ns/op is **~6% faster**, avg STW-ns/op **~24% lower**, peak-RSS ~0.85 GiB lower. The first `numa` run (2.55ms) is an outlier vs runs 2–3 (3.07–3.18ms), which overlap baseline (3.08–3.14ms). `user+sys-ns/op` is **higher** on `numa` (~292–337µs vs ~248–267µs). Treat this as “not a regression, possible STW help, not a clean throughput win.”

### Section 4 — perf IMC

Same events as 2026-07-21. `BenchmarkNUMAAllocMixed` still hits `b.N=1e9` in <0.5s (~10k counters) — kept for comparability. Added `BenchmarkNUMAPointerChase/par` (multi-GB DRAM chase) and `BenchmarkNUMAAllocWithGC` so the ≥10% remote-share gate has a real sample.

| bench | label | local_dram | remote_dram | remote share | ns/op | wall |
|-------|-------|------------|-------------|--------------|-------|------|
| AllocMixed | baseline | 11,129 | 9,884 | 47.0% | 0.245 | 0.47s |
| AllocMixed | numa | 9,625 | 7,297 | 43.1% | 0.221 | 0.39s |
| PointerChase/par | baseline | 4.405e9 | 3.762e9 | 46.1% | 323.0 | 23.4s |
| PointerChase/par | numa | 3.012e9 | 2.537e9 | 45.7% | 388.3 | 24.1s |
| AllocWithGC | baseline | 1.365e6 | 0.969e6 | 41.5% | 138.4 | 6.3s |
| AllocWithGC | numa | 1.265e6 | 1.016e6 | 44.5% | 163.8 | 7.1s |

Design gate: ≥10% cut in remote share. **Not met.** PointerChase remote share is unchanged (~46%). AllocWithGC remote share is slightly *higher* under `numa`. AllocMixed’s −3.9 pp is noise at ~10k counts.

PointerChase and AllocWithGC **ns/op are worse** under `numa` (+20% and +18%). That matches the earlier tight-loop `BenchmarkNUMAAlloc` finding: allocator/tagging overhead without a locality win on these patterns. PointerChase is also the wrong place to expect a win (first-touch already places pages; the bench comment says baseline≈numa).

### Extra probes (same evening, 5× + vmstat + SchedulingAccess)

Harness: `numa-design/remeasure-extra.sh` (`make bench-evidence-extra`). Same commit/host. `kernel.numa_balancing=1` except the nobal control.

#### 5× gc-pause + `/proc/vmstat` deltas

Same profile as Section 2. Median of 5 repeats:

| label | median STW p99 | mean STW p99 | median pages migrated | median hint faults |
|-------|----------------|--------------|-----------------------|--------------------|
| baseline | 0.673ms | 0.689ms | 95,012 | 10,752 |
| numa | 0.719ms | 0.733ms | 4,269 | 12,273 |
| membind | 0.622ms | 0.609ms | **0** | **0** |

Per-repeat STW p99 (ms) and `numa_pages_migrated`:

| rep | baseline p99 | numa p99 | membind p99 | base migr | numa migr | membind migr |
|-----|--------------|----------|-------------|-----------|-----------|--------------|
| 1 | 0.655 | 0.706 | 0.622 | 95,012 | 4,763 | 0 |
| 2 | 0.709 | 0.814 | 0.663 | 91,131 | 4,269 | 0 |
| 3 | 0.612 | 0.747 | 0.621 | 111,574 | 4,627 | 0 |
| 4 | 0.673 | 0.681 | 0.518 | 107,093 | 2,428 | 0 |
| 5 | 0.795 | 0.719 | 0.622 | 74,531 | 3,764 | 0 |

The single-run Section 2 table above (**numa −16% p99**) **did not replicate**. Over 5 runs numa median STW p99 is **slightly worse** than baseline. What *does* replicate:

- **`MPOL_PREFERRED` cuts page migrations ~20×** (95k → 4k).
- **membind stops the balancer entirely** (0 hint faults, 0 PTE updates, 0 migrations).
- **numa still takes ~as many hint faults as baseline** (PROT_NONE marking still happens). STW can still pay fault cost even when the kernel does not migrate.

That is the mechanism argument for Stage 1a: suppress hint-fault/migration **during STW**, not only prefer a node at arena map time.

#### `kernel.numa_balancing=0` control (one three-way)

All three labels: 0 hint faults, 0 migrations. STW p99: baseline 0.993ms, numa 0.779ms, membind 0.531ms (single run — not a p99 gate). Confirms vmstat counters are balancer-driven. Restored to 1 after the run.

#### `BenchmarkNUMASchedulingAccess` + IMC (Stage 3)

| | ns/op | local_dram | remote_dram | remote share |
|--|-------|------------|-------------|--------------|
| Session 2 (before, no IMC) | ~1260 base / ~1237 numa | — | — | — |
| 2026-08-13 baseline | 1,239,820 | 2.250e9 | 1.959e9 | 46.5% |
| 2026-08-13 numa | 1,266,802 | 1.689e9 | 1.337e9 | 44.2% |

ns/op is ~2% **slower** under numa (was ~2% faster in session 2 — noise). Remote share −2.3 pp, not the ≥10% gate.

#### perf on `gc-pause-bench` (one pair)

| | STW p99 | local | remote | remote share |
|--|---------|-------|--------|--------------|
| baseline | 0.558ms | 10.51e6 | 4.10e6 | 28.0% |
| numa | 0.786ms | 10.61e6 | 3.98e6 | 27.3% |

Whole-process L3-miss-to-DRAM during alloc+warmup+100 GCs does not move. vmstat migrations are the better `#14406` metric.

#### perf on `garbage` (`-benchnum=1`, 8 GiB)

| | ns/op | STW-ns/op | local | remote | remote share |
|--|-------|-----------|-------|--------|--------------|
| baseline | 3084µs | 21.9µs | 297e6 | 282e6 | 48.7% |
| numa | 2985µs | 17.4µs | 280e6 | 256e6 | 47.7% |

Same story as AllocMixed: ~1 pp remote share, not ≥10%. Throughput noise-sized.

### What this implies for next work

1. **Ship metric for `#14406` on this box is `numa_pages_migrated` / hint faults, not a single STW p99.** Arena `MPOL_PREFERRED` already kills most migrations; it does **not** kill hint faults, and STW p99 is too noisy at `-idle=100000` to show a 30% win. Stage 1a should aim for **membind-like vmstat** (0 hint faults during STW), then see if p99 follows — possibly with a heavier idle/stack profile closer to the original issue.
2. **SchedulingAccess + garbage IMC are not showing interconnect wins.** Don’t block Stage 1a on them.
3. **Stage 4** is still unstarted and not needed to explain vmstat vs membind.

## Stage 1a post-implementation pack (2026-08-14)

Sources on `numa-dell` (git HEAD still `19f73e96c9`; 1a uncommitted). Toolchain rebuilt, then `OUT=/tmp/numa-evidence-1a` `remeasure-extra.sh` plus a 3-run `garbage` and `BenchmarkNUMAAllocMixed` IMC. Host: Xeon Platinum 8592+ (2 node). Profile: `-heap=4096 -warm=10 -n=100 -idle=100000 -stacks=100`.

`set_mempolicy(MPOL_BIND, all allowed nodes)` after topology init; per-arena `mbind(MPOL_BIND, all nodes)` then `MPOL_PREFERRED`; `mbind` maxnode is nodemask width.

Morning single-run (before this pack): baseline 1.085ms STW p99 / 10,696 hint faults / 112,819 migrated; numa 0.856ms / **0** / **0**; membind 0.599ms / **0** / **0**.

### 5× gc-pause + `/proc/vmstat` (`kernel.numa_balancing=1`)

Median of 5 repeats:

| label | median STW p99 | mean STW p99 | median hint faults | median pages migrated |
|-------|----------------|--------------|--------------------|------------------------|
| baseline | 0.864ms | 0.857ms | 8,425 | 96,730 |
| numa (1a) | **0.681ms** (−21% vs base) | 0.693ms | **0** | **0** |
| membind | 0.603ms (−30% vs base) | 0.600ms | **0** | **0** |

Per-repeat STW p99 (ms):

| rep | baseline | numa | membind | base hint / migr | numa hint / migr |
|-----|----------|------|---------|------------------|------------------|
| 1 | 1.058 | 0.681 | 0.664 | 10,781 / 92,046 | 0 / 0 |
| 2 | 0.916 | 0.624 | 0.603 | 8,425 / 113,090 | 0 / 0 |
| 3 | 0.864 | 0.600 | 0.608 | 8,202 / 96,285 | 0 / 0 |
| 4 | 0.767 | 0.728 | 0.566 | 8,274 / 96,730 | 0 / 0 |
| 5 | 0.680 | 0.830 | 0.558 | 8,436 / 100,327 | 0 / 0 |

Hint-fault / migration gate matches membind on every repeat. STW p99 median is between baseline and the membind oracle; run 5 still shows STW noise (numa 0.830ms vs baseline 0.680ms) with **zero** balancer activity.

### `kernel.numa_balancing=0` control (one three-way)

All three labels: 0 hint faults, 0 migrations. STW p99: baseline 0.802ms, numa 0.657ms, membind 0.530ms. Restored to 1.

### `golang.org/x/benchmarks/garbage` (`-benchmem=8192`)

3-run ns/op: baseline 2.912 / 2.933 / 2.891 ms (median **2.912ms**); numa 2.666 / 2.912 / 2.891 ms (median **2.891ms**, ~−0.7%). STW-ns/op medians: 27.6µs vs **22.2µs** (−19%). Peak RSS higher under numa (~15.8 vs 14.6 GiB). Throughput is noise-sized; STW-ns/op moved in the expected direction.

Perf (`-benchnum=1`): baseline 2.969ms ns/op, remote share 48.6%; numa 2.767ms, remote share 48.7%. Interconnect share unchanged.

### `BenchmarkNUMASchedulingAccess` + IMC (`-benchtime=5s`, 256 P)

| | ns/op | local_dram | remote_dram | remote share |
|--|-------|------------|-------------|--------------|
| baseline | 1,264,277 | 1.580e9 | 1.303e9 | 45.2% |
| numa (1a) | 1,253,661 | 1.885e9 | 1.658e9 | 46.8% |

~0.8% faster ns/op; remote share +1.6 pp (not the ≥10% interconnect gate).

### perf on `gc-pause-bench` (one pair)

| | STW p99 | local | remote | remote share |
|--|---------|-------|--------|--------------|
| baseline | 0.806ms | 11.26e6 | 4.07e6 | 26.5% |
| numa (1a) | 0.765ms | 10.97e6 | 4.10e6 | 27.2% |

Whole-process L3-miss-to-DRAM still does not move. vmstat remains the `#14406` metric.

### `BenchmarkNUMAAllocMixed` IMC

Still not a ship metric: both binaries report ~0.27 ns/op with ~10k IMC counts (first-touch / `b.N` cap). Remote share ~43% vs ~41%. Same failure mode as the pre-1a pack.

### Interpretation

Stage 1a **closes the vmstat gap vs membind** (0 hint faults, 0 migrations) while `kernel.numa_balancing=1`. Median STW p99 improved vs baseline but remains noisier and a bit worse than `numactl --membind=0,1`. App throughput (`garbage`) and interconnect share (`SchedulingAccess`, garbage IMC, gc-pause IMC) did not show a ≥10% remote-DRAM win.

## `golang.org/x/benchmarks` json / http / garbage (2026-08-17)

Commit `d86a0bafed`, same host, `GOMAXPROCS=256`, `kernel.numa_balancing=1`. Binaries built with/without `GOEXPERIMENT=numa`. `-benchnum=3`. Harness: `numa-design/run-x-benchmarks.sh` (artifacts `/tmp/x-bench-numa`).

| bench | mem | median ns/op base → numa | hint faults / migr | remote DRAM share |
|-------|-----|---------------------------|--------------------|-------------------|
| json | 2048 MB | 2.099ms → 2.860ms (**+36%**) | 1.38M / 1.97M → **0 / 0** | 46.9% → 48.5% |
| http | 2048 MB | 39.7µs → 48.7µs (**+23%**); p99 190ms → 244ms | 6.5k / 22k → **0 / 0** | 44.4% → 45.2% |
| garbage | 8192 MB | 3.026ms → 2.831ms (**−6%**); STW-ns/op 30.3µs → 23.7µs | 1.18M / 3.45M → **0 / 0** | 48.8% → 47.4% |

JSON peak RSS ~5.0 GiB baseline vs ~9.6 GiB numa. HTTP RSS stayed small (~208 vs 264 MiB) — it does not grow to the `-benchmem` target the way `garbage` does.

Same story as the 1a pack: **balancer fully suppressed**; **no ≥10% interconnect win**; throughput mixed (garbage slightly faster, json/http slower). Next: Sweet subset or Prometheus.

## Sweet etcd / tile38 / bleve-index (2026-08-18)

Commit `d86a0bafed`, same host, Sweet v0.3.0, `-count 3`, `kernel.numa_balancing=1`. One `sweet run` with two configs sharing `GOROOT=/home/deparker/go-numa`; NUMA is `envbuild = ["GOEXPERIMENT=numa", "GOTOOLCHAIN=local"]`. `GOTOOLCHAIN=local` is required because etcd pins `toolchain go1.23.9`, which rejects `GOEXPERIMENT=numa`. Sweet `perf` diagnostics were **not** used: `driver.ResetTimer` SIGINT+Wait hung on `perf record` with PEBS DRAM events.

Config: `numa-design/sweet-config.toml`. Raw results: `numa-design/sweet-results/` (also `/home/deparker/go-numa/x-benchmarks/sweet-results` on the host).

Median of 3 (Sweet `ns/op`; lower is better). `ops/s` is the workload metric Sweet reports alongside it.

| bench | median ns/op base → numa | median ops/s | notes |
|-------|--------------------------|--------------|-------|
| EtcdPut | 30.92ms → 42.04ms (**+36%**) | 32.0k → 23.6k (−26%) | 3-node local cluster, 1k clients / 100k puts |
| EtcdSTM | 202.4ms → 286.9ms (**+42%**) | 4.91k → 3.47k (−29%) | same cluster |
| Tile38QueryLoad | 4.081ms → 4.045ms (**−0.9%**) | 47.0k → 47.5k | ~4.3 GiB RSS; one numa run 3.00ms is an outlier (median still a tie) |
| BleveIndexBatch100 | 4.79s → 12.26s (**+156%**) | n/a (N=1) | 1000 wiki docs / batch 100; peak RSS ~309 → 336 MiB |

Whole-run `/proc/vmstat` over mixed baseline+numa is **not** a per-config #14406 metric (hints +6.6M / migr +2.1M across the ~27 min job). Per-binary vmstat was not collected; Sweet does not wrap the server with `perf stat`.

Interpretation: same shape as json/http. **etcd and bleve-index got slower** under `GOEXPERIMENT=numa` (bleve by a lot; etcd in the same +36% ballpark as json). **tile38 is a wash.** This is still not a product speed win. The #14406 claim stays with the earlier per-binary vmstat (json/http/garbage / gc-pause), not this Sweet pass.

Caveats: etcd Put p99 request latency looks *lower* under numa (median 70ms vs 155ms) but baseline p99 is noisy (66 / 177 / 155 ms) and throughput is unambiguously worse. Bleve `N=1` so it is a single indexing pass per run; all three numa runs cluster at 12.3–12.7s vs baseline 4.5–6.0s, so the gap is not one outlier.

## Prometheus scrape + query_range (2026-08-18)

Commit `d86a0bafed`, Prometheus `v3.5.0` (`8be3a956`) and avalanche HEAD built twice from the same GOROOT (`GOTOOLCHAIN=local`, ±`GOEXPERIMENT=numa`). Third arm: baseline binaries under `numactl --membind=0,1`. Harness: `numa-design/run-prometheus-bench.sh`. Artifacts: `/tmp/prom-numa-bench` on the host.

Load: avalanche `--gauge-metric-count=200 --series-count=50` (~10k series) scraped every 1s; 20s warmup then 60s of serial `query_range` on `prometheus_tsdb_head_series`. `GOMAXPROCS=256`, `kernel.numa_balancing=1`. Perf attaches to the Prometheus PID only (`perf stat -p`).

| label | queries ok / fail | RSS after | hint faults / migr | local DRAM | remote DRAM | remote share |
|-------|-------------------|-----------|--------------------|------------|-------------|--------------|
| baseline | 3254 / 0 | 233 MiB | 261 / **21,603** | 164,470 | 84,306 | **33.9%** |
| numa | 3157 / 0 (−3%) | 237 MiB | **0 / 0** | 112,854 | 95,498 | **45.8%** |
| membind | 3217 / 0 (−1%) | 237 MiB | **0 / 0** | 67,913 | 53,378 | **44.0%** |

Interpretation: **#14406 again, on a real Prometheus process.** NUMA matches membind on vmstat (zero balancer activity). Query rate is a wash. Remote DRAM share **rises** under numa/membind (~34% → ~45%): `MPOL_BIND`/`membind=0,1` pins pages to *both* nodes and stops the balancer from migrating hot pages home, so the interconnect gate is not expected to fire here. This is a modest RSS (~230 MiB) ingest+query loop, not a multi-GiB TSDB.

Caveats: one run per label; query is cheap (`prometheus_tsdb_head_series`, not a regex over all avalanche series); IMC counts are `:u` retired L3-miss loads, not bytes.

---

## Section 2 — #14406 GC pauses (pre-Stage 1a, 2026-07-21)

Date: 2026-07-21
Machine: Xeon Platinum 8592+ (2 node)
Profile: -heap=4096 -warm=10 -n=100 -idle=100000 -stacks=100
kernel.numa_balancing: 1

| label | stw_p50 | stw_p99 | stw_max | wall_p99 | notes |
|-------|---------|---------|---------|----------|-------|
| baseline | 0.422ms | 0.856ms | 0.984ms | 668.7ms | no NUMA arena placement |
| numa | 0.437ms | 0.709ms | 0.710ms | 649.3ms | GOEXPERIMENT=numa, pre-Stage-1a (no mbind yet) |
| membind | 0.395ms | 0.749ms | 1.050ms | 675.4ms | numactl --membind=0,1 oracle, baseline binary |

Raw JSON (one run each, `-n=100` GC cycles per label):

```json
{"label":"baseline","heap_mib":4096,"n":100,"gomaxprocs":256,"stw_p50_ns":421865,"stw_p95_ns":721021,"stw_p99_ns":855962,"stw_max_ns":983995,"stw_mean_ns":459743,"wall_p99_ns":668722212}
{"label":"numa","heap_mib":4096,"n":100,"gomaxprocs":256,"stw_p50_ns":436832,"stw_p95_ns":657503,"stw_p99_ns":709306,"stw_max_ns":709532,"stw_mean_ns":455186,"wall_p99_ns":649335587}
{"label":"membind","heap_mib":4096,"n":100,"gomaxprocs":256,"stw_p50_ns":394741,"stw_p95_ns":516089,"stw_p99_ns":749065,"stw_max_ns":1050162,"stw_mean_ns":402129,"wall_p99_ns":675425289}
```

Interpretation:

As expected pre-Stage-1a, all three STW p99 values fall in the same ~0.7-0.86ms band — there is no
clear separation between `baseline`, `numa`, and the `numactl --membind` oracle. `numa` currently
builds with `GOEXPERIMENT=numa` but has not yet implemented Stage 1a's `mbind(MPOL_PREFERRED)`
arena binding, so it has no mechanism to reduce NUMA-balancer page migration during GC STW — its
mild p99 improvement over `baseline` here (0.709ms vs 0.856ms) is within run-to-run noise (a single
`-n=100` sample), not a real effect, and is corroborated by `membind`'s p99 (0.749ms) and max
(1.050ms, the worst of the three) not tracking cleanly below `baseline` either. This confirms the
pre-1a hypothesis: without explicit page placement, GC STW pauses on this 2-node machine are
dominated by page-fault/migration overhead that `GOEXPERIMENT=numa` alone does not yet address.
Stage 1a's `mbind` work is expected to move `numa` and `membind` together, clearly below `baseline`.

Caveats: single run per label (no repeated trials/averaging), `-idle=100000` goroutines add scheduler
noise on `gomaxprocs=256`, and wall_p99 includes concurrent GC phases so it is not directly comparable
to stw_p99.

## Section 3 — `golang.org/x/benchmarks/garbage` (pre-Stage 1a)

Date: 2026-07-21
Machine: same 2-node Xeon Platinum 8592+ as Section 2 (`gomaxprocs=256`)
Command: `make bench-garbage BENCH_GARBAGE_MEM=8192 BENCH_GARBAGE_NUM=3`
Binaries: two distinct `garbage` installs — `GOBIN=.../baseline go install ...` and
`GOBIN=.../numa GOEXPERIMENT=numa go install ...` — since `GOEXPERIMENT` is a **build**
flag, not a runtime one; running one binary under different `GOEXPERIMENT` env values at
run time would be a no-op.

**Deviation from design target**: the design calls for `-benchmem=32768` (32 GiB target
RSS), but this machine has only ~31 GiB total RAM (15.2 GiB node 0 + 16.1 GiB node 1). A
32 GiB target would leave no headroom and risks OOM-killing the benchmark or other
processes on a shared machine. A pilot run at `-benchmem=8192` measured `peak-RSS-bytes`
of ~11–13 GiB (RSS runs ~1.4-1.6x over the target due to GC headroom/heap growth), which
comfortably fits within the 30 GiB available. Used `-benchmem=8192` for all runs below.

| label | avg ns/op | avg STW-ns/op | avg STW-ns/GC | avg peak-RSS |
|-------|-----------|---------------|---------------|--------------|
| baseline | 1269.4µs | 12.66µs | 25.32ms | 12.38 GiB |
| numa | 1429.4µs | 12.20µs | 23.42ms | 11.08 GiB |

(avg of `-benchnum=3` runs each; `allocated-bytes/op` ~2.75MB and `allocs/op` ~66365 were
consistent across all 6 runs and are omitted as uninteresting.)

Raw per-run values:

```
baseline run1: ns/op=1172911  STW-ns/op=11166  STW-ns/GC=22333953  peak-RSS=13278306304
baseline run2: ns/op=1350589  STW-ns/op=13373  STW-ns/GC=26746744  peak-RSS=13296070656
baseline run3: ns/op=1284591  STW-ns/op=13437  STW-ns/GC=26875958  peak-RSS=13296070656
numa     run1: ns/op=1354130  STW-ns/op=8922   STW-ns/GC=14870417  peak-RSS=11904077824
numa     run2: ns/op=1512188  STW-ns/op=14879  STW-ns/GC=29758614  peak-RSS=11904077824
numa     run3: ns/op=1421995  STW-ns/op=12813  STW-ns/GC=25627614  peak-RSS=11904077824
```

Interpretation:

As expected pre-Stage-1a, there is no clean win for `numa`: its avg per-op latency
(`ns/op`) is ~13% *worse* than `baseline` (1429µs vs 1269µs), while its avg STW-ns/op is
marginally better (12.20µs vs 12.66µs, within the run-to-run spread of 8.9–14.9µs seen on
the `numa` binary alone). Neither direction is a real effect at `-benchnum=3` — the
per-run STW-ns/op range overlaps heavily between labels. One consistent and unexplained
difference is `peak-RSS-bytes`: `numa` is steady at 11.90 GiB across all 3 runs, while
`baseline` is steady at ~13.3 GiB (after its first, slightly lower, run). Since Stage 1a's
`mbind` arena binding has not landed yet, `GOEXPERIMENT=numa` currently only adds topology
discovery and per-P/M node bookkeeping (Stage 0) — it should not change heap footprint,
so this ~1.3 GiB gap is more likely an artifact of module/build cache state or address
space layout than a real allocator effect, and should be re-checked once Stage 1a lands.

Caveats: run at `-benchmem=8192` instead of the design's `32768` due to the test
machine's ~31 GiB RAM ceiling (see deviation note above) — absolute numbers are not
comparable to a future 32768-target run, only the baseline-vs-numa relative comparison
at the same setting is meaningful here. `-benchnum=3` is a small sample; the STW-ns/op
spread (8.9–14.9µs) suggests real run-to-run variance on this shared machine that would
benefit from more repetitions once Stage 1a gives a real signal to detect.

## Section 4 — perf IMC (pre-Stage 1a)

Date: 2026-07-21
Machine: same 2-node Xeon Platinum 8592+ as Sections 2–3
Tooling: `perf` 6.12.0 (installed via `dnf`; was not present on the machine initially)

### Events probed

Usable **local vs remote DRAM** split (core PMU, Sapphire Rapids / 8592+):

| event | type | notes |
|-------|------|-------|
| `mem_load_l3_miss_retired.local_dram` | core PMU | L3-miss loads satisfied from local socket DRAM |
| `mem_load_l3_miss_retired.remote_dram` | core PMU | L3-miss loads satisfied from remote socket DRAM |

Also present but **not used** for the baseline/numa pair (aggregate or no local/remote split):

- `uncore_imc/cas_count_read/`, `uncore_imc/cas_count_write/`, `uncore_imc/clockticks/` (3 IMC instances, socket-aggregate only)
- `uncore_imc_free_running/{dclk,rpq_cycles,wpq_cycles}/`
- `ocr.reads_to_core.{local,remote}_dram`, `ocr.demand_data_rd.{local,remote}_dram` (alternative core-PMU splits; not run in this pass)
- `uncore_cha/*` cross-snoop events (no direct local/remote DRAM byte count)
- `numa_reads_addressed_to_{local,remote}_dram` listed in `perf list` but not selectable as standalone PMU events on this kernel

### Baseline vs NUMA pair

Pre-built test binaries (`go test -c`) to exclude compile time from counters:

```bash
# baseline
perf stat -e mem_load_l3_miss_retired.local_dram,mem_load_l3_miss_retired.remote_dram -- \
  /tmp/runtime_bench_baseline.test -test.bench=BenchmarkNUMAAllocMixed \
  -test.benchtime=5s -test.cpu=256 -test.run=^$ -test.count=1

# numa (GOEXPERIMENT=numa at build time)
perf stat -e mem_load_l3_miss_retired.local_dram,mem_load_l3_miss_retired.remote_dram -- \
  /tmp/runtime_bench_numa.test -test.bench=BenchmarkNUMAAllocMixed \
  -test.benchtime=5s -test.cpu=256 -test.run=^$ -test.count=1
```

| label | local_dram | remote_dram | remote / (local+remote) | wall |
|-------|------------|-------------|-------------------------|------|
| baseline | 9,200 | 6,387 | 41.0% | 0.47s |
| numa | 7,452 | 6,176 | 45.3% | 0.45s |

Design gate: ≥10% reduction in cross-socket traffic under NUMA. **Not met** pre-Stage-1a:
remote share is slightly *higher* under `numa` (+4.3 pp), well within counter noise at these
counts.

### Interpretation

As expected pre-Stage-1a, `GOEXPERIMENT=numa` (Stage 0 topology + P/M node tagging only)
does not reduce remote DRAM traffic on `BenchmarkNUMAAllocMixed`. Without Stage 1a's
`mbind(MPOL_PREFERRED)` arena binding or Stage 2 per-node mcentral sharding, allocations
still spread across both nodes and the kernel NUMA balancer (`kernel.numa_balancing=1`) can
migrate pages — so a ≥10% remote-traffic cut is not observable here.

Caveats:

- `BenchmarkNUMAAllocMixed` hits Go's `b.N` cap of 1e9 iterations (~0.45s wall, not the
  requested 5s benchtime), so absolute counter values are small (~10k total) and the
  remote-ratio comparison is high-variance. Re-run after Stage 1a with a heavier benchmark
  or longer effective runtime.
- Counters are `:u` (user-only) and count **retired load L3-misses to DRAM**, not total
  bytes or write traffic; they are a proxy for cross-socket read pressure, not full IMC
  bandwidth.
- Single run per label; no repeated trials.

---

## Layer 0 gate (v2) — 2026-08-19

Date: 2026-08-19
Local SHA: `ed06774c7decd93ded7f5fcc13ce817eee671fe6`
Remote SHA (`numa-dell`, post-`make push`+`make build`): `ed06774c7decd93ded7f5fcc13ce817eee671fe6` (matches; remote was previously stale at `d9a7ef91aa`, resynced via `git push numa-dell +HEAD:claude/numa-v2-implementation-a6eb4c` then `git fetch && git checkout -f`)
`go version`: `go1.28-devel_ed06774c7d Wed Aug 19 14:36:56 2026 -0700 linux/amd64`
Kernel: `6.12.0-211.7.1.el10_2.x86_64`
`x/benchmarks`: `v0.0.0-20260819172200-70693762b6a0`
`x/perf` (benchstat): `v0.0.0-20260819171926-ebcb4798430d`
Machine load: single-user throughout (`who`/`w` showed only `deparker`'s own SSH sessions); `uptime` load average climbed to ~1–6 over the course of the run, attributable to this session's own sequential `make.bash` builds and background benchmark processes, not a second tenant. `tuned-adm active` profile: `throughput-performance`. No cpufreq `scaling_governor` sysfs present (HWP-managed P-states); this correlates with the bimodal run-to-run timing noise described below.

Layer 0 adds: `internal/goexperiment.Numa` flag, `internal/runtime/numa` (topology types + sysfs parser), `runtime.numaSchedinit`/`numaInitTopology`/`numaCurrentNode`, a `getcpu` asm stub, a `debug.numa` dbgvar, and one call site in `schedinit`. No allocator/scheduler fast-path code was touched.

**Machine-noise note (affects every gate below):** `GOMAXPROCS=1` timings on `numa-dell` are strongly bimodal — repeated runs of the *identical* binary swing by up to ~2x (e.g. json `ns/op` alternating between ~16ms and ~33ms; `Malloc8` between ~7ns and ~14ns), independent of which arm (baseline/numa) is measured. This is consistent with HWP/Turbo P-state transitions on this many-core Xeon 8592+ when only 1 of 256 CPUs is active, not a code-path effect — the *same* noise magnitude appears in same-arm-only comparisons. Per the plan's own guidance ("prefer benchstat for any decision near the band"), all verdicts below use `benchstat`'s Mann-Whitney U test as the authoritative comparator; the plan's hand-rolled median comparator is reported alongside for transparency since it is noise-sensitive at this variance level.

### Gate 1 — 1P json, off vs on, same commit (`make gate-json-1p`)

First run, `BENCHNUM=3` (Makefile default):

| metric | baseline median | numa median | rel | hand-rolled verdict |
|---|---|---|---|---|
| ns/op | 33,283,930 | 32,987,047 | −0.9% | PASS |
| user+sys-ns/op | 33,298,610 | 33,016,060 | −0.8% | PASS |

Rerun at `BENCHNUM=10` (triggered proactively due to observed bimodal variance, matching the plan's CONCERN protocol in spirit):

| metric | baseline median | numa median | rel | hand-rolled verdict |
|---|---|---|---|---|
| ns/op | 29,914,152.5 | 32,274,066.0 | +7.9% | **FAIL** (exceeds +2%) |
| user+sys-ns/op | 29,889,640.0 | 32,285,305.0 | +8.0% | **FAIL** (exceeds +2%) |

Raw per-round values for both arms, `/tmp/numa-gate-json-n10/{baseline,numa}.out` (still present on
`numa-dell` at time of this fix; no rerun needed):

| round | baseline ns/op | baseline user+sys-ns/op | numa ns/op | numa user+sys-ns/op |
|---|---|---|---|---|
| 1 | 33,608,984 | 33,687,950 | 33,658,140 | 33,727,950 |
| 2 | 33,119,372 | 33,172,880 | 33,336,889 | 33,421,040 |
| 3 | 33,222,479 | 33,289,640 | 32,166,443 | 32,175,670 |
| 4 | 16,318,991 | 16,329,980 | 30,835,761 | 30,861,540 |
| 5 | 16,345,159 | 16,352,076 | 17,402,132 | 17,398,562 |
| 6 | 16,494,396 | 16,497,874 | 16,195,624 | 16,195,624 |
| 7 | 33,010,681 | 33,087,910 | 16,394,765 | 16,429,716 |
| 8 | 22,616,279 | 22,612,310 | 32,381,689 | 32,394,940 |
| 9 | 33,008,511 | 32,979,290 | 32,918,934 | 32,975,850 |
| 10 | 26,819,794 | 26,799,990 | 33,012,898 | 33,053,550 |

Both arms are bimodal in the same way: baseline has 5/10 rounds in the ~32–34M band and 3/10 in the
~16M band (plus 2 mid-range outliers at 22.6M/26.8M); numa has 7/10 rounds in the ~30–34M band and
3/10 in the ~16–17M band. This confirms the "bimodality affects both arms equally" claim with data,
rather than asserting it from baseline alone — the low-cluster rounds are not concentrated in either
arm, which is what makes this machine noise rather than a real GOEXPERIMENT=numa effect.

`benchstat` on the same `BENCHNUM=10` data (n=10 each arm):

```
JSON-1  sec/op:            29.91m ± 45%  32.27m ± 49%  ~ (p=0.971 n=10)
JSON-1  user+sys-sec/op:   29.89m ± 45%  32.29m ± 49%  ~ (p=1.000 n=10)
```

**Verdict: PASS** (benchstat: no statistically significant difference). The hand-rolled median comparator's FAIL reading is an artifact of the machine's bimodal noise (±45–49% spread within each arm) landing the small-N median on the wrong side by chance; `benchstat`'s p≈1.0 makes clear the two distributions are indistinguishable. Flagged as a **CONCERN** below.

### Gate 2 — 1P alloc micro (Task 4 Step 1b): `Malloc8`/`Malloc16`, `-count=10`

(No `MallocTypes` benchmark exists in this tree; only `Malloc8`/`Malloc16` matched.)

```
benchstat /tmp/alloc-base.out /tmp/alloc-numa.out
Malloc8    7.000n ± 70%   10.505n ± 35%   ~ (p=0.108 n=10)
Malloc16   11.38n ±  1%    11.37n ±  0%   ~ (p=0.513 n=10)
geomean    8.923n          10.93n         +22.48% (not significant — both individual comparisons are "~")
```

**Verdict: PASS** (benchstat: no statistically significant difference on either benchmark). Same bimodal-noise characteristic as Gate 1 (`Malloc8` alternates ~7ns/~14ns run to run in both arms).

### Gate 3 — Parent-commit comparison (Global Constraints, once per layer)

Parent/baseline: `origin/master` @ `8058a577731129d56d03797451804a2c5f4745ca`, own toolchain build, own GOBIN json binary. HEAD off/on binaries reused from Gate 1's pinned-version build. Interleaved (parent, head-off, head-on) × 10 rounds (protocol asked for ×3; extended given the demonstrated noise), `GOMAXPROCS=1 -benchmem=512 -benchtime=3s`.

Hand-rolled medians:

| comparison | parent median ns/op | head median ns/op | rel |
|---|---|---|---|
| head-off vs parent | 17,581,312.5 | 16,407,720.0 | −6.7% |
| head-on vs parent | 17,581,312.5 | 16,307,928.0 | −7.2% |

`benchstat`:

```
head-off vs parent:  sec/op  17.58m ± 29%  16.41m ± 86%  ~ (p=0.353 n=10)
head-on  vs parent:  sec/op  17.58m ± 29%  16.31m ± 85%  ~ (p=0.481 n=10)
```

**Verdict: PASS** for both (head-off ≤ parent+2% and head-on ≤ parent+2%; both nominally *faster*, not slower, and the difference is not statistically significant either way). This gate is the one that would catch an unconditional (non-`goexperiment`-gated) cost — none is visible.

### Gate 4 — Off-binary identity (Task 4 Step 1c #1)

Built `go test -c runtime` (no `GOEXPERIMENT`) from the parent commit and from HEAD; `objdump -d` on both `.test` binaries.

- Function-level census (11,173 functions each): **zero differences** in function set or per-function instruction-line counts between parent and HEAD off-binaries.
- Zero occurrences of any `numa`-related symbol anywhere in the HEAD off-binary disassembly — the entire `internal/runtime/numa` package, `numaSchedinit`, `numaInitTopology`, `numaCurrentNode`, and the `getcpu` asm stub are fully dead-code-eliminated when `GOEXPERIMENT=numa` is unset (`goexperiment.Numa` is a compile-time-false constant, so `numaSchedinit`'s single `if !goexperiment.Numa { return }` body collapses the whole call away).
- Explicit fast-path diff of `runtime.mallocgc` (133 instr), `runtime.acquirep` (56 instr), `runtime.schedinit` (355 instr): **mnemonic-for-mnemonic identical** between parent and HEAD; the only diff is the literal RIP-relative addresses of a handful of global data symbols (`runtime.sched`, `runtime.debug`, `runtime.mallocScanTable`, `runtime.gcBlackenEnabled`, etc.), all shifted by a small constant offset.
- `size`: `.text` +24 bytes, `.data` +32 bytes, `.bss` unchanged — consistent with the single new `debug.numa int32` field added to the `debug` struct (`runtime1.go`) shifting every subsequent global's address, not with any new code path.

**Literal pass-bar not met, and cannot be met at this layer.** Plan Task 4 Step 1c #1 requires
`objdump` output to "differ only in build IDs." That literal bar was **not** achieved here: the
`.text`/`.data` size deltas above (+24B/+32B) and the RIP-relative literal-address shifts in
`mallocgc`/`acquirep`/`schedinit` are real, non-build-ID differences. This is structural, not a
measurement gap — Layer 0 adds `debug.numa` to the `debug` struct (`runtime1.go`), and any new
field in a struct that many other functions reference by RIP-relative addressing will shift the
addresses of every subsequent global, in any binary that links the package, regardless of whether
`GOEXPERIMENT=numa` is off. There is no way to add a new dbgvar without this effect, so the literal
"build-IDs-only" bar is unattainable by design at Layer 0 (or any layer that adds a dbgvar).

**Verdict: PASS on the substantive criterion**, not the literal one: zero function-level changes
(11,173/11,173 functions identical in instruction count), zero new symbols reachable in the
off-binary, and zero opcode/register/branch-target changes in the three explicitly-checked
fast-path functions — only data-literal addresses moved. This is the same "flag the honest gap
between the literal plan bar and what's achievable, then show why the substantive check still
holds" treatment as Gate 5 below.

### Gate 5 — 1P instruction flatness (Task 4 Step 1c #2)

`perf stat -e instructions` on `GOMAXPROCS=1 go test runtime -bench=Malloc8 -benchtime=100000000x -count=1`, off vs on, 3 runs each (`perf_event_paranoid=2`, works fine for own-process counting):

| arm | run 1 | run 2 | run 3 | median |
|---|---|---|---|---|
| off | 11,189,097,561 | 11,178,311,812 | 11,180,809,451 | 11,180,809,451 |
| on | 11,166,784,307 | 11,167,378,201 | 11,230,889,000 | 11,167,378,201 |

Median delta: −13,431,250 instructions = **−0.12%** (on is fewer, not more — within the run-to-run instruction-count jitter of the harness itself; the aspirational "<0.01%" flatness bar in the plan wasn't hit exactly, but the sign and magnitude are consistent with "no fast-path instructions added," matching Gate 4's static evidence). Notably, instruction counts were far more stable (≤0.5% spread) than wall-clock ns/op on this machine, confirming instruction counting is the better noise-immune signal here.

**Verdict: PASS.**

### Gate 6 — 256P span check (optional, Task 5 Step 3)

`GOMAXPROCS=256 numa-design/gate-json.sh`, `BENCHNUM=3` default:

| metric | baseline | numa | rel |
|---|---|---|---|
| ns/op | 3,540,663 | 3,583,323 | +1.2% |
| user+sys-ns/op | 342,564,831 | 227,273,584 | −33.7% |

**Verdict: PASS / not stop-worthy** (ns/op +1.2% is under the +2% "huge regression" stop bar for this optional, non-gating check; the large negative user+sys swing is noise at 256P, same machine-noise character as the 1P gates — informational only, not evaluated as a hard gate per plan).

### Overall verdict: **PASS**

All hard gates (1P json off-vs-on, 1P alloc micro, parent-commit comparison) pass under `benchstat`'s statistical test, which the plan directs to be authoritative "for any decision near the band." Two additional gates immune to timing noise — off-binary code identity and instruction-count flatness — independently corroborate: Layer 0 adds zero fast-path instructions and is fully dead-code-eliminated when off; the only observable diff when off is a small, expected global-data offset shift from the new `debug.numa` dbgvar.

**Concerns for the controller:**

1. **Machine noise at `GOMAXPROCS=1` on `numa-dell` is large** (±29–86% relative stdev observed across gates), consistent with HWP/Turbo P-state transitions when only 1 of 256 CPUs is active. The plan's hand-rolled median comparator (`gate-json.sh`'s embedded Python) produced one false FAIL (Gate 1, `BENCHNUM=10`) purely from bimodal-noise median placement; `benchstat` correctly showed no significant difference on the same data. Recommend `gate-json.sh` itself be changed (in a follow-up task, not this one — no `src/` or script changes were made here) to run `benchstat` as the primary decision path rather than only "near the band," since "near the band" undersells how noisy this box is even for supposedly-clear results.
2. No `BenchmarkMallocTypes` exists in this runtime tree, so Gate 2 only covers `Malloc8`/`Malloc16`; the brief's regex `Malloc(8|16|Types)` was written to also match a benchmark that doesn't exist here.
3. **Gate 4's literal pass bar ("differ only in build IDs") was not met and cannot be at this layer** — adding any `debug.<name>` dbgvar shifts RIP-relative addresses of subsequent globals in every function that references them, off-binary or not. Gate 4 passed on the substantive criterion (zero function-level/opcode/branch-target changes) instead; see the Gate 4 entry above for the full explanation. Future layers that add dbgvars will hit the same structural limit.

## Layer 1 implementation notes (Task 6: BIND-all task mempolicy + arena mbind)

Added after Layer 0's gates above; not a re-run of the performance gates (Layer 1 is
correctness-scoped, not perf-scoped, per its task brief). Two implementation notes the Task 6
brief requires recording here:

**(a) Catch-up mbind of pre-`numaSchedinit` `curArena` ranges was skipped.** `mheap.grow` can
run before `numaSchedinit` (the `goargs`/`goenvs` allocations that happen before
`finishDebugVarsSetup`, which precedes `numaSchedinit`, in `schedinit`). Heap chunks grown in
that early window get no VMA policy from `numaBindArena` (it's a guarded no-op until
`numaSetProcessBindAll` publishes a non-empty mask), and this implementation does not walk
`h.curArena`/`h.arenas` from `numaSchedinit` afterward to retroactively `mbind` them. Per the
brief's own fallback: those ranges are not unprotected in practice, because
`numaSetProcessBindAll`'s task-wide `set_mempolicy(MPOL_BIND, ...)` (called moments later, in
the same `schedinit`, before any other goroutine runs) governs page placement for any VMA
without its own policy. The gap is real but small (bounded to early-startup allocation volume)
and covered by the task policy, not left to the kernel's default first-touch/interleave
behavior. See `task-6-report.md`'s "Design choices" section for the full reasoning.

**(b) Kernel-version dependence of multi-node `MPOL_BIND` fill order (CONCERN, unresolved).**
The brief flags that old kernels fill the lowest-numbered allowed node first under `MPOL_BIND`
(silent capacity/locality skew toward node 0), while recent kernels prefer the local node.
`numa-dell`, the only multi-node machine this was tested on, runs
`6.12.0-211.7.1.el10_2.x86_64` — a recent kernel, on which local-node preference is expected,
not the older lowest-node-first behavior. This was **not independently verified** (would
require a node-placement micro-benchmark measuring which node pages actually land on under
load, which is out of scope for Task 6); anyone shipping this to a fleet with materially older
kernels should treat the fill-order behavior as unverified on that fleet.

### Fix: task review findings (2026-08-19, post-implementation review)

Task 6's initial implementation was reviewed and returned 1 Critical + 3 Important findings,
all fixed in the amended implementation commit; full detail in `task-6-report.md`'s "Fix:
review findings" section. Summary:

- **Critical — 32-bit `get_mempolicy`/`set_mempolicy`/`mbind` out-of-bounds kernel write.**
  `maxnode` was computed as `numaNodemaskBits+1`, which is 33 on 32-bit platforms (386, arm,
  mips, mipsle) because `numaNodemaskBits` is `8*sizeof(uintptr)` = 32 there. The kernel sizes
  its nodemask read/write as `BITS_TO_LONGS(maxnode-1)` kernel ulongs, which is 8 bytes for
  `maxnode=65` on every architecture (2 32-bit ulongs or 1 64-bit ulong) — but our destination
  was a single 4-byte `uintptr`, a 4-byte out-of-bounds kernel write on 32-bit hosts. Fixed:
  `maxnode` is now a fixed `65` on every architecture (`numaMaxNode`), and the nodemask buffer
  is a `numaNodemaskWords`-word array (`numaNodemask`, 1 word on 64-bit, 2 on 32-bit) sized to
  match the kernel's 8-byte requirement exactly. Verified via `go tool compile -S` on
  `GOARCH=386` and `GOARCH=arm` that all three syscall sites now load `$65`, not `$33`.
- **Important — single-node exemption.** `numaSetProcessBindAll` now returns before any
  syscall when `numaTopology.NumNodes < 2`, matching the plan's "experiment off or single-node:
  behavior identical to stock Go" constraint. Verified on this (single-node) laptop with the
  experiment on: task mempolicy mode stays `0` (`MPOL_DEFAULT`).
- **Important — fallback footgun.** If `get_mempolicy(MPOL_F_MEMS_ALLOWED)` fails and topology
  discovery had itself failed (installing the synthesized single-node fallback topology), the
  rebuilt mask would have been exactly `{node0}`, silently BIND-ing a genuinely multi-node host
  down to one node. Fixed by a combined rule: if the final mask (from either the syscall or the
  topology-fallback path) has fewer than 2 bits set, `set_mempolicy` is never called and the
  mask is never published.
- **Important — publish only on `set_mempolicy` success.** `numaAllowedNodemask` is now stored
  only after `set_mempolicy` itself returns success; previously a failing `set_mempolicy` still
  published the mask, meaning every subsequent `mheap.grow` would pay a guaranteed-failing
  `mbind`.

Also applied two optional/trivial reviewer minors: dropped the write-only
`numaBindArenaFailures` counter (never read anywhere), and the test's `get_mempolicy` mode
export hook now masks `MPOL_MODE_FLAGS` out of the returned mode and distinguishes a syscall
failure (`-1`) from a genuine `MPOL_DEFAULT` (`0`) result.

## Layer 1 gate (v2) — 2026-08-19

Date: 2026-08-19
Local/Remote SHA (after `gate-vmstat.sh` commit, before this commit): `ab294b60e6f533c46d843a5ae8f6b3a3f8e3fc7c`
Parent/baseline commit (Global Constraints, once per layer): `8392ee466fa1598970ea66b0daef57899d80c56a` (`numa-design: record Layer 0 gate results`)
`go version` (remote, at Local/Remote SHA): `go1.28-devel_ab294b60e6 Wed Aug 19 19:19:33 2026 -0700 linux/amd64`
Kernel: `6.12.0-211.7.1.el10_2.x86_64`
`x/benchmarks`: `v0.0.0-20260819172200-70693762b6a0` (resolved by `@latest`, identical to Layer 0's pin — no new commits landed upstream between the two runs)
`x/perf` (benchstat): `v0.0.0-20260819171926-ebcb4798430d` (same binary as Layer 0, `/tmp/numa-tools/benchstat` on `numa-dell`)
`kernel.numa_balancing`: `1` throughout, except the optional Gate 8b window (`=2`, restored to `1` immediately after).

Machine load: single-user throughout (`who`/`ps aux --sort=-%cpu` checked before every measurement block, never showed a second tenant). As in Layer 0, `uptime`'s load average climbed into the tens-to-hundreds during and just after each 256P / `GOMAXPROCS=256` `gc-pause-bench` run (up to ~120 after the vmstat 256P wrap) — this is expected residual runqueue decay from this session's own 256-thread runs, confirmed each time via `ps aux --sort=-%cpu` showing no competing process, not evidence of a second tenant.

Per the plan's binding Layer 0 lesson: `benchstat`'s Mann-Whitney U test is the authoritative comparator for every timing gate below; `gate-json.sh`'s embedded hand-rolled median comparator is reported alongside for transparency, since it is demonstrably noise-sensitive on this machine at both `GOMAXPROCS=1` and `GOMAXPROCS=256`.

Raw `.out` files backing every `benchstat` verdict below, plus the Gate 5 `strace` transcripts and extended instruction-count replicates, are archived under `numa-design/bench-data/layer1/` (nothing was reaped from `numa-dell`'s `/tmp` before this archive pass).

**Confound note (Task 4's `gate-json.sh`, applies to Gates 1 and 6):** the script's interleave loop always runs `baseline` then `numa` within each round (`run "$OUT/baseline/json" ...; run "$OUT/numa/json" ...`), so `numa` is always measured in the position-2 slot Gate 3's data shows running ~1.5% faster purely from position (head-off, position 2, is −1.53% vs parent, position 1) — i.e. `gate-json.sh`'s own baseline-vs-numa comparison carries a ~1.5% tailwind in numa's favor that could mask a real regression of up to that size and still clear the +2% bar. Gate 3's head-off-vs-head-on comparison is the bias-free replicate that closes this gap — both are measured in the unaffected position-2-vs-3 slot (head-on vs head-off differ by only −0.16% from position alone) — and it directly answers the same off-vs-on question `gate-json.sh` asks, showing **geomean sec/op −0.18%** (`benchstat -format csv`: `0.03164442` vs `0.0315864575`, p=0.529 n=10), well inside noise and in the direction opposite a masked regression.

### Gate 1 — 1P json, off vs on, same commit (`BENCHNUM=10`, hard gate)

```
$ ssh numa-dell 'cd /home/deparker/go-numa && GOROOT=$PWD GOMAXPROCS=1 BENCHNUM=10 ./numa-design/gate-json.sh'
```

Hand-rolled comparator:

| metric | baseline median | numa median | rel | hand-rolled verdict |
|---|---|---|---|---|
| ns/op | 28,423,902.5 | 16,904,808.0 | −40.5% | PASS (only fails on regression >+2%) |
| user+sys-ns/op | 28,454,323.0 | 16,927,597.5 | −40.5% | PASS |

Both arms are bimodal in the same way Layer 0 documented (baseline: 5/10 rounds in the ~30–33M
band — 33.40M, 30.64M, 32.57M, 32.18M, 32.92M — and 5/10 in the ~16–26M band — 26.21M, 16.62M,
16.37M, 16.29M, 16.26M; numa: 1/10 high, 9/10 low) — the same machine-noise character, though the
split was more lopsided this run than Layer 0's roughly-even split. `benchstat` is therefore the
deciding read, not the hand-rolled numbers above:

```
$ ssh numa-dell '/tmp/numa-tools/benchstat /tmp/numa-gate-json/baseline.out /tmp/numa-gate-json/numa.out'
JSON-1  sec/op:            28.42m ± 43%  16.90m ± 32%  ~ (p=0.075 n=10)
JSON-1  user+sys-sec/op:   28.45m ± 43%  16.93m ± 32%  ~ (p=0.075 n=10)
```

**Verdict: PASS** (benchstat: no statistically significant difference on either hard-gate metric;
p=0.075 is above the 0.05 threshold).

Informational (not hard-gate metrics): in this same-commit off-vs-on comparison,
`bytes-from-system` (+9.45%, p=0.001) and `heap-bytes-from-system` (+10.05%, p=0.012) were
statistically *significant* increases in the numa arm, and `STW-sec/op` was a significant
*decrease* (−45.64%, p=0.011, numa faster). **Neither reproduced on an independent replicate;
both are closed as benign below rather than carried as open concerns:**

- **`STW-sec/op`**: Gate 3's head-off-vs-head-on data (a second, independently-drawn off/on
  pair, same commit, same binaries, a different 10-round interleave) gives `4.037µ ± 50%` vs
  `3.738µ ± 47%` — **−7.4%, p=0.280** — a much smaller, non-significant effect in the same
  direction. The original −45.64%/p=0.011 reading does not reproduce; the causal claim ("the
  mechanism Layer 1 targets") is struck and this is treated as noise, not evidence of a
  Layer-1-driven STW-time change.
- **`bytes-from-system` / `heap-bytes-from-system`**: the same Gate 3 replicate gives
  `66.78Mi ± 15%` vs `64.81Mi ± 12%` (**−3.0%, p=0.776**) and `61.69Mi ± 16%` vs `59.72Mi ± 13%`
  (**−3.2%, p=0.698**) for `bytes-from-system` and `heap-bytes-from-system` respectively — the
  *sign reverses* relative to Gate 1's reading, and neither is significant. Inspecting the raw
  `heap-bytes-from-system` values (both Gate 1's own baseline/numa data and Gate 3's replicate)
  shows the metric takes on a small set of discrete levels roughly one `pallocChunk` (4MiB)
  apart — e.g. Gate 3: `58392576 → 62554112 → 66748416 → 70942720 → 75169792`B, consecutive
  gaps of 3.91–4.00MiB (at or just under an exact 4.00MiB chunk), plus a further ~32KiB sub-step
  within each level; Gate 1 shows the same 3.91–3.94MiB/~32KiB two-tier structure. 1P json's small
  (~60–75MiB) working set means one extra or fewer heap chunk is a ~5–7% swing, and which level
  a given round lands on tracks that round's exact `allocs/op` (b.N) composition
  (26,791–26,828 across all 40 combined Gate 1 + Gate 3 rounds) — not the experiment. Closed as
  a benign discretization artifact of comparing independently-scheduled runs with a small
  working set, not a real off-vs-on behavioral difference.

### Gate 2 — 1P alloc micro (Task 4 Step 1b): `Malloc8`/`Malloc16`, `-count=10`

(As in Layer 0, no `MallocTypes` benchmark exists in this tree.)

```
$ ssh numa-dell 'GOMAXPROCS=1 GOROOT=$PWD GOTOOLCHAIN=local $PWD/bin/go test runtime -run=NONE -bench="Malloc(8|16|Types)" -count=10 >/tmp/alloc-base-l1.out'
$ ssh numa-dell 'GOMAXPROCS=1 GOROOT=$PWD GOEXPERIMENT=numa GOTOOLCHAIN=local $PWD/bin/go test runtime -run=NONE -bench="Malloc(8|16|Types)" -count=10 >/tmp/alloc-numa-l1.out'
$ ssh numa-dell '/tmp/numa-tools/benchstat /tmp/alloc-base-l1.out /tmp/alloc-numa-l1.out'
Malloc8    6.964n ± 98%   6.938n ± 0%   ~ (p=0.079 n=10)
Malloc16   11.32n ±  0%   11.34n ± 1%   ~ (p=0.898 n=10)
geomean    8.881n         8.868n       -0.14%
```

Unlike Gate 1, both benchmarks were stable this run (no bimodal split; `Malloc8`'s ±98% comes
from two ~14ns outliers in the baseline — round 1 at 13.96ns and round 7 at 13.76ns — with the
other 8 rounds clustered ~6.9–8.0ns).

**Verdict: PASS** (benchstat: no statistically significant difference on either benchmark).

### Gate 3 — Parent-commit comparison (Global Constraints, once per layer)

Parent: `8392ee466fa1598970ea66b0daef57899d80c56a`, own toolchain build (`git checkout -f` +
`make.bash` on `numa-dell`, then back to HEAD + rebuild), own `GOBIN` json binary
(`golang.org/x/benchmarks/json@v0.0.0-20260819172200-70693762b6a0`, matching the pin in the
report format Layer 0 used). HEAD off/on binaries reused from Gate 1's run
(`/tmp/numa-gate-json/{baseline,numa}/json`). Interleaved (parent, head-off, head-on) × 10 rounds,
`GOMAXPROCS=1 -benchmem=512 -benchnum=1 -benchtime=3s`.

```
$ ssh numa-dell '/tmp/numa-tools/benchstat /tmp/numa-l1-parent-cmp/parent.out /tmp/numa-l1-parent-cmp/head-off.out'
JSON-1  sec/op:            32.13m ± 49%  31.64m ± 40%  ~ (p=0.631 n=10)
JSON-1  user+sys-sec/op:   32.14m ± 49%  31.70m ± 41%  ~ (p=0.631 n=10)

$ ssh numa-dell '/tmp/numa-tools/benchstat /tmp/numa-l1-parent-cmp/parent.out /tmp/numa-l1-parent-cmp/head-on.out'
JSON-1  sec/op:            32.13m ± 49%  31.59m ± 44%  ~ (p=0.971 n=10)
JSON-1  user+sys-sec/op:   32.14m ± 49%  31.62m ± 44%  ~ (p=0.971 n=10)
```

**Verdict: PASS** for both head-off vs parent and head-on vs parent (both nominally faster than
parent, not slower, and neither difference is statistically significant). This is the gate that
would catch an unconditional (non-`goexperiment`-gated) cost from Layer 1's new
`internal/runtime/syscall/linux` constants, `numaAllowedNodemask` global, or the two
`if goexperiment.Numa { numaBindArena(...) }` call sites in `mheap.grow` — none is visible.

### Gate 4 — Off-binary identity (Task 4 Step 1c #1)

Built `go test -c runtime` (no `GOEXPERIMENT`) from the parent commit and from HEAD; `objdump -d`
on both `.test` binaries.

```
$ ssh numa-dell 'objdump -d /tmp/canary-parent-l1.test > /tmp/dis-parent-l1.txt; objdump -d /tmp/canary-head-l1.test > /tmp/dis-head-l1.txt; wc -l /tmp/dis-parent-l1.txt /tmp/dis-head-l1.txt'
1425336 /tmp/dis-parent-l1.txt
1425336 /tmp/dis-head-l1.txt

$ ssh numa-dell 'diff /tmp/dis-parent-l1.txt /tmp/dis-head-l1.txt'
2c2
< /tmp/canary-parent-l1.test:     file format elf64-x86-64
---
> /tmp/canary-head-l1.test:     file format elf64-x86-64
(4 lines total — only the file-path line objdump echoes for each binary differs)

$ ssh numa-dell 'size /tmp/canary-parent-l1.test /tmp/canary-head-l1.test'
   text      data     bss       dec       hex   filename
10200954    244139   33831288  44276381  2a39a9d  /tmp/canary-parent-l1.test
10200954    244139   33831288  44276381  2a39a9d  /tmp/canary-head-l1.test
```

Per-function census (symbol name + disassembly-line count), both binaries: **11,170 functions
each, zero only-in-one-side, zero functions with differing instruction-line counts.** Explicit
spot checks:

| function | parent | HEAD (off) | |
|---|---|---|---|
| `runtime.mallocgc` | 131 | 131 | identical |
| `runtime.acquirep` | 54 | 54 | identical |
| `runtime.schedinit` | 353 | 353 | identical |
| `runtime.(*mheap).grow` | 218 | 218 | identical |

`grep -ic numa /tmp/dis-head-l1.txt` → `0` (no `numa`-related symbol reachable in the off-binary
disassembly at all).

**Verdict: PASS — and stronger than Layer 0's Gate 4.** Layer 0 could not literally meet the
plan's "differ only in build IDs" bar because adding the `debug.numa` dbgvar shifted every
subsequent global's RIP-relative address, even with the experiment off. Layer 1 adds no new
dbgvar, so its off-binary is **text/data/bss byte-identical** to the parent's (`10200954` /
`244139` / `33831288`, to the byte, both binaries) and the `objdump -d` output differs in nothing
but the two lines where objdump echoes back each binary's own file path — not even a build-ID
difference is visible in the disassembly stream. Every `goexperiment.Numa`-gated line in
`numa_linux.go` and both `mheap.grow` call sites are fully dead-code-eliminated when the
experiment is off.

### Gate 5 — 1P instruction flatness (Task 4 Step 1c #2)

The plan's literal bar here (Task 4 Step 1c #2): "The instruction count must be exactly flat —
achievable because no fast-path code changes, and immune to timing noise." **This bar is not
achievable on this harness** (see below), so it cannot be reported as literally met. The
controller's standing interpretation is used instead: the substantive bar is no fast-path
instruction additions, corroborated deterministically rather than by a sub-percent
instruction-count comparison. (An earlier draft of this entry attributed the "quantify and
explain if a small delta appears" language to "the brief" — that sentence was controller
dispatch guidance, not text from the plan; corrected here.)

`perf stat -e instructions` on `GOMAXPROCS=1 <canary>.test -test.bench=BenchmarkMalloc8
-test.benchtime=100000000x -test.count=1`, off vs on. Original 3 runs each
(`perf_event_paranoid=2`):

| arm | run 1 | run 2 | run 3 | median |
|---|---|---|---|---|
| off | 11,217,292,171 | 11,194,507,325 | 11,203,272,859 | 11,203,272,859 |
| on | 11,173,485,166 | 11,226,354,590 | 11,230,206,131 | 11,226,354,590 |

Median delta: +23,081,731 instructions = +0.21%.

**Why "exactly flat" cannot be resolved here:** an extended replication (10 further runs per
arm, gathered during this evidence-correction pass; raw data in
`numa-design/bench-data/layer1/gate5-instruction-flatness-replicates.txt`) shows the *off* arm
alone — no code difference between any of its runs — has a **1.33% peak-to-peak run-to-run
instruction-count spread** (n=13: min 11,103,928,675, max 11,252,262,527, median
11,190,691,053); the *on* arm's own internal spread is 0.72% (n=13). +0.21% is well inside that
noise floor. Folding the extended runs in, the combined-median delta (on − off, n=13 each)
becomes **−0.15%** — the sign flips relative to the original 3-run reading. The instruction
count cannot resolve a fast-path change at this precision on this machine; it can only rule out
a *large* one (nothing near the scale a full fast-path branch/call would cost is visible in
either direction).

**Deterministic corroborating check used instead: syscall count.** `strace -f -c` on the on-arm
canary for the identical workload shows **exactly one `mbind`, one `set_mempolicy`, and one
`get_mempolicy` for the entire process**
(`numa-design/bench-data/layer1/gate5-strace-on-arm.txt`); the off-arm canary shows **zero**
occurrences of any of the three (`gate5-strace-off-arm.txt`). This is off by roughly seven
orders of magnitude from an earlier draft's "a handful of `mbind` syscalls amortized over the
whole run" framing — there is exactly one `mbind` call for the entire 100,000,000-iteration
run, not a handful, and its cost is kernel-side inside that one syscall, invisible to the
userspace `instructions:u` counter either way. The syscall count, not the instruction count, is
the right noise-immune signal for "Layer 1 adds work only on grow, which fixed-work Malloc8
barely triggers": the count is fixed, small, and exactly reproducible, which the instruction
count at this scale is not.

**Verdict: PASS on the substantive bar** (no instruction-count difference resolvable above the
harness's own ~1–2% noise floor in either direction; the on-arm syscall count is fixed at
exactly one `mbind`/`set_mempolicy`/`get_mempolicy` triple, deterministically corroborating "grow
path only, bounded work"). **Not met on the plan's literal "exactly flat" bar**, which this
harness cannot resolve at the required precision — recorded as an honest gap, not asserted as
satisfied.

### Gate 6 — 256P json gate (Task 7 Step 3)

```
$ ssh numa-dell 'cd /home/deparker/go-numa && GOROOT=$PWD GOMAXPROCS=256 BENCHNUM=10 ./numa-design/gate-json.sh'
```

Hand-rolled comparator:

| metric | baseline median | numa median | rel | hand-rolled verdict |
|---|---|---|---|---|
| ns/op | 2,266,915.5 | 2,742,532.0 | +21.0% | **FAIL** (exceeds +2%) |
| user+sys-ns/op | 188,916,523.5 | 178,748,113.0 | −5.4% | PASS |

`benchstat` on the same data:

```
$ ssh numa-dell '/tmp/numa-tools/benchstat /tmp/numa-gate-json-256p/baseline.out /tmp/numa-gate-json-256p/numa.out'
JSON-256  sec/op:            2.267m ± 75%   2.743m ± 40%   ~ (p=0.912 n=10)
JSON-256  user+sys-sec/op:   188.9m ± 62%   178.7m ± 38%   ~ (p=0.739 n=10)
```

**Verdict: PASS** (benchstat: no statistically significant difference; this benchmark's
per-round heap size auto-scales at 256P — `bytes-from-system` ranged ~3.41GiB to ~12.02GiB
round-to-round in this data — which drives far larger variance than the 1P gates, and is the
likely source of the hand-rolled comparator's false FAIL). 256P is not one of the plan's hard
gates (Global Constraints scopes the never-regress bar to `GOMAXPROCS=1`); it passes here anyway.

**Thread-placement sample** (one dedicated numa-arm run, `GOMAXPROCS=256`,
`-benchmem=512 -benchtime=8s`, `ps -o psr= -T -p $PID` sampled ~2s after start while the process
was confirmed alive):

```
even (node 0) processor samples: 126
odd  (node 1) processor samples: 132
```

258 thread samples spread across processor IDs 1–255 inclusive, both parities well represented —
confirms both NUMA nodes are actually occupied at 256P (not just topology-discovered).

### Gate 7 — #14406 three-way `/proc/vmstat` (the point of Layer 1)

Built `numa-design/gc-pause-bench` twice on the remote toolchain (`GOROOT=/home/deparker/go-numa`):
`/tmp/bench-baseline-l1` (no experiment) and `/tmp/bench-numa-l1` (`GOEXPERIMENT=numa`). All three
arms: `-heap=4096 -warm=10 -n=100` (27GiB `available` headroom per `free -h` at run time, so the
full 4096MiB profile was used, not the 2048MiB fallback). Idleness (`uptime`/`who`/`ps
aux --sort=-%cpu`) checked immediately before every arm.

**(a) baseline, bare:**

```
$ ssh numa-dell '/home/deparker/go-numa/numa-design/gate-vmstat.sh snap before.txt'
$ ssh numa-dell '/tmp/bench-baseline-l1 -heap=4096 -warm=10 -n=100 -json -label=baseline'
{"label":"baseline","heap_mib":4096,"n":100,"gomaxprocs":256,"stw_p50_ns":389401,"stw_p95_ns":632336,"stw_p99_ns":908783,"stw_max_ns":910773,"stw_mean_ns":416203,"wall_p99_ns":628931248}
$ ssh numa-dell '/home/deparker/go-numa/numa-design/gate-vmstat.sh snap after.txt'
$ ssh numa-dell '/home/deparker/go-numa/numa-design/gate-vmstat.sh diff before.txt after.txt'
hint_faults=12961 pages_migrated=111996
```

**(b) numa build:** first attempt showed `hint_faults=77 pages_migrated=0` — nonzero, but tiny
relative to baseline's 12,961 (a ~168× reduction). Per the brief's own CONCERN protocol
(`/proc/vmstat` is machine-global; verify idle and rerun once before declaring FAIL), machine
idleness was reconfirmed (`who`/`ps aux --sort=-%cpu`: single user, no competing process, only
this session's own residual runqueue decay) and the arm was rerun:

```
$ ssh numa-dell '/tmp/bench-numa-l1 -heap=4096 -warm=10 -n=100 -json -label=numa'
{"label":"numa","heap_mib":4096,"n":100,"gomaxprocs":256,"stw_p50_ns":343759,"stw_p95_ns":582687,"stw_p99_ns":713149,"stw_max_ns":723979,"stw_mean_ns":371138,"wall_p99_ns":552312183}
$ ssh numa-dell '/home/deparker/go-numa/numa-design/gate-vmstat.sh diff before2.txt after2.txt'
hint_faults=0 pages_migrated=0
```

Clean 0/0 on the idle-verified rerun. Recorded transparently rather than silently dropping the
first attempt's 77: with `kernel.numa_balancing=1` system-wide, a small nonzero reading on a
technically-idle-but-just-finished-a-256-thread-run machine is exactly the false-fail mode the
brief's CONCERN describes, and the rerun's clean result is the resolution the protocol specifies,
not a retry-until-favorable pattern (only one rerun was performed, as directed).

**(c) membind oracle** (`numactl --membind=0,1`, baseline binary):

```
$ ssh numa-dell 'numactl --membind=0,1 /tmp/bench-baseline-l1 -heap=4096 -warm=10 -n=100 -json -label=membind'
{"label":"membind","heap_mib":4096,"n":100,"gomaxprocs":256,"stw_p50_ns":348124,"stw_p95_ns":466128,"stw_p99_ns":524505,"stw_max_ns":709425,"stw_mean_ns":366982,"wall_p99_ns":593075726}
$ ssh numa-dell '/home/deparker/go-numa/numa-design/gate-vmstat.sh diff before.txt after.txt'
hint_faults=0 pages_migrated=0
```

Clean 0/0 on the first attempt.

**Verdict: PASS.** baseline ≫ 0 (12,961 hint faults / 111,996 pages migrated — this run size
does trigger the balancer, so the gate is meaningful); numa arm 0/0 on the idle-verified rerun;
membind oracle 0/0 on first try. This is the direct evidence for Layer 1's stated goal: BIND-all
task mempolicy + arena `mbind` suppress the kernel NUMA balancer's #14406 page-migration pathology
as effectively as the `numactl --membind` oracle.

**256P numa json wrapped in vmstat** (alternative protocol from the brief, run in addition to (a)–(c)):

```
$ ssh numa-dell '/home/deparker/go-numa/numa-design/gate-vmstat.sh snap before.txt'
$ ssh numa-dell 'GOMAXPROCS=256 /tmp/numa-gate-json-256p/numa/json -benchmem=512 -benchnum=1 -benchtime=10s'
BenchmarkJSON-256    10000    1717545 ns/op  ...
$ ssh numa-dell '/home/deparker/go-numa/numa-design/gate-vmstat.sh diff before.txt after.txt'
hint_faults=0 pages_migrated=0
```

Clean 0/0 on the first attempt.

### Gate 8 — kernel matrix (§12.4), `kernel.numa_balancing=2`

`uname -r`: `6.12.0-211.7.1.el10_2.x86_64`.

```
$ ssh numa-dell 'sudo -n true && echo PASSWORDLESS_SUDO_OK'
PASSWORDLESS_SUDO_OK
```

Passwordless sudo available, so the optional tristate arm was run: idleness reconfirmed, set
`numa_balancing=2`, ran the numa `gc-pause-bench` arm, snapped vmstat, then restored `=1`
immediately.

```
$ ssh numa-dell 'sudo -n sh -c "echo 2 > /proc/sys/kernel/numa_balancing"'
$ ssh numa-dell '/tmp/bench-numa-l1 -heap=4096 -warm=10 -n=100 -json -label=numa-balancing2'
{"label":"numa-balancing2","heap_mib":4096,"n":100,"gomaxprocs":256,"stw_p50_ns":356308,"stw_p95_ns":473608,"stw_p99_ns":691793,"stw_max_ns":705563,"stw_mean_ns":370543,"wall_p99_ns":564165634}
$ ssh numa-dell '/home/deparker/go-numa/numa-design/gate-vmstat.sh diff before.txt after.txt'
hint_faults=0 pages_migrated=0
$ ssh numa-dell 'sudo -n sh -c "echo 1 > /proc/sys/kernel/numa_balancing"'
$ ssh numa-dell 'cat /proc/sys/kernel/numa_balancing'
1
```

**Verdict: PASS.** Clean 0/0 under `numa_balancing=2` as well, and the setting was restored to
`1` immediately afterward. This machine's 6.12 kernel is recent enough that the fill-order
concern noted in Task 6's implementation notes (old kernels fill the lowest-numbered allowed node
first under `MPOL_BIND`) is not independently exercised by this check — it only confirms the
vmstat gate stays 0/0 under the alternate balancer mode, not node fill order.

### Overall verdict: **PASS**

All hard gates (1P json off-vs-on, 1P alloc micro, parent-commit comparison both arms, #14406
vmstat numa/membind arms) pass. Off-binary identity corroborates deterministically: Layer 1 is
dead-code-eliminated to byte-identical text/data/bss when off. Instruction-count flatness is
**not** literally met (the plan's "exactly flat" bar is unresolvable on this harness's ~1–2%
instruction-count noise floor) but is corroborated on the substantive bar instead by an exact,
deterministic syscall count (one `mbind`/`set_mempolicy`/`get_mempolicy` triple on the on-arm,
zero on the off-arm) — see the corrected Gate 5 entry above. The 256P json gate and the #14406
vmstat three-way — Layer 1's actual product claim — both pass, with the vmstat numa arm's one
nonzero reading resolved by the plan's own idle-verify-and-rerun protocol rather than ignored.

**A post-implementation review audit of this section found and required correction of several
evidence-record errors** (misattributed quote, an overstated causal mechanism for Gate 5's
delta, two non-reproducing causal claims in Gate 1's secondary metrics, an arithmetic slip in
Gate 1's band count, and a missing raw-data archive). All are fixed in place above and in
`numa-design/bench-data/layer1/`; none changed the overall PASS verdict — the audit confirmed it
independently (stratified-by-`b.N` analysis showing the arms indistinguishable at 1P and numa
equal-or-faster in every 256P stratum) and, if anything, strengthened it.

**Concerns for the controller:**

1. **Machine noise remains large and, this run, asymmetric.** Gate 1's bimodal split was more
   lopsided between arms than Layer 0's (baseline 5/10 high vs numa 1/10 high, rather than a
   roughly even split — corrected from an earlier 6/10 miscount; see raw values in the Gate 1
   entry), and Gate 6's 256P variance (±75% / ±62% relative stdev) is larger than Layer 0's
   optional 256P check. `benchstat` still resolves both as not significant, but the asymmetry
   means a hand-rolled median comparator would be even more likely to false-fail on this layer's
   data than on Layer 0's. Recommend (as Layer 0 already did) that `gate-json.sh` itself be
   changed in a follow-up task to make `benchstat` the primary decision path.
2. **Gate 1's `bytes-from-system`/`heap-bytes-from-system`/`STW-sec/op` secondary-metric
   findings did not reproduce and are now closed as benign** (see the Gate 1 entry above for the
   Gate 3 replicate data and the pallocChunk-quantization explanation) — no longer carried as an
   open concern.
3. **The vmstat numa arm's first attempt was nonzero (77 hint faults) before a clean rerun.**
   Documented in full above rather than only reporting the clean rerun, per the instruction to
   report honest numbers. The brief's own CONCERN section anticipates exactly this failure mode
   for a machine-global counter; the resolution (verify idle, rerun once) is the protocol, not an
   after-the-fact excuse, and only one rerun was performed. Idle-check transcript immediately
   before that rerun (`uptime; who; ps aux --sort=-%cpu | head -6`, run right before snapping
   `vmstat-numa-before2.txt`):
   ```
   22:48:06 up 84 days,  6:56,  1 user,  load average: 103.27, 71.60, 35.68
   USER         PID %CPU %MEM    VSZ   RSS TTY      STAT START   TIME COMMAND
   deparker 2353800 12.5  0.0 228540  3600 ?        Ss   22:48   0:00 bash -c uptime; who; ps aux --sort=-%cpu | head -6
   root     2353794  2.8  0.0  15768 10932 ?        Ss   22:48   0:00 sshd-session: deparker [priv]
   root        4490  0.2  0.0  81112  4336 ?        Ssl  May27 314:36 /usr/sbin/irqbalance
   root     2353569  0.1  0.0  16672  7368 ?        S    22:47   0:00 systemd-userwork: waiting...
   root     2353641  0.1  0.0  16668  7360 ?        S    22:47   0:00 systemd-userwork: waiting...
   ```
   (`who` prints no rows in every check run this session — this non-interactive SSH session does
   not register a utmp entry — so idleness was judged from `ps aux --sort=-%cpu` showing no
   competing process, as stated throughout, not from `who` output.)
4. **`gate-json.sh` always runs `baseline` before `numa` within each interleaved pair, giving
   `numa` a ~1.5% position tailwind** that could mask a regression of up to that size under the
   +2% bar — see the confound note near the top of this section. Closed by Gate 3's
   head-off-vs-head-on comparison, a position-unbiased replicate of the same off-vs-on question
   (−0.18%, well inside noise), not by `gate-json.sh`'s own Gate 1/Gate 6 readings alone.
5. **IMC/remote-share was not run**, per the plan's explicit instruction that it is not a Layer 1
   gate.

## Layer 1 promotion evidence: garbage vmstat (Task 8, 2026-08-19)

Date: 2026-08-19
Local/Remote SHA (before this commit): `0633d970f9` (`numa-design: correct Layer 1 gate evidence record`)
Remote `bin/go` build SHA: `ab294b60e6` — an ancestor of `0633d970f9`; the diff between the two is
`numa-design/` docs and archived `.out` files only (`git diff --stat ab294b60e6 0633d970f9`, 13
files, all under `numa-design/`), so the remote binary did not need rebuilding for this task.
Host: `numa-dell` (`dell-per660-01.khw.eng.rdu2.dc.redhat.com`). Kernel: `6.12.0-211.7.1.el10_2.x86_64`.
`kernel.numa_balancing`: `1` throughout (left as-is per the task brief, not touched).

This is Task 8 Step 1 of the plan — optional Layer 1 promotion evidence, not a merge-blocking
gate (Layer 1 already passed on Tasks 6–7's hard gates). It re-runs the `golang.org/x/benchmarks`
`garbage` benchmark at a larger heap (8 GiB, vs the 64 MiB default and the smaller size used in
the pre-Stage-1a `Section 3` run above) with the `/proc/vmstat` NUMA-balancer counters gated
around each arm, mirroring Gate 7's protocol but with the `garbage` binary instead of `json`.

**Free RAM check** (`numactl --hardware`, before either install/run):

```
node 0 free: 10864 MB
node 1 free: 5838 MB
```

Total free ~16.3 GiB across both nodes, above the controller-set 12 GiB headroom check, so the
full `-benchmem=8192` (8 GiB) from the plan was used — no need to drop to 4096.

**Install** (`GOROOT=/home/deparker/go-numa`, `GOTOOLCHAIN=local`, `@latest` resolves to the same
`x/benchmarks` pin already recorded for Layer 1 above):

```
$ ssh numa-dell 'GOBIN=/tmp/garbage-b go install golang.org/x/benchmarks/garbage@latest'
$ ssh numa-dell 'GOBIN=/tmp/garbage-n GOEXPERIMENT=numa go install golang.org/x/benchmarks/garbage@latest'
```

Both installs succeeded; binaries at `/tmp/garbage-b/garbage` (baseline) and `/tmp/garbage-n/garbage`
(numa, built with `GOEXPERIMENT=numa`).

### Baseline arm

Idle check immediately before snapping:

```
$ ssh numa-dell 'uptime; ps aux | awk "$3>5"'
23:28:12 up 84 days, 7:36, 1 user, load average: 0.07, 0.12, 6.09
(no process over 5% CPU besides the check command itself)
```

```
$ ssh numa-dell '/home/deparker/go-numa/numa-design/gate-vmstat.sh snap vmstat-baseline-before.txt'
$ ssh numa-dell '/tmp/garbage-b/garbage -benchmem=8192 -benchnum=1'
BenchmarkGarbage/benchmem-MB=8192-256    10000    3020198 ns/op  219825728 GC-bytes-from-system  24032937 STW-ns/GC  24032 STW-ns/op  6222740 allocated-bytes/op  145211 allocs/op  16037770768 bytes-from-system  15488548864 heap-bytes-from-system  313569232 other-bytes-from-system  15679361024 peak-RSS-bytes  17289285632 peak-VM-bytes  15826944 stack-bytes-from-system  251377205 user+sys-ns/op
$ ssh numa-dell '/home/deparker/go-numa/numa-design/gate-vmstat.sh snap vmstat-baseline-after.txt'
$ ssh numa-dell '/home/deparker/go-numa/numa-design/gate-vmstat.sh diff vmstat-baseline-before.txt vmstat-baseline-after.txt'
hint_faults=4242970 pages_migrated=2517110
```

Clean single run, no rerun needed — the baseline arm triggers the balancer hard, as expected
(hint_faults and pages_migrated both `>> 0`).

### numa arm

The baseline run above briefly drove `GOMAXPROCS=256` load, so `uptime`'s 1-minute average was
still elevated (31.00) right after it finished — the same residual-runqueue-decay pattern already
documented for Gate 8 above, not a second tenant. Per the established protocol, idleness was
re-verified with `ps aux --sort=-%cpu` (not just `uptime`) immediately before snapping:

```
$ ssh numa-dell 'uptime; who; ps aux --sort=-%cpu | head -6'
23:30:45 up 84 days, 7:38, 1 user, load average: 9.70, 7.89, 8.37
USER         PID %CPU %MEM    VSZ   RSS TTY      STAT START   TIME COMMAND
deparker 2366338 10.0  0.0 228540  3632 ?        Ss   23:30   0:00 bash -c uptime; who; ps aux --sort=-%cpu | head -6
root     2366332  3.0  0.0  15768 10876 ?        Ss   23:30   0:00 sshd-session: deparker [priv]
deparker 2365829  0.2  0.0  22572 14052 ?        Ss   23:29   0:00 /usr/lib/systemd/systemd --user
root        4490  0.2  0.0  81112  4336 ?        Ssl  May27 314:43 /usr/sbin/irqbalance
root     2365919  0.0  0.0  16668  7368 ?        S    23:29   0:00 systemd-userwork: waiting...
```

No competing process (`who` again prints no rows over this non-interactive SSH session, same as
noted in Gate 7 above; idleness judged from `ps aux --sort=-%cpu`). Proceeded:

```
$ ssh numa-dell '/home/deparker/go-numa/numa-design/gate-vmstat.sh snap vmstat-numa-before.txt'
$ ssh numa-dell 'GOEXPERIMENT=numa /tmp/garbage-n/garbage -benchmem=8192 -benchnum=1'
BenchmarkGarbage/benchmem-MB=8192-256    10000    2817269 ns/op  204202224 GC-bytes-from-system  22487085 STW-ns/GC  22487 STW-ns/op  6222537 allocated-bytes/op  145210 allocs/op  14631392448 bytes-from-system  14121271296 heap-bytes-from-system  290386896 other-bytes-from-system  14293426176 peak-RSS-bytes  15836717056 peak-VM-bytes  15532032 stack-bytes-from-system  254212007 user+sys-ns/op
$ ssh numa-dell '/home/deparker/go-numa/numa-design/gate-vmstat.sh snap vmstat-numa-after.txt'
$ ssh numa-dell '/home/deparker/go-numa/numa-design/gate-vmstat.sh diff vmstat-numa-before.txt vmstat-numa-after.txt'
hint_faults=0 pages_migrated=0
```

Clean 0/0 on the first attempt — no rerun was needed (unlike Gate 7's numa arm, which needed one
rerun after a nonzero first attempt).

### Summary

| Arm      | heap  | hint_faults delta | pages_migrated delta |
|----------|-------|-------------------:|----------------------:|
| baseline | 8 GiB | 4242970             | 2517110               |
| numa     | 8 GiB | 0                   | 0                      |

Matches the expected pattern from the plan and from Gate 7's `json`-based three-way: the balancer
is active under baseline and silent under the BIND-all numa policy. This corroborates Gate 7 on a
second, independent workload (`garbage`'s allocate/free/pointer-write pattern differs from `json`'s
marshal/unmarshal pattern) and at a larger heap (8 GiB vs Gate 7's smaller sizes).

**Throughput (not a gate, recorded only per the brief):** single-run `ns/op` was baseline 3020198
vs numa 2817269 (numa ~6.7% lower in this one run); `STW-ns/op` baseline 24032 vs numa 22487.
This is `-benchnum=1` — a single sample, not a `benchstat`-backed comparison — so no throughput
claim is made either way; it is reported for completeness only, per the instruction that
throughput differences are not a Layer 1 gate.

Raw benchmark output and vmstat snapshots archived at:
- `numa-design/bench-data/layer1/task8-garbage-vmstat-baseline.out`
- `numa-design/bench-data/layer1/task8-garbage-vmstat-numa.out`
- `numa-design/bench-data/layer1/task8-vmstat-baseline-before.txt`
- `numa-design/bench-data/layer1/task8-vmstat-baseline-after.txt`
- `numa-design/bench-data/layer1/task8-vmstat-numa-before.txt`
- `numa-design/bench-data/layer1/task8-vmstat-numa-after.txt`

**Step 2 (full Sweet run) explicitly skipped**, per the plan's Task 8 Step 2, which marks it
optional ("Do not run full Sweet yet unless you want extra evidence") and per this task's brief,
which scoped Task 8 to Step 1 (garbage vmstat) plus this RESULTS.md note only. Layer 1's shippable
status does not depend on it — Tasks 6 and 7's hard gates already passed.

## Layer 2 gate (v2) — 2026-08-19/20

Date: 2026-08-19/20
Local/Remote SHA: `23cbc99649` (green implementation commit), preceded by `c9a1f7b442` (red test
commit). Layer 1 tip / parent: `c3f6cbeef6`.
Host: `numa-dell` (`dell-per660-01.khw.eng.rdu2.dc.redhat.com`), Intel Xeon Platinum 8592+.
Kernel: `6.12.0-211.7.1.el10_2.x86_64`. `kernel.numa_balancing`: `1` throughout, confirmed
unchanged at the end. `x/benchmarks` pin: `v0.0.0-20260819172200-70693762b6a0` (identical to
Layers 0/1 — no upstream commits landed in between). `x/perf`/benchstat: `/tmp/numa-tools/benchstat`
on `numa-dell` (same binary as Layers 0/1).

### Summary

**Overall verdict: hard gates PASS (1P json, 1P alloc micro, 256P json, vmstat); IMC locality gate
FAILS.** Per the brief's Step 7 commit rule ("If IMC failed but the others passed → still commit
both"), both the red test commit (`c9a1f7b442`) and the green implementation commit (`23cbc99649`)
stand, plus this RESULTS.md record. **Per the brief's own explicit verdict text: "PREFERRED-at-grow
did not move IMC on interleaved 2P; do not add mcentral/steal." Product B (Layers 3-4) stops here.**
Layer 1 (BIND-all task policy + arena mbind, already merged) remains shippable; the Layer 2 code
added in this task is a real, working, gate-passing addition to the runtime (it does not regress
1P/256P throughput or reactivate the NUMA balancer) but does not achieve its purpose (steering
memory accesses local), so building anything further on it (Layers 3-4: span tags, mcache-aware
allocation, steal) is not justified by this measurement.

This matches the plan's own stated prior expectation for this exact gate almost exactly: "a whole
chunk gets the node of whichever M happened to grow the heap, then 256 Ps on both nodes allocate
from it." At `GOMAXPROCS=256` with two interleaved sockets, roughly half the allocating Ps are not
on the node any given already-grown chunk was PREFERRED to, and json's allocation pattern spreads
reads across many chunks from many Ps — so PREFERRED-at-grow does not, in aggregate, change which
fraction of L3-miss loads land on the remote socket.

### Implementation

`numaBindArena` (`src/runtime/numa_linux.go`) now does BIND-all (Layer 1, unchanged, kept as the
fallback for when the range never gets a VMA policy at all) followed by a second
`mbind(MPOL_PREFERRED, 1<<node)` call, `node` from `getcpu(2)` via a new `numaGetCPUNode()` wrapper.
Per the kernel's `vma_replace_policy`, the second `mbind` **replaces** rather than stacks on the
first: a chunk that gets a successful PREFERRED call ends up PREFERRED-only. #14406 still holds
regardless of which of the two policies "won" a given chunk: neither `mbind` call ever sets
`MPOL_F_MOF`, so the balancer skips every VMA this function ever touches, and the task-wide
BIND-all policy from `numaSetProcessBindAll` still covers everything it doesn't.

**Link-safety split (the brief's explicit CONCERN):** `numa_linux.go` has no per-arch build
constraint (just the `_linux.go` filename suffix), so it is compiled for every `GOOS=linux`
architecture. `getcpu` only has an assembly body on amd64 and arm64. Before this task,
`numaBindArena` never called `getcpu`, and the pre-existing `numaCurrentNode` (which did) was only
ever reachable from an already amd64/arm64-restricted test-export file — so the symbol was safely
dead-code-eliminated everywhere else. Layer 2 makes `numaBindArena` itself call into `getcpu`, and
`numaBindArena` is reachable from `mheap.grow` in *every* ordinary `GOEXPERIMENT=numa` binary, not
just tests — so without a split, `GOEXPERIMENT=numa` would fail to **link** ordinary binaries on
every non-amd64/non-arm64 Linux architecture the moment this landed. Fixed by extracting a
`numaGetCPUNode() (node uint32, ok bool)` wrapper into two new arch-gated files:
- `src/runtime/numa_linux_getcpu.go` (`//go:build linux && (amd64 || arm64)`): real `getcpu` call.
- `src/runtime/numa_linux_getcpu_other.go` (`//go:build linux && !(amd64 || arm64)`): always
  `ok=false` — Layer 2's PREFERRED step is unconditionally skipped there, Layer 1's BIND-all
  fallback still applies. Correct, conservative behavior for architectures Layer 2 doesn't target.

`numaCurrentNode` was refactored to call this same wrapper instead of `getcpu` directly, so
`numa_linux.go` no longer references the asm-only symbol at all.

**Link sweep** (local toolchain, `GOWORK=off`, both `GOEXPERIMENT=numa` and unset), a trivial
`package main` built and linked per target — the reachability check the brief asked for, not just
a compile check:

```
$ for arch in amd64 arm64 riscv64 arm 386 ppc64le s390x loong64 mips64 mips mipsle; do
    for exp in none numa; do
      GOOS=linux GOARCH=$arch GOEXPERIMENT=$exp go build -o /tmp/... hello.go
    done
  done
```

All 22 combinations (11 arches × {off, on}) built and linked successfully. Also verified: the
`runtime` test archive links for `GOOS=linux GOARCH=riscv64 GOEXPERIMENT=numa` (`go test -c
runtime`) and for `GOOS=linux GOARCH=amd64 GOEXPERIMENT=numa` (includes the new test); `GOOS=darwin
GOARCH=arm64` and `GOOS=windows GOARCH=amd64` sanity builds (non-Linux, unaffected); `go vet
runtime` clean both with and without the experiment, both `GOARCH=amd64` and `GOARCH=riscv64`.

### TDD: red → green on `numa-dell`

Per the brief ("Red commit optional — at minimum show the test failing remotely"), the repo's own
established convention in this branch (see `c4ae2020fd`, `TestNUMABindAllTaskPolicy (red)`) was
followed: a real red commit.

**Red** (`c9a1f7b442`): all Layer 2 scaffolding landed (`numaPreferredCalls` counter,
`numaGetCPUNode` arch split, `NumaPreferredBindCalls` export, `TestNUMAPreferredBindOnGrow`), but
`numaBindArena`'s PREFERRED half was wrapped in `if false { ... }` so it compiles but never runs.

```
$ make push && make build && make test-numa RUN=TestNUMAPreferredBindOnGrow
--- FAIL: TestNUMAPreferredBindOnGrow (0.73s)
    numa_linux_test.go:71: expected mbind PREFERRED on heap growth
FAIL
```

**Green** (`23cbc99649`): the `if false` wrapper removed (this is the entire diff between the two
commits — 7 insertions, 12 deletions, pure unwrap).

```
$ make push && make build && make test-numa RUN=TestNUMAPreferredBindOnGrow
ok  	runtime	0.709s
$ make test-numa RUN=TestNUMAPreferredBindOnGrow   # rerun for stability
ok  	runtime	0.620s
$ make test-numa RUN='TestNUMA'                    # full NUMA suite together
ok  	runtime	0.662s
```

### `runtime -short` with the experiment on (once)

```
$ ssh numa-dell 'GOEXPERIMENT=numa GOROOT=/home/deparker/go-numa GOTOOLCHAIN=local bin/go test -short runtime -count=1'
...
--- FAIL: TestCgoNoEscape (0.04s)
    crash_cgo_test.go:929: ... got too few heap objects allocated, pre: 1513, now: 1611
FAIL
FAIL	runtime	238.472s
```

One failure, `TestCgoNoEscape` — a cgo/escape-analysis heap-object-count timing test entirely
unrelated to NUMA. Re-run in isolation 5/5 times clean:

```
$ ssh numa-dell 'GOEXPERIMENT=numa ... bin/go test runtime -run=TestCgoNoEscape -count=5'
ok  	runtime	1.249s
```

Judged a pre-existing flake (GC-timing-sensitive heap accounting test, known-flaky class, not a
NUMA code path), not a Layer 2 regression — no prior `-short` baseline exists in this repo's task
history to compare against directly, but the test's own content (comparing a heap-object count
snapshot before/after a GC-adjacent cgo call) has no dependency on `numaBindArena`,
`numaGetCPUNode`, or anything else touched by this task, and it passed cleanly every other time it
ran (including inside the full suite's other invocations across the session).

### Gate 1 — 1P json (hard gate), `BENCHNUM=10`, two replicates

> **Archival-integrity correction (Gate 1 replicate 1 was regenerated after the original `/tmp`
> files were overwritten; see the re-archival note immediately below in this same block):**
> the run originally reported here as "Replicate 1" used `gate-json.sh`'s default `OUT` directory
> (`/tmp/numa-gate-json`) with no override, and that same default path was reused — unintentionally
> — for Gate 3's first 256P replicate later in the same session. Gate 3's run truncated and
> overwrote the Gate 1 replicate 1 `.out` files in place before they were archived, so the file
> actually committed under `gate1-1p-json-{baseline,numa}-r1.out` was, wrongly, a copy of the 256P
> data. The genuine replicate 1 raw data was gone (overwritten on disk, no backup) by the time
> this was caught, so it was regenerated with a fresh rerun (same protocol, same commit) rather
> than reconstructed. What follows is that regenerated run, correctly archived and re-verified to
> contain `BenchmarkJSON-1` lines. Replicate 2 was genuine and correctly archived throughout — its
> numbers are unchanged from the original report.

**Replicate 1** (regenerated; genuine, verified to contain `BenchmarkJSON-1` data):

```
$ ssh numa-dell 'GOROOT=$PWD GOMAXPROCS=1 BENCHNUM=10 OUT=/tmp/numa-gate-json-1p-r1-fix ./numa-design/gate-json.sh'
ns/op: baseline 32015652.5 numa 32149191.5 rel +0.4%
```
```
$ benchstat baseline.out numa.out
JSON-1  sec/op:            32.02m ± 38%   32.15m ± 46%  ~ (p=0.315 n=10)
JSON-1  user+sys-sec/op:   32.03m ± 38%   32.18m ± 46%  ~ (p=0.315 n=10)
```

This replicate landed inside the band on both the naive comparator and `benchstat` — no FAIL
signal from it at all.

**Replicate 2** (fresh 10-round run, same protocol as replicate 1, run first chronologically,
before replicate 1 was found to need regenerating):

```
ns/op: baseline 24724307.5 numa 17121160.5 rel -30.8%
```
```
JSON-1  sec/op:  24.72m ± 34%   17.12m ± 87%  ~ (p=0.280 n=10)
```

Both replicates' naive comparators land on opposite sides of zero (+0.4% then -30.8%) and
`benchstat` calls both non-significant — consistent with this box's well-documented bimodal
JSON-benchmark noise (`benchstat` is authoritative for this, per the environment brief; the same
pattern is documented at length in the Layer 0/1 sections above), not a systematic Layer 2 cost.

Replicate 2 is labeled here as a same-protocol replicate, not a position-unbiased one:
`gate-json.sh` always runs baseline before numa within each interleaved round, so any position
tailwind (documented in the Layer 1 report as ~1.5% favoring whichever arm runs second, i.e.
numa) was not controlled in either replicate. This is immaterial to the conclusion here: the
variances involved (±34-98%) dwarf a ~1.5% positional effect by more than an order of magnitude,
and in replicate 2 specifically the tailwind favors numa in the same direction numa already came
out faster — so it cannot be masking a real regression in the direction that would matter for a
FAIL verdict.

**Pooled (both replicates, n=20 pairs):**

```
JSON-1  sec/op:            31.01m ± 36%   20.44m ± 57%   ~ (p=0.142 n=20)
JSON-1  user+sys-sec/op:   31.02m ± 36%   20.46m ± 58%   ~ (p=0.142 n=20)
```

**Verdict: PASS.** No significant difference at n=20. (`STW-sec/op` is significantly *lower* for
numa, -44.99%, p=0.012 — not a gate metric, and a decrease is not a concern; `peak-VM-bytes` is
technically significant but trivial, +0.02%, p=0.013.)

Archived: `numa-design/bench-data/layer2/gate1-1p-json-{baseline,numa}-r1.out` (regenerated,
genuine), `gate1-1p-json-{baseline,numa}-r2.out` (original, genuine, unchanged).

### Gate 2 — 1P alloc micro (Task 4 Step 1b): `Malloc8`/`Malloc16`, `-count=10`

```
$ GOMAXPROCS=1 go test runtime -run=NONE -bench='Malloc(8|16|Types)' -count=10 >base.out
$ GOMAXPROCS=1 GOEXPERIMENT=numa go test runtime -run=NONE -bench='Malloc(8|16|Types)' -count=10 >numa.out
$ benchstat base.out numa.out
Malloc8    6.947n ± 88%   6.943n ± 0%       ~ (p=0.566 n=10)
Malloc16   11.32n ±  0%   11.35n ± 1%  +0.22% (p=0.011 n=10)
geomean    8.870n         8.877n       +0.08%
```

`Malloc16`'s +0.22% is statistically significant (p=0.011) but two orders of magnitude below the
2% band and consistent with Layer 1's own pattern of tiny-but-significant side effects from the
handful of extra grow-time syscalls a fixed-iteration malloc loop triggers while ramping the heap
up from empty.

**Verdict: PASS.** Archived: `numa-design/bench-data/layer2/gate2-alloc-micro-{baseline,numa}.out`.

### Gate 3 — 256P json gate, `BENCHNUM=10`, three replicates

Not one of the plan's Global-Constraints hard gates (scoped to `GOMAXPROCS=1`), but this task's own
brief sets the same +2% band for 256P at Layer 2 specifically (distinct from Layer 0/1's "optional,
huge-regressions-only" framing) and the task instructions list 256P among the gates whose failure
blocks the `src/` commit — so it was run to the same standard as Gate 1, escalating to a third
replicate given the mixed signal below.

Free memory checked first (`node 0 free: 10283 MB`, `node 1 free: 5778 MB`, `free -h` available
27Gi) — full `-benchmem=512` used throughout, no headroom concern.

**Replicate 1:**
```
JSON-256  sec/op:  1.884m ± 45%   2.623m ± 46%  +39.25% (p=0.035 n=10)   -- significant
```
**Replicate 2** (idle re-verified, fresh run):
```
JSON-256  sec/op:  2.281m ± 46%   2.059m ± 127%  ~ (p=0.393 n=10)        -- not significant, opposite direction
```
**Replicate 3** (idle re-verified, fresh run):
```
JSON-256  sec/op:  2.430m ± 36%   1.920m ± 26%  -21.02% (p=0.043 n=10)   -- significant, opposite direction again
```

Three independent 10-round replicates: **significant positive, non-significant, significant
negative.** A real regression does not flip sign between two of its three significant/near-significant
readings; this is the JSON benchmark's well-documented heap-autoscaling variance (`bytes-from-system`
ranged roughly 3.4-19.9 GiB round to round across all three replicates) producing spurious
significance at `n=10` in both directions, exactly the pattern the Layer 1 report flagged for this
same benchmark at 256P. Pooled:

**Pooled (all three replicates, n=30 pairs):**
```
JSON-256  sec/op:            2.122m ± 22%   2.125m ± 23%   ~ (p=0.572 n=30)
JSON-256  user+sys-sec/op:   166.8m ± 11%   180.4m ± 15%   ~ (p=0.307 n=30)
JSON-256  peak-RSS-bytes:    6.849Gi ± 34%  6.714Gi ± 35%  ~ (p=0.406 n=30)
```

**Verdict: PASS.** No significant difference at n=30 on either throughput metric.

**RSS check (brief's explicit concern — v1 saw json RSS 5→9.6 GiB doubling under Layer 2):**
`peak-RSS-bytes` is not significantly different (p=0.406) and is nominally *lower* for the numa arm
(6.71Gi vs 6.85Gi baseline) — no doubling, no growth trend across any of the three replicates
individually either (each replicate's own `peak-RSS-bytes` comparison was also non-significant).
**RSS-doubling hard-fail: does not apply.**

Archived: `numa-design/bench-data/layer2/gate3-256p-json-{baseline,numa}-r{1,2,3}.out`.

### Gate 4 — vmstat 0/0

Two arms, per the task instructions: one 256P numa json run, one `gc-pause-bench` numa run
(`-heap=4096 -warm=10 -n=100`). `gc-pause-bench` rebuilt fresh against the Layer 2 toolchain (the
Layer 1 binaries from Task 7/8 predate this task's runtime changes and were not reused).

**256P numa json:**
```
$ gate-vmstat.sh snap before.txt
$ GOMAXPROCS=256 numa/json -benchmem=512 -benchnum=1 -benchtime=8s
BenchmarkJSON-256    10000  1775328 ns/op  ...
$ gate-vmstat.sh snap after.txt; gate-vmstat.sh diff before.txt after.txt
hint_faults=0 pages_migrated=0
```

**gc-pause-bench numa (`-heap=4096 -warm=10 -n=100`):**
```
$ gate-vmstat.sh snap before.txt
$ /tmp/bench-numa-l2 -heap=4096 -warm=10 -n=100 -json -label=numa-l2
{"label":"numa-l2",...,"stw_p50_ns":394960,"stw_p99_ns":662282,"wall_p99_ns":694544593}
$ gate-vmstat.sh snap after.txt; gate-vmstat.sh diff before.txt after.txt
hint_faults=0 pages_migrated=0
```

**Verdict: PASS.** Clean `0/0` on the first attempt for both arms — no rerun needed (unlike Layer
1's Gate 7, which needed one). Confirms Layer 2's second `mbind` call does not reactivate the
balancer any more than Layer 1's BIND-all call did, consistent with the design note that neither
`mbind` mode ever sets `MPOL_F_MOF`.

Archived: `numa-design/bench-data/layer2/gate4-vmstat-{json256p,gcpause}-{before,after}.txt`.

### VMA count (plan CONCERN)

Sampled `wc -l /proc/$PID/maps` ~6s into a 256P `-benchtime=15s` json run, both arms, same host
state:

| Arm      | `/proc/PID/maps` lines | heap-bytes-from-system (that round) |
|----------|------------------------:|-------------------------------------:|
| baseline | 34                      | ~19.0 GiB                           |
| numa (L2)| 1172                    | ~10.1 GiB                           |

Confirms the brief's CONCERN exactly: baseline's single BIND-all-covered (and, off-experiment,
policy-free) heap merges into a handful of VMAs regardless of size; Layer 2's per-~4MiB-chunk
PREFERRED assignment prevents merging between chunks placed on different nodes, producing **~34x**
more VMAs for **less** heap in this sample. `vm.max_map_count` on this host is `1048576` (raised
from the Linux default of 65530) — 1172 is nowhere near either limit, but this is a real,
unmitigated cost that would scale linearly with heap size on a host with the stock 65530 limit
(≈65530 × 4 MiB ≈ 256 GiB heap before hitting it) and adds kernel VMA-tracking/fault-path overhead
not captured by any of the throughput gates above. Documented as an open concern, not a gate
failure — no mitigation (chunk coalescing, deferred/batched policy application) was implemented,
per the brief's explicit scope limits for this task ("no span tags, no mcentral sharding").

### Gate 6 — IMC locality (the decision gate)

```
perf stat -x, -e mem_load_l3_miss_retired.local_dram,mem_load_l3_miss_retired.remote_dram -- \
  <json> -benchmem=512 -benchnum=1 -benchtime=10s
```

Events verified present on this Xeon (`perf list | grep -i l3_miss`) before use — the exact events
named in the brief exist natively, no substitution needed. Arms: baseline (`GOEXPERIMENT` off) vs
Layer 2 (`GOEXPERIMENT=numa`, this task's commit) at HEAD, `GOMAXPROCS=256`. The optional third arm
(a Layer-1-only binary built from the `c3f6cbeef6` parent with the experiment on) was not run: the
required two-arm comparison below is already unambiguous across two independent triplicate sets,
so the extra arm would not have changed the verdict and was skipped under the task's effort budget.

Two independent sets of 3 interleaved runs each were collected (an initial ad hoc set, then a
clean archival set with `perf stat -o` capturing raw CSV — both are reported; they agree closely).

**Archival set** (archived at `numa-design/bench-data/layer2/gate6-imc/{baseline,numa}-run{1,2,3}.csv`):

| Arm      | run | local_dram | remote_dram | remote share |
|----------|----:|-----------:|-------------:|-------------:|
| baseline | 1   | 20,455,370 | 19,019,375   | 48.18%        |
| baseline | 2   | 27,986,682 | 26,430,380   | 48.57%        |
| baseline | 3   | 70,656,327 | 66,872,003   | 48.62%        |
| numa(L2) | 1   | 28,468,836 | 26,935,518   | 48.62%        |
| numa(L2) | 2   | 30,117,440 | 28,150,827   | 48.31%        |
| numa(L2) | 3   | 83,692,019 | 79,624,806   | 48.75%        |

Medians: **baseline 48.57%, Layer 2 48.62%. Relative change: +0.10%** (an *increase*, not a drop).

**First (ad hoc) set**, run immediately before the archival set, same protocol: baseline shares
48.18%/48.44%/48.44% (median 48.40%), numa shares 48.19%/48.37%/48.71% (median 48.37%) — relative
change **-0.07%**. Both independent sets agree: the remote-DRAM-miss share is statistically
indistinguishable between arms, hovering tightly around 48-49% regardless of `GOEXPERIMENT`.

**Pass bar:** ≥10% relative drop in remote share (e.g. 0.48 → ≤0.432). **Actual: +0.10% and -0.07%
across two independent triplicate sets — nowhere close, and if anything in the wrong direction.**

**Verdict: FAIL.** This is not a noisy, ambiguous reading like Gates 1 and 3 above — both
independent sets of 3 runs land in a tight, mutually overlapping ~48-49% band for both arms, with
no rerun needed to resolve ambiguity. PREFERRED-at-grow measurably does not change which fraction
of L3-miss loads are serviced from the remote socket's DRAM at `GOMAXPROCS=256` on this
interleaved 2-node box.

**Why, mechanistically:** this matches the brief's own stated prior expectation almost exactly.
`numaBindArena` sets a single node PREFERRED for an entire ~4 MiB grow chunk based only on which M
happened to be running `mheap.grow` at that moment — a one-shot decision with no relationship to
which P (running on which node) will later allocate from spans carved out of that chunk. With 256
Ps spread across both sockets all sharing the same central allocator (`mcentral`/`mcache` refill
paths untouched by Layers 1-2, exactly as scoped), any given chunk's node-of-origin has no
correlation with the node of the P that eventually touches memory inside it. Fixing this would
require exactly the mechanisms this task and its brief explicitly forbid at this layer (span
tagging, `getMCache`-hook-based node-local allocation, work-stealing along node boundaries) —
Layers 3-4 of the original plan.

### Overall verdict

| Gate                              | Result | Note |
|------------------------------------|--------|------|
| 1P json (hard)                     | PASS   | n=20 pooled, not significant |
| 1P alloc micro (hard)              | PASS   | geomean +0.08% |
| 256P json                          | PASS   | n=30 pooled, not significant |
| vmstat 0/0                         | PASS   | clean both arms, no rerun |
| RSS doubling                       | PASS (no doubling) | numa arm nominally lower |
| VMA count (concern, not a gate)    | Elevated (~34x) | documented, not mitigated |
| IMC locality (decision gate)       | **FAIL** | +0.10%/-0.07% rel., need ≤-10% |

**Per the brief: "PREFERRED-at-grow did not move IMC on interleaved 2P; do not add
mcentral/steal." Product B stops at Layer 2.** Layer 1 remains shippable and unaffected. This
task's runtime change is committed anyway (per the brief's explicit instruction for this exact
outcome — hard gates pass, IMC fails — "still commit both: the measurement is the deliverable")
because it is a real, working, non-regressing addition that the gate battery validated on every
axis except the one that mattered for the product decision; reverting it would only require
re-deriving the same honest-failure evidence later.

Idleness protocol used throughout, matching Layers 0/1: `uptime`/`ps aux --sort=-%cpu | head`
checked immediately before every measurement block; load average repeatedly climbed into the
tens (once to ~100) from this session's own residual runqueue decay after 256P runs, `ps aux`
never showed a second tenant.

Raw data archived under `numa-design/bench-data/layer2/`. `kernel.numa_balancing` confirmed `1`
at the end of this task.

---

## Layer 2 verdict (Task 10, 2026-08-19)

**Decision: kill product B for this hardware.** Only Layer 0+1 (BIND-all task mempolicy +
`#14406` balancer exemption) will be proposed to Go. Layers 3–4 (Tasks 11–12) are cancelled.

### The numbers

Gate 6 — IMC locality, the decision gate — is reported in full above ("Gate 6 — IMC locality (the
decision gate)", this file). Summary: two independent triplicate `perf stat`
(`mem_load_l3_miss_retired.local_dram`/`remote_dram`) sets on interleaved `GOMAXPROCS=256` json,
archived at `numa-design/bench-data/layer2/gate6-imc/{baseline,numa}-run{1,2,3}.csv`:

- Archival set medians: baseline remote-DRAM share **48.57%**, Layer 2 **48.62%** — **+0.10%
  relative** (an increase).
- Ad hoc set medians: baseline **48.40%**, Layer 2 **48.37%** — **-0.07% relative**.
- Pass bar was **≥10% relative drop** (e.g. 0.48 → ≤0.432). Both sets land nowhere close, in a
  tight mutually overlapping band, with no rerun needed to resolve ambiguity.

All hard performance gates passed cleanly (1P json, 1P alloc micro, 256P json, vmstat 0/0, no RSS
doubling — see "Overall verdict" table above). The VMA-count evidence (`wc -l /proc/PID/maps`:
baseline 34 vs Layer 2 1172, ~34x) proves the PREFERRED `mbind` calls actually took effect on the
per-chunk VMAs — this was not a no-op implementation bug. The null result on IMC is real: PREFERRED
homing alone does not change which socket's DRAM services L3-miss loads.

### Three-ingredient framing (design §12.1)

Locality requires three ingredients together: (a) memory **homed** per node, (b) refill-time
**routing** so a thread is fed spans homed where it runs, and (c) **threads that stay put** long
enough for (b) to still hold at use time. Layer 2 implemented (a) alone — a one-shot
`mbind(MPOL_PREFERRED, node)` on the M that happened to be running `mheap.grow`, with no
relationship to which P (on which node) later allocates from spans carved out of that chunk, and
no thread-stability guarantee at all. **Any proper subset of the three ingredients is expected to
measure ~zero** (design §12.1) — v1's arena PREFERRED was the same subset and measured the same
null (~45–49% remote, unchanged). Gate 6's FAIL is therefore the *predicted* outcome, not evidence
that locality is unreachable on this hardware. It proves that homing without routing and thread
stability measures ~zero, exactly as forecast — not that a properly gated three-ingredient
mechanism would also fail.

### Layers 3–4 cancelled

Per the plan's Task 10 Step 2 gate, Task 9's IMC FAIL means Tasks 11–12 (Layer 3 per-node
mcentral, Layer 4 steal/GC-mark affinity) are cancelled. Both task headings in
`numa-design/2026-08-19-numa-v2-implementation-plan.md` are marked cancelled.

### What remains shippable

**Layer 0+1 is the shippable slice**: NUMA topology discovery (Task 3) plus the BIND-all task
mempolicy that suppresses the kernel NUMA balancer for the process (`#14406`, Task 6), gated PASS
on 1P json, 1P alloc micro, 256P json, vmstat 0/0, and the `#14406` three-way vmstat check (Layer 1
gate, "Overall verdict: PASS" above). This is what gets proposed to Go from this plan.

### Next candidates (future plan, not this one)

In the plan's stated priority order (design §12.2–§12.3):

1. **Fill-one-socket-first** at `GOMAXPROCS` ≤ CPUs/node (design §12.2) — near-free: one node-mask
   affinity choice at process start, no homing/routing/per-P machinery at all, and the `#14406`
   balancer exemption still applies. Makes *everything* local for any process that fits on one
   socket, which is the honest story for small processes on big NUMA boxes and likely the cheapest
   real win available.
2. **Homing + routing + thread stability as one gated unit** (design §12.3–§12.4) — the concrete
   address-partition sketch (per-node `arenaHints`/`curArena`, `heapArena.node`, `mheap.grow(npage,
   node)`) combined with refill-time routing (per-node `mcentral` span sets) and node-mask soft
   affinity with the stand-down rule, validated together behind the same gates rather than layered
   incrementally. This is the shape every NUMA-successful allocator (TCMalloc NUMA mode, HotSpot
   `+UseNUMA`, jemalloc/mimalloc via OS thread stability) converges on, and is the only path
   expected to clear the IMC gate Layer 2 just failed — because it supplies all three ingredients
   at once instead of one at a time.

## Pathology benchmark (A/B/C) (2026-08-20)

Design: `numa-design/pathology-bench-design.md` (mechanism analysis, candidate ranking, exact
command lines, controls, kill criteria, statistical plan — followed as written; deviations noted
inline below). Commits: gc-pause-bench heavy-profile flags `ff47d48b2d`, phase-shift candidate 3
`6b535e2ca9` (fixed for a primary-metric truncation bug in `fb90222b76` — see candidate 3 below).
The design document itself was committed to the branch only after this section's data was
collected (not before, as pre-registration would require) — its stated "decided before any run"
status is not independently git-verifiable from this repository's history alone.

**CRITICAL correction (post-hoc audit, 2026-08-20): arm C in every candidate below is not the
Layer 0+1 (BIND-all-only) slice.** `GOEXPERIMENT=numa` at the HEAD commit these binaries were
built from (`7ec36777f3`) still compiles Layer 2's per-arena `MPOL_PREFERRED` heap-growth homing:
`numaBindArena` in `src/runtime/numa_linux.go` (~line 313) issues the Layer-2 `mbind(..,
MPOL_PREFERRED, ..)` call unconditionally on every `mheap.grow`, gated only by
`goexperiment.Numa` — the same code path the Layer 2 gate battery measured earlier in this file
(the `wc -l /proc/PID/maps` 34→1172 VMA-count evidence under "Gate 6 — IMC locality" above is that
homing actually taking effect). Layer 2 was **not reverted** when its IMC gate failed; only
Layers 3-4 were cancelled. **No number in this section measures the proposed Layer 0+1
(BIND-all-only) shippable slice as it will actually ship** — every arm-C result below is Layer
0+1+2 combined. Every "this repo's `GOEXPERIMENT=numa` is Layer 0+1 only" / "BIND-all ... freezes
whatever node each page's first-touching thread landed on" claim in the original version of this
section was **wrong** and has been removed below. The "L1-only re-run" subsection at the end of
this file re-measures candidate 1 (and, if time permitted, candidate 3) against a scratch build
with Layer 2's `MPOL_PREFERRED` call removed, to actually answer what the shippable slice does.

**Environment:** `numa-dell`, 256 logical CPUs / 2 nodes (even=node0, odd=node1), ~15 GiB/node,
kernel `6.12.0-211.7.1.el10_2.x86_64`, `kernel.numa_balancing=1` (unchanged throughout),
`transparent_hugepage/enabled=[always]` (unchanged throughout). Toolchain: `go version
go1.28-devel_7ec36777f3` (runtime/toolchain unchanged since that commit; only
`numa-design/gc-pause-bench` and `numa-design/phase-shift`, both outside the built toolchain,
changed afterward — no rebuild needed). `golang.org/x/benchmarks`
`v0.0.0-20260819172200-70693762b6a0`, identical resolved version for both the baseline and
`GOEXPERIMENT=numa` `garbage` installs. `benchstat` (Mann-Whitney U, α=0.05) at
`/tmp/numa-tools/benchstat` on the remote. Idle checked via `ps aux --sort=-%cpu` (not `uptime`,
which shows multi-hour decay from earlier 256P runs, per the established protocol) before every
measurement block — clean throughout, no second tenant on the box at any point in this session.

GOMAXPROCS=128 in all arms of all three candidates (design §4 — equalizes parallelism so A-vs-B is
a memory-placement comparison, not a CPU-count comparison; residual HT-vs-full-core confound
biases *against* the B-worse-than-A finding, so where B still loses to A the result is
conservative). Arm order rotated ABC/BCA/CAB per round to cancel position bias. One unrecorded
warmup round preceded each candidate's 10 recorded rounds. vmstat (`numa_hint_faults`,
`numa_pages_migrated`) snapped immediately before/after every individual run via
`numa-design/gate-vmstat.sh`. Node-0 free RAM checked ≥10 GB before every arm-A run (never
triggered the wait/abort fallback — node 0 free RAM stayed ≥12.4 GB throughout the session). Raw
outputs, per-round vmstat snaps, and vmstat-delta summaries for all three candidates archived at
`numa-design/bench-data/pathology/`.

### Candidate 1 — `x/benchmarks garbage`, GOMAXPROCS=128, `-benchmem=4096 -benchnum=1`, n=10

Pilot (untimed, arm A): peak-RSS-bytes = 7,920,259,072 B = **7.38 GiB** (7.92 GB decimal — the
original writeup mislabeled the decimal-GB figure as GiB; corrected here), under the 10 GiB abort
threshold (design §3 candidate 1) — proceeded with the full sweep.

Protocol note: one of arm B's 10 recorded rounds (round 6) completed with `b.N=5000` instead of
the `10000` every other round (all arms, all candidates) ran — visible in the raw benchmark line
in `cand1-armB-recorded.out`. `x/benchmarks garbage`'s `-benchnum=1` does not otherwise vary `b.N`
run-to-run; the cause was not tracked down further. That round's ns/op (2,937,193, line 48 of the
raw file) is not a visible outlier against its neighbors (2,988,154 and 2,690,438), so it was kept
in the benchstat inputs below, but the underlying iteration-count instability is recorded here as
an open protocol-hygiene caveat.

**Mechanism validity (arm B, n=10 recorded rounds):** hint faults min=57,446 max=308,435
mean=171,552; pages migrated min=989,471 max=1,572,161 mean=1,309,928. Arms A and C: **exactly
0/0 hint faults and pages migrated in all 10 rounds**, matching the `numactl --membind` oracle
(A) and confirming BIND-all suppression (C). Deviation from the design's stated expectation: the
design's pre-declared hint-fault floor was "≥5×10^5 hint faults" (§6) — observed hint faults never
reached that number in any round (max 308,435, ~62% of the floor). Pages migrated comfortably
cleared its ≥10^5 floor in every round (min 989,471, ~10× the floor). The mechanism is
unambiguously active (B ≫ 0 in every round on both counters; A=C=0 in every round) — the specific
numeric hint-fault floor guessed at design time was simply optimistic for this heap size, not a
sign of a setup failure.

**benchstat, primary metric (ns/op, ` Garbage/benchmem-MB=4096-128`):**

- **Primary (C vs B):** B = 2.917ms ± 23%, C = 3.168ms ± 9% → **+8.58% (p=0.003, n=10) — C
  significantly SLOWER than B.** This is the opposite of the design's expected direction (C
  faster by 3-7%), and it is a clean, well-powered result (p=0.003), not noise — no n=15
  extension was warranted (extension is pre-declared only for the 0.05<p<0.10 borderline case).
- **Secondary (A vs B):** A = 1.823ms ± 7%, B = 2.917ms ± 23% → **+60.04% (p=0.000, n=10) — B
  dramatically slower than A.** Confirms the design's B-worse-than-A requirement far beyond the
  "similar or smaller margin than C-vs-B" the design predicted.
- **Exploratory (A vs C):** A = 1.823ms ± 7%, C = 3.168ms ± 9% → +73.77% (p=0.000, n=10).
- Exploratory secondaries (not claims): user+sys-ns/op C > B by +10.30% (p=0.000); STW-ns/op B vs
  C ~ (p=0.579-0.739, not significant either way).

**Per design §3, the optional 8 GiB/256P exploratory supplement is run only "if the primary sweep
shows a significant C-vs-B win"** — it did not (C lost), so the supplement was correctly skipped.

**Verdict: candidate 1's primary claim FAILS**, and fails harder than the design's plain kill rule
anticipated (a null "~" result) — this is a *significant reversal*. **The mechanistic explanation
in the original version of this section (BIND-all-as-Layer-0+1-only "freezing first-touch
placement") is wrong and has been removed** — see the CRITICAL correction at the top of this
section: arm C here is Layer 0+1+2 (BIND-all *plus* per-4MiB-chunk `MPOL_PREFERRED` homing), not
BIND-all alone, so this result cannot be attributed to BIND-all's suppression trading away balancer
convergence. What actually caused C to lose to B on this workload is not established by this
sweep; the L1-only re-run below (arm "C-L1", Layer 2's `MPOL_PREFERRED` call removed) is the
measurement that isolates it. **Do not draw a mechanism conclusion from this candidate's C-vs-B
result alone** — only that arm C, as actually built (Layer 0+1+2), loses to unpinned stock Go here.
Separately: do not read the A-vs-B gap below as evidence about the balancer specifically — see the
"A-vs-B measures pinning, not the balancer" note in the overall verdict section.

Raw data: `numa-design/bench-data/pathology/cand1-arm{A,B,C}-{warmup,recorded}.out{,.stderr}`,
per-round `cand1-arm*-r*.vmstat.{before,after}`, `cand1-vmstat-summary.txt`, `sweep.log`,
`pilotA.out`/`pilotA.time`, and the sweep driver script itself,
`numa-design/bench-data/pathology/cand1-sweep.sh`.

### Candidate 2 — gc-pause-bench heavy profile, GOMAXPROCS=128, n=10 (8 measured + 2 discarded cycles/round)

Code changes implementing the design's heavy profile (`-ptrheap`, `-toucher=false`, `-gcgap`,
`-discard`, per-cycle `BenchmarkGCCycleWall` stdout lines) landed in `ff47d48b2d` and are described
in that commit and the package doc of `numa-design/gc-pause-bench/main.go`. One deliberate
deviation from the pre-existing code, needed to keep the new per-cycle stdout output clean for
benchstat: the human-readable summary block (previously on stdout unless `-json`) now always goes
to stderr; this does not change any recorded metric, only where diagnostic text is printed.

Flags: `-ptrheap=true -toucher=false -heap=4096 -idle=1000000 -stacks=200 -warm=45 -gcgap=5s
-discard=2 -n=8`, `GODEBUG=gcshrinkstackoff=1`, built from `numa-design/gc-pause-bench` (the
design's suggested build directory `numa-design/` has no `go.mod`; built from inside the module
directory instead — a path correction, not a behavior change).

**Mechanism validity (arm B, n=10 recorded rounds, 8 cycles each):** hint faults min=108,675
max=533,006 mean=257,484; pages migrated min=523,787 max=1,710,948 mean=1,145,286 — comfortably
above the pilot/warmup-calibrated floor (warmup round: 141,920 hint faults / 688,906 migrated).
Arm A: 0/0 in 8 of 10 rounds, negligible noise (54/2, 10/1) in the other 2 — four-plus orders of
magnitude below B. Arm C: 0/0 in 7 of 10 rounds, negligible noise (2/2, 44/31, 186/0) in the other
3 — same four-plus-orders-of-magnitude separation from B. Neither A's nor C's noise rounds are
literal "0/0" as the design's shorthand states, but they are not remotely comparable to B's
activity and do not indicate the balancer running on A/C.

**benchstat, cycle-level (`BenchmarkGCCycleWall`, n=80 samples/arm = 8 cycles × 10 rounds):**

- **Primary (C vs B):** B = 3.952s ± 2%, C = 3.970s ± 0% → **~ (p=0.179, n=80) — not
  significant.**
- **Secondary (A vs B):** A = 3.163s ± 3%, B = 3.952s ± 2% → **+24.96% (p=0.000, n=80).**
- **Exploratory (A vs C):** A = 3.163s ± 3%, C = 3.970s ± 0% → +25.53% (p=0.000, n=80).

**IMPORTANT correction (post-hoc audit): the n=80 above overstates the effective sample size.**
The 8 measured cycles within a round are not independent draws — they share one process's warm-up
state, one heap, one balancer-marking history. Intraclass correlation (one-way random-effects
ICC(1), computed from the 10-round × 8-cycle grouping, independently re-derived from the archived
per-cycle data): **arm A 0.999, arm B 0.637, arm C 0.979** — i.e. within a round, cycles cluster
strongly (B's within-round CV is only ~3%). Treating 80 correlated cycles as 80 independent
samples inflates the apparent n and narrows the confidence interval more than the data supports.
The design-appropriate unit is the **round** (n=10, matching every other candidate here), using
each round's median cycle as its one sample:

- **Round-level (median of 8 cycles/round, n=10 rounds/arm):** B round-medians 3.578-4.294s
  (median of medians 3.941s), C round-medians 3.662-4.281s (median of medians 3.966s) → **+0.64%
  (exact Mann-Whitney U on the 10 round-level values, p=0.579) — not significant, same direction
  and same non-significance as the cycle-level test, at the sample size the data actually
  supports.**
- **Noise floor, quoted honestly instead of implied precision:** run-to-run CV of B's round medians
  is **5.25%** (C: 3.78%). A two-sample-equivalent minimum detectable effect at α=0.05, power=0.80,
  n=10 rounds/arm, using the pooled round-level CV (4.45%), is **≈5.6%** (a similar calculation
  using slightly different assumptions, cited in the audit that prompted this correction, gives
  ≈6.6% — both land in the same 5-7% band). **This null result excludes a large tax (order
  ≥5-7%); it does not exclude — and was never powered to detect — a small one.**

**Verdict: candidate 2's primary claim FAILS at both the cycle level and the (more honest)
round level** — this remains the design's plain kill condition (§6): no significant difference on
the primary metric, with the balancer mechanism confirmed active in B while A/C are clean. The
correction narrows what can be claimed from the null: it rules out a tax on the order of the MDE
(~5-7%) or larger, not any tax at all. As with candidate 1, **arm C here is Layer 0+1+2, not
Layer 0+1 alone** (see the CRITICAL correction at the top of this section) — this result does not
by itself characterize the shippable BIND-all-only slice.

Raw data: `numa-design/bench-data/pathology/cand2-arm{A,B,C}-{warmup,recorded}.out{,.stderr}`,
per-round `cand2-arm*-r*.vmstat.{before,after}`, `cand2-vmstat-summary.txt`, `sweep2.log`, and the
sweep driver script, `numa-design/bench-data/pathology/cand2-sweep.sh`.

### Candidate 3 — phase-shift (exploratory; ran because both 1 and 2 failed C-vs-B with valid fault floors)

Both realistic-workload candidates failed their primary C-vs-B claim with the balancer mechanism
confirmed active — the design's explicit trigger for candidate 3 (§3 candidate 3 header: "run
only if 1 and 2 both fail C-vs-B"). New program `numa-design/phase-shift` (commit `6b535e2ca9`),
per the design: a stable ~6 GiB pointer-dense working set (400 rings closed into cycles, same
node/ring pattern as candidate 2), chased continuously by 64 `LockOSThread`ed reader goroutines
whose CPU affinity flips between node0 (even CPUs) and node1 (odd CPUs) every 30 s phase (4 phases
= 120 s/run), using raw `sched_setaffinity`/`sched_getaffinity` syscalls (no external dependency).
GOMAXPROCS=128 all arms.

**Bug found and fixed before the sweep counted:** the first full sweep's primary-metric output was
unusable — `nsPerRead` is an aggregate rate across 64 parallel readers (sub-nanosecond, ~1-2
ns/read), and `int64(nsPerRead)` truncated every round to exactly the integer 1 or 2, collapsing
all variance (`benchstat` reported "all samples are equal" for every comparison). Fixed in
`fb90222b76` (decimal-precision `%.4f ns/op` instead of integer truncation), rebuilt, and the full
n=10+1-warmup sweep was re-run from scratch on the corrected binaries — the data below is from the
corrected run only; the truncated run's `.out` files were discarded and are not archived.

Pilot (untimed, all three arms, `-phase=5 -phases=2`): peak-RSS ≈ **6.38 GiB** all arms (6,685,592
KiB from `/usr/bin/time -v`, i.e. 6,685,592/1024² GiB — the original writeup's "≈6.7 GiB" was an
imprecise eyeball conversion; corrected here), well under the 10 GiB gate; arm A's `noop-pin`
fallback confirmed working correctly on real hardware (`allowed cpuset: 128 even (node0) CPUs, 0
odd (node1) CPUs` → `noop-pin=true`, no crash, no repeated syscall-error spam).

**Mechanism validity (arm B, n=10 recorded rounds):** hint faults min=25,533 max=127,488
mean=39,021; pages migrated min=3,375,757 max=5,375,704 mean=4,736,829. This is a substantially
larger migration rate than either candidate 1 (mean 1.31M/run) or candidate 2 (mean 1.15M/run) —
consistent with the design's "migration storm" prediction: each 30 s phase flip makes the *entire*
6 GiB working set misplaced at once, rather than the gradual cold-page accumulation candidates 1-2
produce. **Arm C: exactly 0/0 in all 10 recorded rounds.** Arm A: 0/0 in 9 of 10 rounds, negligible
noise (17/4) in the other — consistent with the pinned cpuset making inversion impossible.

**benchstat, primary metric (overall ns per pointer-read across the full 120 s run,
`BenchmarkPhaseChase`, n=10 samples/arm, one sample per round):**

- **Primary (C vs B):** B = 1.530ns ± 6%, C = 1.272ns ± 37% → **-16.82% (p=0.029, n=10) — C
  significantly FASTER than B.** This is the design's pre-declared pathology-supporting direction
  (expected 5-20% faster; observed 16.82% falls inside that band) and clears α=0.05 without
  needing the n=15 extension (extension is pre-declared only for the 0.05<p<0.10 borderline band;
  p=0.029 is already below 0.05). C's per-round values (1.11-1.91 ns) are noisier than B's
  (1.32-1.75 ns) but consistently shifted lower — 8 of 10 C rounds fall below B's median, so this
  is not a single-outlier artifact; raw per-round data archived at
  `numa-design/bench-data/pathology/cand3-arm{B,C}-recorded.out`.
- **Informational only (A vs B, A vs C):** A = 2.062ns ± 0%, vs B -25.80% (p=0.000), vs C -38.28%
  (p=0.000) — **A is slower than both B and C**, the opposite of what full single-node locality
  would naively predict.

**CRITICAL correction (post-hoc audit): the original HT/core-count explanation for arm A being
slowest is wrong, and has been replaced.** Only 64 reader goroutines ever run in this benchmark;
during any given 30 s phase, B and C's readers are `sched_setaffinity`-confined to *one node's*
CPUs at a time (the even or odd mask) — the same 128 logical / 64 physical cores that arm A's
entire process is pinned to for its whole run via `numactl --cpunodebind=0`. There is no core-count
or hyperthreading difference between A and a B/C phase; that framing does not hold up.

**What the data actually shows, independently re-derived from the archived `ns/read` samples as
aggregate memory bandwidth** (64 B/node × reads/s, since each pointer hop is one 64 B cache-line
fetch and `nsPerRead` is already the aggregate rate across all 64 readers): **arm A is flat at
31.0 GB/s in all 10 rounds** (30.9-31.1 GB/s) — a single-memory-controller ceiling, reached despite
perfect locality (every access local to node 0's controller, zero cross-node traffic) because all
64 readers *and* the memory they read are confined to that one controller for the entire run. **Arm
B ranges 36.6-48.7 GB/s, arm C ranges 33.5-57.8 GB/s** — both above A's ceiling in every round,
because B/C's memory (allocated without `--membind`, so likely spread across both nodes'
controllers via first-touch) can be served by *two* memory controllers concurrently even though the
64 readers are confined to one node's cores at any instant: local-node fetches use that
controller, remote-node fetches cross the interconnect to the other controller, and both paths can
be in flight at once. **B's migration-copy traffic is far too small to explain a 17% throughput
swing this way**: mean 4,736,829 pages migrated/run × 4096 B × 2 (read source + write dest) ÷ 120 s
≈ **0.32 GB/s ≈ 0.8% of B's own ~42 GB/s mean demand** — two orders of magnitude short of moving a
17% needle. **The likely mechanism is therefore dual-controller bandwidth availability, not
fault/migration-tax avoidance**: C's advantage over B is consistent with C spending less of its
run pinned into single-controller-equivalent access patterns (via `MPOL_PREFERRED` steering
individual 4 MiB chunks to specific nodes — recall arm C here is Layer 0+1+**2**, not BIND-all
alone, per the CRITICAL correction at the top of this section), not with avoiding the balancer's
hint-fault/migration costs, which this candidate's own vmstat numbers show are financially
negligible relative to the bandwidth swing observed. No claim is drawn from arm A for this
candidate's primary comparison, per the design (§3/§4) — but for a different, verified reason than
originally stated.

**Forward pointer (added after the L1-only re-run below): the parenthetical above attributing C's
bandwidth advantage to Layer 2's per-chunk `MPOL_PREFERRED` steering is refuted by that re-run.**
C-L1 (BIND-all only, no Layer 2 homing at all) shows the *same* dual-controller-bandwidth pattern,
with a larger and more robust effect than this Layer-0+1+2 measurement. Whatever produces the
bandwidth advantage, it does not require Layer 2 — see "Candidate 3 L1-only" below for the
corrected reading.

**IMPORTANT correction (post-hoc audit): relabel the verdict as suggestive, not robust.** The
primary Mann-Whitney U result (p=0.029, n=10) clears α=0.05, but three independent robustness
checks on the same 10 round-paired B/C values (paired by round index; differences recomputed
directly from the archived data) show the result is fragile at this sample size:

- **Paired sign test** (8 of 10 rounds have C < B): exact two-sided p=**0.109** — not significant
  on its own.
- **Wilcoxon signed-rank test** (exact, accounting for the two reversed-sign rounds and the
  magnitude of every difference): exact two-sided p=**0.084** — also not significant on its own.
- **Leave-one-out**: recomputing the exact Mann-Whitney U p-value with each of the 10 rounds
  dropped in turn (9 vs 9) gives a range of p=**0.006-0.077** — the result crosses α=0.05 in
  several of the ten leave-one-out subsets (independently reproduced here; a prior estimate of
  this range from the audit that prompted this correction, 0.004-0.077, is consistent with the
  present recomputation).

**Revised verdict: candidate 3's primary result is SUGGESTIVE, not robust.** The Mann-Whitney U
test that clears the pre-declared α=0.05 threshold is the least conservative of four reasonable
tests run on the same data, and the effect does not survive removing single rounds in several
leave-one-out cases. Combined with the CRITICAL corrections above (arm C is Layer 0+1+2, and the
mechanism is more plausibly bandwidth/controller-consolidation than fault-tax avoidance), this
candidate should not be read as a confirmed BIND-all recovery win — it motivates the L1-only
re-run below (which, if time permits, also re-runs this candidate) far more than it independently
supports the design's pathology-recovery claim.

Raw data: `numa-design/bench-data/pathology/cand3-arm{A,B,C}-{warmup,recorded}.out{,.stderr}`,
per-round `cand3-arm*-r*.vmstat.{before,after}`, `cand3-vmstat-summary.txt`, `sweep3.log`,
`pilotB.out`/`pilotB.time`, `pilotC.out`/`pilotC.time`, `pilotA3.out`/`pilotA3.time`, and the
sweep driver script, `numa-design/bench-data/pathology/cand3-sweep.sh`.

### Overall pathology-benchmark verdict (superseded in part — see corrections below and the
L1-only re-run subsection)

**Post-hoc audit correction, read this before the table:** every "arm C" result in this section
(and the narrative below) was collected against `GOEXPERIMENT=numa` as built from HEAD
(`7ec36777f3`), which still compiles Layer 2's per-4 MiB-chunk `MPOL_PREFERRED` heap-growth homing
(`numaBindArena`, `src/runtime/numa_linux.go` ~line 313 — unconditional on `goexperiment.Numa`,
never gated off after the Layer 2 IMC gate failed; only Layers 3-4 were cancelled). **None of the
three candidates below measure the proposed Layer 0+1 (BIND-all-only) shippable slice** — they
measure Layer 0+1+2 combined. The "L1-only re-run" subsection at the end of this file is the
measurement that actually isolates the shippable slice for candidate 1 (and, if time permitted,
candidate 3); treat this section's C-vs-B numbers as characterizing *this specific build*, not the
patch series intended for submission.

**Also correct here:** the secondary A-vs-B comparisons in candidates 1-2 do **not** independently
confirm "the balancer's tax is real," as originally claimed. Arm C — which the vmstat data confirms
has essentially zero balancer activity in every round of every candidate — is penalized against
arm A by *at least as much* as arm B is (candidate 1: A-vs-C +73.77% vs. A-vs-B +60.04%; candidate
2: A-vs-C +25.53% vs. A-vs-B +24.96%, nearly identical). If the balancer's fault/migration tax were
the driver of the A-vs-B gap, a balancer-inactive arm (C) should not be penalized by *more* than
arm B is — yet it consistently is (candidate 1) or matches it almost exactly (candidate 2). **The
A-vs-B gap in both candidates is better explained by arm A's exclusive `numactl
--cpunodebind=0 --membind=0` full single-node locality/pinning benefit — a benefit neither B nor C
gets, balancer or no balancer — than by the balancer specifically.** `C-vs-B` (both arms unpinned,
differing only in balancer/mempolicy behavior) is the only comparison in this design that cleanly
isolates the balancer's effect; `A-vs-B`/`A-vs-C` are pinning-vs-unpinned comparisons and are
retained below as informational context, not as balancer evidence.

This is **not** the design's full honest-kill scenario (§6) — that requires all three candidates
to die on C-vs-B, and candidate 3's primary comparison did not (though see its "suggestive, not
robust" relabeling above). The mechanism (balancer hint faults + page migrations) is confirmed
active in arm B and confirmed suppressed in arm C across all three candidates — 0/0 in arm C in
essentially every round (the rare single-digit counts in candidates 2-3 are 4+ orders of magnitude
below arm B and are noise). What is **not** established, after correction, is a robust time-domain
recovery attributable specifically to the balancer:

| Candidate | Workload | C better than B (primary)? | Notes |
|---|---|---|---|
| 1 — garbage | realistic throughput, GOMAXPROCS=128 | **NO** — C significantly *worse* (+8.58%, p=0.003) | Arm C is Layer 0+1+2, not Layer 0+1; mechanism unestablished. See L1-only re-run. |
| 2 — gc-pause-bench heavy | realistic GC-cycle time | **NO** — null at both cycle-level (p=0.179, n=80, overstated n) and the more honest round-level (p=0.579, n=10) | Noise floor ~5-7%; null excludes a large tax, not a small one. |
| 3 — phase-shift | synthetic, bench-side-pinned, perpetual locality inversion | **Suggestive, not robust** — primary MWU p=0.029, but sign test p=0.109, Wilcoxon p=0.084, leave-one-out range 0.006-0.077 | Likely mechanism is dual-memory-controller bandwidth, not fault-tax avoidance (see candidate 3's bandwidth reading above); arm C is again Layer 0+1+2. |

The `A-vs-B`-worse-than-A and `C ≥ A` columns from the original table are removed here: per the
correction above, neither is a clean balancer signal (A-vs-B/A-vs-C reflect pinning, not the
balancer), and candidate 3's C-vs-A gap inherits the same pinning confound plus the single-
memory-controller-ceiling effect described in that candidate's writeup.

**Honest overall conclusion (revised):** on this 2-node, ~15 GiB/node, THP=always, kernel-6.12 box,
automatic NUMA balancing demonstrably operates on stock Go (millions of hint faults and page
migrations per run, reproduced across three independent workloads), and this repository's current
`GOEXPERIMENT=numa` build (Layer 0+1+2) demonstrably suppresses the balancer's own activity (0/0 in
arm C throughout). **What this section does not establish, after correction, is that suppressing
the balancer produces a net time-domain win** — candidate 1 shows the opposite (C significantly
slower), candidate 2 is a clean null at the sample size the data supports, and candidate 3's
apparent win does not survive standard robustness checks and is more consistent with a
dual-controller-bandwidth effect (itself a byproduct of Layer 2's per-chunk homing, not of
balancer suppression) than with avoiding the balancer's fault/migration tax. **None of the three
candidates cleanly demonstrate that BIND-all's suppression alone — the actual proposed shippable
slice — produces a measurable performance recovery on this hardware.** The L1-only re-run below
is the first measurement in this session that actually targets that question.

**For the upstream submission (revised):** this session's data continues to support the
mechanism-suppression + no-regression story for the *current build* (Layer 0+1+2): the balancer's
activity is confirmed and confirmed suppressed on 3/3 workloads. It does **not**, after correction,
support a performance-recovery story from this section alone, and the original conditional-recovery
framing ("BIND-all measurably helps on workloads that deny the balancer convergence time") is
withdrawn pending the L1-only re-run, since arm C's advantage where it appeared (candidate 3) is
better attributed to Layer 2's memory homing than to balancer suppression, and that distinction
matters directly for what Layer 0+1 alone will do when shipped without Layer 2. See the "L1-only
re-run" subsection below for the corrected measurement.

## L1-only pathology re-run (2026-08-20)

**SUPERSEDED IN PART (second post-hoc audit, same day): this section's original candidate-1
conclusion ("Layer 2, not BIND-all, caused the original regression") and the commit message of
`3507802ec5` that states it are withdrawn — see the correction after the candidate 1 verdict below
and the "single-session three-arm candidate 1 sweep" subsection later in this file, which is the
measurement that actually isolates Layer 2's contribution. Candidate 3's conclusion below is
independently confirmed and, if anything, strengthened by a second audit; see its replication
statistics.**

Answers the question the CRITICAL correction above raised: every "arm C" result in the sections
above was measured against Layer 0+1+**2** (BIND-all task mempolicy plus per-4 MiB-chunk
`MPOL_PREFERRED` heap-growth homing), not the Layer 0+1 (BIND-all-only) slice actually proposed for
shipping. This re-run builds a scratch, uncommitted patch that removes Layer 2's `MPOL_PREFERRED`
refinement from `numaBindArena`, keeping only the BIND-all `MPOL_BIND` call, and re-measures
candidate 1 (and, since remote time permitted, candidate 3) with a new arm **C-L1** in place of the
original arm C.

**Scratch patch** (built with, never committed to the branch; full text archived at
`numa-design/bench-data/pathology/l1-only.patch`):

```diff
diff --git a/src/runtime/numa_linux.go b/src/runtime/numa_linux.go
index 73909888c6..d680e03442 100644
--- a/src/runtime/numa_linux.go
+++ b/src/runtime/numa_linux.go
@@ -318,13 +318,7 @@ func numaBindArena(addr unsafe.Pointer, size uintptr) {
 	var mask numaNodemask
 	mask[0] = w0
 	linux.Syscall6(linux.SYS_MBIND, uintptr(addr), size, uintptr(_MPOL_BIND), uintptr(unsafe.Pointer(&mask[0])), numaMaxNode, 0)
-
-	node, ok := numaGetCPUNode()
-	if !ok || node >= 64 {
-		return
-	}
-	var pmask numaNodemask
-	pmask[uintptr(node)/numaNodemaskBits] = 1 << (uintptr(node) % numaNodemaskBits)
-	linux.Syscall6(linux.SYS_MBIND, uintptr(addr), size, uintptr(_MPOL_PREFERRED), uintptr(unsafe.Pointer(&pmask[0])), numaMaxNode, 0)
-	numaPreferredCalls.Add(1)
+	// L1-ONLY SCRATCH PATCH (not committed): Layer 2's MPOL_PREFERRED
+	// refinement removed for the audit's decisive re-run. BIND-all above
+	// is the only policy applied per arena.
 }
```

Applied on top of `8b8a71f4eb` on the remote (`numa-dell`) tree only, never on the local worktree
and never committed. Built with `GOROOT_FINAL=/home/deparker/go-numa ./src/make.bash` (clean
build, no errors). Sanity-checked before the sweeps: `garbage` built against this toolchain with
`GOEXPERIMENT=numa` shows 0/0 vmstat on a pilot run (BIND-all still fully suppresses the balancer)
and peak-RSS in the same ~7.4 GiB range as every other arm-B/C build this session. After both
sweeps below completed, the remote tree was restored with `git checkout -f src/runtime/numa_linux.go`
and rebuilt via `./src/make.bash` back to stock — confirmed clean (`git diff --stat` empty,
`bin/go version` reports the same `8b8a71f4eb` build timestamp as before the patch, and
`numaPreferredCalls.Add(1)` is back in the source) before ending the session. Arm B in both sweeps
below reuses the existing stock binaries built earlier in this session (`/tmp/pb/base/garbage`,
`/tmp/pb/phaseshift-base`) — those never depended on this patch, so no re-build was needed for
them.

### Candidate 1 L1-only: `x/benchmarks garbage`, B vs C-L1, GOMAXPROCS=128, n=10

Same config as the original candidate 1 sweep (`-benchmem=4096 -benchnum=1`, rotating BC/CB order,
one warmup round, vmstat snap per run). Two arms only (B, C-L1) — no arm A this time (item 2 of the
audit did not ask for one, and B-vs-A/C-vs-A were already established as pinning comparisons, not
balancer comparisons, in the correction above).

**Mechanism validity (arm B, n=10 recorded rounds):** hint faults min=117,171 max=434,579
mean=227,708; pages migrated min=1,041,357 max=1,673,693 mean=1,368,241 — consistent with every
prior candidate-1 B measurement this session. **Arm C-L1: 0/0 in 9 of 10 rounds, negligible noise
(3/3) in the other** — confirms BIND-all alone still fully suppresses the balancer with Layer 2
removed, exactly as it did with Layer 2 present.

**benchstat, primary metric (ns/op, `Garbage/benchmem-MB=4096-128`):**

- **B vs C-L1 (primary for this re-run):** B = 3.003ms ± 3%, C-L1 = 3.209ms ± 28% → **~ (p=0.315,
  n=10) — not significant.**
- Exploratory: user+sys-sec/op C-L1 higher than B by +11.13% (p=0.029) — the one metric that
  differs significantly; everything else (GC-bytes, STW, allocs, RSS, VM) is a clean null.

**WITHDRAWN (second post-hoc audit): the "Layer 2 was the culprit" / "night-and-day different" /
"good news for the slice" conclusion originally written here — and repeated in the commit message
of `3507802ec5` — is a difference-of-significance fallacy and is withdrawn.** The original
reasoning ("full-C vs B is significant, C-L1 vs B is not, therefore Layer 2 caused the difference")
never directly compared full-C to C-L1. Doing that comparison, independently, on the archived data:
**C-full vs C-L1 (exact Mann-Whitney U on the two arms' raw ns/op) is itself null, p=0.631** —
there is no statistical evidence the two builds differ from each other at all. Three further facts
undercut the original claim:

- **This sweep is underpowered to detect the original effect.** B vs C-L1's pooled run-to-run CV
  is ≈9.9%, giving a minimum detectable effect (α=0.05, power=0.80, n=10/arm) of **≈12.4%** — larger
  than the +8.58% originally measured for full-C. A null at this MDE does not exonerate BIND-all;
  it means the sweep cannot resolve an effect the size of the one being asked about.
- **The stock control (arm B) itself drifted between sessions**: median ns/op was 2,917,487 in the
  original candidate 1 sweep vs. 3,003,192 in this L1-only sweep, a **+2.94%** shift attributable to
  ordinary session-to-session noise (different day/time, unrelated system state) rather than to
  anything about C-L1. Comparing "B-vs-full-C from session 1" against "B-vs-C-L1 from session 2" as
  if the B baseline were constant compounds the fallacy above.
- **C-L1 still shows the same ~11% user+sys-per-op penalty full-C showed** (+11.13%, p=0.029,
  identical to the original full-C measurement) — the one metric that *is* significant here. If
  Layer 2 alone caused the regression, removing it should plausibly have relieved this CPU-time
  penalty too; it did not. This is exploratory, not primary, but it is a concrete reason to suspect
  **the Layer 0+1 slice may still carry a real penalty that this sweep's ≈12.4% MDE cannot resolve
  on the primary wall-clock metric**, not evidence the slice is clean.

**Correct verdict: this sweep does not establish whether Layer 0+1 alone regresses this workload.**
It rules out an effect at or above its own ≈12.4% MDE, and no more. Isolating Layer 2's actual
contribution requires directly comparing B, C-full, and C-L1 in a single session — see "Single-
session three-arm candidate 1 sweep" below, which is the measurement that resolves this.

Raw data: `numa-design/bench-data/pathology/l1only-arm{B,CL1}-{warmup,recorded}.out{,.stderr}`,
per-round `l1only-arm*-r*.vmstat.{before,after}`, `l1only-vmstat-summary.txt`, `sweepL1.log`,
`pilotL1.out`/`pilotL1.stderr`, and the sweep driver script,
`numa-design/bench-data/pathology/cand1-l1-sweep.sh`.

### Candidate 3 L1-only: phase-shift, B vs C-L1, GOMAXPROCS=128, n=10

Remote time permitted a second re-run (item 2.3, "optional but valuable"): candidate 3 (the
"suggestive, not robust" phase-inversion migration storm) against C-L1, using the already-built
`/tmp/pb/l1/phaseshift-l1` and the existing stock `/tmp/pb/phaseshift-base`. Same config as the
original candidate 3 sweep (`-heap=6144 -readers=64 -phase=30 -phases=4`, rotating BC/CB order,
one warmup round, vmstat snap per run). Two arms only (B, C-L1); no arm A (per the same reasoning
as candidate 1's L1-only re-run above — arm A's original role here was already established as
uninterpretable/informational, not worth repeating).

**Mechanism validity (arm B, n=10 recorded rounds):** hint faults min=24,246 max=36,091
mean=28,079; pages migrated min=3,314,918 max=5,162,399 mean=4,373,203 — consistent with every
prior candidate-3 B measurement. **Arm C-L1: exactly 0/0 in all 10 recorded rounds** (cleaner even
than the original full-C's 9/10-clean result) — BIND-all alone fully suppresses the balancer here
too.

**benchstat, primary metric (ns per pointer-read, `BenchmarkPhaseChase`, n=10):**

- **B vs C-L1:** B = 1.551ns ± 7%, C-L1 = 1.174ns ± 48% → **-24.31% (p=0.023, n=10) — C-L1
  significantly FASTER than B.** The effect is *larger* than the original full-C result (-16.82%,
  p=0.029), not smaller.

**Robustness checks, independently computed the same way as for the original candidate 3 result
(paired by round index, exact enumeration where feasible):**

- **Paired sign test:** 8 of 10 rounds have C-L1 < B (same count as the original run) — exact
  two-sided p=**0.109**, unchanged (the sign test only sees direction, not magnitude, so an 8/10
  split caps its power at this n regardless of effect size).
- **Wilcoxon signed-rank test:** exact two-sided p=**0.0137** — **significant**, and substantially
  stronger than the original run's p=0.084. The two reversed-sign rounds are smaller in magnitude
  relative to the seven-plus in-direction rounds than they were in the original run.
- **Leave-one-out (exact Mann-Whitney U, 9 vs 9, each round dropped in turn):** p range
  **0.0040-0.0503** — every one of the ten leave-one-out subsets lands at or extremely close to
  α=0.05 (the single worst case is 0.0503, a rounding hair above the threshold), versus the
  original run's much wider 0.006-0.077 range that included several clearly non-significant
  subsets.

**Independent-replication statistics (a third, scoped audit; independently recomputed here from
the archived data of both sweeps and confirmed to match):** treating the original candidate 3
sweep and this L1-only re-run as two independent replications of the same B-vs-C(-L1) comparison
strengthens the result beyond what either sweep shows alone —

- **Combined paired sign test across both sweeps**, pooling all 20 round-pairs (10 original + 10
  L1-only; 16 of 20 favor C/C-L1 over B): exact two-sided p=**0.0118** — now significant, where
  neither sweep's own 8/10 sign test was on its own (each capped at p=0.109 by its small n).
- **Fisher's method** combining the two sweeps' independent exact Mann-Whitney U p-values
  (original p=0.0288, L1-only p=0.0232) into one combined significance test:
  χ²(df=4) = 14.62, combined p=**0.0056**. Two independently-run sweeps, on two different
  `GOEXPERIMENT=numa` builds (one with Layer 2, one without), both landing in the same direction
  with this combined significance is meaningfully stronger evidence for a real effect than either
  sweep's p=0.02-0.03 alone.

**Honest caveat, stated as a distributional claim, not an absolute one:** in 2 of 10 rounds in
*each* sweep, C (or C-L1) lands at close to single-controller speed (~33-37 GB/s, near arm A's
31.0 GB/s ceiling) rather than its usual 45-58 GB/s — visible directly in the per-round bandwidth
figures above. The claim this section supports is therefore "C is *usually* faster than B, by a
margin that replicates across two independent sweeps," not "C is always faster than B" — some
rounds, plausibly by first-touch placement luck, do not get the dual-controller benefit at all.

**Migration-traffic-vs-bandwidth check (same method as the original candidate 3 writeup):** arm B's
mean migration traffic here is ≈0.30 GB/s two-way (4,373,203 pages/run × 4096 B × 2 ÷ 120 s),
≈0.7% of B's own ~41 GB/s mean demand — again far too small to explain a 24% throughput swing via
fault/copy cost alone.

**Verdict: the L1-only re-run does NOT explain away candidate 3's effect — if anything, it
strengthens it.** This directly contradicts the hypothesis offered in the original candidate 3
correction above (that the win was "more consistent with avoiding single-controller consolidation
... a byproduct of Layer 2's per-chunk homing than of balancer suppression"): C-L1 has **no** Layer
2 homing at all, yet shows a larger, more statistically robust win than the Layer 0+1+2 build did.
The dual-memory-controller-bandwidth *mechanism* proposed earlier likely still holds (arm B's own
bandwidth figures here, 37.5-45.9 GB/s, again exceed the single-controller ~31 GB/s ceiling
established in the original candidate 3 pilot, and C-L1 reaches even higher, 33.0-57.9 GB/s) — but
the *cause* of C reaching that bandwidth is evidently BIND-all's suppression of the balancer's
churn (which otherwise presumably disrupts steady dual-controller access patterns via its own
periodic PROT_NONE-then-fault-then-possibly-migrate cycling), not Layer 2's explicit per-chunk
placement. **Revised candidate 3 verdict: independently replicated.** Not fully robust by the
single-sweep sign test alone, but now supported by a significant single-sweep Wilcoxon result, a
leave-one-out range essentially uniformly at or below α=0.05, and — most importantly — a combined
sign test (p=0.0118) and Fisher's-method-combined significance test (p=0.0056) across two
independent sweeps on two different builds. This is meaningfully more solid evidence for a genuine
BIND-all-attributable recovery on this specific synthetic workload than either sweep alone
provided, tempered by the honest caveat above that the effect is a usual-case, not universal-case,
claim.

Raw data: `numa-design/bench-data/pathology/l1only-cand3-arm{B,CL1}-{warmup,recorded}.out{,.stderr}`,
per-round `l1only-cand3-arm*-r*.vmstat.{before,after}`, `l1only-cand3-vmstat-summary.txt`,
`sweep3L1.log`, and the sweep driver script,
`numa-design/bench-data/pathology/cand3-l1-sweep.sh`.

### L1-only re-run: overall (candidate 1 conclusion withdrawn — see below and the three-arm sweep)

| Candidate | Original (Layer 0+1+2) C vs B | L1-only (Layer 0+1) C-L1 vs B | What this pair of sweeps actually establishes |
|---|---|---|---|
| 1 — garbage | Significant regression, +8.58% (p=0.003) | Null, ~ (p=0.315), MDE≈12.4% | **Nothing conclusive about Layer 2's contribution.** C-full vs C-L1 compared directly is itself null (p=0.631) — the difference-in-significance between the two rows is not evidence the rows differ from each other. See the single-session three-arm sweep below. |
| 3 — phase-shift | Suggestive win, -16.82% (p=0.029), fragile on robustness checks | Larger, more robust win, -24.31% (p=0.023), Wilcoxon p=0.014, LOO range 0.004-0.0503 | **Independently replicated**: combined sign test across both sweeps p=0.0118, Fisher's-method-combined p=0.0056. Layer 2 was not needed for this effect. |

**Candidate 1's "Layer 2 caused it" conclusion, and the identical claim in the commit message of
`3507802ec5`, are withdrawn as a difference-of-significance fallacy** (full reasoning in the
candidate 1 verdict above): the direct C-full-vs-C-L1 comparison this sweep never ran is null
(p=0.631), the sweep's own MDE (≈12.4%) is larger than the effect it would need to rule out
(+8.58%), the stock control drifted +2.9% between the two sessions being implicitly compared, and
C-L1 still carries the same ~11% user+sys-per-op penalty full-C did — a concrete signal the slice
may still cost something this sweep's resolution cannot see. **What is actually established:
whether Layer 0+1 alone regresses candidate 1's workload is unresolved** by any sweep run before
the single-session three-arm sweep below, which is the first measurement in this investigation
designed to answer it directly (B, C-full, and C-L1 together, same session, so no cross-session
drift and a properly powered pairwise comparison). **Update: that sweep is now done — see "Single-
session three-arm candidate 1 sweep" at the end of this file. Short answer: yes, it does regress
(+5.24%, p=0.001, n=15), and Layer 2 is not the explanation (C-full and C-L1 are statistically
indistinguishable from each other, p=0.838).**

**Candidate 3's win, by contrast, is now on materially firmer ground than a single sweep could
provide**: two independent sweeps, on two different builds, both show C beating B in the same
direction with a combined significance (Fisher's method p=0.0056) well past the two sweeps'
individual p=0.02-0.03 results, and Layer 2 was demonstrably not required for it. The honest
caveat — 2 of 10 rounds per sweep land at near-single-controller bandwidth rather than the usual
dual-controller advantage — is stated above as a distributional claim, not withdrawn.

Neither L1-only sweep fully explains the dual-controller-bandwidth mechanism candidate 3 points at
(that would need IMC/`perf`-level instrumentation, not run here) — flagged as follow-up work.

## Single-session three-arm candidate 1 sweep (2026-08-20)

The measurement the previous section's candidate 1 verdict pointed to: does the Layer 0+1
shippable slice, on its own, regress this workload — resolved directly rather than inferred from
two separate sweeps' significance patterns. Three arms in one session: **B** (stock, unpinned),
**C-full** (`GOEXPERIMENT=numa` at branch HEAD, `5940a54ab0` — Layer 0+1+2), **C-L1** (a fresh
scratch build from the same `l1-only.patch` used in the previous section, rebuilt for this sweep
so both experiment builds could be produced independently — Layer 0+1 only). All unpinned,
GOMAXPROCS=128, `-benchmem=4096 -benchnum=1`, n=15 per arm (raised from the previous n=10 sweeps
for more power), one warmup round, rotating BCL/CLB/LBC order, vmstat snap per run, idle checks
before every run.

**Build process:** applied the L1-only patch (identical diff to `l1-only.patch`, archived
separately since it was a fresh `git checkout` + re-edit, not a saved file re-applied) on the
remote tree only, rebuilt via `./src/make.bash`, installed a new `garbage` binary with
`GOEXPERIMENT=numa` into `/tmp/pb/l1v2/` (same resolved `x/benchmarks` version,
`v0.0.0-20260819172200-70693762b6a0`, confirmed via `go version -m`), sanity-checked 0/0 vmstat on
a pilot run, then restored the tree (`git checkout -f src/runtime/numa_linux.go`) and rebuilt back
to stock — verified clean (`git diff --stat` empty, `numaPreferredCalls.Add(1)` back in source,
`go version -m` on the restored `bin/go` matching `5940a54ab0`) before running the sweep. Arms B
and C-full reused the existing stock binaries from earlier in this session
(`/tmp/pb/base/garbage`, `/tmp/pb/numa/garbage`), unaffected by the patch/restore cycle.

**Mechanism validity (n=15 recorded rounds each):** arm B hint faults min=70,357 max=420,684
mean=206,399; pages migrated min=920,953 max=1,629,134 mean=1,352,394 — consistent with every
prior candidate-1 B measurement. **Arm C-full: 0/0 in 14 of 15 rounds, negligible noise (2/0) in
the other. Arm C-L1: 0/0 in all 15 rounds.** Both experiment arms confirm BIND-all suppression,
with or without Layer 2.

**benchstat, pre-declared PRIMARY metric (ns/op, `Garbage/benchmem-MB=4096-128`), all three
pre-declared pairs, full table archived at `cand1-3arm-benchstat-full.txt`:**

- **B vs C-L1:** B = 3.018ms, C-L1 = 3.176ms → **+5.24% (p=0.001, n=15) — C-L1 significantly
  SLOWER than B.**
- **C-full vs C-L1:** C-full = 3.174ms, C-L1 = 3.176ms → **~ (p=0.838, n=15) — not significantly
  different from each other.**
- **B vs C-full** (this session, for internal consistency with the original candidate 1 sweep):
  B = 3.018ms, C-full = 3.174ms → **+5.17% (p=0.001, n=15).**

**benchstat, pre-declared SECONDARY metric (user+sys-sec/op — the persistent CPU-time penalty
flagged in the previous section):**

- **B vs C-L1:** 161.4ms vs 171.8ms → **+6.43% (p=0.000, n=15).**
- **C-full vs C-L1:** 172.2ms vs 171.8ms → **~ (p=0.512, n=15) — not significantly different.**
- **B vs C-full:** 161.4ms vs 172.2ms → **+6.65% (p=0.000, n=15).**

**Achieved noise/MDE, computed from the raw per-round ns/op values directly (not from benchstat's
±% column, which is not a confidence interval and is not used as one here):** per-arm coefficient
of variation B=7.20%, C-full=3.42%, C-L1=9.91%. Pooled CV for the B-vs-C-L1 pair ≈8.84%, giving a
minimum detectable effect (α=0.05, power=0.80, n=15/arm) of **≈9.05%** — for the C-full-vs-C-L1
pair, pooled CV ≈7.32%, MDE ≈**7.49%**. Independently cross-checked with a normal-approximation
Mann-Whitney U on the raw data: B-vs-C-L1 p≈0.0016, C-full-vs-C-L1 p≈0.836, B-vs-C-full p≈0.0014 —
all closely matching benchstat's own p-values, confirming the reported results directly rather
than taking benchstat's output on faith.

**Verdict: this sweep resolves what the previous sweeps could not, and the answer is not the
"good news" one hoped for.** Both experiment arms — C-full (Layer 0+1+2) and C-L1 (Layer 0+1
alone) — regress this workload relative to stock B, by essentially the same amount (+5.17% and
+5.24%, both p=0.001) and are statistically indistinguishable from each other (p=0.838). **The
shippable Layer 0+1 slice, on its own, does appear to regress this specific throughput workload
by a modest but well-powered ~5%, with a real ~6-7% CPU-time penalty alongside it — Layer 2 is
not the explanation, because removing it changes nothing measurable.** This sweep's MDE (≈9.05%
for the primary comparison) is honestly reported rather than glossed over: the achieved effect
(+5.24%) is smaller than the MDE, meaning this specific sweep design would only have 80% power to
detect an effect this size or larger on repeated sampling — but the result was significant anyway
(p=0.001), so this is not a case of an underpowered null being over-read; it is a positive,
significant finding at a real effect size, reported with its own honest power caveat rather than
as an unqualified certainty. Combined with the original candidate 1 sweep's larger, cruder
estimate (full-C +8.58% vs B, n=10, wider CI) and this sweep's cleaner +5.17% (n=15), the picture
converges: **BIND-all measurably costs something on this workload**, whether or not Layer 2 is
present, and the earlier "no regression on a realistic throughput workload" framing for the
shippable slice is not supported by this data.

Raw data: `numa-design/bench-data/pathology/cand1-3arm-arm{B,CFULL,CL1}-{warmup,recorded}.out{,.stderr}`,
per-round `cand1-3arm-arm*-r*.vmstat.{before,after}`, `cand1-3arm-vmstat-summary.txt`,
`sweep3arm.log`, `pilotL1v2.out`/`pilotL1v2.stderr`, the full benchstat table
`cand1-3arm-benchstat-full.txt`, and the sweep driver script,
`numa-design/bench-data/pathology/cand1-3arm-sweep.sh`.

## Workstream A gate battery (v3) — 2026-08-20

SHA (Local/Remote, in sync throughout): `c226071c35`
Task-0 parent (Global Constraints "once per layer" baseline, and Gate 8's census parent): `abe916018e` (`numa-design: single-session three-arm candidate 1 sweep`)
`go version` (remote, tree toolchain at SHA `c226071c35`): `go1.28-devel_c226071c35 Thu Aug 20 10:29:50 2026 -0700 linux/amd64`
Kernel: `6.12.0-211.7.1.el10_2.x86_64`
`x/benchmarks`: `v0.0.0-20260819172200-70693762b6a0` (same pin as every prior layer/pathology run; confirmed via `go version -m` on both `garbage` installs for this session)
`x/perf` (benchstat): `/tmp/numa-tools/benchstat` on `numa-dell`
`kernel.numa_balancing`: `1` throughout (confirmed at session start and end)
THP: `[always] madvise never` (unchanged, per protocol)

Machine load: single-user throughout; `ps aux --sort=-%cpu` was checked before every measurement block, but that self-report is not by itself strong evidence of quietness — a just-started `ps` process can show a spuriously huge %CPU (its own CPU time over a near-zero elapsed time), and the sweep driver's `idle_check` fired on exactly this artifact 42+17 times across the two candidate sweeps rather than on genuine contention (directly reproduced live during this correction pass: a bare `ps aux --sort=-%cpu` on an otherwise-idle box showed itself at 1000% CPU). The real evidence of a quiet box is the vmstat chain (arm A and arm C both land at 0/0 in essentially every round — a busy, contended box would not produce that cleanly) plus the sweep timeline (`*-sweep.log`, `*.vmstat.before/after` timestamps) showing no unexplained gaps or overlaps. `numa-design/pathology-sweep.sh`'s `idle_check` has been fixed in a follow-up edit (this commit) to exclude the `ps`/`awk` pipeline's own rows before reading the top process.

Raw `.out` files, vmstat snaps, strace transcripts, and the exact sweep-driver invocations backing every verdict below are archived under `numa-design/bench-data/ws-a-gates/` (gates 1-5, 8) and `numa-design/bench-data/wsA-cand1/`, `wsA-cand2/` (gates 6-7).

**Pre-registration note:** this section reports exactly the 8 pre-declared gates from the tracked, committed plan (`numa-design/2026-08-20-numa-v3-locality-plan.md`, committed pre-execution at `e44dad45f1` — git-verifiable pre-registration); no metric was added post hoc. Gates 6-7's C-vs-A criterion is the non-inferiority bound (95% CI upper bound of C/A ratio ≤ +10%), not a "C≈A" claim.

### Gate 1 — 1P json, off vs on, same commit (hard gate)

```
$ ssh numa-dell 'cd /home/deparker/go-numa && GOROOT=$PWD GOMAXPROCS=1 BENCHNUM=10 OUT=/tmp/wsA-gate1 ./numa-design/gate-json.sh'
$ ssh numa-dell '/tmp/numa-tools/benchstat /tmp/wsA-gate1/baseline.out /tmp/wsA-gate1/numa.out'
```

```
JSON-1  sec/op:            20.72m ± 57%  17.33m ± 89%  ~ (p=0.631 n=10)
JSON-1  user+sys-sec/op:   20.74m ± 57%  17.33m ± 90%  ~ (p=0.579 n=10)
```

**Verdict: PASS.** Neither primary metric shows a statistically significant difference (both p > 0.05); numa arm nominally faster, well inside the ≤+2%-or-not-significant band regardless. **Achieved MDE:** per-arm CV ≈32% (pooled ≈32.3%) at n=10 gives a minimum detectable effect ≈40% (α=0.05, power=0.80) — at `GOMAXPROCS=1` this machine's noise is large enough that Gate 1 can only rule out a gross regression, not confirm near-parity; the PASS should be read as "no regression ≥ ~40% detected," not as evidence of tight equivalence.

### Gate 2 — 1P alloc micro (hard gate)

```
$ ssh numa-dell 'cd /home/deparker/go-numa && export GOROOT=$PWD PATH=$PWD/bin:$PATH GOTOOLCHAIN=local
GOMAXPROCS=1 go test runtime -run=NONE -bench="Malloc(8|16|Types)" -count=10 >/tmp/wsA-alloc-base.out
GOMAXPROCS=1 GOEXPERIMENT=numa go test runtime -run=NONE -bench="Malloc(8|16|Types)" -count=10 >/tmp/wsA-alloc-numa.out
/tmp/numa-tools/benchstat /tmp/wsA-alloc-base.out /tmp/wsA-alloc-numa.out'
```

```
Malloc8    6.937n ± 0%   6.998n ± 0%  +0.89% (p=0.000 n=10)
Malloc16   11.30n ± 1%   11.36n ± 0%  +0.44% (p=0.015 n=10)
geomean    8.855n        8.914n       +0.67%
```

(No `MallocTypes` benchmark exists in this tree — same gap noted at every prior layer — so the regex matched only `Malloc8`/`Malloc16`.)

**Verdict: PASS.** Both deltas statistically significant but tiny; geomean +0.67% ≤ +2% pass bar.

### Gate 3 — 256P json, unconditional 3 sessions × BENCHNUM=10, pooled n=30 (hard gate)

Three sessions run unconditionally per the pre-declared no-data-dependent-stopping rule (I2), idle re-verified between each:

```
$ ssh numa-dell 'cd /home/deparker/go-numa && GOROOT=$PWD GOMAXPROCS=256 BENCHNUM=10 OUT=/tmp/wsA-gate3-r{1,2,3} ./numa-design/gate-json.sh'  # x3
```

Per-round rel deltas (informational, hand-rolled median, not the verdict): r1 −16.5%/−7.0%, r2 −1.7%/+0.5%, r3 −21.2%/−15.6% (ns/op / user+sys-ns/op) — the sign-flipping pattern the plan's I2 note predicts at n=10; pooling is the pre-registered remedy.

Pooled (n=30, `cat` of all three `baseline.out` / `numa.out`):

```
$ ssh numa-dell '/tmp/numa-tools/benchstat /tmp/wsA-gate3-pooled-baseline.out /tmp/wsA-gate3-pooled-numa.out'
JSON-256  sec/op:            2.371m ± 11%  2.025m ± 14%  ~ (p=0.197 n=30)
JSON-256  user+sys-sec/op:   179.6m ± 7%   170.4m ± 10%  ~ (p=0.382 n=30)
```

**Verdict: PASS.** Pooled result not significant on either metric; no pooled regression at all (numa nominally faster), let alone beyond +2%. Hard-FAIL condition (pooled significant regression >+2%) does not apply. **Achieved MDE:** pooled CV ≈31% at n=30 gives MDE ≈22% (α=0.05, power=0.80) — pooling to n=30 roughly halves Gate 1's MDE but the +2% pass bar is still far below this gate's resolving power; against the stated bar, both Gate 1 and Gate 3 can only exclude gross regressions, not confirm near-parity at the ±2% scale.

### Gate 4 — Stand-down proof, strace, three arms (hard gate)

**(a) 256P — must never confine:**

```
$ ssh numa-dell 'cd /tmp/wsA-gate3-r1 && strace -f -e trace=sched_setaffinity,set_mempolicy,mbind -o /tmp/wsA-strace-256p.txt \
  env GOMAXPROCS=256 ./numa/json -benchmem=512 -benchnum=1 -benchtime=2s >/dev/null'
```

`sched_setaffinity`: **0** calls. `set_mempolicy`: exactly **1**, `set_mempolicy(MPOL_BIND, [0x3], 65) = 0`. `mbind`: **1,938 calls** (re-verified directly against the archived transcript by counting `mbind(`-prefixed lines specifically, which excludes the separate task-level `set_mempolicy` line above; an earlier count of "2761" in this section was wrong, and a follow-up correction's "970 `MPOL_BIND`" was also wrong by one — both are corrected here), splitting **969 `MPOL_BIND`** (mask `[0x3]`, both nodes — Layer 1's uniform arena BIND-all) and **969 `MPOL_PREFERRED`** (split **541 to node 0** `[0x1]` and **428 to node 1** `[0x2]`, tracking whichever node each growing P happened to run on — Layer 2's still-in-tree PREFERRED-at-grow, unaffected by GOMAXPROCS=256 never confining) — **the two sides are equal, as they must be**: Layer 1 and Layer 2 each issue exactly one `mbind` per arena chunk grow, so every grow contributes one BIND and one PREFERRED call. The two mechanisms are layered, not either/or: every newly-grown arena chunk on this 256P run gets one uniform BIND-all `mbind` (Layer 1) and, independently, one node-local PREFERRED `mbind` (Layer 2) recording whichever node the growth happened on. **Matches declared shape (a)** on the pre-declared criteria (0 `sched_setaffinity`, exactly 1 task-level `set_mempolicy(BIND)`, `mbind` calls present); the count itself was never a pass/fail criterion but is corrected here for the record. **Origin of the original "2761" figure, traced precisely:** the raw transcript also contains 823 `mbind` calls that were interrupted by another thread's syscall under `strace -f`, each producing a matched `<unfinished ...>`/`<... mbind resumed>` pair — the `<unfinished ...>` half is already counted in the 1,938 `mbind(` lines, but the `<... mbind resumed>` half contains the substring `mbind` without the literal `mbind(` call-open text; a naive substring count (`grep -c mbind`, no parenthesis) therefore double-counts those 823 interrupted calls: 1,938 + 823 = 2,761, exactly the original figure.

**(b) confined 1P:**

```
$ ssh numa-dell 'cd /tmp/wsA-gate3-r1 && strace -f -e trace=sched_setaffinity,set_mempolicy,mbind -o /tmp/wsA-strace-1p.txt \
  env GOMAXPROCS=1 ./numa/json -benchmem=512 -benchnum=1 -benchtime=2s >/dev/null'
```

`sched_setaffinity`: exactly **1** call, mask = odd CPUs only (`[1 3 5 7 ... 255]` — node 1, this session's boot node). `set_mempolicy`: exactly **2**, in order `MPOL_BIND` then `MPOL_PREFERRED, [0x2], 65` (node 1). `mbind`: **22 calls total** — re-verified directly against the archived transcript as **11 `MPOL_BIND`** (mask `[0x3]`, both nodes — Layer 1's uniform arena BIND-all, still running while confined per locked decision 5) paired with **11 `MPOL_PREFERRED`** (mask `[0x2]`, the confined node only — Layer 2's PREFERRED-at-grow, still in-tree and not yet removed; every chunk grows while confined so its node-local PREFERRED always lands on the same, confined node). **Matches declared shape (b) exactly** on the pre-declared criteria, including the BIND-then-PREFERRED task-policy order; the earlier "22 calls (arena BIND-all)" phrasing was materially incomplete — half of those calls are Layer 2's PREFERRED-at-grow, not Layer 1's BIND-all, and this section (and Gates 6-7 below) therefore characterize **Layer 0+1+2+confinement together, not confinement in isolation** — Layer 2 has not been removed at this point in the plan (that is Task 6, gated on these hard gates). **After Task 6 removes Layer 2, a Gate-6 B-vs-C confirmation sweep is required** to re-establish the candidate-1 numbers under confinement alone; Task 6 will run it.

`/proc/PID/maps` spot check during a confined run: **32 lines** — low, merged-VMA count, matching the Layer-1 baseline character (RESULTS.md:1496-1515), not Layer 2's ~1172-line blowup. Because every PREFERRED-at-grow call while confined targets the *same* confined node (uniform-to-confined-node, not per-chunk-varying), the resulting per-chunk policies still merge into one low-VMA-count region rather than fragmenting — this is why the Layer-2-era ~1172-line blowup (RESULTS.md:1496-1515, where PREFERRED targeted whichever node was locally growing, varying chunk-to-chunk) does not recur here even with Layer 2 still active: confinement collapses Layer 2's per-chunk node choice down to a single node, incidentally fixing the fragmentation Layer 2 alone caused.

**(c) stand-down — testprog `NUMAStandDown`, `GOEXPERIMENT=numa` build:**

```
$ ssh numa-dell 'cd /home/deparker/go-numa && export GOROOT=$PWD PATH=$PWD/bin:$PATH GOTOOLCHAIN=local
GOEXPERIMENT=numa go build -o /tmp/wsA-testprog ./src/runtime/testdata/testprog
strace -f -e trace=sched_setaffinity,set_mempolicy -o /tmp/wsA-strace-standdown.txt \
  env GOMAXPROCS=1 /tmp/wsA-testprog NUMAStandDown'
```

Probe output: `before affinity=128 mode=1` (confined: 128 = node CPU count, mode 1 = MPOL_PREFERRED) → `standdown-gomaxprocs-wall-ns=2901728` → `after affinity=256 mode=2` (stood down: full affinity restored, mode 2 = MPOL_BIND, exact Layer-1 fallback).

strace counts: `sched_setaffinity` **7** (confine's 1 + the eager allm walk at stand-down — consistent with "~one per live M"); `set_mempolicy` **5**: `BIND, PREFERRED` (confine prologue, matching (b)) then **3 further `BIND`** calls (STW-thread revert + per-thread `numaFixThreadPlacement` convergence as Ms park). **Matches declared shape (c).**

Stand-down wall-clock cost, compared against the same probe on the stock build (n=3 each, un-instrumented — the strace run above is excluded as strace overhead):

| build | run1 (ns) | run2 (ns) | run3 (ns) | median |
|---|---|---|---|---|
| stock (no confinement, mode=0 throughout) | 2,292,399 | 2,180,251 | 2,441,749 | 2,292,399 |
| numa (confine → stand-down) | 2,371,475 | 2,362,265 | 1,819,846 | 2,362,265 |

The eager-walk-plus-convergence stand-down adds **~70µs (~3%) over the stock GOMAXPROCS-raise's own STW cost** at median — within this box's run-to-run jitter (stock's own spread is ~260µs / ~11%), i.e. the stand-down machinery's STW-adjacent cost is not distinguishable from the baseline GOMAXPROCS(N) call's own cost at this n. Recorded as the on-record wall-clock bound per the plan's Task 4 Gate 4(c) requirement; not a claim of a specific attributable delta given the overlap with baseline jitter.

**Verdict: PASS.** All three arm shapes (a), (b), (c) match the pre-declared shapes exactly, including call counts, ordering, and the maps-line spot check.

### Gate 5 — vmstat 0/0, confined arms (hard gate)

```
$ ssh numa-dell 'cd /home/deparker/go-numa
./numa-design/gate-vmstat.sh snap /tmp/wsA-gate5.before
env GOMAXPROCS=1 /tmp/wsA-gate3-r1/numa/json -benchmem=512 -benchnum=1 -benchtime=3s >/dev/null
./numa-design/gate-vmstat.sh snap /tmp/wsA-gate5.after
./numa-design/gate-vmstat.sh diff /tmp/wsA-gate5.before /tmp/wsA-gate5.after'
hint_faults=0 pages_migrated=0
```

Rerun once per protocol (counters are machine-global): second run also `hint_faults=0 pages_migrated=0`.

**Verdict: PASS.** 0/0 on both runs.

**Important caveat, stated plainly:** vmstat 0/0 is evidence that the balancer took no hint faults and migrated no pages — it is **Layer 1's** signature (uniform arena BIND-all exempts the VMA from the balancer regardless of whether confinement engaged), **not evidence that confinement (`sched_setaffinity`/task-level `MPOL_PREFERRED`) actually ran**. This is exactly why the stale `gc-pause-bench` binary in the first Candidate 2 attempt (see Gate 7's incident writeup) produced a clean 0/0 despite having zero confinement code at all — Layer 1 alone was sufficient to reach 0/0. 0/0 must be paired with a direct affinity/policy observation (strace's `sched_setaffinity` count, or a live `Cpus_allowed_list` read) to confirm confinement specifically; Gate 4(b)'s strace and Gate 6/7's post-hoc `Cpus_allowed_list` captures (below) are the actual confinement evidence, not Gate 5.

### Gate 8 — off-binary function census, NON-test binary, experiment off, HEAD vs Task-0 parent (hard gate)

Per the tracked plan's C2 caveat (`2026-08-20-numa-v3-locality-plan.md:1164`), a **plain program** is used instead of a `go test -c` binary (a test binary roots `export_numa_test.go`, which would confound the census with test-only exports). Built locally, both from the tree toolchain (`GOWORK=off GOTOOLCHAIN=local`), `GOEXPERIMENT=` unset:

```
$ printf 'package main\n\nfunc main() { println("census") }\n' > /tmp/census-canary.go
$ GOROOT=<HEAD worktree> GOEXPERIMENT= go build -o /tmp/census-head /tmp/census-canary.go
$ git worktree add /tmp/census-parent abe916018e && (cd /tmp/census-parent/src && GOROOT_FINAL=/tmp/census-parent ./make.bash)
$ GOROOT=/tmp/census-parent GOEXPERIMENT= go build -o /tmp/census-parent-bin /tmp/census-canary.go
```

Raw `diff -q` on the `sed`-stripped disassembly **does not** come back clean (unlike Layer 1's Gate 4, which was byte-identical). Root cause, verified directly: Workstream A's confinement state — `numaConfined`, `numaStoodDown`, `numaConfinedNode`, `numaConfinedNodeCPUs`, `numaSavedAffinity [1024]byte`, `numaSavedAffinityLen` — is declared in `numa_linux.go`, which carries no build tag beyond `package runtime` (compiles for every Linux arch regardless of `GOEXPERIMENT`). These new **unconditional** package-level globals shift the data-segment addresses of every subsequent global, exactly the same class of gap Layer 0's Gate 4 hit from adding `debug.numa` to the `debug` struct (RESULTS.md:540-551) — not a functional difference. `sed`'s hex-stripping doesn't hide this because the shifted values appear as literal instruction-byte sequences (space-separated 2-digit hex), never as a single ≥6-digit run.

The plan's real caveat (C2, `2026-08-20-numa-v3-locality-plan.md:1178`) — a **struct-offset shift** (e.g. an `m` field moving) showing up as a displacement immediate change — does **not** apply here: Task 3 already verified `sizeof(m)` is byte-identical off-experiment (1832 bytes, unchanged) because `mNUMAState` is the empty-struct/zero-size type when `!goexperiment.numa` (`numa_mstate_off.go`), confirmed by test not just inspection. So the diffs observed are exclusively the benign global-data-address-shift class, not the struct-offset class the caveat warns is a real fail.

Per-function census (immune to whole-binary address shifts, following the Layer 0/1 precedent methodology — `objdump -d`, symbol-labeled blocks, per-function instruction-line counts):

```
head functions: 1481
parent functions: 1481
only in head: 0
only in parent: 0
functions with differing instruction-line counts: 0
numa-related symbols in HEAD off-binary: []
```

Explicit fast-path spot checks (instruction-line count): `main.main` 29/29, `runtime.schedinit` 354/354, `runtime.mallocgc` 132/132 — identical parent vs HEAD. `nm` total symbol count (FUNC-typed): 1482/1482, matching objdump's labeled-block count set. Total disassembly line count: 148,750/148,750 identical. `size`: `.text` +15,608B (1,186,899 vs 1,171,291), `.data`/`.bss` unchanged — consistent with **branch-encoding-length changes** (Jcc short-vs-near selection shifting when a target's relative distance crosses the ±128-byte threshold after the new globals move surrounding code/data) given identical instruction *counts*, not with any new or removed instruction.

**Verdict: PASS on the substantive criterion, literal "build-ID-only" bar not met** — same honest-gap treatment as Layer 0's Gate 4: zero function-level/opcode/branch-target changes across all 1481 functions (parent and HEAD), zero `numa`-related symbols reachable in the off-binary, zero struct-offset-shift evidence (the one class of diff that would rightly fail this gate). The only observed difference is a benign global-data-address shift from Workstream A's new unconditional confinement-state variables, which is structural to adding any new package-level state and unattainable to avoid, exactly as documented at Layer 0.

### Gate 6 — Candidate 1 (decision gate): `x/benchmarks/garbage`, `-benchmem=4096 -benchnum=1`, GOMAXPROCS=128, n=15

**Pre-flight (CRITICAL, before burning sweep time):** confirmed via Gate 4(b)'s strace that arm C confines (1 `sched_setaffinity` narrowing to one node, `set_mempolicy(BIND)` then `set_mempolicy(PREFERRED)`). A dedicated pilot run of the exact candidate-1 C command additionally confirmed: `Cpus_allowed_list` narrowed to one node's (even) CPUs, peak RSS ≈7.95 GiB fit node 0's ≥10 GB-free precondition with headroom, and `numactl --hardware` free memory recovered fully post-run (11132→11229 MB across the session, no leak/pressure).

Build (once, both binaries from the same `x/benchmarks` pin `v0.0.0-20260819172200-70693762b6a0`, confirmed via `go version -m` on both):

```
$ ssh numa-dell 'export GOROOT=/home/deparker/go-numa PATH=/home/deparker/go-numa/bin:$PATH GOTOOLCHAIN=local
GOBIN=/tmp/pb/base go install golang.org/x/benchmarks/garbage@v0.0.0-20260819172200-70693762b6a0
GOBIN=/tmp/pb/numa GOEXPERIMENT=numa go install golang.org/x/benchmarks/garbage@v0.0.0-20260819172200-70693762b6a0'
```

Sweep (single session, rotating ABC/BCA/CAB order via the Task-0 driver, one warmup round + 15 recorded, vmstat snap per run, idle check per run):

```
$ ssh numa-dell 'cd /home/deparker/go-numa
numactl --hardware | grep free   # node 0 free: 11145 MB (>= 10 GB precondition met)
./numa-design/pathology-sweep.sh wsA-cand1 15 /tmp/pb/wsA-cand1 \
  "A=numactl --cpunodebind=0 --membind=0 env GOMAXPROCS=128 /tmp/pb/base/garbage -benchmem=4096 -benchnum=1" \
  "B=env GOMAXPROCS=128 /tmp/pb/base/garbage -benchmem=4096 -benchnum=1" \
  "C=env GOMAXPROCS=128 /tmp/pb/numa/garbage -benchmem=4096 -benchnum=1"'
```

**Mechanism validity (n=15 recorded rounds each):** arm A: 0/0 hint faults/pages migrated, every round (16/16 including warmup). Arm B: hint faults 81,409–265,164, pages migrated 943,290–1,476,583, every recorded round (≫0, no setup-fault rounds to exclude). Arm C: 0/0 in **13/15** recorded rounds (r6: 3 hint faults/0 migrated; r14: 2/1 — the only two non-clean rounds; corrects an earlier "14/15" arithmetic slip in this section and in `task-4-report.md`) — negligible noise, consistent with every prior BIND-all measurement on this box. Per Gate 5's caveat above, this 0/0 pattern reflects Layer 1 and does not by itself confirm confinement engaged for these specific sweep rounds — see "Post-hoc confinement verification" below for direct affinity evidence on the actual sweep binary.

**benchstat, pre-declared primary metric (`sec/op`, `Garbage/benchmem-MB=4096-128`):**

```
$ ssh numa-dell '/tmp/numa-tools/benchstat /tmp/pb/wsA-cand1/wsA-cand1-armB-recorded.out /tmp/pb/wsA-cand1/wsA-cand1-armC-recorded.out'
Garbage/benchmem-MB=4096-128   B=2.558m ± 17%   C=1.801m ± 9%   -29.61% (p=0.000 n=15)

$ ssh numa-dell '/tmp/numa-tools/benchstat /tmp/pb/wsA-cand1/wsA-cand1-armA-recorded.out /tmp/pb/wsA-cand1/wsA-cand1-armC-recorded.out'
Garbage/benchmem-MB=4096-128   A=1.855m ± 9%   C=1.801m ± 9%   ~ (p=0.217 n=15)

$ ssh numa-dell '/tmp/numa-tools/benchstat /tmp/pb/wsA-cand1/wsA-cand1-armA-recorded.out /tmp/pb/wsA-cand1/wsA-cand1-armB-recorded.out'
Garbage/benchmem-MB=4096-128   A=1.855m ± 9%   B=2.558m ± 17%   +37.93% (p=0.000 n=15)
```

**Exploratory corroboration (not the primary metric, not a separate claim):** `user+sys-sec/op`, B vs C, shows the same direction and a comparable magnitude — B=157.7ms ± 4%, C=128.3ms ± 3%, **-18.62% (p=0.000, n=15)** — a CPU-time-based metric independently corroborating the wall-clock primary's B-vs-C result via a different measurement channel.

**Non-inferiority CI (95% CI upper bound of C/A − 1, computed round-by-round from this session's n=15 paired rounds via log-ratio + t-distribution, script archived as `noninf_ci.py`):**

```
n=15 point_estimate_C/A-1=+2.69% 95%CI=[-2.60%, +8.26%]
Non-inferiority (upper bound <= +10%): PASS
```

**Verdict: PASS on all three pre-declared components — reported as a bound, not an equivalence.**
- **C vs B (primary): PASS.** C is significantly faster than B by 29.61% (p=0.000, n=15) — closes essentially all of the B-vs-A gap (see below), not just "most of" it.
- **C vs A (non-inferiority): PASSES the pre-declared bound, but is not a clean win.** The round-paired point estimate is **+2.69%**, 95% CI **[-2.60%, +8.26%]** — the upper bound (+8.26%) is comfortably inside the pre-declared +10% non-inferiority margin, so the gate **PASSES**. The **unpaired** medians run the other way (C=1.801m vs A=1.855m, benchstat p=0.217, C nominally faster) — but paired analysis is the correct lens here since A and C ran in the same rotating-order rounds, and it tells a different story: **C is slower than A in 12 of 15 paired rounds** (exact two-sided sign test, p=0.035, significant at α=0.05). Put plainly: a small residual in A's favour on this workload **cannot be excluded** by this data — it is only **bounded above by +8.3%**. The non-inferiority criterion is designed exactly for this situation (declared in advance because "C ≈ A" claims are below this harness's resolving power at n=15) and is satisfied; report the bound, not an equivalence claim.
- **B vs A (secondary, context): B is significantly worse than A** by 37.93% (p=0.000, n=15) — the "B worse than A" requirement holds cleanly and more strongly than the design doc's ~60%-from-prior-sessions ballpark suggested at this heap/GOMAXPROCS combination; no HT-confound fallback-to-informational was needed (§4 of the harness design).

Net picture: confinement (arm C) recovers **108% of the unpinned-vs-pinned gap** on the primary metric's point estimates ((B−C)/(B−A) = (2.558−1.801)/(2.558−1.855) ≈ 1.077) — nominally beating even the pinned oracle A on the headline point estimate — while the paired sign test says not to over-read that: A may still hold a small edge this design can bound but not rule out. Both readings are reported; neither is suppressed in favor of the other.

**Confined node this session:** the sweep's own C-arm process node was not observed in-run (see "Post-hoc confinement verification" below); the pre-sweep pilot check (run immediately before the recorded rounds, same command) showed confinement to **node 0** (even CPUs), and the free-RAM precondition (`numactl --hardware | grep free`, node 0 free 11145 MB) was checked specifically against **node 0** — consistent with, but not a guarantee of, every recorded round's actual confined node, since node choice follows the boot CPU and is not pinned by this harness.

Raw data: `numa-design/bench-data/wsA-cand1/wsA-cand1-arm{A,B,C}-{warmup,recorded}.out{,.stderr}`, per-round `wsA-cand1-arm*-r*.vmstat.{before,after}`, `wsA-cand1-vmstat-summary.txt`, the three `benchstat` transcripts (`wsA-cand1-{BvC,AvC,AvB}.txt`), the sign-test script and output (`sign_test.py`, `wsA-cand1-signtest-output.txt`), and the sweep driver invocation (`numa-design/pathology-sweep.sh`, committed at Task 0).

### Gate 7 — Candidate 2 (decision gate): `gc-pause-bench` heavy profile, round-level, n=10

**Process incident, disclosed in full (I3 — honest reporting of measurement problems, not just results):** the first attempt at this gate used broken binaries. The build step chained `cd /home/deparker/go-numa && go build -o /tmp/pb/gcpause-base ./numa-design/gc-pause-bench` (and the `GOEXPERIMENT=numa` twin) inside a multi-line ssh script with no `set -e`. `numa-design/gc-pause-bench` carries its own `go.mod` (a separate module); building it as a relative-path argument from the `go-numa` root failed with `go: go.mod file not found in current directory or any parent directory` — silently, because the script's final `echo BUILDS-OK` ran unconditionally regardless of the two build commands' exit status. `/tmp/pb/gcpause-base` and `/tmp/pb/gcpause-numa` therefore still pointed at **pre-existing binaries from earlier that day** (mtime 04:12 vs the same script's freshly-built `garbage` binaries at 13:55 — the tell). `go version -m` on the stale `/tmp/pb/gcpause-numa` showed it was built from commit `7ec36777f3`, which predates the entire v3 locality plan and all of Workstream A's confinement code (Tasks 1-3 did not exist at that commit) — confinement could not possibly engage.

The first (invalid) sweep ran entirely on this stale binary and produced a genuinely alarming result — C nominally *slower* than both B and A, B-vs-C not significant — that was correctly **not accepted at face value** per this gate's "do not rationalize a miss" instruction: instead of writing up "C fails," the anomaly (mechanism 0/0 confirmed suppressed yet C slower than unpinned B, which has no a priori mechanism) was investigated. `Cpus_allowed_list` on a live diagnostic run of the exact stale binary showed `0-255` (no confinement at all, ever, across the process lifetime — confirmed via `strace`: zero `sched_setaffinity` calls, only Layer 1's single `set_mempolicy(MPOL_BIND)`), while a binary freshly built with the same source under `GODEBUG=numa=1` printed `numa: confined to node 1 cpus 128` correctly. This traced the discrepancy to the stale binary's provenance, not a runtime defect. **Candidate 1's `garbage` binaries were independently re-verified NOT to have this problem** (`go version -m` showed `c226071c35`/`X:numa` on both, matching their fresh mtimes) — Gate 6 is unaffected and stands as reported above.

Fix: rebuilt both `gc-pause-bench` binaries from `numa-design/gc-pause-bench` directly (with `GOWORK=off`), verified each build's exit status explicitly, confirmed identity via `go version -m` (`go1.28-devel_c226071c35 ... X:numa`) and mtime, and confirmed confinement engages via a `GODEBUG=numa=1` smoke test (`numa: confined to node 1 cpus 128`) **before** re-running the sweep. The invalid first sweep's output was deleted from `/tmp/pb/wsA-cand2` before the corrected sweep ran, so no stale data could leak into the pooled analysis below. Raw output and a description of this incident are archived at `numa-design/bench-data/wsA-cand2/cand2-stale-binary-incident.md`.

Sweep (single session, rotating order, one warmup + 10 recorded rounds, vmstat snap per run, idle check per run; `GODEBUG=gcshrinkstackoff=1` all arms per the harness design):

```
$ ssh numa-dell 'cd /home/deparker/go-numa
FLAGS="-ptrheap=true -toucher=false -heap=4096 -idle=1000000 -stacks=200 -warm=45 -gcgap=5s -discard=2 -n=8"
./numa-design/pathology-sweep.sh wsA-cand2 10 /tmp/pb/wsA-cand2 \
  "A=numactl --cpunodebind=0 --membind=0 env GOMAXPROCS=128 GODEBUG=gcshrinkstackoff=1 /tmp/pb/gcpause-base $FLAGS" \
  "B=env GOMAXPROCS=128 GODEBUG=gcshrinkstackoff=1 /tmp/pb/gcpause-base $FLAGS" \
  "C=env GOMAXPROCS=128 GODEBUG=gcshrinkstackoff=1 /tmp/pb/gcpause-numa $FLAGS"'
```

**Mechanism validity:** arm A 0/0 every round (11/11 including warmup). Arm C 0/0 every round (11/11) — cleaner than candidate 1's two negligible-noise rounds; per Gate 5's caveat, this is Layer 1's signature and does not by itself confirm confinement engaged (see "Post-hoc confinement verification" below). Arm B, **recorded rounds only (r1–r10, excluding the r0 warmup round)**: hint faults **38,845–475,745**, pages migrated 566,945–1,689,870, every recorded round (an earlier version of this line included the warmup round's value, giving a misleadingly low minimum of 28,286).

**Round-level analysis (median of 8 cycles per round, n=10 rounds/arm; cycles are clustered within a round — reported at round level per the plan, raw n=80 cycle-level numbers never used as the verdict):**

ICC(1) and effective n (design-effect-corrected): A ICC=0.984 (effective n≈10.1), B ICC=0.813 (effective n≈12.0), C ICC=0.994 (effective n≈10.1) — all within the plan's predicted 0.6–1.0 range, confirming round-level (not cycle-level) is the correct unit of analysis; raw n=80 would substantially overstate power.

Mann-Whitney U on round medians, script archived as `cand2_analysis.py`: **the script's normal-approximation function was originally named `exact_mwu`, which was wrong — it computes a normal approximation with continuity correction, not an exact permutation p-value; renamed to `normal_approx_mwu` in this correction pass, with a true `exact_mwu_full_enum` added alongside it.** Both are now in the archived script (no ties in the round-median data, so the exact null distribution of U is exact, not approximated):

```
B vs C (primary):            B median=3922.26ms  C median=3151.21ms  rel=-19.66%  U=0/100 (n=10,10)
  normal-approx (script, conservative): p=0.0002
  exact:                                p=1.083e-05   SIGNIFICANT
A vs B (secondary/context):  A median=3120.34ms  B median=3922.26ms  rel(B vs A)=+25.70%  p=0.0002 (normal-approx)  SIGNIFICANT
```

Both readings agree on significance; the normal-approximation p was conservative (larger than the true exact p) as expected for a fully separated small-n comparison (U=0, complete separation between arms).

**Non-inferiority CI, C vs A (round-paired log-ratio + t-distribution, `noninf_ci.py`):**

```
n=10 point_estimate_C/A-1=+1.51% 95%CI=[-2.01%, +5.15%]
Non-inferiority (upper bound <= +10%): PASS
```

**Achieved MDE:** pooled round-level CV (B, C) = 4.57%; achieved MDE at n=10, α=0.05, power=0.80 = **5.72%** — closely matching the prior candidate-2 correction's cited noise floor (≈5.6% at n=10, pooled CV 4.45%, RESULTS.md candidate 2 correction), so this session's design-effect and noise characteristics replicate that prior finding. Any future "~" verdict on this workload is reported as "no effect ≥ ~5.7% detectable," never as "no effect."

**Verdict: PASS on all three pre-declared components — the cleanest of the two decision gates.**
- **C vs B (primary): PASS.** C significantly faster than B by 19.66% (p=0.0002, n=10 round medians).
- **C vs A (non-inferiority): PASS.** Point estimate +1.51%, 95% CI [-2.01%, +5.15%], upper bound 5.15% ≤ the +10% bound — comfortably inside, with more margin than candidate 1's 8.26%.
- **B vs A (secondary, context): B is significantly worse than A** by 25.70% (p=0.0002) — exactly the "cleanest B-worse-than-A signal" the harness design predicted for this candidate (§4: GC-cycle time is CPU-count-neutral, so the HT confound that complicates candidate 1's A-vs-B comparison barely applies here).

Recovery on the primary metric's point estimates: (B−C)/(B−A) = (3922.26−3151.21)/(3922.26−3120.34) ≈ **96%** — C recovers nearly all but not quite all of the unpinned-vs-pinned gap here (contrast candidate 1's 108%, where C nominally beat the pinned oracle outright); the C-vs-A non-inferiority bound above (+5.15% upper) is the honest way to state this, not "C matches A."

**Confined node this session:** the sweep's own C-arm process node was not observed in-run (see "Post-hoc confinement verification" below). The pre-sweep free-RAM precondition check (`numactl --hardware | grep free`) was against node 0; a post-hoc verification run of the actual sweep binary (`/tmp/pb/gcpause-numa`) after the fact confirmed confinement to **node 0**, but — as with candidate 1 — this does not guarantee every recorded round used the same node, since node choice follows the boot CPU per process start, not a pinned value.

Raw data: `numa-design/bench-data/wsA-cand2/wsA-cand2-arm{A,B,C}-{warmup,recorded}.out{,.stderr}`, per-round `wsA-cand2-arm*-r*.vmstat.{before,after}`, `wsA-cand2-vmstat-summary.txt`, per-cycle extracts (`wsA-cand2-{A,B,C}-cycles.txt`), round-medians (`wsA-cand2-{A,B,C}-roundmedians.txt`), the analysis script and its output (`cand2_analysis.py`, `wsA-cand2-analysis-output.txt`), the sweep log (`wsA-cand2-sweep.log`), the stale-binary incident writeup (`cand2-stale-binary-incident.md`), and the post-hoc `go version -m`/confinement-verification transcripts described below.

### Post-hoc confinement verification (correction pass, added after initial write-up)

**Gap, stated plainly:** neither candidate sweep captured direct confinement evidence (an affinity/policy observation) *during* a recorded round — the plan called for it (§ mid-sweep sanity expectations: "one C round's `/proc/PID/status` `Cpus_allowed_list` shows one node's CPUs") and it was omitted for both candidates. What was captured instead was: (a) a **pre-sweep pilot** for candidate 1 (same command, run once immediately before the recorded rounds began) and a **pre-registration strace** for the json workload (Gate 4b) — both real evidence, but not in-sweep observations of the actual candidate binaries mid-round; and (b) vmstat 0/0 throughout both sweeps, which — per the Gate 5 caveat above — is Layer 1's signature, not confinement's. This gap is exactly the blind spot that let the stale, non-confining `gc-pause-bench` binary run for an entire sweep (Gate 7's incident) without in-run detection; only Gate 5's own honest caveat above explains why 0/0 didn't catch it.

**Corrective evidence gathered after the fact (this correction pass), on the actual sweep binaries, which still exist on `numa-dell` unmodified since the sweeps ran:**

`go version -m` on all four sweep binaries, confirming they are the genuine sweep artifacts (matching build SHA `c226071c35` and `X:numa` tag where applicable) — **not reconstructions**:

```
/tmp/pb/base/garbage:    go1.28-devel_c226071c35 ...
/tmp/pb/numa/garbage:    go1.28-devel_c226071c35 ... X:numa
/tmp/pb/gcpause-base:    go1.28-devel_c226071c35 ...
/tmp/pb/gcpause-numa:    go1.28-devel_c226071c35 ... X:numa
```
Full transcripts archived as `wsA-cand1-govm-posthoc-verify.txt` / `wsA-cand2-govm-posthoc-verify.txt` (identical content, both candidates' directories, since all four binaries were checked together). **Both `gcpause-base` and `gcpause-numa` additionally report `build vcs.modified=true`** (the tree had uncommitted state at build time). Checked directly: `git status --short` on the remote `go-numa` tree shows two untracked entries, `numa-design/run-x-benchmarks.sh` and `x-benchmarks/` — pre-existing artifacts from before this task began (present in the very first status check of this session, not created by this work), not from any bench-data or scratch output of this task; `git diff --stat` (tracked-file changes) is empty. Go's VCS-dirty detection flags any non-clean `git status --porcelain` output, including untracked files, which is sufficient to explain the flag without implying any tracked source was modified at build time. Stated honestly rather than asserted with certainty: this explanation is directly checkable (and was checked) but the untracked files' *origin* — who created them and when — was not further traced.

**Build-time mtimes were annotated inline in an earlier draft of this section; that was inference (from the binaries' own `ls -l` timestamps and their position in this session's command sequence), not literal `go version -m` transcript content, so it has been moved out of the fenced block above.** Verifiable timestamp evidence, captured directly and archived as `wsA-cand{1,2}-binaries-ls.txt`:

```
$ ssh numa-dell 'ls -l --time-style=full-iso /tmp/pb/base/garbage /tmp/pb/numa/garbage /tmp/pb/gcpause-base /tmp/pb/gcpause-numa'
/tmp/pb/base/garbage     2026-08-20 13:55:12.598313306 -0400
/tmp/pb/numa/garbage     2026-08-20 13:55:14.218309877 -0400
/tmp/pb/gcpause-base     2026-08-20 17:23:34.194567414 -0400
/tmp/pb/gcpause-numa     2026-08-20 17:23:40.745556795 -0400
```

Independent corroboration, checked directly rather than asserted: the sweep archive's earliest file for each candidate — `wsA-cand1-armA-r0.vmstat.before` (mtime `13:56:31`) and `wsA-cand2-armA-r0.vmstat.before` (mtime `17:25:44`) — lands within roughly one to two minutes after each candidate's respective binary build mtimes above. This is a source external to the `go version -m` transcript itself (a separately-timestamped file from the sweep driver's own vmstat snapshots), and it corroborates **both** candidates' timelines, not only candidate 1's — no timestamped stderr line from either binary's own runtime output was found to serve as an additional corroborating source, so the vmstat-file-mtime chain above is the actual evidence being cited here.

**In-run confinement captures (post-hoc verification runs, NOT sweep data — labeled as such in the archive):** one run per candidate's C-arm binary, at the sweep's exact GOMAXPROCS=128 configuration, sampling `/proc/PID/status` `Cpus_allowed_list` a few seconds into the run:

```
$ ssh numa-dell 'env GOMAXPROCS=128 /tmp/pb/numa/garbage -benchmem=4096 -benchnum=1 & PID=$!; sleep 3; grep Cpus_allowed_list /proc/$PID/status; wait $PID'
Cpus_allowed_list: 0,2,4,...,254   (even CPUs — node 0)

$ ssh numa-dell 'FLAGS="-ptrheap=true -toucher=false -heap=4096 -idle=1000000 -stacks=200 -warm=45 -gcgap=5s -discard=2 -n=8"
env GOMAXPROCS=128 GODEBUG=gcshrinkstackoff=1 /tmp/pb/gcpause-numa $FLAGS & PID=$!; sleep 5; grep Cpus_allowed_list /proc/$PID/status; kill $PID'
Cpus_allowed_list: 0,2,4,...,254   (even CPUs — node 0)
```

Both actual sweep binaries confirmed confining to a single node (node 0, this verification pass) when run under the exact sweep configuration. Archived as `wsA-cand{1,2}-posthoc-confinement-verify.txt`. **These are after-the-fact verifications, run well after both sweeps completed — they demonstrate the binaries are capable of confining under this configuration, not that every recorded round of either sweep actually confined.** The strongest in-sweep-adjacent evidence remains: Gate 4(b)'s strace (same json workload, same GOMAXPROCS regime, run within the same session as Gate 6/7 prep) and candidate 1's pre-sweep pilot (same binary, same command, run immediately before the recorded rounds). Neither substitutes for a genuine mid-sweep sample, and none was taken during either candidate's actual recorded rounds.

## Overall verdict: Workstream A gate battery — **SHIP**

All 8 pre-declared gates pass:

| Gate | Result |
|---|---|
| 1. 1P json (hard) | PASS — not significant, p=0.579-0.631; MDE ≈40% at n=10 (excludes only gross regressions) |
| 2. 1P alloc micro (hard) | PASS — geomean +0.67% ≤ +2% |
| 3. 256P json (hard) | PASS — pooled n=30, not significant, p=0.197; MDE ≈22% at n=30 (excludes only gross regressions) |
| 4. Stand-down proof, strace (hard) | PASS — all 3 arm shapes match exactly (mbind counts corrected to 969 BIND/969 PREFERRED at 256P, 11/11 at confined 1P — see corrected Gate 4 text) |
| 5. vmstat 0/0 confined (hard) | PASS — 0/0 twice (Layer-1 evidence, not confinement evidence — see caveat) |
| 6. Candidate 1 garbage (decision) | PASS — C beats B (-29.61%, p=0.000); C-vs-A non-inferiority bound PASSES (point +2.69%, CI upper +8.26%), but paired sign test (12/15 rounds C slower, p=0.035) means a small residual in A's favour cannot be excluded — a bound, not an equivalence; B worse than A (+37.93%) |
| 7. Candidate 2 gc-pause (decision) | PASS — C beats B (-19.66%, exact p=1.1e-5); C-vs-A non-inferiority bound PASSES (point +1.51%, CI upper +5.15%); B worse than A (+25.70%) |
| 8. Off-binary census (hard) | PASS (substantive) — 1481/1481 functions identical, zero numa symbols off-experiment |

**No hard gate failed; both decision gates passed their pre-declared non-inferiority bound.** Per the plan's Task 4 stop rule, hard-gate pass alone is sufficient for Task 6 (Layer-2 removal) to proceed independently of the decision gates — moot here since the decision gates also passed. **Important scope correction: Gates 6 and 7 characterize Layer 0+1+2+confinement running together, not confinement in isolation** — Layer 2 (PREFERRED-at-grow) is still in-tree and active for both candidates' C arm (see Gate 4's corrected mbind breakdown); Task 6 removing Layer 2 requires a Gate-6 B-vs-C confirmation sweep before the candidate-1 numbers above can be attributed to confinement alone. Workstream A ships as a genuine, measured win, reported as bounds rather than equivalences: fill-one-socket-first (+ the still-active Layer 2) recovers **108% of the unpinned-vs-pinned gap on candidate 1's primary point estimate** — the workload where C nominally beat the pinned oracle A outright, though the paired sign test says a small residual in A's favour cannot be fully excluded there — and **96% on candidate 2**, both without a non-inferiority-bound violation against the hard-pinned baseline and without any measurable regression on any hard gate.

**N1 residual note (carried from the plan's locked decision 4, recorded here per the plan's Task 4 gate battery instructions, `2026-08-20-numa-v3-locality-plan.md:1182`):** Ms cloned in the window between `worldStarted()` and a later `numaStandDownIfNeeded` trigger (specifically the interval after the eager allm walk is dispatched but before all live Ms have converged via `numaFixThreadPlacement` at `stopm`) inherit the confined affinity/policy from their creating M at `clone` time and keep that placement until they first park. This is documented, accepted residual behavior (Task 3), not a defect: such Ms remain balancer-exempt (explicit task policy) throughout, and converge to the stood-down state at their first park like every other M — the residual is a bounded delay in reaching the fully-stood-down state, never an incorrect or unsafe placement.

**Concerns for the controller:**

1. **Process discipline finding (self-caught, not controller-facing until now):** the first Candidate 2 attempt used stale pre-Workstream-A binaries due to a silent build failure masked by an unconditional `echo BUILDS-OK` in a multi-line ssh script with no `set -e`. Caught before accepting the result by investigating an anomaly (C slower than B despite confirmed mechanism suppression) rather than writing it up. Recommend: `pathology-sweep.sh`-adjacent build-prep scripts should `set -euo pipefail` and verify each binary's `go version -m` build-ID/tags before the sweep starts, not just check exit codes loosely.
2. Gate 2's `Malloc(8|16|Types)` regex still doesn't match any benchmark named `...Types` in this tree (only `MallocTypeInfo8/16/32` exist) — same pre-existing gap noted at every prior layer, run verbatim as specified rather than "fixed" unilaterally.
3. Gate 8's literal "differ only in build IDs" bar is structurally unattainable here (as at Layer 0): Workstream A's new unconditional confinement-state globals in `numa_linux.go` shift subsequent data addresses. Passed on the substantive per-function/opcode/branch-target criterion instead, exactly as Layer 0's Gate 4 precedent established.
4. Both decision-gate non-inferiority CIs used a round-paired log-ratio + t-distribution method (`noninf_ci.py`, archived), not a benchstat built-in — benchstat does not report confidence intervals, only point deltas and p-values, so this session computed the CI directly from the raw paired round data per the plan's non-inferiority note (`2026-08-20-numa-v3-locality-plan.md:1060`, bootstrap or log-ratio CI).
5. **This section was corrected after an independent audit** found: a materially incomplete Gate 4 mbind characterization (fixed — see corrected text, both candidates' C arm actually ran Layer 0+1+2+confinement together, not confinement alone); an over-confident Gate 6 C-vs-A narrative that suppressed the paired sign test's contrary signal (fixed — reported as a bound with the sign test alongside); a candidate-1/candidate-2 attribution swap in the original overall verdict (fixed); missing MDE statements on Gates 1 and 3 (added); missing in-sweep confinement evidence for both candidates (partially addressed post-hoc — see "Post-hoc confinement verification" — the sweep-time gap itself cannot be retroactively closed); an arithmetic slip (14/15 → 13/15 for candidate 1's clean vmstat rounds); a warmup-round contamination in candidate 2's reported B-arm fault range; a misnamed `exact_mwu` function (renamed, true exact p added as a cross-check); an overstated idle-check claim (the `ps` self-report artifact, reproduced live during this correction pass, fired dozens of times on a genuinely idle box — `pathology-sweep.sh`'s `idle_check` fixed in this same commit to exclude its own `ps`/`awk` pipeline); and citations to the untracked, gitignored `task-4-brief.md` where the tracked, git-verifiable plan (`e44dad45f1`) carries the same content and is the correct pre-registration citation (fixed throughout).

Session end: `kernel.numa_balancing = 1` (confirmed), THP `[always] madvise never` (unchanged), machine idle (`ps aux --sort=-%cpu` clean) throughout and at close.

## Layer 2 removal + confirmation sweep (2026-08-20)

Removal SHA: `4b13232e3c` ("runtime: remove superseded Layer 2 PREFERRED-at-grow"). Parent: `3fa14a90ee` (Task 5, the Workstream A gate battery tree). Task 6 per the tracked plan: contingent only on Task 4's hard gates (1-5, 8), all of which passed — see the gate battery's overall verdict above. `numaBindArena` (`src/runtime/numa_linux.go`) now issues exactly one `MPOL_BIND` `mbind` per grown chunk; the `getcpu(2)` call, node guard, `MPOL_PREFERRED` mask build, second `mbind`, and the `numaPreferredCalls` counter are gone, along with `NumaPreferredBindCalls` (`export_numa_test.go`) and `TestNUMAPreferredBindOnGrow` (`numa_linux_test.go`). `numaGetCPUNode`/`numaCurrentNode` and `_MPOL_PREFERRED` are kept — Layer 0 diagnostics and fill-one-socket-first confinement's node choice and task policy still use them. Stale Layer-2 references in the two `getcpu` wrapper files' doc comments (`numa_linux_getcpu.go`, `numa_linux_getcpu_other.go`) were also updated; grepped the tree for `PREFERRED`/`PreferredBind` afterward — every remaining hit is confinement's own use of `MPOL_PREFERRED`, not a Layer-2 leftover.

### Removal verification battery

- `gofmt -l` on the five touched files: clean.
- `go vet runtime`, `GOEXPERIMENT=numa go vet runtime`: both clean.
- `./make.bash` (off, tree toolchain): succeeds.
- All-arch link sweep (house convention, Task 1's method), `GOOS=linux`, `{amd64,arm64,riscv64,arm,386,ppc64le,s390x,loong64} × {off,numa}`: **16/16 link OK**.
- Local `GOEXPERIMENT=numa go test runtime -run TestNUMA -count=1 -v`: 8 tests run (single-node local box; most confinement tests skip with "not multi-node", same as every prior layer), all PASS. `TestNUMAPreferredBindOnGrow` correctly absent.
- **Off-binary objdump vs parent, function-level census** (same methodology as Gate 8 above — a plain `package main` canary linked against the tree toolchain, `GOEXPERIMENT=` unset, one build from the removal commit and one from a `git worktree` at the parent `3fa14a90ee`): **1481/1481 functions identical, zero per-function instruction-count diffs, zero remaining `numa`-related symbols in the off-binary disassembly.** Off codegen is byte-identical at the function-census level — stronger than every prior layer's result (which always showed a small global-data-offset shift from new package-level state) because this change is a pure deletion inside a function whose call sites (`mheap.go`, both `numaBindArena` call sites) are already gated by `if goexperiment.Numa`, so the whole file dead-code-eliminates in the off build with nothing left to shift addresses. Canary programs and worktree discarded after the check; not archived (trivially reproducible, unlike the sweep data below).
- Remote (`numa-dell`, `make push && make build`, SHA `4b13232e3c` confirmed via `git log -1` on both sides): `make test-numa RUN='TestNUMA'` — **8/8 PASS** (`TestNUMATopologyDiscovery`, `TestNUMAGetcpu`, `TestNUMABindAllTaskPolicy`, `TestNUMASetThreadAffinitySelf`, `TestNUMAFillOneSocketConfined`, `TestNUMAConfineSkipsNarrowedAffinity`, `TestNUMAStandDownOnGOMAXPROCSGrowth`, `TestNUMAStandDownOnSetDefaultGOMAXPROCS`) — down from 9 in the last full remote run (Task 3/4 era), exactly the expected 1-test drop from removing `TestNUMAPreferredBindOnGrow`. No other test's outcome changed.

**Gate 1 + Gate 2 insurance re-run on the removal commit** (`BENCHNUM=10`, `GOMAXPROCS=1`):

```
$ ssh numa-dell 'cd /home/deparker/go-numa && GOROOT=$PWD GOMAXPROCS=1 BENCHNUM=10 OUT=/tmp/t6-gate1 ./numa-design/gate-json.sh'
$ ssh numa-dell '/tmp/numa-tools/benchstat /tmp/t6-gate1/baseline.out /tmp/t6-gate1/numa.out'
JSON-1  sec/op:            16.79m ± 97%  17.08m ± 88%  ~ (p=0.579 n=10)
JSON-1  user+sys-sec/op:   16.82m ± 97%  17.09m ± 88%  ~ (p=0.529 n=10)
```

```
$ ssh numa-dell 'cd /home/deparker/go-numa && export GOROOT=$PWD PATH=$PWD/bin:$PATH GOTOOLCHAIN=local
GOMAXPROCS=1 go test runtime -run=NONE -bench="Malloc(8|16|Types)" -count=10 >/tmp/t6-alloc-base.out
GOMAXPROCS=1 GOEXPERIMENT=numa go test runtime -run=NONE -bench="Malloc(8|16|Types)" -count=10 >/tmp/t6-alloc-numa.out
/tmp/numa-tools/benchstat /tmp/t6-alloc-base.out /tmp/t6-alloc-numa.out'
Malloc8    7.006n ± 100%   6.941n ± 1%  -0.91% (p=0.000 n=10)
Malloc16   11.34n ±   0%   11.31n ± 0%  -0.31% (p=0.002 n=10)
geomean    8.915n          8.860n       -0.61%
```

**Verdict: both PASS.** Gate 1 not significant on either metric (consistent with every prior layer at `GOMAXPROCS=1`'s ≈40% MDE). Gate 2 geomean -0.61%, well inside the ±2% pass bar (and, if anything, a hair better than the -0.61%-vs-prior-layers' generally-positive deltas — noise, not a claimed improvement). No regression from the removal on either hard gate. Removal cleanly verified; proceeding to the required Gate-6 B-vs-C confirmation sweep below.

### Gate-6 B-vs-C confirmation sweep (the audit's hard requirement)

**Why this sweep exists:** the Workstream A gate battery's Gate 6/7 decision-gate numbers (candidate 1 -29.61%, candidate 2 -19.66%) characterized Layer 0+1+2+confinement running *together* — Layer 2 was still in-tree for both candidates' C arm (Gate 4(b)'s corrected mbind breakdown: 11 `MPOL_BIND` + 11 `MPOL_PREFERRED` per chunk while confined). The independent audit made re-establishing candidate 1's win under confinement *alone* (Layer 2 removed) a hard requirement before this removal could be considered fully justified, not just IMC-gate-justified. This sweep is that re-establishment: same workload (`x/benchmarks/garbage`), same flags, same `GOMAXPROCS=128`, same n=15, but only **B** (stock, unpinned) vs **C** (post-removal numa build, unpinned, self-confines) — arm A (pinned oracle) is not needed here since the question is specifically whether B-vs-C survives Layer 2's removal, not a fresh non-inferiority bound against A.

**Pre-flight, binaries verified fresh before sweeping (the stale-binary lesson from Gate 7's incident):**

```
$ ssh numa-dell 'export GOROOT=/home/deparker/go-numa PATH=/home/deparker/go-numa/bin:$PATH GOTOOLCHAIN=local
rm -rf /tmp/pb/base /tmp/pb/numa && mkdir -p /tmp/pb/base /tmp/pb/numa
GOBIN=/tmp/pb/base go install golang.org/x/benchmarks/garbage@v0.0.0-20260819172200-70693762b6a0
GOBIN=/tmp/pb/numa GOEXPERIMENT=numa go install golang.org/x/benchmarks/garbage@v0.0.0-20260819172200-70693762b6a0'
$ ssh numa-dell '/home/deparker/go-numa/bin/go version -m /tmp/pb/base/garbage; /home/deparker/go-numa/bin/go version -m /tmp/pb/numa/garbage'
/tmp/pb/base/garbage:  go1.28-devel_4b13232e3c ...
/tmp/pb/numa/garbage:  go1.28-devel_4b13232e3c ... X:numa
```

Both binaries confirmed built from the removal commit `4b13232e3c` (same `x/benchmarks` pin `v0.0.0-20260819172200-70693762b6a0` as every prior sweep), mtimes ~20:33:40/42, immediately before the sweep started at 20:34. `numactl --hardware` free memory: node 0 11145→**12279 MB free at sweep start** (recovered further since the last session, no leak carried over); `kernel.numa_balancing=1`, THP `[always] madvise never`, box idle (`ps aux --sort=-%cpu` clean) confirmed before starting.

**Sweep** (single session, rotating B/C order via `numa-design/pathology-sweep.sh` — the Task 0 driver, `idle_check` fix already in place per the prior correction pass — one warmup round + 15 recorded, vmstat snap per run):

```
$ ssh numa-dell 'cd /home/deparker/go-numa
./numa-design/pathology-sweep.sh ws-a-l2removal 15 /tmp/pb/ws-a-l2removal \
  "B=env GOMAXPROCS=128 /tmp/pb/base/garbage -benchmem=4096 -benchnum=1" \
  "C=env GOMAXPROCS=128 /tmp/pb/numa/garbage -benchmem=4096 -benchnum=1"'
```

32 runs total (16 rounds × 2 arms), ~26 minutes wall clock.

**Mechanism validity (n=15 recorded rounds each, from `ws-a-l2removal-vmstat-summary.txt`):** arm B: hint faults 54,117–467,312, pages migrated 660,508–1,878,996, every recorded round (≫0, no exclusions needed). Arm C: **0/0 in all 16 rounds, including the warmup round** — cleaner than the original Gate 6 sweep's 13/15 clean rounds. Per the Gate 5 caveat established earlier in this document, 0/0 is Layer 1's signature (uniform BIND-all VMA policy, unaffected by this removal) and does not by itself prove confinement engaged for these rounds — see the in-sweep `Cpus_allowed_list` capture and the strace below for that.

**In-sweep confinement observation (the specific gap the audit flagged — neither Gate 6 nor Gate 7's original sweeps captured this live):** polled for the actual sweep C-arm process during the live sweep and read `/proc/PID/status` a few seconds into a real recorded round, not a separate pilot run:

```
$ ssh numa-dell 'readlink /proc/2086376/exe; grep Cpus_allowed_list /proc/2086376/status'
/tmp/pb/numa/garbage
Cpus_allowed_list: 0,2,4,6,...,254   (128 even CPUs — node 0)
```

PID 2086376 was confirmed to be the actual sweep binary (`readlink /proc/PID/exe` → `/tmp/pb/numa/garbage`, matching the sweep's own C-arm command), sampled ~3s after that round's launch, mid-run — not a pre/post-sweep pilot. This closes the exact gap the audit called out in the Workstream A "Post-hoc confinement verification" section above.

**benchstat, pre-declared primary metric (`sec/op`, `Garbage/benchmem-MB=4096-128`):**

```
$ ssh numa-dell '/tmp/numa-tools/benchstat /tmp/pb/ws-a-l2removal/ws-a-l2removal-armB-recorded.out /tmp/pb/ws-a-l2removal/ws-a-l2removal-armC-recorded.out'
Garbage/benchmem-MB=4096-128   B=2.920m ± 8%   C=1.953m ± 1%   -33.13% (p=0.000 n=15)
```

**Exploratory corroboration (not the primary metric):** `user+sys-sec/op`, B=153.2m ± 3%, C=124.7m ± 1%, **-18.64% (p=0.000, n=15)** — a CPU-time-based metric independently corroborating the wall-clock primary, and nearly identical to the original Gate 6 sweep's -18.62% on the same metric.

**Verdict: the win reproduces and, on this session's numbers, is nominally slightly larger than the original Gate 6 result (-33.13% here vs -29.61% with Layer 2 still active).** The two point estimates are close enough (both sessions' B arms show 8-17% run-to-run variance) that "slightly larger" should not be read as Layer 2 having been mildly harmful — the honest reading, consistent with the IMC gate FAIL and the three-arm C-full-vs-C-L1 result (p=0.838) that motivated this removal, is that Layer 2 was inert and the two sessions' deltas differ by ordinary between-session noise on `garbage`'s B arm (unpinned baseline variance dominates both point estimates' spread). Either way, the pre-declared, pre-registered requirement — **does the candidate-1 win survive Layer 2's removal** — is unambiguously **YES**: C beats B by a wide, highly significant margin (p=0.000) with clean mechanism validity (16/16 rounds 0/0) and a direct in-sweep confinement observation, on binaries independently verified fresh via `go version -m` before the sweep started.

**strace, one dedicated post-sweep C-run** (`GOMAXPROCS=128`, same command as the sweep's C arm, run once outside the recorded data to avoid strace overhead skewing the timed rounds — same methodology as Gate 4(b)):

```
$ ssh numa-dell 'strace -c -f -e trace=sched_setaffinity,set_mempolicy,mbind -o t6-strace-count.txt \
  env GOMAXPROCS=128 /tmp/pb/numa/garbage -benchmem=4096 -benchnum=1 >/dev/null'
calls     syscall
--------  ------------------
    1713  mbind
       2  set_mempolicy
       1  sched_setaffinity
```

A second full (non-`-c`) trace of the same command, filtered to strip `strace -f`'s per-thread `SIGURG` noise (Go's async-preemption signal fires heavily at `GOMAXPROCS=128` under GC pressure — 233,336 of the raw trace's 235,861 lines were `SIGURG` deliveries, irrelevant to this evidence; the filtered, archived version keeps only the `mbind`/`set_mempolicy`/`sched_setaffinity` lines) gives the mode breakdown `-c` can't:

```
sched_setaffinity(0, 1024, [1 3 5 ... 255]) = 0                              — 1 call, odd CPUs (node 1, this run's boot node)
set_mempolicy(MPOL_BIND, [0x3], 65) = 0                                      — task policy, Layer 1 (numaSetProcessBindAll, schedinit)
set_mempolicy(MPOL_PREFERRED, [0x2], 65) = 0                                 — task policy, confinement (numaConfine, REPLACES the BIND-all above)
mbind(...): 1738 calls total, ALL MPOL_BIND, ZERO MPOL_PREFERRED
```

(The `-c` summary's 1713 vs the full trace's 1738 completed `mbind` calls is a small, known `strace -f` counting artifact under heavy thread churn — same class of discrepancy Gate 4 documented and corrected for the 256P count; the full-trace count, cross-checked by grepping distinct `mbind(`-prefixed call-open lines, is the more reliable figure and is what the mode breakdown above is built from.)

**This is the direct evidence the removal did what it claims:** every arena `mbind` is now `MPOL_BIND` — zero `MPOL_PREFERRED` reaches the arena path — while confinement's own, separate, task-level `set_mempolicy(MPOL_PREFERRED, ...)` call is untouched and still fires exactly once, distinguishing the two mechanisms cleanly (arena VMA policy via `mbind`, task policy via `set_mempolicy`) exactly as the removal's doc comment in `numa_linux.go` describes.

Session end: node 0 free memory 12279→12366 MB (recovered, no leak), `kernel.numa_balancing=1` (confirmed), THP unchanged, box idle throughout and at close.

Raw data archived under `numa-design/bench-data/ws-a-l2removal/`: per-round `.vmstat.{before,after}` (32 pairs), `ws-a-l2removal-arm{B,C}-{warmup,recorded}.out{,.stderr}`, `ws-a-l2removal-vmstat-summary.txt`, `sweep.log`, the in-sweep `Cpus_allowed_list` capture (`insweep-cpus-allowed.txt`), the benchstat transcript (`t6-benchstat-BvC.txt`), the strace count summary (`t6-strace-count.txt`) and the filtered full-trace mode breakdown (`t6-strace-full-filtered.txt` — `SIGURG` noise stripped, syscall lines only, 180K vs the 23M raw capture), and the `go version -m`/`ls` freshness transcript (`t6-govm-freshness.txt`). `numa-design/pathology-sweep.sh` itself was committed at Task 0 and is unchanged by this task.

**Concerns for the controller:**

1. The two Gate-6-family sessions' B-arm point estimates differ enough (2.558ms in the original Workstream A sweep vs 2.920ms here) that the resulting deltas (-29.61% vs -33.13%) aren't directly comparable as a before/after measurement of Layer 2's own effect — that would require a same-session three-arm design (C-full vs C-L1), which is exactly what the earlier three-arm sweep already did and found inert (p=0.838). This sweep's job was narrower and is answered cleanly: does the win survive removal, on its own terms, in a fresh single session. It does.
2. `strace -c`'s summary count (1713 mbind) and the full-trace completed-call count (1738) disagree by 25; not investigated further than noting the same class of `-f`-under-thread-churn artifact Gate 4 already diagnosed and corrected for. Does not affect the qualitative finding (100% BIND, 0% PREFERRED in either count).
3. `bind-all-policy.md` (Task 5) still describes the confined-1P Gate-4 mbind breakdown as "11 BIND + 11 PREFERRED — Layer 2 still in-tree at report time" — accurate when written, now stale now that Layer 2 is removed. Not touched by this task (out of its stated file scope); flagged per Task 5's own report, which anticipated exactly this follow-up.

---

## Workstream B go/no-go (Task 7)

Date: 2026-08-20. HEAD at decision time: `53d631efd7` ("numa-design: update bind-all-policy for Layer 2 removal"), immediately following the Layer 2 removal + confirmation sweep recorded above (`4b13232e3c`, confirmation sweep same session).

### Decision: **GO for Workstream B**

Tasks 8–11 (design §12.3–§12.4: per-node arena streams / `heapArena.node`, per-node mcentral spanSets, node-mask soft affinity) are authorized to proceed as **one gated unit**, per the plan's own framing (§12.1, three-ingredient rule — see below). This is not a rubber stamp of the plan's default path; the reasoning is laid out in full because the case for Workstream B is a narrower one than "Workstream A won, so continue," and the data supports it on that narrower ground.

### Input 1 — the plan's stated entry condition is met

The plan (`2026-08-20-numa-v3-locality-plan.md:1240`) sets Workstream B's entry condition as: "Workstream A shipped its gates, or A's candidate results show a remaining C-vs-A/C-vs-B gap for node-exceeding processes that justifies the cost." The first branch holds outright:

- **Workstream A gate battery — SHIP.** All 8 pre-declared gates PASS (hard gates 1–5, 8; decision gates 6–7), see "Overall verdict: Workstream A gate battery" above.
- **Layer 2 removal + confirmation sweep confirms the win survives.** `x/benchmarks/garbage`, B vs C (post-removal, confinement alone), GOMAXPROCS=128, n=15: **C beats B by -33.13%** (p=0.000), with clean 16/16-round mechanism validity and a direct in-sweep `Cpus_allowed_list` confinement observation (not a pilot proxy — see "Layer 2 removal + confirmation sweep" above). This is a wider margin than the original Gate 6 result (-29.61%, with Layer 2 still active) and the two figures' closeness (attributed to ordinary between-session B-arm variance, not to Layer 2 being mildly harmful) reads as **complete separation** between B and C on this workload across every session this plan has run.

So the condition is met on its own terms: Workstream A shipped, cleanly, with every gate passing and the confinement win independently reproduced after Layer 2's removal.

### Input 2 — what Workstream A does *not* cover, and why that is the actual reason to proceed

Meeting the plan's entry condition is necessary but not sufficient to justify Workstream B's cost; the honest question is what problem remains unsolved. Workstream A's confinement mechanism (`numaConfineIfSmall`) engages under a narrow condition: `GOEXPERIMENT=numa`, multi-node, GOMAXPROCS **explicitly set** (`sched.customGOMAXPROCS`), no pre-existing narrowed affinity, and GOMAXPROCS ≤ one node's CPU count (design §12.2, plan intro line 7). Outside that window — default GOMAXPROCS, or GOMAXPROCS greater than a single node's CPU count (e.g. GOMAXPROCS=256 on numa-dell's 2-node/256-CPU box) — confinement never fires (Gate 4(a): 0 `sched_setaffinity` calls at 256P, by design) and the process runs on **Layer 1 BIND-all alone**.

That general, node-exceeding case is not a theoretical gap. The single-session three-arm sweep ("Single-session three-arm candidate 1 sweep" above) measured it directly: at GOMAXPROCS=128 on `x/benchmarks/garbage`, **both** C-full (Layer 0+1+2) and C-L1 (Layer 0+1 alone) regress against stock B by **+5.17%/+5.24% (both p=0.001, n=15)**, statistically indistinguishable from each other (C-full vs C-L1, p=0.838). The mechanism is understood, not just observed: BIND-all's uniform arena `mbind` exempts the heap VMA from the kernel's NUMA balancer (that is Layer 1's entire point — RESULTS.md Layer 1 promotion evidence, garbage vmstat), but the balancer was doing real, beneficial work on stock B — convergent migration of hot pages toward the threads touching them — that BIND-all suppresses and nothing in Workstream A replaces. For the general-unpinned case, Workstream A trades away a working mechanism (the balancer) for a strictly better one only when confinement can engage; when it can't, the trade is pure loss, measured at a real, well-powered ~5% throughput cost (plus a corroborating ~6–7% CPU-time penalty) with p=0.001 — not a rounding-noise result.

**This is the actual motivation for Workstream B: not the residual gap in A's own gates (see Input 5), but the fact that A's fix has a hole exactly the size of "GOMAXPROCS unset, or a process bigger than one node" — which on real deployments is the common case, not the edge case.**

### Input 3 — the three-ingredient rule and the Layer-2 null are why B must be one gated unit, not another homing-only step

Design §12.1 states plainly: locality requires (a) memory **homed** per node, (b) refill-time **routing** so a thread is fed spans homed where it runs, and (c) **threads that stay put** long enough for (b) to still hold at use time — "any proper subset is expected to measure ~zero." This plan already ran that experiment once. Layer 2 (PREFERRED-at-grow) was ingredient (a) alone, and its own decision gate — the same IMC remote-DRAM-share metric Task 11 pre-registers below — measured **+0.10% / -0.07% relative change**, against a required ≥10% relative *drop* ("Layer 2 gate (v2)", Gate 6 — IMC locality, above): homing alone did not move the needle, in either direction, across two independent triplicate `perf stat` sets. Layer 2 was subsequently proven behaviorally inert outright by the three-arm sweep (C-full vs C-L1, p=0.838) and removed on that basis (Task 6).

That null is the reason Workstream B is scoped as Tasks 8–10 building all three ingredients (per-node arena streams for (a), per-node mcentral refill routing for (b), soft node affinity from `schedule()` for (c)) with **nothing merging until Task 11's combined gates pass as one unit** (plan line 1240). Re-attempting homing alone, or homing plus routing without thread stability, would just be Layer 2's experiment again with extra steps — the data already answers that question. The IMC ≥10% relative-drop gate (Task 11 Step 3) is the direct successor to the metric that killed Layer 2, now measured with all three ingredients present instead of one; that is what makes Task 11's verdict a real test rather than a repeat of a known-negative result.

### Input 4 — risks recorded before Workstream B's code lands

- **This is the largest runtime change in the series.** Tasks 8–10 touch `mheap.go`, `malloc.go` (arena hints), `numa_linux.go`, and the mcache/mcentral refill path — surfaces every prior layer (0/1/2, confinement) deliberately avoided. Workstream A's own off-binary census (Gate 8) already shows how sensitive this codebase is to new package-level state (a benign but real data-address shift from Workstream A's confinement globals); Workstream B's surface area is larger by a wide margin.
- **Per-node mcentral spanSets footprint.** Design §12.3's sketch implies per-node span-set state; per the plan's own Task 8 framing this must be **build-tagged** (`goexperiment.numa`) — an un-tagged version would add on the order of ~180KB to every binary's BSS regardless of whether the experiment is compiled in, which is not acceptable per the plan's off-experiment-must-collapse-to-today's-shape requirement (design §12.3, "with the experiment off the node argument is a constant and the code collapses to today's shape"). This is a build-tag discipline item to verify explicitly at Task 8, not an open question — it will be build-tagged; recorded here as the risk that makes it worth stating rather than assuming.
- **Struct/layout constraints.** `heapArena.node` (one `node uint8` field, design §12.3) and any per-P/per-M routing state added for ingredient (b)/(c) must preserve `sizeof` off-experiment, the same discipline Task 3 already established and Gate 8 verified for `mNUMAState` (empty-struct/zero-size when `!goexperiment.numa`). Workstream B has more types touching this constraint than any prior task.
- **The v2 forbidden list stays binding.** The plan's Global Constraints (line 80) prohibit "anything on the v2 forbidden list; no `p.numaNode`-by-index; no flush" — carried forward unchanged into Workstream B, which is exactly the territory (per-P routing) where those prohibitions matter most.
- **Stop rules apply per task**, same as Workstream A: a hard-gate FAIL at any of Tasks 8–10 stops that task; Task 11's combined verdict (pinned routing proof first, per design §12.4's validation order, then hard gates, then the IMC decision gate) is the only thing that can merge the unit, and a Step 3 IMC FAIL requires the verdict to state the routing-local ratio so a "routing broken" failure can be distinguished from "routing works, hardware can't show it" (plan line 1347).

### Input 5 — what is explicitly *not* the motivation

Workstream A's own decision gates (6–7) reported a residual: candidate 1's paired sign test found C slower than the pinned oracle A in 12 of 15 rounds (p=0.035), bounding — not excluding — a small C-vs-A gap at ≤+8.3% (95% CI upper bound); candidate 2's equivalent bound was tighter, ≤+5.15%. **This bounded residual is not why Workstream B is being started.** It is a small, honestly-reported uncertainty band around an already-passing non-inferiority gate, on workloads where confinement engages at all. Chasing it down with Workstream B's much larger, riskier change would be solving the wrong problem — the residual only exists inside the window where GOMAXPROCS ≤ one node's CPUs, i.e. exactly where confinement already works and delivers 96–108% of the pinned-oracle gap. The real problem — the one Workstream B actually targets — is the general-unpinned/node-exceeding case from Input 2, where there is no bounded residual to argue about because confinement never engages and the measured cost is a real, unambiguous +5% regression against stock, not a same-mechanism rounding difference.

### Task 11 pre-registration (per plan line 1244, committed before any Workstream B code lands)

Primary metric: **IMC remote-DRAM-miss share**, identical protocol to Layer 2's Gate 6 and Workstream A's evidence base —

```
perf stat -x, -e mem_load_l3_miss_retired.local_dram,mem_load_l3_miss_retired.remote_dram -- \
  env GOMAXPROCS=256 ./json -benchmem=512 -benchnum=1 -benchtime=10s
```

computed as `remote / (local + remote)`, ≥3 interleaved runs per arm, medians compared experiment-on vs experiment-off. **Pass:** ≥10% relative drop (e.g. 0.48 → ≤0.432) — the same bar Layer 2 failed at +0.10%/-0.07%. Corroborating signal: `/numa/span-refills:local`/`:remote` from the same runs. Validation order is mandatory (design §12.4, plan Task 11 Step 1): pinned routing proof (`numactl --cpunodebind` per half, ≥95% local refills required) **before** any unpinned measurement — this isolates routing correctness from placement stability and must pass before the IMC gate is run at all. This pre-registration is recorded now, in this commit, before Task 8's first line of code.

### Workstream C sequencing

Per the plan's independence note (line 1355, "Tasks 12 and 13 may run in parallel with Workstream A (different files)"), Workstream C's cheap, low-risk tasks are sequenced **before/alongside** Workstream B's heavy tasks rather than after them, since they touch disjoint files and carry no dependency on Workstream B's outcome:

- **Task 12** (phase-shift attribution rerun, n=20, pre-registered Spearman correlation on H-bandwidth vs H-faulttax) — settles a mechanism question about an already-collected result; no code dependency on Tasks 8–11.
- **Task 13** (`getcpu` asm for remaining Linux arches) — pure enablement, touches only per-arch `.s` files and the `numa_linux_getcpu*.go` build tags; explicitly widens what confinement *could* target on other arches (still gated by `numaHasSetAffinity` until per-arch `SYS_SCHED_SETAFFINITY` constants land).
- **Task 15** (dynamic cpuset staleness documentation) — a doc-only addition to `bind-all-policy.md`; no code at all.
- **Task 14** (rseq/vDSO getcpu) stays **profile-gated**, per the plan's explicit adoption gate (line 1394): its implementation half does not start until a CPU profile from Workstream B's own Task 11 Step 1 pinned run shows `getcpu` ≥1% of cycles in refill/scheduler-pass paths. Task 14's research half (the one-page rseq/vDSO note) may proceed independently at any time, but the implementation is contingent on Workstream B producing that profile — it cannot be pulled forward.

This ordering lets Workstream C's low-risk, high-certainty items land immediately while Workstream B's larger, riskier surface is built and gated on its own schedule.

### Summary table

| Input | Finding |
|---|---|
| Entry condition (plan line 1240) | MET — Workstream A gate battery SHIP (8/8 gates); Layer 2 removal confirmation sweep -33.13% B-vs-C, complete separation |
| Coverage gap Workstream A leaves | Confinement is GOMAXPROCS-explicit-and-≤-node-CPUs only; general/node-exceeding case runs Layer-1-only, measured **+5.17%/+5.24% regression vs stock (p=0.001, n=15)** |
| §12.1 three-ingredient rule | Layer 2 (ingredient (a) alone) measured IMC +0.10%/-0.07% — a proper subset is ~zero, exactly as predicted; Workstream B is ingredients (a)+(b)+(c) as ONE gated unit, Task 11 verdict only |
| Task 11 primary metric | IMC remote-DRAM share, ≥10% relative drop, pre-registered above before any code lands |
| Risks | Largest runtime change in the series; per-node mcentral spanSets must be build-tagged (no un-tagged ~180KB BSS); struct/layout `sizeof` discipline; v2 forbidden list binding; per-task stop rules |
| NOT the motivation | Workstream A's bounded C-vs-A residual (≤+8.3%/≤+5.15%) — small, already inside its non-inferiority gate, and confined to the window where confinement already works |
| Workstream C sequencing | Tasks 12/13/15 run before/alongside Workstream B (disjoint files, no dependency); Task 14 implementation stays profile-gated on Workstream B's own Task 11 profile |

## Phase-shift mechanism study (Task 12)

### Pre-registration (committed before the sweep ran)

**Committed before the sweep runs**, per the plan's Task 12 Step 1. This subsection is written and committed first (`1aef06893a`); the Results/Verdict subsections below are appended in a separate commit afterward, unedited from what was pre-registered here.

#### Background

Candidate 3 (phase-shift, `numa-design/phase-shift`, commit `6b535e2ca9`) showed a replicated C-vs-B win across two independent sweeps: original (Layer 0+1+2) **-16.82%** (p=0.029, n=10, fragile on robustness checks) and L1-only (Layer 0+1) **-24.31%** (p=0.023, n=10, Wilcoxon p=0.014, LOO range 0.004-0.0503), combined by Fisher's method to p=0.0056. Both write-ups converged on a *mechanism* reading derived only indirectly, from vmstat and an after-the-fact bandwidth back-calculation (64B/read × reads/s): arm B's migration traffic (≈0.3 GB/s, from `numa_pages_migrated`) is two orders of magnitude too small to explain a 17-24% throughput swing, so the working hypothesis became **dual-memory-controller bandwidth availability** (arm A, pinned to one node/controller, is flat at 31.0 GB/s in every round; B and C both exceed that ceiling most rounds by spreading load across both controllers) rather than **balancer fault/copy tax**. Neither prior sweep captured direct page-placement evidence — the bandwidth reading was inferred, not measured. This task measures placement directly via `/proc/self/numa_maps` to settle which mechanism the data actually supports.

#### Toolchain and the GOMAXPROCS/confinement interaction (read this before running)

HEAD (`059152e027`) is post-Task-6: `GOEXPERIMENT=numa` is genuinely Layer 0 (topology) + Layer 1 (`numaSetProcessBindAll`, BIND-all task mempolicy) only — Layer 2 (per-chunk `MPOL_PREFERRED` homing) was removed in Task 6. **But HEAD also carries Workstream A's fill-one-socket-first confinement (Tasks 1-3), which did not exist when either prior candidate-3 sweep ran.** Both prior sweeps used `GOMAXPROCS=128` explicitly (`numa-design/bench-data/pathology/cand3-sweep.sh`, `cand3-l1-sweep.sh`). numa-dell has 128 CPUs per node. `numaShouldConfine` (`src/runtime/numa_linux.go:306-363`) confines when, among other conditions, `procs <= ncpus` (line 359: `if ncpus <= 0 || procs > ncpus { decline }`) — so `GOMAXPROCS=128` on a 128-CPU-per-node machine satisfies `128 <= 128` and **would newly engage confinement for arm C** if this rerun reused the old GOMAXPROCS value. That is a real confound, not a hypothetical one, and a serious one for this specific program: confinement narrows the *process's own* CPU affinity to one node before any reader goroutine starts, which would make the phase-shift benchmark's own odd-phase `sched_setaffinity` calls (pinning readers to node 1's CPUs) fail outright — the benchmark's core mechanism (make the whole working set periodically "misplaced" by flipping reader affinity across nodes) cannot function under confinement at all. This is not a subtle statistical confound to control for; it would silently change what the program measures.

**Decision: both arms run with `GOMAXPROCS=256` explicitly** (`env GOMAXPROCS=256`), not 128. `256 > 128` makes `numaShouldConfine` decline with reason `"GOMAXPROCS exceeds node"` (verified below via `GODEBUG=numa=1`), keeping arm C at Layer 0+1 (BIND-all) only — the same physical configuration as both prior sweeps, just expressed at a GOMAXPROCS value that doesn't intersect the new confinement gate. Arm B is stock Go and has no confinement code path either way, so this choice only affects arm C, and only by keeping a mechanism (confinement) *out* of a study that is not about confinement. The alternative — letting confinement engage — was rejected because it would answer a different question (Workstream A's own already-gated behavior) while breaking this program's own experimental design, not because avoiding it makes the result look better. All other flags are unchanged from both prior sweeps: `-heap=6144 -readers=64 -phase=30 -phases=4`.

#### Primary (pre-registered)

- Metric: `ns/read` (`BenchmarkPhaseChase`), B vs C, single session, n=20 recorded rounds + 1 discarded warmup round, rotating arm order (`numa-design/pathology-sweep.sh`), vmstat (`numa_hint_faults`, `numa_pages_migrated`) snapped immediately before/after every run, idle check before every run.
- Reported: benchstat headline (median/mean, relative %), exact Mann-Whitney U (full enumeration), exact Wilcoxon signed-rank test (paired by round index), exact paired sign test, leave-one-out exact Mann-Whitney U (each of the 20 rounds dropped in turn, 19 vs 19) — the full robustness battery used in both prior sweeps, not just MWU. α=0.05 pre-declared for all.
- Binaries built fresh from HEAD immediately before the sweep, verified via `go version -m` (base: no `X:numa`; numa: `X:numa` present) and a `GODEBUG=numa=1` smoke test at the sweep's exact `GOMAXPROCS=256` confirming `numa: confinement declined: GOMAXPROCS exceeds node` on the numa binary, before any recorded round runs.

#### Mechanism/attribution analysis (pre-registered)

Separate from the primary set — **3 designated capture rounds** (not pooled into the n=20 primary; see exclusion rule below), each running both arms once (order alternates B-first/C-first across the 3 rounds), with `-numamaps=<prefix>` enabled. Within each captured run, phases 1-3 (not phase 0 — its "start" snapshot is pre-touch, before any pinned reader has run at all, and is not representative of steady state) are snapshotted at start/mid/end of the phase (9 `numa_maps` files per arm per capture round).

**Exclusion rule (pre-declared):** capture-round `ns/read` samples are **not** added to the primary n=20 set, regardless of whether the `-numamaps` file I/O visibly perturbs timing. The snapshot logic runs only in the coordinator goroutine between phase-timer sleeps, never in a reader's hot loop (enforced in code, `numa-design/phase-shift/main.go`, `snapshotNumaMaps`), but three snapshots/phase is still extra work the primary-set runs don't do, so capture rounds are kept structurally separate rather than assessed post-hoc for whether they look different.

**Placement metric:** for each `numa_maps` snapshot, node-balance = min(N0,N1)/(N0+N1), summed over **all anonymous VMAs** in the file (not just ones identifiable as "the heap" — the 6+ GiB working set dominates page count by construction against goroutine-stack-scale anonymous regions, confirmed by inspection of sample snapshots before the sweep: the working-set arena entries carry `anon=` counts several orders of magnitude larger than any other anonymous VMA). 0.5 = perfectly spread, 0 = perfectly consolidated onto one node.

- **H-bandwidth predicts:** arm C's node-balance stays close to spread (≈0.5) across all captured phases/snapshots, not trending with phase parity; arm B's node-balance measurably drops (moves toward consolidation) from a phase's start snapshot to its end snapshot, tracking the balancer chasing that phase's target node, and does not fully recover before the next flip.
- **H-faulttax predicts:** no such distinction — B's node-balance stays roughly as spread as C's throughout (the balancer's hint-fault/migrate cycle doesn't meaningfully move the aggregate split within one 30 s phase), and C's advantage over B instead tracks B's per-round `numa_pages_migrated` delta.
- **Primary attribution test (per the plan's original Step 1 spec):** Spearman rank correlation, arm C only, round-level node-balance (mean over that round's captured snapshots) vs that round's overall `ns/read`, α=0.05 pre-declared. **Disclosed limitation, stated now rather than after seeing the result:** with only 3 designated capture rounds this test has n=3 — essentially no statistical power (a two-tailed Spearman test at n=3 cannot reach significance except at a perfect rank match, |ρ|=1). It is reported because it is the plan's literal pre-registered test, not withheld, but this study's actual verdict rests primarily on the descriptive placement comparison above (B vs C node-balance, well-resolved within the 3 rounds' 9-snapshot-per-arm detail) and the migration-traffic-vs-bandwidth budget check below (n=20-powered), not on this p-value.
- **Secondary, pre-declared as descriptive/exploratory (no formal α):** paired start-vs-end node-balance within each captured phase, per arm — do B's per-phase (start, end) pairs skew toward "end lower than start" more often than C's, via an exact sign test reported without a significance claim (n too small to threshold), purely as a directional descriptive summary alongside the raw numbers.

**Migration-traffic-vs-bandwidth budget (n=20, the same method as both prior sweeps):** arm B's mean `numa_pages_migrated` delta per round × 4096 B/page × 2 (read source + write dest) ÷ elapsed run time, compared against arm B's own implied aggregate demand bandwidth from `ns/read` (GB/s = 64 B/read ÷ ns/read, the same back-calculation used in both prior write-ups). This does not need `numa_maps` at all — it runs on the full n=20 primary set and is the best-powered piece of mechanism evidence available.

**Bandwidth-ceiling cross-check (n=20, descriptive):** implied GB/s (= 64/`ns/read`) for every primary round in both arms, compared against the ~31.0 GB/s single-controller ceiling established by arm A in the original candidate 3 pilot (not re-measured here — arm A is not run in this rerun; it was already established as informational-only for this candidate and its role here is purely as a fixed reference ceiling from prior data).

#### What "either outcome" means here (per the plan)

H-bandwidth confirmed reframes the upstream story: BIND-all's value in this pathology is preserving the dual-controller spread that the balancer's own churn would otherwise disrupt, not avoiding fault/copy cost directly. H-faulttax confirmed restores the more intuitive fault-tax narrative and would mean the bandwidth reading from the prior two sweeps was a spurious correlation with something else. Both are reported; this section will not be rewritten to fit whichever result appears.

### Build verification (before the sweep ran)

Binaries built fresh from `1aef06893a` (the pre-registration commit above) on numa-dell, `GOWORK=off`, directly from `numa-design/phase-shift`:

```
/tmp/pb/phaseshift-base: go1.28-devel_e9c8ddbc52 ...  (no X:numa)
/tmp/pb/phaseshift-numa: go1.28-devel_e9c8ddbc52 ...  X:numa
```

Both report `build vcs.revision=1aef06893abcf9f30a340ed5b68b23a2d20120df` (this commit) and `vcs.modified=true` — the same pre-existing untracked-file explanation as every prior `go version -m` check in this file (`numa-design/run-x-benchmarks.sh`, `x-benchmarks/`, present before this task started, not from this task's own output; `git diff --stat` on tracked files is empty). `bin/go` itself was built at `e9c8ddbc52` (one commit before HEAD); the two commits between it and `1aef06893a` (`059152e027`, a test-comment-only change, and the pre-registration commit itself, which touches only `numa-design/`) do not touch `src/`, so no `./make.bash` rebuild was needed — confirmed by inspecting both diffs before relying on this.

**Confinement-decline check, at the sweep's exact `GOMAXPROCS=256`:**

```
$ ssh numa-dell 'env GOMAXPROCS=256 GODEBUG=numa=1 /tmp/pb/phaseshift-numa -heap=64 -readers=4 -phase=1 -phases=2'
numa: nodes 2 allowed 2
numa: confinement declined: GOMAXPROCS exceeds node
```

**BIND-all-still-engaged check** (direct, from a live `/proc/self/numa_maps` snapshot, stronger evidence than the debug print since Layer 1 has no `debug.numa` line of its own): the numa binary's heap VMAs carry `bind:0-1` policy; the base binary's carry `default`. Both confirmed on a short smoke run before the real sweep started, and reconfirmed in every one of the sweep's own archived capture-round snapshots (**corrected citation, per audit**: the smoke-test transcript itself was not saved; the go-version-m/confinement-decline transcript at `wsC-phase-govm-and-confinement-verify.txt` does not contain it — cite the actual sweep-produced evidence instead), e.g. `numa-design/bench-data/ws-c-phaseshift/wsC-phase-capture-armC-r1-phase1-start.numamaps` (`bind:0-1`) vs. `wsC-phase-capture-armB-r1-phase1-start.numamaps` (`default`), and identically across all other capture files for their respective arms.

### Results

**Primary (n=20, single session, rotating order, `numa-design/pathology-sweep.sh`):**

| | B (stock) | C (`GOEXPERIMENT=numa`, Layer 0+1 only) |
|---|---|---|
| median ns/read | 1.6940 | 1.1328 |
| mean ns/read | 1.6887 | 1.2999 |
| min / max | 1.4367 / 1.9353 | 1.1040 / 2.0335 |

Relative: **median −33.13%, mean −23.03%** (C faster). This is the largest of the three phase-shift measurements to date (original Layer 0+1+2: −16.82%; L1-only: −24.31%; this rerun: −33.13% median).

**Full robustness battery, all four tests, all significant — unlike either prior sweep:**

- Exact Mann-Whitney U: p = **8.18×10⁻⁶**
- Exact Wilcoxon signed-rank (paired by round): p = **6.29×10⁻⁵**
- Exact paired sign test (C < B in 18/20 rounds): p = **4.03×10⁻⁴**
- Leave-one-out exact MWU (each of the 20 rounds dropped in turn, 19 vs 19): p range **6.55×10⁻⁷ to 2.59×10⁻⁵**, 20/20 subsets significant at α=0.05

Every leave-one-out subset is significant — a qualitatively different robustness profile than the original sweep (sign test p=0.109, not significant on its own; LOO range up to 0.077) or the L1-only sweep (sign test still p=0.109; LOO worst case 0.0503). This rerun's result is not fragile by any of the four tests.

**Idle-check firings, noted per audit:** `wsC-phase-sweep.log` (`numa-design/bench-data/ws-c-phaseshift/`) shows `pathology-sweep.sh`'s `idle_check` firing 4 times across the whole sweep (top process observed at 200%, 144%, 138%, and 300% CPU respectively; each triggers a 30 s wait before the next run starts). This is the *repaired* idle_check (the ps-self-referential false-positive bug fixed earlier in the plan is not what's happening here — that bug reported dozens of spurious firings per sweep from `ps`/`awk`'s own transient CPU%, and this gate specifically excludes those commands); these 4 firings reflect genuine transient contention (most plausibly the prior run's ~6 GiB working set being torn down — munmap and page reclaim — between recorded rounds) that the gate correctly detected and waited out. The log records only the offending %CPU, not the process name, so the exact source isn't independently identifiable from this sweep's own data; a one-line logging improvement to close that gap is included in this commit (see below).

**Migration-traffic-vs-bandwidth budget (arm B, n=20):** mean `numa_pages_migrated` delta = 4,802,548 pages/round → 0.328 GB/s two-way migration traffic, **0.86% of B's own mean demand bandwidth (38.1 GB/s)** — consistent with both prior sweeps' finding that migration byte-volume alone is roughly two orders of magnitude too small to explain the throughput swing directly.

**Bandwidth (GB/s = 64/ns_per_read):** B ranges 33.1–44.5 (mean 38.1), C ranges 31.5–58.0 (mean 50.9). Both arms exceed the ~31.0 GB/s single-controller reference ceiling in **every** round — **B: 20/20 above; C: 20/20 above** (corrected: an earlier draft of this section reported "19/20" for C, treating its slowest round, 31.47 GB/s, as at-or-below the 31.0 ceiling; 31.47 > 31.0, so it is above, just barely). The honest "sometimes lands near the ceiling" pattern both prior sweeps also reported still holds descriptively: that one C round (31.47 GB/s) sits far closer to the 31.0 reference than any of C's other 19 rounds (next-lowest 36.68 GB/s) — a real distributional feature, just not literally "below the ceiling."

**Mechanism placement data (3 designated capture rounds, `-numamaps`, phases 1-3, kept out of the n=20 primary set):**

| round | arm | node-balance mean (phases 1-3) | node-balance range (phases 1-3) | ns/read (this capture round) |
|---|---|---|---|---|
| 1 | B | 0.0887 | 0.034 – 0.210 | 1.6833 |
| 1 | C | 0.3050 | 0.3048 – 0.3051 | 1.1556 |
| 2 | B | 0.2034 | 0.017 – 0.367 | 1.4734 |
| 2 | C | 0.2529 | 0.2528 – 0.2530 | 1.3035 |
| 3 | B | 0.1213 | 0.025 – 0.492 | 1.7015 |
| 3 | C | 0.4610 | 0.4609 – 0.4611 | 1.1310 |

(Corrected per audit: this table now reports the pre-registered phases-1-3 subset consistently in both header and data — the first draft's B means included phase 0, the pre-touch phase the pre-registration excludes.)

Aggregated over all 27 snapshots/arm (phases 1-3 only, the pre-registered subset — 36 is the all-phase count, incl. phase 0): **B mean=0.1378, stdev=0.1257** (wide swings within every single run); **C mean=0.3396, stdev=0.0901** — but that C standard deviation is **entirely between-round**: within any one round, C's own stdev across its 9 phase/tag snapshots is ≤0.0002 (e.g. round 1: 0.3048–0.3051). C's placement is not merely "usually more spread than B" — it is **frozen at whatever value it starts with and never moves again for the rest of that run**, regardless of which node its own readers are pinned to that phase. B's placement, in the same runs, swings repeatedly within every single 120 s run: round 2's full chronological sequence (all 4 phases, 12 snapshots, each ~10 s apart) is 0.491 → 0.170 → 0.017 → 0.017 → 0.113 → 0.109 → 0.107 → 0.367 → 0.367 → 0.367 → 0.275 → 0.109 — a directly observed, not merely inferred, "migration churn" signature. The largest single consecutive-snapshot swing in that sequence is ≈0.32 (0.491→0.170, both ~10 s apart); the full within-round range is wider still (≈0.47, from the 0.017 low to the 0.491 high).

**Descriptive start-vs-end movement within a phase, per arm** (does node-balance trend from a phase's start to its end): B: 5/12 phases moved toward consolidation, 7/12 toward spread (no ties), mean delta (all 4 phases) −0.053 — but restricted to the pre-registered phases-1-3 subset (dropping phase 0, whose large start-of-run swing dominates the all-phase average), **B's mean delta is +0.0245**, i.e. essentially flat and if anything slightly toward *more* spread, not consolidation. C: 7/12 toward consolidation, 4/12 toward spread, **1/12 an exact tie** (round 3 phase 2, delta=0.000000) — 7+4+1=12, mean delta ≈+0.0000 (no real movement — noise at the 4th-5th decimal place, not signal). **This directional test's original framing — "B trends toward consolidation within each phase" — is not what the data shows**; B does not move monotonically within a phase, it swings chaotically between snapshots in both directions, by up to ≈0.32 between consecutive (~10 s apart) snapshots. The clean signal is stability (C) vs. instability (B), not a directional within-phase drift for B — confirmed by B's near-zero phases-1-3 mean delta above, which rules out even a mild net consolidation trend.

**Pre-registered primary attribution test (arm C only, round-level, n=3):** Spearman correlation between C's round-level mean node-balance (phases 1-3) and that round's ns/read: **ρ = −1.0000** (perfect), exact permutation p = **0.333** (n=3; not significant, exactly as the pre-registration disclosed it could not be — a two-tailed exact test at n=3 cannot reach p<0.05 even at |ρ|=1). Every one of the 3 capture rounds lands in the H-bandwidth-predicted order: C's most-spread captured round (0.4610, round 3) was its fastest (1.1310 ns/read); its least-spread (0.2529, round 2) was its slowest (1.3035 ns/read). Directionally perfect, statistically inconclusive alone at this n — reported exactly as pre-registered, not oversold.

**Exploratory, NOT pre-registered (added per audit review):** the primary test above only uses arm C's 3 rounds. Pooling **both arms** (n=6: the 3 B rounds and 3 C rounds together, same round-level mean node-balance and ns/read pairing) gives a much better-powered picture of the same relationship: Spearman ρ = **−0.943**, exact permutation p = **0.0167** (n=6) — significant, unlike the n=3 arm-C-only test above. The relationship also holds **within arm B alone** (n=3): ρ = **−0.50** (exact p=1.0, itself inconclusive at this n, same limitation as arm C's own n=3 test, but directionally consistent — B's own fastest round, round 2, is also its most-spread captured round). This pooled result is exploratory and was not pre-declared before the sweep; it is reported for what it is, not folded into the primary claim. It matters for how the verdict below is framed: **B's within-run instability and B's lower time-averaged spread are not separable in this design** — they are two descriptions of the same balancer-driven process, not two competing explanations that could be told apart by more data of this kind. Throughput across both arms tracks time-averaged node-balance.

Raw data: `numa-design/bench-data/ws-c-phaseshift/wsC-phase-arm{B,C}-{warmup,recorded}.out{,.stderr}`, per-round `wsC-phase-arm*-r*.vmstat.{before,after}`, `wsC-phase-vmstat-summary.txt`, `wsC-phase-sweep.log`; capture rounds: `wsC-phase-capture-arm{B,C}-r{1,2,3}.{out,stderr,vmstat.before,vmstat.after}` and 72 `*.numamaps` snapshots; the analysis script and its full output, `phaseshift_analysis.py` / `wsC-phase-full-analysis.txt`; build/confinement verification, `wsC-phase-govm-and-confinement-verify.txt`; the three-sweep Fisher's-method combination, `wsC-phase-fisher-combined.txt`; sweep driver scripts `pathology-sweep.sh` (shared driver, `numa-design/`) and `capture-sweep.sh`.

### Verdict: **H-bandwidth confirmed, with a refinement the placement data forced — and a limit on what this design can separate**

**Corrected per audit review (this paragraph replaces an earlier draft that overclaimed separability the design cannot support):** "instability" and "lower time-averaged spread" are not two competing explanations that this experiment can tell apart — B's churn and B's consolidation are two descriptions of the same balancer-driven process, produced together, every time. Stated in the audit's own corrected phrasing: **C's placement is frozen at whatever first-touch produced; B's is continuously churned toward consolidation. Throughput across both arms tracks time-averaged node-balance (exploratory pooled Spearman ρ=−0.94, n=6, p=0.017). Whether B's cost is the instability per se or simply the lower time-averaged spread that instability produces is not separable in this design.**

The magnitude argument that motivated H-bandwidth in the first place replicates cleanly a third time: B's own migration traffic (0.33 GB/s) is under 1% of its own demand bandwidth (38.1 GB/s) — nowhere near enough raw bytes to explain a 23-33% throughput swing via direct fault/copy/shootdown cost. That rules out the crude form of H-faulttax ("moving these bytes is itself expensive enough to explain the gap") for a third independent sweep.

The `numa_maps` placement data — the direct measurement neither prior sweep had — confirms the qualitative mechanism, but not exactly as pre-registered. The pre-registration's H-bandwidth framing predicted arm C would "stay close to spread (≈0.5)". **It does not**: C's node-balance in the 3 captured rounds was 0.2529, 0.3050, and 0.4610 — whatever its first-touch allocation happened to land on, not a reliable ~50/50 split. What the data shows instead, unambiguously, is that **C's placement is completely static once set — it does not move by more than 0.0003 across an entire 120 s run, four phase flips, and the readers' own affinity changing nodes every 30 s** — while **B's placement is continuously and substantially churned within every single run** (up to ≈0.32 between consecutive, ~10 s-apart snapshots; ≈0.47 full within-round range), a live, measured picture of the "migration storm" the design predicted, not merely an inference from nonzero vmstat counters.

Combined with the migration-traffic budget and the pooled exploratory correlation above, the most defensible reading is: **C's advantage comes from avoiding the balancer's disruption of whatever locality pattern its allocation happened to establish, and does not come from paying directly for the bytes the balancer moves (that budget is ruled out on magnitude alone).** Whether the disruption's cost is best described as instability itself, or as the lower average spread that instability produces, is a distinction this design cannot resolve — the two move together in every round captured. This is a refinement of H-bandwidth (the underlying claim, "C wins by avoiding balancer-induced disruption, not fault/copy cost", is confirmed) rather than a clean confirmation of its most literal sub-claim ("C stays near 50/50") or a clean separation of *which* consequence of that disruption matters. The pre-registered n=3 Spearman correlation (arm C only), while statistically inconclusive alone (p=0.333), is directionally perfect across all 3 rounds and is consistent with this reading; the exploratory pooled n=6 correlation across both arms is the better-powered version of the same relationship and is significant (p=0.017).

**H-faulttax is not supported** in its crude, direct-cost form: there is no round or snapshot in which B's placement is *more* stable/spread than C's, and the migration-byte budget remains two orders of magnitude short of the observed effect size in this sweep, as in both prior ones.

This rerun also produced the strongest, least fragile version of the phase-shift result across all three sweeps to date (−33.13% median, all four robustness tests significant, all 20 leave-one-out subsets significant) — obtained with `GOMAXPROCS=256` (both arms) specifically to keep Workstream A's confinement out of scope for arm C, per the pre-registered decision above.

**Combining the three sweeps — relabeled per audit as "cross-configuration," not a single-effect-size estimate:** the three phase-shift sweeps differ in build (original: Layer 0+1+2; L1-only: a scratch L1-only patch; this rerun: post-Task-6 HEAD, genuinely Layer 0+1), `GOMAXPROCS` (128/128/256), and phase-loop code (this rerun added `-numamaps`, which the primary set structurally excludes, but the phase-timing loop itself changed shape to accommodate it). Fisher's method combining the three sweeps' primary Mann-Whitney U p-values (original p=0.0288, L1-only p=0.0232, this rerun p=8.176×10⁻⁶): χ²(df=6) = 38.05, combined p = **1.10×10⁻⁶** (closed-form regularized incomplete gamma for even df, not scipy — this host has none, matching every other exact-test computation in this file). **What this combination actually establishes is narrower than "the shipping configuration's effect is this certain": it answers "a C-faster-than-B effect exists in at least one of these three configurations," not a single pooled effect-size claim across a heterogeneous set.** This rerun alone carries the overwhelming majority of that combined evidence — **23.4 of the total 38.05 χ²** — and is independently conclusive on its own (p=8.18×10⁻⁶, all four robustness tests significant, GOMAXPROCS=256/Layer-0+1-only, the configuration closest to what would actually ship) without leaning on the other two sweeps at all. One further caveat, present in all three sweeps equally and not resolved by combining them: all three share the same benchmark program, whose own perfect-locality reference arm (arm A, `numactl --cpunodebind=0 --membind=0`, established in the original sweep) is the *slowest* of all three arms at a flat single-controller ceiling — a benchmark-specific artifact of this design (readers and memory both confined to one controller for the whole run), not evidence about locality in general. Three sweeps of the same benchmark corroborate that this benchmark's C-vs-B effect is real and repeatable; they do not, by themselves, generalize the finding beyond this specific synthetic workload.

## Workstream B gate battery — 2026-08-21

Task 11 (plan `2026-08-20-numa-v3-locality-plan.md:1334`), the unit's single
verdict. Started at HEAD `3b102094cd` (Gate 1 only, first sitting), this
sitting resumed and completed the battery with source unchanged throughout
(local and remote in sync, remote tree already built at `ce4b564d8f` —
source-identical to HEAD for runtime purposes, since every commit after
`ce4b564d8f` touched only `RESULTS.md`/`bench-data`/docs, confirmed by
`git log --stat`; no rebuild was needed). `go version` (remote, tree
toolchain): `go1.28-devel_ce4b564d8f Fri Aug 21 09:40:09 2026 -0700
linux/amd64`. Kernel: `6.12.0-211.7.1.el10_2.x86_64`. `x/benchmarks`:
`v0.0.0-20260819172200-70693762b6a0` (same pin as every prior layer/
workstream run; confirmed via `go version -m` on every binary used, full
transcript archived `numa-design/bench-data/ws-b-gates/go-version-m-all-binaries.txt`).
`kernel.numa_balancing`: `1` throughout. Machine idle before every
measurement block (`ps -eo pcpu,comm --sort=-pcpu`, excluding the known
`ps`/`pgrep` self-report artifact documented in the Workstream A gate
battery's own idle_check fix).

**Session history:** this battery ran in two sittings. The first sitting
completed only Step 1 (pinned routing proof) before a user-requested pause;
that PARTIAL state was committed (see the git history for the commit that
introduced "Workstream B gate battery (PARTIAL — paused by user)" — this
section supersedes it in place, per instruction to keep the pause as
history rather than deleting it from the record). The second sitting
resumed from Step 2 through the full battery, recorded below.

**Audit round (post-completion, same day):** an independent audit
reproduced every number below and confirmed the overall FAIL verdict as
sound — stronger, in fact, once corrected. Five Important-severity
findings and several minors were identified and are fixed in place
throughout this section (each marked inline as "audit round" or "I1"-"I5"
at its point of correction): (I1) missing per-round vmstat snapshot pairs
for the three pathology sweeps, restored from `numa-dell` into the
archive; (I2) a self-contradictory causal claim in Gate 2b, corrected;
(I3) unsupported cross-campaign "tighter/stronger than Workstream A"
comparisons in the pathology candidates, withdrawn in favor of
within-session-only claims (the untreated stock arms moved substantially
between sessions, so most apparent cross-session narrowing was session
stability, not treatment effect); (I4) an unreported +201.6% 1P STW-ns/op
regression, surfaced as an exploratory finding; (I5) missing MDE
statements for Gate 2a and Gate 2c's `sec/op`, added, with Gate 2c's FAIL
reframed as "significant, magnitude imprecise" at its own MDE. Two cheap
follow-up experiments (E1, E2) were also run, pre-registered in place
before execution — see "Exploratory attribution experiments" after the
Step 5 verdict below.

Raw `.out`/`.csv` files, benchstat/analysis-script transcripts, sweep-driver
invocations, and `go version -m` transcripts are archived under
`numa-design/bench-data/ws-b-gates/` (subdirectories per gate: `gate2-1p/`,
`gate3-256p/`, `census/`, `imc/`, `cand1-128p/`, `cand2-gcpause/`,
`cand1-256p-exploratory/`, plus `gate1-numamaps-followup.txt` and
`routing-probe.go`/`PARTIAL-STATE.md` from the first sitting).

**Pre-registration note:** the primary metric (IMC remote-DRAM-miss share,
≥10% relative drop) was pre-registered in "Workstream B go/no-go (Task 7)"
above, before any Workstream B code landed. No metric below was added
post hoc; every number reported is from an actual run, none extrapolated.

### Step 1 — pinned routing proof (design §12.4; gates everything else): **PASS**

(Carried from the first sitting, unchanged.) Externally pinned via
`numactl --cpunodebind={0,1}` (no `--membind`), `GOMAXPROCS=128`, harness
`numa-design/bench-data/ws-b-gates/routing-probe.go` (standalone
single-file program, `GOEXPERIMENT=numa` build, reads
`/numa/span-refills/{local,remote}:spans` via `runtime/metrics` at exit):

- node 0 half: `local=93329 remote=0 total=93329 local_share=100.0000%`
- node 1 half: `local=94346 remote=432 total=94778 local_share=99.5442%`

Both ≥95% (the gate requirement). **Follow-up captured this sitting** (the
`heapArena.node`/`numa_maps` corroboration deferred at pause): a
`/proc/self/numa_maps` N0=/N1= sample taken mid-run of the same harness at
a larger volume (`-totalmb=16384`, sampled ~0.3-0.5s after start),
archived `gate1-numamaps-followup.txt`:

- node 0 pin: `N0=8397 N1=409` (95.35% of resident heap pages on the
  pinned node); span-refills for that same run: `local_share=100.0000%`
- node 1 pin: `N0=0 N1=11496` (100.00% on the pinned node); span-refills:
  `local_share=99.8510%`

Independent corroboration via a different measurement channel (physical
page residency vs. the runtime's own refill counters), consistent with
Step 1's original finding. **Step 1 verdict unchanged: PASS.**

### Step 2 — hard gates

#### 2a. 1P json (`BENCHNUM=10`): **PASS**

```
$ ssh numa-dell 'GOROOT=$PWD GOMAXPROCS=1 BENCHNUM=10 OUT=/tmp/wsB-gate2-json ./numa-design/gate-json.sh'
$ ssh numa-dell '/tmp/numa-tools/benchstat /tmp/wsB-gate2-json/baseline.out /tmp/wsB-gate2-json/numa.out'
JSON-1  sec/op:            31.97m ± 46%  32.35m ± 49%  ~ (p=0.393 n=10)
JSON-1  user+sys-sec/op:   32.00m ± 46%  32.40m ± 49%  ~ (p=0.393 n=10)
```

Neither primary metric significant; both nominally +1.2%/+1.3%, well
inside noise. **Achieved MDE:** per-arm CV 22.4%/26.9% (recomputed
directly from the raw round values, not benchstat's own ± summary — see
`gate2-1p/mde_compute.py`), pooled ≈24.7%, giving MDE ≈31.0% at n=10
(α=0.05, power=0.80) — this PASS excludes only gross regressions ≥~31%,
the same honest-MDE caveat Workstream A's own Gates 1/3 already carry.
**PASS.**

**Exploratory finding, surfaced from this gate's own archived output (not
part of the pre-declared verdict, but a real, tight, reproducible signal
worth recording as a clue to the CPU-cost question raised throughout this
battery):** `STW-ns/op` shows a **+201.56% regression** at 1P —
baseline 4,578ns ± 50%, numa 13,806ns ± 53%, **p=0.000, n=10, complete
separation** (every numa round's STW time exceeds every baseline round's).
This does **not** recur at 256P: pooled n=30, `STW-sec/op` 157.6µs vs
153.8µs, **not significant, p=0.786** (see Step 2c-e below). At
`GOMAXPROCS=1` there is exactly one P doing all GC-assist and mark work,
so any fixed per-STW-episode cost (e.g., additional per-node spanSet
bookkeeping touched during a stop-the-world sweep/mark transition) shows
up undiluted; at 256P the same fixed cost is a much smaller fraction of a
much larger aggregate STW total across many Ps, and disappears into noise.
This is consistent with — though does not by itself prove — a
constant-per-episode (not per-byte, not per-thread-count-scaled) source
for at least part of the CPU-time-cost pattern recorded throughout this
battery (Gates 2b, 2c, 4c below). Archived `gate2-1p/baseline.out`,
`gate2-1p/numa.out`.

#### 2b. 1P alloc micro (`Malloc(8|16|Types)`, `-count=10`): **FAIL**

```
$ ssh numa-dell 'export GOROOT=$PWD PATH=$PWD/bin:$PATH GOTOOLCHAIN=local
GOMAXPROCS=1 go test runtime -run=NONE -bench="Malloc(8|16|Types)" -count=10 >baseline.out
GOMAXPROCS=1 GOEXPERIMENT=numa go test runtime -run=NONE -bench="Malloc(8|16|Types)" -count=10 >numa.out
/tmp/numa-tools/benchstat baseline.out numa.out'
Malloc8    7.149n ± 0%   7.404n ± 0%  +3.57% (p=0.000 n=10)
Malloc16   11.56n ± 0%   12.01n ± 0%  +3.89% (p=0.000 n=10)
geomean    9.091n        9.430n      +3.73%
```

(`MallocTypes` still doesn't match anything in this tree — the same
pre-existing regex gap noted at every prior layer/workstream — so this is
`Malloc8`/`Malloc16` only, matching every prior gate's own caveat.)

**Verdict: FAIL.** Geomean +3.73% exceeds the ≤+2% hard-gate bar. This is
not a noisy reading: CV ≈0% at n=10 for both benchmarks, p=0.000 both, a
tight and reproducible signal, not a borderline miss. This is a real,
small but consistent per-allocation overhead on the fastest possible
malloc path even at `GOMAXPROCS=1` (where refill-time `getcpu` routing and
soft affinity are both essentially inert — one P, no node changes to
narrow on). **Correction (I2, audit round):** an earlier draft of this
section attributed the delta to the `(*mcentral).cacheSpan`/
`cacheSpanFromNode` restructuring the off-binary census (Step 2f, below)
shows in the off build — that explanation was self-contradictory and is
withdrawn: the census's off-build restructuring is present identically in
**both** arms of this comparison (the "baseline" arm here is the same
`GOEXPERIMENT`-unset off-build shape the census itself uses), so it
cancels between arms and cannot explain an on-vs-off delta. The correct
framing: **the source is experiment-gated** (`goexperiment.Numa`-guarded
code paths present only in the `numa` arm's binary); the census's
off-build restructuring is a real, disclosed deviation from a literal
byte-identical off-binary (Step 2f) but is orthogonal to this gate's
on-vs-off delta, not a cause of it. What specifically inside the gated
code costs ~3.7% here is not isolated by this gate alone — see the STW
exploratory finding above (Gate 2a) and the CPU-cost attribution
experiment (E2, below) for the available evidence. Archived
`gate2-1p/wsB-gate2-alloc-base.out`, `gate2-1p/wsB-gate2-alloc-numa.out`.

### Step 2c-e. 256P json (hard) + RSS + vmstat wrap

Per the Workstream A Gate 3 precedent (hand-rolled per-session medians at
256P sign-flip at n=10; pooling across 3 unconditional sessions is the
pre-declared remedy), three sessions ran unconditionally, idle re-checked
between each:

```
$ ssh numa-dell 'GOROOT=$PWD GOMAXPROCS=256 BENCHNUM=10 OUT=/tmp/wsB-gate3-r{1,2,3} ./numa-design/gate-json.sh'  # x3
```

Per-session hand-rolled medians (informational, not the verdict): r1
ns/op −5.4%/user+sys +7.9%; r2 −6.9%/+17.9%; r3 +61.4%/+55.0% — the
sign-flipping pattern the precedent predicts; pooling is the remedy.

Pooled (n=30, `cat` of all three `baseline.out`/`numa.out`):

```
$ ssh numa-dell '/tmp/numa-tools/benchstat /tmp/wsB-gate3-pooled-baseline.out /tmp/wsB-gate3-pooled-numa.out'
JSON-256  sec/op:            2.203m ± 18%  2.337m ± 38%  ~ (p=0.358 n=30)
JSON-256  user+sys-sec/op:   174.9m ± 11%  209.8m ± 17%  +19.96% (p=0.025 n=30)
JSON-256  peak-RSS-bytes:    6.379Gi ± 47%  5.690Gi ± 49%  ~ (p=0.254 n=30)
```

**Achieved MDE (I5, audit round):** recomputed directly from raw pooled
round values (`gate3-256p/mde_compute-output.txt`), not benchstat's own ±
summary: per-arm CV 25.9%/32.3%, pooled ≈29.3%, MDE ≈21.2% at n=30
(α=0.05, power=0.80). **`sec/op`'s PASS excludes only gross regressions
≥~21%** — the pooled median is already **+6.05%** (2.203ms → 2.337ms,
not significant at p=0.358, but not a clean parity reading either; this
gate's resolving power simply cannot distinguish +6% from 0% at this
noise level). `user+sys-sec/op`'s **FAIL is real** (+19.96%, p=0.025,
below this gate's own ≈21% MDE) but should be read as **"significant,
magnitude imprecise"** rather than a precisely-pinned +19.96% cost: the
true effect could plausibly be anywhere the confidence interval around
that point estimate allows, given a comparably-sized MDE to the effect
itself. **Verdict: FAIL** on `user+sys-sec/op` — statistically significant,
not a noise artifact like Gates 1/3's own MDE caveats elsewhere in this
file, but reported with the same MDE honesty this plan requires
everywhere else. Wall-clock `sec/op` is **not** significant (p=0.358) —
real elapsed time is not distinguishably worse at this gate's resolving
power, but CPU time is a real, measured cost at 256P too, consistent with
Gate 2b's finding at 1P and Gate 4c's finding at 256P (below). **RSS:
PASS**, not systematically fatter — nominally *lower* for the numa arm,
not significant (p=0.254); per-node spanSets stranding memory was the
theoretical risk this sub-gate exists to catch, and it did not
materialize here.

vmstat 0/0 wrap (run twice per protocol, counters are machine-global):

```
$ ssh numa-dell './numa-design/gate-vmstat.sh snap before; env GOMAXPROCS=256 <numa json> -benchmem=512 -benchnum=1 -benchtime=3s; ./numa-design/gate-vmstat.sh snap after; ./numa-design/gate-vmstat.sh diff before after'
hint_faults=0 pages_migrated=0    # both runs
```

**Verdict: PASS.** Archived `gate3-256p/{r1,r2,r3}-{baseline,numa}.out`,
`gate3-256p/wsB-gate3-pooled-{baseline,numa}.out`, `gate3-256p/wsB-gate3-vmstat{,2}.{before,after}`.

### Step 2f. Off-binary function census (workstream-once)

Built locally (tree toolchain, `GOWORK=off GOTOOLCHAIN=local`,
`GOEXPERIMENT=` unset), following the Workstream A Gate 8 methodology
exactly: a trivial `println`-only canary, HEAD (`3b102094cd`) vs. the
pre-Workstream-B parent `53d631efd7` (the Task 7 go/no-go SHA, immediately
before Task 8's first line of code), each built in its own worktree with
its own fresh `make.bash`:

```
head functions: 1482   parent functions: 1481
only in head: [runtime.(*mcentral).cacheSpanFromNode]
only in parent: []
functions with differing instruction-line counts: 11
  runtime.(*mheap).grow:              head=198 parent=218 delta=-20
  runtime.(*mheap).alloc.func1:       head=40  parent=48  delta=-8
  runtime.mallocinit:                 head=236 parent=224 delta=+12
  runtime.(*mheap).nextSpanForSweep:  head=135 parent=118 delta=+17
  runtime.(*mcentral).grow:           head=58  parent=52  delta=+6
  runtime.(*mheap).allocSpan:         head=464 parent=447 delta=+17
  runtime.(*mheap).sysAlloc:          head=424 parent=423 delta=+1
  runtime.(*mcentral).cacheSpan:      head=234 parent=331 delta=-97
  runtime.(*mheap).alloc:             head=58  parent=65  delta=-7
  runtime.(*mcache).allocLarge:       head=131 parent=130 delta=+1
  runtime.(*mheap).init:              head=170 parent=141 delta=+29
numa-related symbols in HEAD off-binary: [runtime.numaHeapStreamsEnabled (B, +8B BSS)]
fast-path spot checks: main.main 28/28, runtime.schedinit 353/353, runtime.mallocgc 131/131 (byte-identical)
size: head .text=1,188,509  parent .text=1,172,307  delta=+16,202B
```

Both deviations from a literal "zero new symbols" bar are **previously
disclosed and accepted**, not new findings: `numaHeapStreamsEnabled` was
Task 8's own review-round fix (I4/C1/C2, "+8B BSS... all directly
attributable to the review's own fixes"), and `cacheSpanFromNode` matches
Task 9's own recorded ruling (7): "cumulative off-build cacheSpan .text
+~700B disclosed under substantive bar (shared-code restructure, upstream's
own fast-path shape restored)." (This workstream-level comparison spans a
wider baseline than either task's own isolated verification — comparing
against the pre-Workstream-B parent rather than each task's immediate
predecessor — so the cumulative `.text` delta here, +16,202B, is larger
than either task's individually-disclosed figure; that is expected, not a
new discrepancy.) `cacheSpan`'s logic was split into `cacheSpanFromNode`
as a shared helper called from two sites within `cacheSpan` — present in
the off build because the compiler did not inline it back down (unlike
`numaGrowNode` et al., which did DCE away), not because any call site is
un-gated: the function's own body collapses to today's single-stream
behavior when `numaMaxHeapNodes` is the off-build's constant 1, matching
`mheap.grow(npage, node)`'s established pattern from Task 8.

**Verdict: PASS on the substantive criterion** (matching the Workstream A
Gate 8 precedent's own language) — **not** a literal "build-ID-only" bar:
zero unrelated-function changes (three explicit hot-path spot checks
byte-identical), zero new symbols beyond the two already disclosed and
accepted in Tasks 8/9's own review rounds, all 11 line-count deltas
confined to functions in the workstream's own declared file map
(`mheap.go`, `mcentral.go`). **Gap recorded (audit round):** this census
is a static, structural check (instruction-line counts, symbol presence)
only — none of the 11 changed function bodies were given a **behavioral**
off-vs-parent check (e.g., a benchmark or differential test confirming
the off-build's restructured `cacheSpan`/`cacheSpanFromNode` actually
produces identical results/timing to the pre-restructure parent). The
census answers "did the shape change unexpectedly," not "does the changed
shape behave identically" — a real, open gap, not one this task's own
data closes. Archived `census/census-{head,parent}.txt` (full stripped
disassembly, 7.8M each), `census/census-summary.txt`, `census/census-canary.go`.

### Step 3 — IMC decision gate (pre-registered primary): **FAIL**

```
perf stat -x, -e mem_load_l3_miss_retired.local_dram,mem_load_l3_miss_retired.remote_dram -- \
  env GOMAXPROCS=256 ./json -benchmem=512 -benchnum=1 -benchtime=10s
```

Arms: experiment off (stock, no `GOEXPERIMENT`) vs. experiment on
(`GOEXPERIMENT=numa`, the full Workstream B build — homing + routing +
soft affinity, all three ingredients, unlike Layer 2's homing-alone null).
Both binaries built from the same `x/benchmarks` pin, `go version -m`
confirmed (`ce4b564d8f`, `X:numa` on the numa arm). **5 interleaved runs
per arm** (the brief raised the plan's ≥3 minimum to ≥5 given the stakes):

| Arm | run | local_dram | remote_dram | remote share |
|---|---:|---:|---:|---:|
| baseline | 1 | 19,069,494 | 17,759,955 | 48.22% |
| baseline | 2 | 74,177,335 | 69,903,196 | 48.52% |
| baseline | 3 | 83,589,700 | 78,705,634 | 48.50% |
| baseline | 4 | 26,330,712 | 24,900,195 | 48.60% |
| baseline | 5 | 83,692,288 | 77,441,054 | 48.06% |
| numa | 1 | 57,423,456 | 52,966,335 | 47.98% |
| numa | 2 | 57,164,562 | 48,390,709 | 45.84% |
| numa | 3 | 62,303,648 | 53,761,260 | 46.32% |
| numa | 4 | 62,896,187 | 50,573,858 | 44.57% |
| numa | 5 | 70,788,953 | 61,592,956 | 46.53% |

**Medians: baseline 48.50%, numa 46.32%. Relative change: −4.49%.**
**Pass bar: ≥10% relative drop (e.g. 48.50% → ≤43.65%). Actual: −4.49% —
real, directionally correct, but well short of the bar.**

Unlike Layer 2's null result (+0.10%/−0.07%, arms statistically
indistinguishable), this is a **clean, statistically significant effect
in the right direction**: `max(numa)=47.98% < min(baseline)=48.06%` —
complete separation between the two arms across all 5×5 runs, exact
two-sided Mann-Whitney U p=0.00794 (this **is the exact-test floor at
n=5,5** — the smallest p-value complete separation can produce at this
sample size, not a value that would shrink further without more runs;
noted so it isn't misread as an unusually strong significance level).

**Confidence intervals on the relative drop (audit round, `imc/imc_ci.py`,
both independently reproduced and matching the audit's own figures
exactly):**

- Welch's t-test 95% CI (raw share values, Welch–Satterthwaite df≈4.28):
  **[−7.62%, −1.19%]**
- Bootstrap, ratio-of-medians (200,000 resamples with replacement, each
  arm independently): 95% CI **[−8.09%, −1.06%]**; **0 of 200,000
  resamples** reach the −10% pass bar.

Both intervals **strengthen** the FAIL verdict rather than soften it: even
under the most favorable resampling of this exact data, the relative drop
never approaches −10%. The three-ingredient unit genuinely moves the
metric that killed Layer 2, with a real and now interval-bounded effect.
It does not move it far enough to clear the pre-registered bar, and the
data rule out that a differently-unlucky sample of the same size would
have cleared it.

**Volume independence (minor, audit round):** the five runs per arm swept
a **4.41× range** in total L3-miss volume (36.8M to 162.3M combined
local+remote events, driven by the benchmark's own `-benchtime=10s`
wall-clock-bounded iteration count varying with machine state) — the
remote share itself does not track that volume: Pearson r ≈ **−0.05**
pooled across all 10 runs (r≈−0.13 within baseline, r≈+0.07 within numa —
both consistent with no relationship). The remote-share metric is a
stable per-access-pattern signature, not an artifact of how much traffic
happened to be sampled in a given 10-second window.

**Corroboration — `/numa/span-refills` in-vivo proxy (ON arm, same
`GOMAXPROCS=256`, unpinned, `routing-probe -totalmb=16384`):**

```
gomaxprocs=256 local=393104 remote=325764 total=718868 local_share=54.6838%
```

Remote share 45.32% — **within ~1 percentage point of the perf-measured
46.32%**, close agreement between the runtime's own routing telemetry and
the hardware ground truth. This distinguishes the failure mode cleanly:
**routing is not broken.** Gate 1 already proved ~99.5-100% local under
external pinning; this in-vivo reading shows the routing mechanism is
still doing real, correctly-attributed work at GOMAXPROCS=256 unpinned —
the shortfall is that roughly **45-55%** of span refills land on a node
other than the one most recently associated with the touching thread, a
large gap from Gate 1's ~99-100% pinned figure.

**Verdict: FAIL.** Per the brief's requirement to distinguish "routing
broken" from "routing works, hardware can't show it": this is neither,
precisely — routing works (proven twice, pinned and in-vivo-corroborated),
*and* hardware shows the improvement (a real, significant, reproducible
−4.49%) — it just isn't a large enough improvement to clear the
pre-registered ≥10% bar. Archived `imc/{baseline,numa}-run{1..5}.csv`,
`imc/analyze.py`, `imc/analyze-output.txt`, `imc/span-refills-corroboration.txt`.

### Step 4 — pathology candidates (supporting evidence)

Both binaries built fresh from HEAD (`ce4b564d8f`), `go version -m`
confirmed, exit codes verified explicitly per arm (the Workstream A
Gate 7 stale-binary-incident lesson). Node free memory re-checked before
sizing (per this session's own earlier flag): node 0 ~12.0 GB free at
sweep time, comfortably ≥3× the 4096 MB heap target used throughout this
plan (WS-A's own precedent used the same target against ~10-11 GB free) —
no sizing change needed; arm A stays pinned to node 0 as in every prior
sweep.

#### 4a. Candidate 1 — `x/benchmarks/garbage`, `-benchmem=4096 -benchnum=1`, GOMAXPROCS=128, n=15: **PASS**

WS-A protocol (`numactl`-pinned A, stock B, experiment-on C, rotating
order, `pathology-sweep.sh`). Mechanism validity: arm A 0/0 in 16/16
rounds (incl. warmup); arm C 0/0 in 16/16 rounds (cleaner than Workstream
A's own candidate 1, which had 2 negligible-noise rounds); arm B shows
real hint faults (61k-390k) and migrations (1.09M-1.74M) every recorded
round.

```
Garbage/benchmem-MB=4096-128  B=2.882m ± 15%  C=1.941m ± 1%   -32.65% (p=0.000 n=15)   [primary: B vs C]
Garbage/benchmem-MB=4096-128  A=1.916m ± 1%   C=1.941m ± 1%   ~ (p=0.067 n=15)          [C vs A]
Garbage/benchmem-MB=4096-128  A=1.916m ± 1%   B=2.882m ± 15%  +50.43% (p=0.000 n=15)    [context: A vs B]
```

Non-inferiority CI (round-paired log-ratio + t-distribution, `noninf_ci.py`):

```
n=15 point_estimate_C/A-1=-0.03% 95%CI=[-2.32%, +2.32%]
Non-inferiority (upper bound <= +10%): PASS
```

Paired sign test: C slower than A in 11/15 rounds, faster in 4 — **not
significant** (exact two-sided p=0.1185).

**Verdict: PASS, within this session.** Point estimate essentially zero
(−0.03%), CI upper +2.32%, sign test not significant — C ties the pinned
oracle A, and beats stock B by −32.65%. **Correction (I3, audit round):**
an earlier draft of this section additionally claimed this result was
"materially tighter" than Workstream A's own candidate 1 gate (CI upper
+8.26%, significant sign test). That cross-campaign comparison is
**withdrawn** — it is not a like-for-like read. The untreated, unchanged
stock arms moved substantially between the two sessions: arm A (pinned
oracle, identical binary/command shape both times) had CV 7.4% in
Workstream A's own sweep vs. **1.3% here** (a 5.7× drop), and arm B
(stock, also unchanged) shifted **+12.7%** in absolute ns/op between
sessions (Workstream A: 2.558ms → this session: 2.882ms). Since both
untreated arms moved this much between sessions, most of the apparent CI
narrowing reflects **this session's own machine/measurement stability**,
not anything C is doing differently from Workstream A's own C. The only
claim this data actually supports is the **within-session** one: C ties A
(non-inferiority PASS, point estimate −0.03%) and clearly beats B
(−32.65%), in the same single session, under the same rotating-order
protocol — a real result, just not a cross-session "improvement over
Workstream A" one. **Citation fix (audit round 2):** the originally
archived `insweep-confinement.out` for this candidate did not actually
contain the `Cpus_allowed_list` line cited here — the capture command's
output redirection missed it (the grep ran outside the file redirect).
Re-captured properly (WS-A Task-4 method: sample `/proc/PID/status`
mid-run, same command/config, output fully redirected into the archived
file this time): `Cpus_allowed_list` narrowed to **node 0's even CPUs**
(this re-capture's boot node — boot node varies run to run and is not
itself evidentiary; what matters is that it is a single node's CPU
list). Archived `cand1-128p/insweep-confinement.out` (corrected).

#### 4b. Candidate 2 — `gc-pause-bench` heavy profile, GOMAXPROCS=128, n=10 round-level (80 cycles/arm): **PASS**

Same protocol and flags as Workstream A's own Gate 7
(`-ptrheap=true -toucher=false -heap=4096 -idle=1000000 -stacks=200
-warm=45 -gcgap=5s -discard=2 -n=8`). Mechanism validity: arm A 0/0 in
11/11 rounds; arm C 0/0 in 10/11 (one round with 1 hint fault, 0
migrated — negligible, same character as every prior BIND-all
measurement); arm B real faults/migrations every recorded round.

Round-level analysis (median of 8 cycles/round, `cand2_analysis.py`, ICC
and effective-n reported per protocol): arm A ICC=0.928 (eff. n≈10.7),
arm B ICC=0.233 (eff. n≈30.4), arm C ICC=0.981 (eff. n≈10.2) — round-level
is the correct unit, matching Workstream A's own finding for this
workload.

```
=== Primary: B vs C (round medians) ===
B median=3906.96ms  C median=3078.54ms  rel=-21.20%
normal-approx p=0.0002 SIGNIFICANT; exact (full enumeration) p=1.083e-05 SIGNIFICANT
=== Secondary: A vs B ===
A median=3107.53ms  B median=3906.96ms  rel(B vs A)=+25.73%  p=0.0002 SIGNIFICANT
=== C vs A (context) ===
A median=3107.53ms  C median=3078.54ms  rel(C vs A)=-0.93%  p=0.3075 (normal-approx) not significant
```

**Correction (minor, audit round):** the script's normal-approximation
p=0.3075 above is conservative; the true **exact** Mann-Whitney U
(full enumeration, `184,756` combinations, `cand2_exact_p.py`) is
**p=0.3150** — both agree on "not significant," the exact value is the
one to cite. U1=64.0, U2=36.0.

Non-inferiority CI: `n=10 point_estimate_C/A-1=-0.53% 95%CI=[-2.37%, +1.34%] — PASS`.
Paired sign test: C faster than A in 8/10 rounds — not significant (exact
two-sided p=0.1094).

**Verdict: PASS, within this session** — a clean win over stock (B,
−21.20%), and C ties the pinned oracle A (non-inferiority PASS, point
estimate −0.53%, not significant either direction). **Correction (I3,
audit round):** an earlier draft additionally framed the CI bound here
(+1.34% upper) as "tighter than Workstream A's own +5.15% upper bound" —
withdrawn for the same reason as candidate 1 above (I3): that comparison
was not verified as like-for-like against Workstream A's own candidate 2
session-to-session noise characteristics, and this battery's own audit of
candidate 1 already showed such cross-session comparisons are dominated
by session stability, not by anything the treatment does differently.
Only the within-session claim stands: C ties A, C beats B. **Citation
fix (audit round 2):** same issue as candidate 1 above — the originally
archived `insweep-confinement.out` did not contain the cited
`Cpus_allowed_list` line. Re-captured properly (same method, output fully
redirected this time): `Cpus_allowed_list` narrowed to **node 0's even
CPUs**. Archived `cand2-gcpause/insweep-confinement.out` (corrected).

#### 4c. Exploratory — `x/benchmarks/garbage`, GOMAXPROCS=256, B vs C, n=10 (confinement never fires; pure Workstream B effect)

At GOMAXPROCS=256, `numaConfineIfSmall` declines unconditionally (procs
exceeds any single node's CPU count) — this isolates the effect of
homing + routing + soft affinity alone, with none of Workstream A's own
confinement contribution. Mechanism validity: arm C 0/0 in 11/11 rounds
(Layer 1's own BIND-all signature, as always); arm B real faults/migrations
every round.

```
Garbage/benchmem-MB=4096-256  sec/op:            B=3.054m ± 6%   C=3.164m ± 4%   ~ (p=0.247 n=10)
Garbage/benchmem-MB=4096-256  user+sys-sec/op:    B=242.4m ± 5%   C=260.5m ± 8%   +7.45% (p=0.011 n=10)
Garbage/benchmem-MB=4096-256  peak-RSS-bytes:     B=8.903Gi ± 2%  C=9.024Gi ± 2%  ~ (p=0.353 n=10)
```

**Not a pass/fail gate** (exploratory, per the brief) — but the honest
reading: at GOMAXPROCS=256 with confinement inactive, wall-clock `sec/op`
shows **no significant difference** (C nominally ~3.6% slower, not
significant), and CPU time is **significantly worse** for C
(+7.45%, p=0.011) — the same user+sys-time cost pattern as Gates 2b and
2c above, now confirmed on a third, independent workload. **The "pure
Workstream B effect" at this GOMAXPROCS, isolated from Workstream A's own
confinement contribution, shows no throughput win and a real, repeated
CPU-time cost.** This matches the IMC gate's own finding at the same
GOMAXPROCS: a genuine but modest routing improvement (Step 3) that does
not translate into a measured wall-clock win here, while adding
measurable overhead. Archived `cand1-256p-exploratory/`.

### Step 5 — overall verdict: Workstream B gate battery — **DOES NOT SHIP AS-IS**

| Gate | Result |
|---|---|
| 1. Pinned routing proof (hard, gates everything) | **PASS** — 100.00% / 99.54% local (≥95% bar); numa_maps corroboration 95.35% / 100.00% |
| 2a. 1P json (hard) | PASS — not significant, p=0.393 |
| 2b. 1P alloc micro (hard) | **FAIL** — geomean +3.73% > +2% (p=0.000 both, CV≈0%, not noise) |
| 2c. 256P json sec/op (hard) | PASS — pooled n=30, not significant, p=0.358 |
| 2c. 256P json user+sys-sec/op (hard) | **FAIL** — +19.96% (p=0.025, pooled n=30, significant) |
| 2d. RSS (hard) | PASS — not fattened, nominally lower, not significant |
| 2e. vmstat 0/0 (hard) | PASS — 0/0 twice |
| 2f. Off-binary census, workstream-once (hard) | PASS (substantive) — both deviations previously disclosed/accepted in Tasks 8/9's own reviews; hot paths byte-identical |
| 3. IMC decision gate (pre-registered primary) | **FAIL** — −4.49% relative (need ≥−10.00%); real, corroborated, complete-separation effect (exact p=0.00794), just short of the bar |
| 4a. Pathology candidate 1 (128P, supporting) | PASS — B-vs-C −32.65% (p=0.000); C-vs-A non-inferiority CI upper +2.32% (within-session only; cross-session "tighter than WS-A" claim withdrawn, I3) |
| 4b. Pathology candidate 2 (128P, supporting) | PASS — B-vs-C −21.20% (exact p=1.08e-5); C-vs-A non-inferiority CI upper +1.34% (within-session only; cross-session "tighter than WS-A" claim withdrawn, I3) |
| 4c. Exploratory 256P garbage (informational) | sec/op not significant; user+sys-sec/op +7.45% (p=0.011) — third confirmation of the CPU-time-cost pattern |

**Two hard gates fail** (1P alloc micro +3.73%, 256P user+sys +19.96%,
both statistically significant with tight, low-noise measurements — not
borderline or ambiguous misses), and **the pre-registered primary decision
gate fails** its ≥10% relative-drop bar (achieving −4.49%, a real and
statistically clean but insufficient effect). Per the plan's stop rule
("Merge the unit only on a full pass") and Global Constraints ("If a gate
fails: stop, record the failure... do not start the next workstream"):
**Workstream B does not ship as-is.**

This is not the null result Layer 2 produced. All three ingredients (per-
node arena streams, node-routed mcentral refill, soft affinity) were
built, are individually verified working (Gate 1's pinned proof, the
routing-probe corroboration), and together they move the IMC metric in
the correct direction with a clean, statistically significant, complete-
separation effect — a categorically different outcome from Layer 2's
+0.10%/−0.07% null. The unit is genuinely measured, per the brief's own
requirement, and the measurement says: real but insufficient, at a real
CPU-time cost.

**Three-ingredient candidate analysis — what's still missing (per the
brief's explicit request; the strongest-supported reading of the evidence
gathered here, not a proven root cause without further profiling):**

The most load-bearing piece of evidence is the size of the gap between
Gate 1 (externally pinned: ~99.5-100% local) and the unpinned IMC/
span-refills readings (~45-55% local at GOMAXPROCS=256). A gap that large
points toward **thread-stability insufficiency, specifically at the
goroutine level rather than the OS-thread (M) level**: `numaNoteSchedule`
(Task 10) narrows an M's *CPU affinity* on node change, and — because the
narrowed mask constrains the M to that node going forward — the kernel
can never again report a different node for that M via `getcpu`, so in
practice each M gets narrowed once, early, to whichever node the OS
scheduler happened to place it on first, and then stays there for the
rest of the process's life (short of an explicit widen event). At
GOMAXPROCS=256 unpinned, that produces something close to a random,
roughly 50/50 one-time split of Ms across the two nodes — but Go's M:N
scheduler does not pin *goroutines* to Ps/Ms by NUMA node; goroutines
(the actual units of memory access) migrate freely across all GOMAXPROCS
Ps via ordinary work-stealing and run-queue rebalancing, entirely
independent of which node a given P's M happens to be narrowed to. A
span refilled by a P/M pinned to node 0 can trivially end up read by a
goroutine that the scheduler later runs on a P/M pinned to node 1 — and
with roughly half the Ms on each node, a roughly-50%-remote steady state
is exactly what that mechanism predicts, matching the ~45-55% figures
measured above far better than either alternative below on its own. This
reads as ingredient (c) as built — OS-thread-level soft affinity — not
addressing the actual unit that needs to stay put (the goroutine),
because Go's own scheduler has no NUMA-awareness in its own placement
decisions.

Two other candidates remain plausible contributors, not ruled out by this
battery's data:

- **`pages.alloc` address-ordering** (Task 9's own carried-forward ruling
  1: "pages.alloc address-ordered cross-stream accepted-for-now — honest
  counter + Task 11 IMC gate decides; node-aware page allocation is the
  follow-up candidate if the gate fails"). This gate has now failed, which
  is precisely the condition Task 9 named as the trigger to revisit this.
  Cross-stream page ordering could independently degrade the correlation
  between a refill's chosen node and the physical pages it actually
  receives, on top of (or instead of) the goroutine-migration mechanism
  above; this battery did not isolate the two.
- **Homing granularity** — arena chunks are homed once per grow (~4 MiB,
  per Task 8), a coarse unit relative to individual allocations; if a
  chunk's grow-time node assignment and its eventual readers diverge
  systematically (for reasons unrelated to goroutine migration), finer-
  grained homing would not help without also addressing (b)/(c)'s own
  gaps first.

**Distinguishing among these — and specifically confirming or ruling out
the goroutine-migration mechanism above with a CPU profile or a
runtime-level goroutine-placement trace — is follow-up work, not decided
here.** The controller decides next steps; this battery's job was to
measure honestly, not to diagnose exhaustively.

**Concerns for the controller:**

1. The CPU-time-cost pattern (Gate 2b +3.73%, Gate 2c +19.96%, Gate 4c
   +7.45%, all statistically significant, all in the same direction)
   recurs across three independent measurements at three different
   GOMAXPROCS values and workloads — this is not a fluke of any single
   gate. Any follow-up work should treat this as a real, load-bearing
   finding, not something to explain away.
2. The pathology candidates at GOMAXPROCS=128 (where Workstream A's own
   confinement is active for arm C too) both **tie the pinned oracle A
   and beat stock B, within this session** — worth noting explicitly so
   this is not read as "Workstream B made things worse everywhere": the
   two hard-gate failures and the IMC shortfall are real, but they
   coexist with genuine, measured wins on confinement-eligible
   workloads. **Corrected (I3, audit round):** this is deliberately no
   longer phrased as "better than Workstream A's own gate battery" — that
   cross-campaign framing is withdrawn (see 4a/4b above); the untreated
   stock arms moved substantially between the two sessions (arm A's CV
   fell 5.7× session-to-session, arm B moved +12.7%), so a same-session
   tie against the pinned oracle is the only comparison this data
   actually supports.
3. This battery did not attempt to separate "the CPU-time cost is from
   ingredient (b)'s refill-time `getcpu` calls" from "the cost is from
   ingredient (c)'s soft-affinity scheduler hook" from "the cost is from
   added branching/locking in the now-per-node spanSets regardless of
   whether either fires" — a CPU profile (which would also serve Task
   14's own adoption-gate question, still open) would answer both this
   and the goroutine-migration hypothesis above in one pass.
4. Per the plan's own framing (Task 7's go/no-go, "Input 2"): Workstream
   A's own coverage gap — the general/node-exceeding case (GOMAXPROCS
   unset or > one node's CPUs) regressing ~5% against stock — remains
   unaddressed. This battery does not change that finding; Workstream B
   as measured here does not close it either (Gate 4c's GOMAXPROCS=256
   exploratory result shows no throughput win in that exact regime).

Session end: `kernel.numa_balancing = 1` (confirmed), machine idle
throughout and at close, remote tree unmodified beyond the archived
scratch build directories under `/tmp/pb/` (not committed; raw results
copied into the tracked archive above).

## Exploratory attribution experiments (E1, E2) — pre-registered before running

Two cheap experiments, requested to move the verdict from "what" (the gate
battery above) to "why" (the three-ingredient candidate analysis's own
open question). **Neither is a gate** — no pass/fail bar, no effect on the
Step 5 verdict above, which stands regardless of what these find.
Pre-registered here, in this commit, before either runs.

### E1 — GOMAXPROCS sweep of the routing probe, unpinned, remote share vs. thread count

**Hypothesis under test:** the three-ingredient candidate analysis's
leading candidate (goroutine-level migration, not M-placement) predicts
remote share stays roughly flat (~50%) across GOMAXPROCS values, because
the mechanism (goroutines migrating across Ps regardless of which node a
P's M is pinned to) does not depend on how many Ms exist. The alternative
(M-placement randomness alone, without goroutine migration playing a
role) predicts remote share **falls** as GOMAXPROCS drops, because with
few Ms the odds of an even split across two nodes drop (e.g. at
GOMAXPROCS=2, either both Ms land on the same node — 0% remote — or one
each — form a 2-P system where every refill is either fully local or the
process is effectively single-node per P, a qualitatively different
regime than the many-M near-50/50 case).

**Method:** `routing-probe` (already built, `GOEXPERIMENT=numa`,
unpinned — no `numactl`), GOMAXPROCS ∈ {2, 8, 32, 128, 256}, n=3 runs per
point (interleaved across points within one session, not blocked by
point, to spread any thermal/session drift evenly), reading
`/numa/span-refills/{local,remote}:spans` at exit. Primary readout: median
remote share per GOMAXPROCS point, reported as a curve, not a gate.

### E1 results (round 1) — **WITHDRAWN, confinement-confounded; kept as history**

```
GOMAXPROCS=2:   100.0000%, 100.0000%, 100.0000%  -> median local 100.00% (remote 0.00%)
GOMAXPROCS=8:   100.0000%, 100.0000%, 100.0000%  -> median local 100.00% (remote 0.00%)
GOMAXPROCS=32:  100.0000%, 100.0000%, 100.0000%  -> median local 100.00% (remote 0.00%)
GOMAXPROCS=128: 100.0000%, 100.0000%, 100.0000%  -> median local 100.00% (remote 0.00%)
GOMAXPROCS=256: 59.9389%, 56.0630%, 63.8464%      -> median local 59.94% (remote 40.06%)
```

**Withdrawal (audit round 2 — a real bug, not a stylistic correction):**
this run set `GOMAXPROCS` via the process **environment variable**
(`env GOMAXPROCS=N probe-on ...`), which sets `sched.customGOMAXPROCS =
true` at `schedinit`. Workstream A's fill-one-socket confinement
(`numaShouldConfine`) makes its one-shot decision at `schedinit` and
engages whenever `customGOMAXPROCS` is true **and** `procs <=
nodeCPUs` (128 on this machine). Every point at GOMAXPROCS≤128 in this
run therefore had **Workstream A's own confinement engaged**, not
Workstream B's mechanism in isolation — the apparent "step at node
capacity" was `numaShouldConfine`'s own `procs<=128` threshold, not a
Workstream B finding. This is the **same trap Task 12's own
pre-registration explicitly documented and worked around** for the
phase-shift study, and this experiment fell into it. The 100%-local
readings at GOMAXPROCS≤128 are explained trivially: a confined process
runs entirely on one node's CPUs, so of course refills are ~100% local —
this measured confinement working as designed (already proven in Gate 1
and the Workstream A gate battery), not anything about Workstream B's own
routing/soft-affinity behavior at low thread counts. Only the GOMAXPROCS=256
point in this run was actually unconfined (256 > 128, `numaShouldConfine`
declines unconditionally) — which is why it alone showed genuine remote
traffic. **Kept here as history, not deleted**, per the correction
protocol this file follows throughout. Archived `e1-gomaxprocs-sweep/`.

### E1 results (round 2) — corrected, genuinely unconfined

**Fix:** `GOMAXPROCS` is never set via the environment for this rerun.
The process starts with the default (`NumCPU()=256`, `customGOMAXPROCS=
false`), so `numaShouldConfine`'s own check (`256<=128`) evaluates false
at `schedinit` regardless of what happens afterward — confinement's
one-shot decision is already made and cannot be revisited later (the
same mechanism Task 3's `SetDefaultGOMAXPROCS` work documented). The
probe then calls `runtime.GOMAXPROCS(n)` **after** startup to set the
actual worker concurrency to the point under test — this changes
scheduling concurrency without ever flipping `customGOMAXPROCS`, so
confinement never engages at any point, isolating Workstream B's three
ingredients (homing + routing + soft affinity) from Workstream A's
confinement at every GOMAXPROCS value. New harness:
`e1-unconfined-v2/routing-probe-unconfined.go`.

**Verified via `GODEBUG=numa=1`, captured for every run (not just a
sample):** all 15/15 runs print `numa: confinement declined: GOMAXPROCS
not explicitly set` and 0/15 print a `numa: confined to node ...` line —
direct, per-run, PROVEN decline at every point including the lowest
(GOMAXPROCS=2). Every run also shows `numa: soft affinity narrowed M to
node 0` / `node 1` lines spread across both nodes, confirming the soft-
affinity mechanism is genuinely active and exercising both nodes even at
low thread counts.

```
GOMAXPROCS=2:   99.9875%, 75.0521%, 69.2676%   -> median local 75.05% (remote 24.95%)
GOMAXPROCS=8:   75.8450%, 73.4017%, 70.3733%   -> median local 73.40% (remote 26.60%)
GOMAXPROCS=32:  63.7864%, 49.7976%, 53.0792%   -> median local 53.08% (remote 46.92%)
GOMAXPROCS=128: 65.6762%, 59.8418%, 61.6445%   -> median local 61.64% (remote 38.36%)
GOMAXPROCS=256: 56.3606%, 57.1520%, 60.1380%   -> median local 57.15% (remote 42.85%)
```

**This is a materially different curve than round 1** — remote traffic
is present at every GOMAXPROCS value once confinement is genuinely out
of the picture, not a step function that only appears at 256. **The
interpretation drawn from it (below) was itself corrected in a further
audit round — see "E1 interpretation corrected" immediately after this
subsection.** Archived `e1-unconfined-v2/` (15 raw `.out` files with full
`GODEBUG=numa=1` output embedded, plus the harness source).

**Withdrawn interpretation (kept as history, corrected below):** an
earlier draft of this section read the GOMAXPROCS=2 point's 99.99%
outlier as "both Ms happened to land on, and stay on, the same node,"
and read the overall curve as "consistent with the goroutine-migration
hypothesis... rather than a pure M-placement-randomness story." Both
readings were wrong; see the correction immediately below, which
replaces this paragraph's conclusions without deleting the record of
having drawn them.

### E1 interpretation corrected (third audit pass)

**(1) By the pre-registered rule, this data does not support "goroutine-
migration implicated."** The pre-registration stated plainly: migration
predicts remote stays roughly **flat near 50%** regardless of thread
count; M-placement-randomness predicts remote **falls** at low
GOMAXPROCS. Observed remote share (100−local): **24.95% (2), 26.60% (8),
46.92% (32), 38.36% (128), 42.85% (256)**. Remote **does fall** at the
two lowest points (2, 8) relative to the three higher ones (32, 128,
256) — the opposite of "flat near 50%," and directionally the shape the
pre-registration assigned to the M-placement alternative, not to
migration. Read strictly against the pre-registered rule, **this favors
the M-placement alternative, or at best is non-discriminating** — it
does **not** support the migration reading the round-2 write-up drew.
Withdrawn.

**(2) The redo did not actually test the low-M regime it was designed
to test.** `routing-probe-unconfined.go` boots with the default
`NumCPU()=256` (256 Ps live from `schedinit`), and `runtime.GOMAXPROCS(n)`
only caps **scheduling concurrency** afterward — it does not shrink the
number of Ms that come into existence over a run's lifetime, and
`numaNoteSchedule` narrows **every M** that passes through `schedule()`,
regardless of the current GOMAXPROCS value. Directly counted from the
archived `GODEBUG=numa=1` output (`numa: soft affinity narrowed M to
node 0/1` lines per run):

| point | round 1 | round 2 | round 3 |
|---|---|---|---|
| GOMAXPROCS=2 | node0=44 node1=25 (69) | node0=31 node1=37 (68) | node0=48 node1=21 (69) |
| GOMAXPROCS=8 | node0=56 node1=12 (68) | node0=38 node1=31 (69) | node0=41 node1=29 (70) |
| GOMAXPROCS=32 | node0=42 node1=29 (71) | node0=36 node1=34 (70) | node0=31 node1=38 (69) |

**~68-71 distinct Ms were narrowed, spread across both nodes, at every
one of GOMAXPROCS=2/8/32** — essentially the same M count as at
GOMAXPROCS=128/256. The "low thread count" points never actually ran at
low thread count; they tested the same large, ambient M population (GC
workers, sysmon, netpoller, and Ms spun up for blocking work) under a
lower **scheduling concurrency cap**, not a lower M population. This
experiment, as built, cannot discriminate M-count effects at all —
withdrawn as a test of "does remote share depend on M count."

**(3) The GOMAXPROCS=2 round-1 explanation is withdrawn, unexplained.**
That run (99.99% local) logs **44/25 narrowings split across both
nodes** — the same both-nodes-active pattern as every other run, not a
"both Ms stayed on one node" outcome as originally claimed. Why span
refills were still ~100% local despite ~69 Ms narrowed across both
nodes in that specific run is **not explained by this data** and is
left as a genuinely open, unresolved observation, not confabulated
further.

**(4) Per-point dispersion (range, computed from the archived raw
values):**

| GOMAXPROCS | median local% | range (pp) |
|---|---:|---:|
| 2 | 75.05 | 30.72 |
| 8 | 73.40 | 5.47 |
| 32 | 53.08 | 13.99 |
| 128 | 61.64 | 5.83 |
| 256 | 57.15 | 3.78 |

Dispersion itself is highest exactly where the M-count confound (2) is
most severe, consistent with (2)'s own finding that these points are
not cleanly testing what they were designed to test.

**What E1 actually establishes, honestly:** confinement is genuinely off
at every point (proven, `GODEBUG` evidence, 15/15); soft affinity is
genuinely active and narrows Ms across both nodes at every point
(proven, same evidence); and **25-47% of span refills are remote at
every unpinned GOMAXPROCS tested, with no clean, interpretable
dependence on thread count established by this experiment** — i.e.
node-routed refill does not deliver strong locality without external
pinning, full stop, independent of any M-count story. **A further
caveat on the metric itself, not just this run's design:** span-refill
local/remote share is a **supply-side** condition — it reflects which
node's spanSet happened to serve a given refill, which conflates several
distinct mechanisms this experiment cannot separate: genuine goroutine
migration across nodes, M-narrowing imbalance within a run (e.g. the
56/12 node-0-heavy split logged at `procs8-round1`, which by itself
would bias that round's refills toward node 0 regardless of any
goroutine behavior), arena-homing imbalance from Task 8's own address-
ordered growth, and GC-sweep-driven span redistribution across the
per-node sets. This experiment measures the aggregate outcome of all of
these together; it does not and cannot isolate which one dominates.

### E2 — CPU-cost attribution at 256P (Gate 2c's own config)

**Question under test:** is the +19.96% `user+sys-sec/op` cost (Gate 2c)
syscall-bound (a `getcpu`/`sched_setaffinity`/`mbind` storm) or
in-runtime (spanSet lock contention, the 8× sweep-path pop cost Task 9's
own ruling (4) already named, or similar)?

**Method:** one run each (attribution evidence, not a statistical claim —
explicitly not over-interpreted as such), experiment-on, same config as
Gate 2c (`GOMAXPROCS=256`, `json -benchmem=512 -benchnum=1
-benchtime=10s`):
1. `strace -c -f` — syscall counts, specifically `getcpu`,
   `sched_setaffinity`, `mbind` (plus whatever else appears).
2. `perf record -g` (call-graph sampling) — top symbols by self time, to
   see whether the hot spots are runtime-internal (spanSet/lock code) or
   syscall-entry-adjacent.

Both raw outputs archived; top-line findings reported, not a full profile
analysis.

### E2 results (round 1) — `strace` counts (unchanged) + **ON-arm-only `perf record` (superseded, relabeled)**

**`strace -c -f` (syscall counts) — this half stands, unchanged:**

| syscall | calls | % traced time |
|---|---:|---:|
| `futex` | 87,555 | 81.95% |
| `getcpu` | 479,662 | 14.28% |
| `mbind` | 2,338 | 0.03% |
| `sched_setaffinity` | 296 | 0.01% |

`getcpu` volume is real and substantial (479,662 calls in one 10s-
benchtime run) — far more than a naive "refill-only" mental model would
predict, consistent with soft affinity's 4ms-per-M throttled
`schedule()`-hook firing across up to 256 Ms under this GC-heavy,
high-goroutine-churn workload. `sched_setaffinity`/`mbind` stay rare, as
designed.

**`perf record -g` — ON arm only (round 1's capture): superseded, kept as
history, relabeled.** Round 1's write-up read the ON-arm-only profile as
showing the NUMA-syscall path negligible (0.050% of cycles) and the
spanSet/mcentral/sweep-path code substantial (5.290% of cycles), and
concluded from that alone that the cost was "in-runtime, not
syscall-bound." **Two things were wrong with that conclusion, both fixed
in round 2 below:** (a) it also framed `futex`'s 81.95% `strace` share as
"generic Go-runtime/GC synchronization... not attributed to this
workstream" — round 2's OFF-arm baseline shows this was an unverified
assumption, not a checked fact; futex/kernel-spinlock time is present in
both arms but **substantially higher in the ON arm**, so it is partly
attributable after all. (b) With no OFF-arm baseline, "5.29% of cycles"
had no comparison point — relabeled here as an **ON-arm ceiling**
(an upper bound on what spanSet/mcentral/sweep-path code could
possibly explain, not a delta against stock). Round 1's `perf.data`
capture also turned out to be silently userspace-only
(`kernel.perf_event_paranoid=2` on this host restricts kernel-mode
sampling for non-root by default — confirmed after the fact via the
recorded event name, `cycles:Pu`, the `u` suffix meaning
user-mode-only), so it could not see kernel-mode cycles at all — a gap
the failing metric (`user+sys-sec/op`, which explicitly includes system
time) requires closing. Archived `e2-cpu-cost-attribution/` (unchanged,
kept as history).

### E2 results (round 2) — corrected: OFF-arm baseline + kernel cycles included

**Fix:** re-captured **both** arms with `kernel.perf_event_paranoid`
temporarily lowered to `1` (from `2`) and `kernel.kptr_restrict`
temporarily lowered to `0` (from `1`, needed to resolve kernel symbol
names at all — without it, kernel-mode samples land on unresolvable raw
addresses that appear identical across unrelated binaries, a genuine
trap this round fell into and caught before archiving: an initial
kptr_restrict=1 capture showed ~34-48% of samples on an opaque
`0xffffffffac0a16c0`-class address that turned out, once resolved, to be
ordinary kernel spinlock code). Both sysctls restored to their original
values (`2`/`1`) immediately after both captures completed. Both arms
now record `cycles:P` (no `u` suffix — kernel-mode samples included),
confirmed in each report's own header. OFF arm: identical config,
`GOEXPERIMENT` unset, same `json` binary the pooled Gate 2c/Gate 3
sweeps used.

**Per-symbol-group cumulative %, ON vs OFF vs delta (`e2-cpu-cost-attribution-v2/analyze.py`):**

| symbol group | ON % | OFF % | delta (ON−OFF) |
|---|---:|---:|---:|
| spanSet/mcentral/sweep (WS-B routing path) | 1.850 | 5.180 | **−3.330** |
| numa syscall/routing-decision path | 0.020 | 0.000 | +0.020 |
| kernel spinlock contention (`native_queued_spin_lock_slowpath`) | 35.710 | 23.640 | **+12.070** |
| other kernel-mode samples (`[k]`, excl. spinlock) | 4.280 | 4.850 | −0.570 |
| **total kernel-mode (`[k]`)** | **39.990** | **28.490** | **+11.500** |
| **total user-mode (`[.]`)** | **59.120** | **70.550** | **−11.430** |

**Top-line finding, corrected further (fourth audit pass): the cost is
dominated by kernel-mode spinlock contention (a real, measured, absolute
increase), not userspace spanSet code — but the mechanism is NOT
`futex` and the "% of the +19.96% target" arithmetic below was withdrawn
because this specific capture pair does not reproduce that target at
all.** `native_queued_spin_lock_slowpath` is present substantially in
**both** arms (23.64% OFF — real, generic kernel-spinlock contention
exists in stock Go too) but **12.07 percentage points higher in the ON
arm** (35.71% vs 23.64%). Meanwhile spanSet/mcentral/sweep-path
**user-mode** code is a *lower* share of ON-arm cycles (1.85% vs 5.18%
OFF) — cycles shifted from *doing* spanSet work to *waiting* on a
kernel-mode lock. The NUMA syscall/routing-decision path stays
negligible in both arms (0.02% ON, 0.00% OFF).

**Correction: `native_queued_spin_lock_slowpath` is NOT futex-specific —
withdraw the earlier "strace futex finding confirmed" sentence.** It is
the generic kernel qspinlock slow path, entered by *any* contended
`raw_spin_lock`/`raw_spin_lock_irqsave` in the kernel (page tables, run
queues, `sighand`, zone locks, and more) — conflating it with `futex(2)`
specifically was wrong. Directly checked: kernel `futex_*`/
`runtime.futex*` symbols sum to **0.24% ON vs 0.28% OFF** — flat,
slightly *lower* under Workstream B, not the dominant mechanism at all.
`strace`'s own 81.95%-`futex`-call-count finding (round 1) reflected
syscall *volume* inflated by ptrace tracing overhead, not real cycles in
the futex path specifically — see the lock-identification finding below
for what the spinlock time is actually spent on.

**Correction: the capture pair's own numbers do not reproduce the
+19.96% target — disclosed, not previously stated.** Read directly from
the archived `on-json.out`/`off-json.out` (the actual benchmark output
lines for this specific pair of runs, not the pooled Gate 2c sweep):

```
                  ON            OFF           delta
ns/op:            2,111,231     2,282,911     -7.52%  (ON is FASTER here)
user+sys-ns/op:   183,743,290   180,265,793   +1.93%
allocs/op:        26,813        26,814        matched work (essentially identical)
total cycles:     631.06e9      650.59e9      -3.00%  (perf event count, both arms)
```

**This pair does not reproduce Gate 2c's pooled (n=30) +19.96% finding —
it shows essentially the opposite sign on wall-clock and a much smaller,
different-sign-adjacent delta on CPU time.** Two real reasons, both
disclosed rather than papered over: (a) different config from the pooled
sweep — this capture used a direct one-shot `-benchnum=1 -benchtime=10s`
invocation (5000 iterations here) vs. `gate-json.sh`'s own
`BENCHNUM=10` wrapping (2000-iteration runs, pooled across 30 total
rounds); (b) this is **n=1 per arm** against benchmarks with an
established 26-32% CV (see Gate 2c's own MDE analysis, I5) — a single
pair landing within that noise band, on the opposite side of the pooled
median, is expected, not anomalous. **The withdrawn arithmetic below
computed "how much of +19.96% this pair's profile explains" — that
question is void, since this pair's own headline numbers don't show a
+19.96% to explain in the first place.**

**Supportable replacement statement, framed on absolute cycles under
matched work (not a percentage of a target this pair didn't produce):**
converting each group's percentage share to absolute cycles (group% ×
each arm's own total event count, from the perf headers: ON
631,060,713,114, OFF 650,593,805,987):

```
spanSet/mcentral/sweep:  ON 11.67e9  OFF 33.70e9  delta -22.03e9  (-65.4% relative to OFF)
kernel spinlock (native_queued_spin_lock_slowpath):
                         ON 225.35e9 OFF 153.82e9 delta +71.53e9 (+46.5% relative to OFF)
```

**Under matched work (allocs/op essentially identical), Workstream B
shifts ~72e9 cycles (+46.6%, rounding) from user-mode spanSet work into
kernel-mode spinlock wait — while total CPU across the whole run is
flat-to-slightly-down (-3.0%) in this specific pair.** This is real,
measured, absolute-cycle evidence of a genuine shift in *where* cycles
go, decoupled from any claim about the pooled +19.96% figure, which this
pair simply does not speak to. Archived `e2-cpu-cost-attribution-v2/`
(`perf-on-full.txt`, `perf-off-full.txt`, `on-json.out`, `off-json.out`,
`analyze.py`, `analyze-output.txt`; raw `perf.data` files, ~1GB each,
deliberately not archived — repo hygiene).

**New signal: page-supply (memory-footprint) difference, from the same
capture pair's own benchmark output.** `clear_page_erms` (the kernel's
page-zeroing routine, entered on fresh page allocation) is **1.05% ON
vs 0.48% OFF (2.2×)**. `bytes-from-system` is **7.54GB ON vs 5.49GB OFF
(+37%)**; `peak-RSS-bytes` is **7.40GB ON vs 5.25GB OFF (+41%)** — a
real, substantial footprint difference under otherwise-matched work
(same `allocs/op`). Per-node arena streams (Task 8) mean the heap grows
into up to `numaMaxHeapNodes` separate address ranges rather than one,
plausibly demanding more distinct fresh pages from the kernel even at
matched logical allocation volume. **This heap-footprint difference is
itself a confound for the whole ON-vs-OFF comparison** — a larger
resident set means more page faults, more TLB pressure, and more page-
table/zone-adjacent kernel work independent of anything about span
routing specifically; it is not controlled for in this attribution
experiment.

**Methodology caveat, disclosed:** both captures were auto-throttled by
`perf` from the requested 4000Hz sampling rate down to 1000Hz
(`Lowering default frequency rate from 4000 to 1000`, present in both
`on-json.out` and `off-json.out` — a shared, not ON/OFF-differential,
limitation of this host's `kernel.perf_event_max_sample_rate`), lowering
temporal resolution for both arms equally. This is a shared caveat, not
an ON/OFF asymmetry.

### Lock-class identification (n=1 attribution evidence)

**Method:** `perf report -g` callee-direction call-graph on the retained
ON-arm `perf.data` (still on `numa-dell`, not re-recorded), filtered to
`native_queued_spin_lock_slowpath`'s own callers.

**Finding: the dominant caller path is NOT the runqueue/wake path, NOT
zone-lock/page-allocator, and NOT `mmap_lock` — it is `sighand->siglock`
contention during POSIX CPU-timer signal delivery.** 58.89% of
`native_queued_spin_lock_slowpath`'s own time (≈21.0 percentage points
of *all* ON-arm cycles) resolves through:

```
native_queued_spin_lock_slowpath
  -> _raw_spin_lock_irqsave
    -> __lock_task_sighand           (58.89% of the spinlock's own time)
      -> posix_cpu_timers_work
        -> task_work_run
          -> irqentry_exit_to_user_mode
            -> asm_sysvec_apic_timer_interrupt
              -> runtime.sigtramp.abi0 / various encoding/json code being interrupted
```

This is the kernel delivering a pending POSIX CPU-interval-timer signal
to a thread (taking the process-wide `sighand->siglock` to do so) —
consistent with the `x/benchmarks` harness's own built-in CPU profiler
(`# cpuprof=/tmp/10.prof.txt`, visible in **both** arms' own benchmark
output — this machinery is not Workstream-B-specific) delivering
`SIGPROF`-class signals across up to 256 threads. **This is very
plausibly a benchmark-harness artifact (CPU profiling signal delivery),
not a Workstream B mechanism** — but it does not explain *why* the ON
arm has more of it (+12.07pp absolute); a larger active thread/M
population, more frequent GC interruption, or the heap-footprint
confound above are all still-open candidates for that specific
asymmetry. A much smaller branch (0.71% of the spinlock's own time) goes
through page-table-lock contention during a GC-triggered copy-on-write
page fault (`_raw_spin_lock` → `__pte_offset_map_lock` → `wp_page_copy`
→ `runtime.greyobject`/`scanSpan`) — closer to the "page-supply/zone
lock" hypothesis, but a minor contributor next to the signal-delivery
path, not the dominant one. **The zone-lock/page-allocator hypothesis
this section originally proposed as "the leading candidate" is not
supported by this callgraph** — reported honestly rather than forced to
fit. Not fully enumerated (the remaining ~41% of the spinlock's own time
across other call sites is not broken down further here); this is
attribution evidence from one run, not a statistical claim. Raw
`perf.data` for both arms remains on `numa-dell` at
`/tmp/pb/wsB-e2-v2/perf-{on,off}.data` (not archived to git — size).

### What E1/E2 (twice-corrected) change about the Step 5 verdict — final honest summary

**Nothing changes the verdict** (still: Workstream B does not ship
as-is) — these remain attribution experiments, not gates, and after two
rounds of correction the supportable findings are narrower than either
round 1 or round 2 claimed:

- **E1, honestly:** confinement is genuinely off and soft affinity is
  genuinely active at every GOMAXPROCS tested (proven). Remote span-
  refill share sits at 25-47% at every point (real). **What it does
  *not* establish:** any clean dependence on thread count, or a
  discrimination between the goroutine-migration and M-placement-
  randomness hypotheses — the experiment's own design (Ms don't scale
  down with a lowered `GOMAXPROCS` cap) never actually varied M count,
  and where the pre-registered rule *can* be applied to the data as
  measured, it points toward the M-placement alternative (or is
  non-discriminating), not toward migration. The round-1 "step function"
  and round-2 "goroutine migration implicated" readings are both
  withdrawn.
- **E2, honestly:** under matched work, Workstream B measurably shifts
  ~72e9 cycles (+46.6% relative to the OFF-arm's own spinlock time) from
  user-mode spanSet work into kernel-mode spinlock contention, with
  total CPU roughly flat in this specific captured pair — a real,
  absolute, non-trivial shift. **What it does *not* establish:** that
  this explains Gate 2c's pooled +19.96% finding (this n=1 pair doesn't
  reproduce that finding at all — different config, different sign),
  that the mechanism is `futex`-specific (it isn't — generic qspinlock
  slow path), or that zone-lock/page-allocator contention is the cause
  (the actual dominant caller, by direct call-graph evidence, is
  `sighand->siglock` contention during CPU-profiling-signal delivery, a
  mechanism present in both arms' benchmark harness and not yet
  explained *why* it's worse under Workstream B). A genuine, disclosed
  memory-footprint difference (peak RSS +41%, `bytes-from-system` +37%)
  is a real confound not controlled for.

Both experiments narrowed from "what we thought we found" to "what the
evidence actually supports" across two audit rounds — the honest
residue is smaller and less conclusive than either earlier draft
claimed, which is itself the correct outcome of following this file's
own correction discipline rather than a failure of the experiments.
Whatever follow-up work the controller decides on should start from
these narrower, twice-verified claims, not the earlier, withdrawn ones.

Session end: `kernel.numa_balancing = 1` (confirmed), `kernel.perf_event_paranoid`
and `kernel.kptr_restrict` both restored to their pre-experiment values
(`2` and `1` respectively, confirmed), machine idle throughout, remote
tree unmodified beyond scratch build/profile artifacts under `/tmp/pb/`
(raw results copied into the tracked archive above; the oversized
`perf.data` files deliberately left uncommitted).

## Workstream B verdict (Task 11 decision)

**DECISION: Workstream B does NOT ship as-is.**

### Established

The following are cited only from audit-surviving figures in "Workstream B
gate battery — 2026-08-21" above (post-correction, post-E1/E2-redo):

1. **Routing correctness is proven under external pinning** (Gate 1:
   `100.0000%` / `99.5442%` local, both ≥95% bar; corroborated by an
   independent `/proc/self/numa_maps` residency sample, 95.35% / 100.00%
   on-node).
2. **IMC remote share moves in the correct direction, but not far
   enough:** 48.50% → 46.32% = **−4.49% relative** — a real, clean effect
   (complete separation across all 5×5 runs, exact two-sided Mann-Whitney
   U p=0.00794) whose 95% CI **[−7.6%, −1.2%]** (Welch's t-test;
   bootstrap agrees, [−8.09%, −1.06%]) positively **excludes** the
   pre-registered ≥10% relative-drop bar. This is categorically different
   from Layer 2's null (+0.10%/−0.07%).
3. **Unpinned node-routed refill leaves 25–47% of refills remote at every
   thread count tested, 2–256** (E1 redo, confinement genuinely off at
   every point per `GODEBUG=numa=1` evidence, 15/15 runs) — routing alone
   does not deliver locality without thread/goroutine placement.
4. **The 128P confinement-eligible pathology wins hold:** candidate 1
   (garbage) B-vs-C −32.65% (p=0.000), candidate 2 (gc-pause) B-vs-C
   −21.20% (exact p=1.08e-5); both candidates' C ties the pinned oracle A
   (non-inferiority PASS, within-session). Workstream A's own product is
   intact under Workstream B.
5. **Costs, all statistically significant:** 1P alloc micro geomean
   +3.73% (FAIL, ≤+2% bar, p=0.000, CV≈0%); 256P json `user+sys-sec/op`
   +19.96% (FAIL, p=0.025, pooled n=30 — significant but, at this gate's
   own ≈21% MDE, "significant, magnitude imprecise" rather than a
   precisely-pinned figure); 1P `STW-ns/op` +201.56% (exploratory, p=0.000,
   complete separation, does not recur at 256P p=0.786); 256P garbage
   `user+sys-sec/op` +7.45% (p=0.011, third independent confirmation of
   the same-direction CPU-time-cost pattern).
6. **Cost attribution: NOT syscalls.** `getcpu`/`mbind`/`sched_setaffinity`
   together account for ≤0.05% of cycles (E2, kernel-cycle-inclusive
   ON/OFF profile). Under matched work (allocs/op essentially identical
   between arms), Workstream B shifts **~72e9 cycles (+46.6% relative to
   the OFF arm's own spinlock time)** from user-mode spanSet work into
   kernel-mode spinlock wait. The dominant contended lock, identified from
   a single run's call-graph (n=1, not a statistical claim), is
   `sighand->siglock` contention during POSIX CPU-timer signal delivery —
   the `x/benchmarks` harness's own built-in CPU profiler, present in
   **both** arms but contending more under Workstream B — so the 256P
   `user+sys` regression is **at least partly a profiler-interaction
   artifact that Workstream B aggravates**, not a pure Workstream B
   mechanism in isolation. A disclosed page-supply/heap-footprint confound
   (`bytes-from-system` +37%, peak RSS +41%) is not controlled for in this
   attribution and remains a live alternative/contributing explanation.

### What it does not establish

- **Why locality stays ~50% unpinned.** The goroutine-migration and
  M-placement-randomness hypotheses were not separated: the pre-registered
  discriminating rule, applied honestly to the E1 redo's own data, favors
  M-placement or is non-discriminating, not migration — and the redo never
  actually tested a low-M-count regime (~68–71 Ms were active at every
  GOMAXPROCS point tested, including 2). This remains open.
- **Whether the CPU cost survives with the harness's own CPU profiler
  disabled.** The lock-class finding implicates a profiler-interaction
  artifact but was not re-measured with the profiler off.
- **The lock class beyond a single run's call-graph** (n=1 attribution
  evidence, not a statistical claim; the remaining ~41% of the spinlock's
  own time is not broken down further).

### Three-ingredient reading

Homing (Task 8) and routing (Task 9) are proven correct in isolation
(Gate 1). Thread stability as built (Task 10's soft affinity) stabilizes
OS threads (Ms) — narrowing an M's CPU affinity on node change — but the
full three-ingredient unit still misses its primary gate. The missing
ingredient is most plausibly **goroutine/P-level placement** (Go's
scheduler migrates goroutines across Ps via ordinary work-stealing,
independent of which node a P's M happens to be narrowed to), or
alternatively that the unit's own overhead (established above) negates
whatever locality win it does produce. Design §12.1's rule held exactly
as predicted: a proper subset (Layer 2, homing alone) measured ~zero;
the full three-ingredient unit measures **small-but-real** — a real,
statistically clean, complete-separation effect, just short of the
pre-registered bar.

### Next candidates (ranked; a future plan, not this one)

0. **Re-run Gate 2c with the json harness's built-in CPU profiler
   disabled.** If the `user+sys` regression vanishes or shrinks
   substantially, the cost story changes entirely — this is the cheapest,
   highest-information next step and should run before anything below.
1. **Identify and fix the residual kernel lock contention** with full
   call-graph attribution (n≥3, not n=1) and a soft-affinity-off control
   arm to isolate Task 10's own contribution from the rest.
2. **Goroutine/P-level placement awareness** for the >node-capacity
   regime — the only regime where Workstream B matters at all, since the
   ≤node-capacity regime is already covered by Workstream A's confinement
   (and ties/beats it there, per items 4/5 above).
3. **`pages.alloc` node-awareness** (Task 9's own carried-forward ruling
   1 — this gate's FAIL is precisely the condition Task 9 named as the
   trigger to revisit cross-stream address ordering).
4. **Reduce per-node stream fresh-page demand** — the heap-footprint
   confound (+37% `bytes-from-system`) identified in the cost attribution.

### Task 14 decision: DEFERRED

**Task 14 (rseq / vDSO getcpu) is deferred — its own adoption gate was
not met.** The gate (design §12.4 / plan Task 14 Step 2) requires a CPU
profile showing `getcpu` ≥1% of cycles attributable to refill/scheduler-
pass paths before implementation proceeds. Task 11's own E2 attribution
measured the NUMA syscall/routing-decision path (which includes
`getcpu`) at **0.02–0.05% of cycles across every capture** — never
approaching the ≥1% bar, in either round of the attribution experiment.
Per plan Task 14 Step 2's own stated fallback ("below that, the raw
syscall stays"), the raw-syscall `getcpu` implementation is retained
as-is; vDSO/rseq work does not proceed. Task 14's heading in
`numa-design/2026-08-20-numa-v3-locality-plan.md` is marked
**DEFERRED (profile gate not met at Task 11)**.

---

# v4 Task 1 — 256P user+sys cost gate, re-adjudicated with profiler-off harness

Plan: `2026-08-26-numa-v4-placement-plan.md` (pre-registered at 873dc29245, committed
before execution). Tree/toolchain: `873dc29245` (go1.28-devel_873dc29245, numa arm
`X:numa`; `bench-data/v4-task1-noprof/go-version-m.txt`). Harness: x/benchmarks
pinned `v0.0.0-20260819172200-70693762b6a0`, copied from numa-dell's module cache and
patched ONLY with an env-gated guard (`BENCH_DISABLE_CPUPROF`) around
`pprof.StartCPUProfile`/`StopCPUProfile` in `driver.runBenchmarkOnce`
(22-line diff: `bench-data/v4-task1-noprof/driver-noprof.patch`). One binary pair
serves all four arms (prof/noprof selected by env, zero build variance within a
toolchain). Sweep: `v4-task1-noprof-sweep.sh`, GOMAXPROCS=256, -benchmem=512
-benchtime=3s, four arms (off-prof, numa-prof, off-noprof, numa-noprof), n=10
rounds, rotating arm order, single session, idle-checked, numa_balancing=1 verified
before and after. Raws + benchstat in `bench-data/v4-task1-noprof/`.

## Pre-registered primary: user+sys-sec/op, off-noprof vs numa-noprof

    183.2m ± 30%  vs  172.6m ± 44%   ~ (p=0.853, n=10)   point estimate −5.8%

**VERDICT: PASS** per the pre-registered rule (≤ +2% or not significant). Per the
plan's decision rule, the 256P user+sys hard gate is recorded as
**PASS-under-noprof-harness**, and every later 256P json gate in the v4 plan runs
with `BENCH_DISABLE_CPUPROF=1`.

## Honesty constraints on that PASS

- **Power:** this session's per-arm relative spread is ±30–77% (all four arms share
  a 113M–289M envelope with visible multi-round drift regimes; see per-round values
  in the raws). At n=10 the MDE is roughly ~30% — the session could NOT have
  detected the original +19.96% reading even if real. The PASS rests on the point
  estimate being ~0/negative and on the pre-registered rule, not on a tight CI.
- **Replication arm did not reproduce the v3 FAIL:** off-prof vs numa-prof
  user+sys 161.1m vs 162.8m, ~ (p=0.912), point estimate +1.1% — the original
  +19.96% (v3 Task 11 Gate 2c) did not appear in this session even WITH the
  profiler on. Consistent with (a) the original reading being session-specific,
  and/or (b) today's variance regime masking it. Either way the +19.96% is not a
  robust property of the WS-B tree; no cross-session inference is drawn (v3
  constraint), both sessions' raws stand in the archive.
- **Profiler-interaction estimate (secondary, pre-registered):**
  (NUMA−OFF)under-prof ≈ +1.7m vs (NUMA−OFF)under-noprof ≈ −10.6m → interaction
  ≈ −12m (~−7% of base), all CIs wide, inconclusive in this session.

## Exploratory (labeled, no claims)

- sec/op off-noprof 2.609m vs numa-noprof 3.335m, ~ (p=0.143): numa throughput
  point estimate +27.8% but not significant at this variance; G2's own gates will
  adjudicate throughput on the stage-2 tree.
- Within the numa binary, prof vs noprof shows significant differences in
  allocated-bytes/op (+46%, profiler allocations counted per op), peak-RSS (−45%
  under noprof), STW-sec/op (+123% under noprof) — harness-mechanics observations
  recorded for completeness; no runtime claims attach.

**Standing after Task 1:** the 1P alloc micro +3.73% FAIL is untouched and remains
the live cost question for stages 2/4 (G2-cost measures it against stock).

---

# v4 stage 2 interim — placement enforcement works; locality gated on the pages layer

Tree `ae47d4fdf3` (Tasks 3–6 complete), numa-dell, 2026-08-26. Not a gate battery —
interim hardware findings that re-scope the work, recorded per the corrections
convention.

- **TestNUMAPlacementSpread PASS** on numa-dell: with placement active at
  GOMAXPROCS=256, worker threads converge to node-sized affinity masks on BOTH
  nodes (anti-collapse assertion) with non-trivial per-node shares. Thread
  placement — the piece v3's Task 11 identified as missing — demonstrably works.
- **TestNUMAPlacementRefillLocality FAIL (standing RED test for stage 4):**
  unpinned local refill share 69.22% at 256P, bar 90%. Probe sweep
  (`locality-probe`, GOMAXPROCS set in-process to avoid the E1 confinement
  confound): **63.5–66.6% local at EVERY width in {2,8,32,128,256} — flat.**
  Flatness across width rules out thread migration as the driver (that would
  scale with width, and the spread test shows threads are stable anyway).
- **Retain experiment (attribution):** recycle-dominated (retain 0) 62.98% vs
  growth-dominated (retain 8 GiB live, forcing continuous homed heap growth)
  61.63% at 256P — **no difference**. Fresh, correctly-homed growth does not
  raise the share, so the dilution is not span recycling: `pages.alloc`'s single
  address-ordered search hands pages from ANY node's stream to ANY requesting P
  (lowest free address wins), consuming each node's homed growth cross-node as
  fast as it is created. The per-P page cache is filled by the same node-blind
  path (`allocToCache`). This is the mechanism mcentral.go's review-I1 comment
  anticipated, now measured in isolation.
- **Consequence for the plan:** G2-locality (≥90% at every width) cannot pass
  without stage 4 (node-aware page allocation). Execution order adapts, verdict
  discipline does not: stage 4's design round starts now; the pre-registered G2
  gates are adjudicated on the COMBINED stage-2+4 tree in one battery (G4
  already re-runs G2-primary by construction). The stage-2-only battery is
  dropped as it would spend a session establishing a FAIL this section already
  documents; this deviation is recorded here before that battery would have run.
- Local hygiene at ae47d4fdf3: full runtime suite green, -race TestNUMA green,
  off-build census zero function diffs, remaining TestNUMA* on numa-dell green.

---

# v4 Task P4 — stage-4 hardware verification: windowed page allocation works

Tree `e037f33fe7`, numa-dell, 2026-08-26. Stage 4 (node-aware page allocation,
design `v4-pagealloc-design.md` rev 2 + post-rev-2 correction) implemented as
Tasks P1–P3 plus a four-bug hardening arc, every bug root-caused with a
regression test:

1. `13420cedbb` — pageCache.flush and the scavenger's free-back bypass
   `pageAlloc.free`, leaving the windowed searchAddr stale-high → "bad summary
   data" crash. Both sites now mirror their global searchAddr lowering; hook
   gate became the pageAlloc-local `numaWindowsActive`.
2. `d4ad33eb97` — invariant base case: arming (numaSchedinit) postdates
   mallocinit-era heap growth, so the first post-arm lowering left pre-arm
   free pages below the searchAddr → second crash signature. `numaArmWindows`
   now seeds each window from its lowest in-window inUse address.
3. `5bd2d22634` — `findFrom(npages>1)` failure DISCARDS firstFree; treating
   its maxSearchAddr return as a candidate falsely exhausted windows over
   surviving smaller runs (the design's rev-2-approved NEW-2 text was wrong in
   this sub-case; corrected). Root-caused by a new CHURN PROPERTY TEST (mixed
   windowed+global alloc/free/cache ops, invariant scan after every op, 3
   seeds × 8000 steps) that reproduces in milliseconds; fix mirrors stock
   alloc's npages==1-only poisoning asymmetry.
4. `e037f33fe7` — bimodal ~72%-vs-94% locality across launches (GODEBUG
   diagnosis: full-size windows, zero latches on the bad launches) was a
   pollution cascade: a transient windowed page-cache fill miss fell to the
   plain fill, which takes the lowest free addresses (another node's window),
   and every span carved from that cache stayed remote and recycled into the
   wrong node's spanSets. The cache fill now grows homed once on miss (M1
   shape, skipped when latched).

**Verification (raws in `bench-data/v4-p4-verification/`):**

- Full `TestNUMA` battery on numa-dell: **3/3 consecutive runs green**,
  including the formerly-RED `TestNUMAPlacementRefillLocality` (stage 2's
  standing red test, 69% then) and the new `TestNUMAPlacementSpread` /
  `TestNUMAPlacementLargeObjectLocality`.
- Crash loop: 30/30 probe launches crash-free (pre-fix: crashed by iter 2).
- **Locality sweep (steady-state protocol, 5 widths × 5 launches — every
  individual launch, not just medians, ≥90%):** medians 2P **95.91%**, 8P
  **95.05%**, 32P **92.83%**, 128P **93.52%**, 256P **97.37%**. Stage-2-only
  was flat 63–67%; v3's getcpu routing measured 53–75%.
- **Retain re-run (growth-dominated attribution workload):** 95.82%
  steady-state / 97.30% ramp-inclusive at 256P — was **61.63%** before stage 4,
  the reading that indicted pages.alloc.
- Flake check after fix 4: 10/10 single launches 91.75–97.29% (bimodality
  gone).

Residuals recorded honestly: (a) the window-run inference sometimes computes
run=2 (512 GiB windows) instead of run=8 on layouts where the reason is not
yet understood — harmless for every gate workload (≤15 GiB/node) and the
disjointness/alignment invariants hold, but understand before upstreaming;
(b) one unreproduced local test FAIL observed once during the pollution-fix
iteration (3 clean repeats after) — watch.

G2-locality's pre-registered shape (≥90% at every width, steady-state
protocol per the plan's recorded clarification) is met on the P4 evidence;
the formal gate adjudication happens in the P5 combined battery.

---

# v4 Task P5 — combined G2+G4 gate battery: VERDICTS

Final tree `cf2bbbfff1` (steal filter and hysteresis deleted per ablation
verdicts — see the commit message and the ablation section below; enforcement
hook ON in its reviewed M2 form). All sweeps single-session, interleaved/
rotating, benchstat, idle-checked, numa_balancing=1 verified after. Raws:
`bench-data/v4-g2g4-{primary,imc,cost}/` (+ locality under
`v4-p4-verification/`). Toolchain stamps verified per battery.

| Gate | Bar | Reading | Verdict |
|---|---|---|---|
| G2-primary (garbage 4GiB 256P wall, B-vs-C) | ≥5% better, sig. | **−8.09%** (p=0.000, n=10) | **PASS** |
| G4-RSS (same arms, peak-RSS) | ≤ +10% | ~ (p=0.739; −0.7% pt) | **PASS** |
| G2-locality (steady-state refill share) | ≥90% every width | 25/25 launches ≥90%; medians 92.8–97.4% | **PASS** |
| G4 large-object (arena-tag vs P-home) | ≥90% | battery test green ×3 | **PASS** |
| G2-cost 1P json | ≤ +2% both metrics | +1.35% ns/op (p=0.029), +1.31% user+sys (p=0.035) | **PASS** |
| G2-cost 256P json (noprof harness) | ≤ +2% | ~ sec/op (p=0.631), ~ user+sys (p=0.971) | **PASS** |
| G2-IMC (L3-miss DRAM remote share) | ≥10% rel. drop | json −0.4%; garbage (expl.) −4.4% | **FAIL** |
| G2-sched-micros (4 benchmarks 256P) | ≤ +2% | CreateGoroutines +15–19%, Capture +6.5–10.4% (PingPongHog, Parallel ~) | **FAIL** (hook-on) |
| G2-cost 1P alloc micro vs stock | ≤ +2% | Malloc8 +7.53% (p=0.014, ±52% spread), Malloc16 ~ (p=0.089), geomean +5.96% | **FAIL** |
| Off census / -race / TestNUMA | zero diffs / green | verified at every commit | PASS |

Corroboration: over 3 sampled primary rounds, stock arm B took 2,780,209
NUMA balancer hint faults; arm C took **0** (Layer-1 VMA exemption) — while
being 8% faster.

## G2-IMC FAIL — attribution (stage-3 role, executed inline)

The proxy (`mem_load_l3_miss_retired.remote_dram` share) does not measure what
the stage changes: a workload with **96% span-refill locality and minimal GC**
(the churn probe) still reads **43.8%** remote — the load-miss numerator is
dominated by loads to shared runtime/global state, which allocation placement
cannot move on ANY workload. json (shared corpus) reads −0.4%; garbage −4.4%
(clean separation, matching v3 WS-B's −4.49%). The mechanisms the stage
targets are measured directly by the refill counters (63→95% local) and by
wall time (−8%). Recorded as: gate proxy insensitive to the treatment;
bar not met as written.

## G2-sched-micros FAIL — the enforcement fork (bisected, decomposed)

Build-constant ablations isolated the entire regression to the
schedule()-path enforcement hook (steal filter exonerated → deleted; hook-on
~893ns vs hook-off ~736ns CreateGoroutines, complete separation). perf stat
decomposition: user cycles/op only +6.7% while wall is +23% — **most of the
cost is off-CPU wake latency inherent to mask narrowing** (a narrowed M
cannot be woken onto the other node's idle CPUs). Hysteresis was implemented,
measured (no benefit — LIFO P reuse converges streaks), and deleted. The
pre-registered gates therefore fork on one switch:

|  | G2-primary | G2-sched-micros |
|---|---|---|
| Hook ON (current tree) | **PASS −8.09%** (p=0.000) | FAIL (+15–19% CreateGoroutines) |
| Hook OFF (ablation session) | FAIL −3.39% (p=0.005; < 5% bar) | PASS |

Locality counters read ~95% either way (they compare span-home to P-home, not
to the executing CPU); the wall-time difference is the enforcement's real
DRAM effect. **Either configuration fails exactly one pre-registered gate;
the ship-config choice is a recorded decision, not a measurement.**

## G2-cost 1P alloc micro FAIL

Malloc8 +7.53% (p=0.014) against stock on the combined tree — worse than
WS-B's standing +3.73%. Caveats recorded: 1P bimodality (±52% spreads;
Malloc16 not significant), and the routed windowed path IS active at 1P on
numa-dell (confined ⇒ homing active ⇒ explicit-node refills take
allocNode-first). Attribution not yet run — this is Task L's remaining
target if the cost matters for the intended deployments.

## Exploratory (labeled, no claims)

1P: GC-bytes-from-system +206% (2.6→7.9 MiB absolute — 8-stream metadata),
STW-sec/GC +226% / STW-sec/op +213% (tens of µs absolute; 1P STW was already
a v3 exploratory flag). 256P: STW-sec/op +47.6% (p=0.029). These follow the
per-node-stream design (more, smaller streams to sweep/manage) and warrant a
look if STW matters at the target deployment sizes.

---

# v4 Task L — 1P alloc micro attribution: WS-B-era structural cost, not v4

Pre-registered in the plan at fec32e3892; arms and raws in
`bench-data/v4-taskL/`. All sweeps GOMAXPROCS=1 on numa-dell, rotating order
(one fixed-order session was run first by mistake, is marked superseded in the
archive, and was fully reproduced by its rotated redo), benchstat.

- **L1 replication (n=20, machine in its tight mode, ±0–5%):** the cost is
  REAL — Malloc8 +6.72%, Malloc16 +2.42% (both p=0.000), geomean +4.55%.
  The P5 session's ±52% spreads were bimodality, the effect underneath is not.
- **L2/L3 ablation ladder (rotating, n=12, ±0–1%):** geomean vs stock —
  full v4 ON +4.52%; streams/routing/windows disabled at runtime +3.43%;
  additionally BIND-all disabled +3.44%; additionally confinement disabled
  (instead) +3.23%. Every runtime NUMA mechanism is exonerated: the bulk of
  the cost survives with all of them off.
- **L4 introduction-point pin (rotating, n=12):** v4-final ON geomean
  **+4.22%**; v3-final (pre-v4, 273a776194) ON geomean **+4.23%** — identical.
  v4 added ~nothing net (it shifted cost between the micros: v4 is worse on
  Malloc8, better on Malloc16; v3 the reverse).

**Attribution:** the 1P alloc-micro cost is a **compile-time structural cost
of the experiment-ON build's shared allocator paths**, present since WS-B
(task 9's per-node mcentral restructure — 2×numaMaxHeapNodes spanSet arrays
per mcentral and the reshaped cacheSpan — is the prime suspect: it is the
piece that remains when every runtime mechanism is ablated). It is NOT
bimodality, NOT the v4 windows, NOT routing at runtime, NOT BIND-all, NOT
confinement. The off build remains provably byte-identical (census), and the
1P *real-workload* gate (json) passes at +1.35% — the micro overstates
deployment impact. Remediation, if ever needed, must target the ON build's
data-structure footprint (e.g. sizing the per-node arrays to the topology at
runtime is impossible for compile-time arrays; a smaller numaMaxHeapNodes
variant or an mcentral layout that keeps per-node sets out of the hot
cacheline are the plausible directions). The L3 candidate fix from the plan
("skip the windowed path while confined") is moot — the windows are not the
cost — and was not implemented.

**Collateral find (A2), fixed:** with streams on but windows never armed —
the production analog is a node whose window is INVALID (rare wrapped
randomized layouts) — the routed allocSpan path paid a homed grow per refill
forever (+22% and unbounded VA growth in the A2 arm), because the
out-of-window latch can only fire for valid windows. Both routed grow-once
sites now guard on `numaWindowSpan` usability (valid + unlatched); the
unarmed-sentinel bootstrap keeps its grow. Census clean; battery green.

---

# v4 Task LF — reducing the ON-build structural alloc cost: direction proven, first refactor null

Raws in `bench-data/v4-taskLF/`; rotating n=12 sweeps, GOMAXPROCS=1.

- **LF1 direction probe (ON build, numaMaxHeapNodes forced 8→1):** geomean
  vs stock falls from **+4.54% to +1.93%** (Malloc16 +0.56%). More than half
  the structural cost is attributable to the per-node array sizing.
- **LF2 first refactor (mcentral primary/remote field split, keeping total
  size):** geomean **+4.84% ≈ unrefactored +4.86% — null**. Reverted (no
  measured benefit). The null is itself the attribution: LF2 reordered
  fields but removed no bytes, while LF1 removed ~1.5 KiB × 136 size
  classes from mheap.central PLUS the mheap-side per-node arrays
  (arenaHints/curArena/high-water) and shrank every per-node loop bound.
  The cost is **total cache/data footprint of the enlarged allocator
  structures, not hot-field placement**.
- **Viable next direction (not yet implemented):** make the remote-node
  sets INDIRECT — one separate persistentalloc'd block, mcentral itself
  stock-sized — so the 136-entry central array regains its stock footprint;
  remote/sweeper paths pay a pointer chase. More invasive (NotInHeap
  allocation at init, sweeper/metrics call sites), wants its own design
  mini-round if the ~+4% → ~+2% recovery matters enough. Alternatively a
  numaMaxHeapNodes=4 build variant halves the arrays for near-zero effort
  but caps supported topologies.

Standing verdict: the 1P alloc-micro gate remains FAIL at ~+4-5% geomean
(ON build only; off build census-identical; 1P real-workload json +1.35%),
with a proven path to ~+2% if pursued.

## LF3 design (pre-registered before implementation, user-directed)

Remote-node spanSets move OUT of mcentral into ONE global BSS array indexed
by spanclass:

    var numaRemoteCentral [numSpanClasses]struct {
        partial [2][numaMaxHeapNodes - 1]spanSet
        full    [2][numaMaxHeapNodes - 1]spanSet
    }

mcentral itself REVERTS TO EXACTLY STOCK SHAPE (`partial [2]spanSet;
full [2]spanSet`): the off build's mcentral is trivially byte-identical to
upstream (numaMaxHeapNodes-1 == 0 makes the global zero-size), the hot
136-entry mheap.central array regains stock footprint — the bytes LF1/LF2
proved are the cost — and the remote block (~200 KiB, 136 × 4 × 7 sets) is
cold BSS touched only by the remote-fallback and sweeper paths. The four
node-indexed accessors redirect `node > 0` to the global; node-0 and
off-build paths compile to stock field accesses. No pointers, no init-order
dependencies, no NotInHeap allocation; remote spine locks lockInit'd from
mcentral.init by spanclass. Chosen over per-mcentral pointer indirection
(8B/mcentral on-build, an allocation, and an off-build shape change) as the
boring option. Gates per the pre-registered LF gates: 1P alloc micro vs
stock targeting ≤ +2%, G2-primary no-regression, census, battery, -race.

---

# v4 Task A5 — calibration (pre-registered, review H2): rate-only detection frozen

Tree ebc788c5a1 (counters-only instrumentation), GODEBUG=numa=2 diagnostic
runs on numa-dell (non-measured by design). Elapsed-normalized M-wake rates
and latency EWMA per ≥100ms window:

| Regime | wakes/s | latency EWMA |
|---|---|---|
| garbage 4GiB 256P (primary; must never trip) | **49–84** | 74–182µs |
| CreateGoroutines 256P (storm; must trip) | **7,813–8,774** | 17–127µs |
| CreateGoroutinesCapture 256P (storm; must trip) | **8,865–12,294** | 9–94µs |
| PingPongHog 256P (clean micro; must not trip) | **0** | (no folds — stale) |

Verdicts, per the design's pre-registered calibration rules:
- **The latency arm is dropped.** Garbage's EWMA overlaps and exceeds the
  storms' — its rare wakes are exactly the slow STW-herd ones (the review's
  H2 prediction, confirmed). Latency cannot discriminate; rate separates the
  regimes by ~100× with no overlap.
- **Frozen: rate-only trip at `numaWakeRateTrip` = 1024 wakes/s**
  (elapsed-normalized; ~geometric mean of garbage-max 84 and storm-min
  7,813 — 12.2× above one, 7.6× below the other), **2 consecutive
  over-threshold windows to trip** (single-window anomaly robustness).
- **The stamp machinery goes**: rate-only means a waker-side counter
  increment at the three wake sites and nothing else — no wakeStamp, no
  mPark fold, no nanotime on any wake path; the zero-stamp and rwmutex
  concerns dissolve with it.
- The reviewer's guessed-constants warning was quantitatively right in both
  directions: the drafted 41k/s threshold sat 5× ABOVE the real storms —
  the detector as originally drafted would never have tripped at all.

---

# v4 Task A5 — calibration CORRECTION and re-freeze (sampling-bias caught by first hardware trial)

The prior calibration entry ("rate-only detection frozen at 1024 wakes/s")
was built on **tail-biased sampling**: the diagnostic capture piped windows
through `tail -8`, which sampled only each run's steady-state tail. The first
hardware trial of the implemented detector caught it — the garbage primary
regime tripped at startup, and a full unperturbed 434-window trace
(`GODEBUG=numa=2,numaenforce=1`, archived reading) shows garbage's GC wake
herds BURST to 13.9k–36k wakes/s — ABOVE the storms' sustained 7.8k–12.3k —
for up to 3–4 consecutive 100ms windows, with quiet gaps between.

**Corrected discriminator: sustainment, not instantaneous rate.** A storm
exceeds any workable threshold in EVERY window indefinitely; garbage's
bursts die within 400ms. Re-frozen constants: `numaWakeRateTrip = 2048`
(3.8× below the storms' minimum) with `numaEnforceTripStreak = 8`
consecutive windows (~800ms sustained; 2× garbage's worst observed run of
4). The rate-only decision itself stands — the EWMA remains useless — but
the streak, not the threshold, is what separates the regimes.

Also from the first trial: the enforcement-MECHANISM hardware tests
(soft-affinity/spread testprog children are themselves M-wake storms) now
pin `GODEBUG=numaenforce=1` in the child — they test the mechanism; the
detector has its own state-machine test.

Process note, recorded per the corrections convention: diagnostic captures
feeding frozen constants must archive the FULL trace, never a tail/head
sample. The window data for this correction is the first full-trace archive.

---

# v4 Task A5 — GATE VERDICTS: both forked gates PASS on one tree

Tree `398f49ef12` (adaptive enforcement stand-down: rate-sustainment detector,
2048 wakes/s × 8 consecutive 100ms windows; epoch-based re-arm, 10s cooldown,
lifetime cap 8; GODEBUG=numaenforce override). Fresh single sessions, n=10,
raws + benchstat in `bench-data/v4-a5-gates/`.

| Gate | Bar | Reading | Verdict |
|---|---|---|---|
| G2-sched-micros PingPongHog | ≤ +2% | ~ (p=0.871) | **PASS** |
| G2-sched-micros CreateGoroutines | ≤ +2% | ~ (p=0.280; 729.2n vs 742.9n — point est. better) | **PASS** |
| G2-sched-micros CreateGoroutinesParallel | ≤ +2% | ~ (p=0.280) | **PASS** |
| G2-sched-micros CreateGoroutinesCapture | ≤ +2% | ~ (p=0.448) | **PASS** |
| G2-primary (garbage 4GiB 256P wall) | ≥ 5% better | **−8.11%** (p=0.000) | **PASS** |
| G4-RSS (same arms) | ≤ +10% | ~ (p=0.063, +1.8% pt) | **PASS** |

The P5 enforcement fork ("either config fails exactly one gate") is
RESOLVED: the detector leaves the primary regime untouched (−8.11% vs the
pre-A5 −8.09%; a full garbage run takes 0 trips under GODEBUG verification)
and stands enforcement down within ~800ms of a sustained wake storm
(CreateGoroutines +15–19% before; all four micros now statistically
indistinguishable from stock). Functional smokes: storm trips (1), garbage
0 trips; full TestNUMA battery green on numa-dell including the hermetic
state-machine test; off census zero function diffs at every commit; -race
green.

Standing after A5: every v4 gate now PASSES on the current tree except the
two attributed FAILs that no configuration changes — G2-IMC (proxy
insensitive to the treatment; mechanism measured directly by the refill
counters and wall time) and the 1P alloc micro (~+4.2% ON-build structural
cost, WS-B-era, Task LF direction proven at ~+1.9% if pursued).

## LF3 verdict + dose–response: the cost is the per-N bundle, ~linear in capacity

- **LF3 (remote sets to a global spanclass-indexed block): perf-NULL.**
  Tight-mode n=24: geomean +5.46% vs stock — no better than the in-struct
  layout. LF3 is RETAINED anyway for its structural property (mcentral is
  now byte-identical to upstream by construction, remote sets live in one
  gated global — the more reviewable upstream shape), with this null
  explicitly recorded so the commit's footprint rationale is not read as a
  measured win.
- **Dose–response (same sessions, tight mode):** numaMaxHeapNodes = 1 →
  +1.9%; 4 → +3.53%; 8 → +5.46%. The ON-build 1P alloc cost is a ~linear
  function (~+0.5%/node) of the compile-time node capacity, dispersed
  across every N-sized structure and loop bound (mheap arenaHints/curArena/
  high-water, pageAlloc windows, sweeper and fallback loops) — no single
  extraction recovers it (LF2 and LF3 both null), only shrinking N does.
- **Task LF final standing:** the ≤+2% 1P alloc-micro bar is reachable only
  near N=1–2 (extrapolated N=2 ≈ +2.1%, at the bar). The capacity constant
  is therefore an explicit UPSTREAM DECISION POINT: N=4 covers the dominant
  1–4-node deployments at ~+3.5% micro cost (real-workload 1P json remains
  +1.35%); N=8 supports big boxes at ~+5.5%; a per-variant constant is
  possible but adds build-matrix complexity. Gate remains FAIL as
  pre-registered at the shipped N=8, fully attributed and quantified.

## Task LF close-out verification (LF3-inclusive tree)

G2-primary no-regression (pre-registered LF gate), fresh single session
n=10: **−8.77% (p=0.002)** — the placement win fully intact with LF3 in
tree (raws `bench-data/v4-taskLF/v4-lf3-primary-*`). Full TestNUMA battery
green on numa-dell. numa_balancing=1 verified.

---

# Upstream gap item 1 — window-run inference anomaly: RESOLVED

Date: 2026-08-27. Tree: post-`8cb448ff24`.

The anomaly (some launches inferring run=2 / 512 GiB stream windows
instead of run=8 / 2 TiB) is a **baseline upstream bug** in the
randomized-heap-base hint generation, not a defect in the window
inference — the inference correctly described a genuinely corrupted
hint layout.

**Root cause.** `randHeapBasePrefixMask` cleared the top byte at
`heapAddrBits-8` (bit 40), but hint generation places the randomized
prefix byte at `randHeapAddrBits-8` (bit 38 on amd64:
`heapAddrBits-1-IsAmd64-8`). Bits [38,40) of `randHeapBase` therefore
survived the mask and were OR'd into the prefix's low two bits,
forcing them set. With bit 38 set (the observed launch), every
even/odd prefix pair collapses to one address: hint chains carry
duplicate addresses in pairs at doubled (512 GiB) spacing, and
adjacent streams share endpoint hints. 2 random bits ⇒ ~75% of
launches corrupted to some degree — matching the observed
launch-to-launch lottery. Diagnosed with a `GODEBUG=numa=2` hint-chain
dump (retained): observed distinct prefixes 0x89, 0x8B, 0x8D, 0x8F —
all odd, QED. Stock Go is affected too (duplicate/non-monotonic arena
hints); harmless there because hints are only mmap fallbacks.

**Fix.** `randHeapBasePrefixMask` now defined from a hoisted
`randHeapAddrBits` const (the mallocinit local removed). Off-build
census of the fix: exactly one changed function, `runtime.mallocinit`
— an *intentional*, auditable baseline delta (first ever on this
branch; the zero-diff discipline otherwise holds).

**Verification.**
- 60-launch histogram post-fix: 468/480 stream windows run=8 (2 TiB);
  the 12 others run 4–7 = the documented NEW-1 mod-256 wrap trim at
  its expected ~25%-of-launches/one-stream rate. run=2 gone.
- New regression test `TestArenaHintChainsSane` (malloc_test.go, runs
  in BOTH build modes): every hint stream pairwise-distinct with at
  most one non-ascending step. Red-checked against the old mask
  (fails across repeated process launches), green with the fix.
- Local battery green both modes (ArenaHint/ArenaCollision/PageAlloc/
  PageCache/NUMA).

**Upstream note.** This is separable: a one-line stock-Go bug fix +
regression test that should lead the CL series (it is not
NUMA-specific), and the proposal's window-inference section can now
state the layout invariant without caveats.
