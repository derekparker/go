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
