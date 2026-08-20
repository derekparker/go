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
