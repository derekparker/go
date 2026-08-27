# v4 Stage 4 — node-aware page allocation: design (rev 2)

Status: rev 1 REJECTED by adversarial review (verdict recorded at bottom);
this rev 2 folds in every finding (C1, C2, M1–M5, m1–m5) and goes back for
re-review before implementation. Line references: tree at `2f8f8a7e1b`.

## Evidence this design answers (RESULTS.md "v4 stage 2 interim")

Placement enforcement works (spread test PASS), yet unpinned local-refill share
is flat at ~63–67% across every GOMAXPROCS width, and a growth-dominated
workload (8 GiB retained) is exactly as diluted as a recycle-dominated one.
Attribution: `pages.alloc` is one global address-ordered first-fit — per-node
arena streams put growth in per-node address regions, but the search hands the
lowest free address to ANY requesting P. The per-P page cache is filled by the
same node-blind path. `TestNUMAPlacementRefillLocality` (≥90% bar) stands RED
for this stage.

## The address-space facts (corrected per review C1)

Streams distribute the 64 heap hints as `node = i / (0x40/numaMaxHeapNodes)`
(malloc.go:710-716) — stream n owns hints i ∈ [8n, 8n+8). What ADDRESSES those
hints get is layout-dependent, and the layout that matters is NOT the
`default:` arm:

- **`randomizeHeapBase` is baseline-ON** (`RandomizedHeapBase64: true` in
  internal/buildcfg's experiment baseline; GOEXPERIMENT=numa is additive), so
  every default linux/amd64 build — including every numa-dell binary — uses
  `p = (randHeapBasePrefix + byte(i)) << (randHeapAddrBits-8) | ...`
  (malloc.go:667-669). Hint spacing is `1<<(randHeapAddrBits-8)` = **256 GiB**
  on amd64 (windows ≈ 2 TiB), and the prefix byte **wraps mod 256**: when
  `randHeapBasePrefix > 0xC0` (~25% of launches, uniform random byte), one
  stream's hints split across the top and bottom of the address range
  (non-contiguous) and stream order stops being monotone in address.
- The non-randomized `arm64`/`aix`/`default` arms have contiguous 1 TiB-spaced
  hints (aix skips i==0, leaving stream 0 with 7 hints — harmless since windows
  are hint-derived). race / riscv64-sv39 / 32-bit never get here
  (`numaHeapStreamsEnabled` false, malloc.go:649, 728).

**Consequences (C1):** windows must be computed from the ACTUAL hint addresses
as mallocinit generates them, never from an assumed layout: per stream, take
the hint addresses' longest contiguous run at the layout's actual spacing
(contiguous = consecutive hints differ by exactly the spacing). Window =
[run min, run max + spacing). A stream whose longest run is < 2 hints gets an
**invalid window** (windowed search permanently misses for that node; growth,
tagging, and metrics honesty are unaffected — tags are truth, below). The
disjointness assertion test must run across many simulated prefixes, including
wrapped ones, not just the default layout. Wrap-trimmed windows mean the
affected node reaches its out-of-run hints only via grow, whose arenas land
outside the window — the M1 latch (below) then degrades that node to today's
behavior deterministically. Accepted, documented residual: on ~25% of launches
one node's window is smaller (or, rarely, invalid), reducing that node's
windowed hit rate; the gates' locality bars absorb this because trimmed
windows still cover ≥half the stream in the common case, and the latch
prevents any pathological cost. Windows are heapArenaBytes-aligned (hint
addresses are), hence chunk-aligned: no chunk straddles a window boundary.

**Tags are the truth, windows are a heuristic:** memory outside every window
(hint-fallback growth, trimmed-off hints) is still tagged per-arena
(`numaArenaNode`) and counted honestly by the metrics; the windowed search
just never finds it (miss → fallback does).

## 1. pageAlloc additions (`mpagealloc.go` struct; code in a new
`mpagealloc_numa.go`, no build tag, all entry points referenced only from
goexperiment-gated callers so linker DCE strips them from the off binary — M5)

```go
	// numaWindows[n] is the address window of NUMA node n's heap arena
	// stream ([lo, hi), chunk-aligned, computed once in mallocinit from
	// the ACTUAL hint addresses -- see malloc.go's hint loop; lo == hi
	// means "no valid window" (wrapped randomized layout, review C1)).
	// numaSearchAddr[n] is the windowed analog of searchAddr and obeys
	// THE SAME invariant (review C2): it points into p.inUse or is
	// maxSearchAddr(); additionally "no free memory in window n below
	// it". Initialized to maxSearchAddr() (window not yet grown into /
	// exhausted); lowered by grow/free into the window; raised only to
	// findFrom candidates (which findMappedAddr keeps inside inUse) or
	// back to maxSearchAddr() (exhausted sentinel).
	numaWindows    [numaMaxHeapNodes]struct{ lo, hi offAddr }
	numaSearchAddr [numaMaxHeapNodes]offAddr

	// numaWindowLatch[n] latches true when homed growth for node n
	// landed outside node n's window (hint-run exhaustion or trimmed
	// wrap runs -- review M1): from then on the windowed path is
	// suppressed for node n entirely (window can no longer represent
	// the node's memory; arenas stay correctly tagged).
	numaWindowLatch [numaMaxHeapNodes]bool
```

## 2. `findFrom` — a SEPARATE function, `find` untouched (M5)

`find` (mpagealloc.go:654) stays byte-for-byte. `findFrom(npages uintptr, from
offAddr) (uintptr, offAddr)` duplicates its search with `from` replacing the
`p.searchAddr` reads, lives in `mpagealloc_numa.go`, and is referenced only
from `allocNode`/`allocToCacheNode` — the off binary's function census is
unchanged by construction (linker deadcode). A harness test asserts
`findFrom(n, p.searchAddr)` ≡ `find(n)` over the existing find test cases
(duplication guarded by equivalence test, not by hope).

## 3. `allocNode` (the windowed alloc; C2-corrected)

```go
// allocNode is pageAlloc.alloc constrained to node's stream window.
// At most ONE windowed search per call (P10); on any miss it returns
// ok=false WITHOUT allocating and the caller falls back. Never loops.
// p.mheapLock must be held; systemstack, like alloc.
func (p *pageAlloc) allocNode(npages uintptr, node int32) (addr, scav uintptr, ok bool)
```

- Fast-outs: invalid window (lo==hi), `numaWindowLatch[node]`,
  `numaSearchAddr[node] == maxSearchAddr()` (not grown / exhausted — the cheap
  steady-state miss, m3), `chunkIndex(numaSearchAddr[node].addr()) >= p.end`.
- Chunk fast path: only when `numaSearchAddr[node] < windowHi` AND the
  invariant holds (searchAddr points into inUse, so summary/chunk metadata is
  mapped — same justification as alloc:891-895); the chunk is in-window by
  chunk alignment of the bounds.
- Slow path: `findFrom(npages, maxOffAddr(numaSearchAddr[node], windowLo))`.
  - `addr == 0` (nothing free anywhere above from) OR
    `addr + npages*pageSize > windowHi` (everything in [from, windowHi) is
    allocated): **miss** — no `allocRange`, and set
    `numaSearchAddr[node] = maxSearchAddr()` (exhausted sentinel; sound
    because the failed search proved the window empty above `from`, and below
    `from` was already excluded by the invariant). Next free/grow into the
    window re-arms it by lowering.
  - Hit: `allocRange`; if candidate ≥ windowHi set the sentinel, else raise
    `numaSearchAddr[node]` to the candidate (valid searchAddr per find's
    contract, mpagealloc.go:644-646).
- Global `p.searchAddr` is never raised by allocNode (windowed searches start
  above it and prove nothing below — leaving it conservative is slow-only,
  never wrong; confirmed clean by review probe B).

## 4. Windowed maintenance on free and grow

Gated on `numaHeapHomingActive()` (m5 — experiment-on single-node hosts pay
nothing): after the existing global lowering (grow: mpagealloc.go:396-399;
free: the mirror in free), locate the window containing `base` (≤8 compares)
and lower `numaSearchAddr[n]` if `base` is below it. Additionally in the homed
grow path (mheap.grow with a node): if the grown range lies OUTSIDE node n's
window, set `numaWindowLatch[n]` (M1). Scavenger, summaries, chunk metadata:
untouched (P8).

## 5. Node-aware page-cache fill (`mpagealloc_numa.go`)

`allocToCacheNode(node)` mirrors `allocToCache` (mpagecache.go:119) against
`numaSearchAddr[node]`/`findFrom`, same C2 discipline, miss → empty pageCache
→ caller falls back to plain `allocToCache` under the same lock acquisition.
A filled cache never crosses a window boundary (64-page blocks are
chunk-contained, mpagecache.go:138/155; windows chunk-aligned — review probe D
clean). No drain/flush of existing caches.

## 6. Caller routing (`mheap.go` allocSpan; M3/M4 corrected)

Routing happens ONLY when ALL of: `goexperiment.Numa` (compile-time),
`numaHeapStreamsEnabled`, `numaHeapHomingActive()`, `typ == spanAllocHeap`
(M4b — allocManual/stack/workbuf/etc. untouched), and a **syscall-free node
key** is available (M3):
  (a) an explicit node argument (the mcentral refill/grow path), or
  (b) placement active and the current P has a home (byte read).
`numaAllocNodeAuto` callers WITHOUT placement stay exactly as lazy as today
(resolution at grow frequency, mheap.go:1716-1722) — **no getcpu is ever added
at allocSpan frequency**.

Direct-path order for a routed allocation (M1 — explicit, bounded):
1. `allocNode(npages, node)` — hit: done.
2. Miss: homed `h.grow(npages, node)` ONCE. If the grown range landed outside
   the window, the latch is now set (§4).
3. Retry `allocNode` ONCE. Hit: done.
4. **Unconditional fallback** to today's unrestricted sequence
   (`pages.alloc` → grow → retry) regardless of why steps 1–3 missed. One
   windowed find, one homed grow, one retry per allocSpan call, ever.
pcache path: try `allocToCacheNode(node)`, miss → plain `allocToCache`.
The needPhysPageAlign stack case (mheap.go:1402-1429) is unreachable here
(spanAllocHeap filter).

Footprint honesty (M2): homed-grow-before-unrestricted-reuse converts some
cross-node reuse into growth; eager growth-scavenge (mheap.go:1478-1500) and
the background scavenger bound RSS but the mechanism can add grow/scavenge/
refault churn and VA growth under node-skewed workloads. **G4 gains a hard
RSS gate** (plan amendment): peak-RSS-bytes on the G2-primary garbage arms,
experiment-vs-stock, ≤ +10% (benchstat, same session). If it fails, the
grow-first order (not the windowed search) is the first suspect.

## 7. Cost guards (P10, updated)

- allocNode: ≤1 windowed find per call; the maxSearchAddr sentinel makes the
  steady-state miss O(1) (m3) — full radix walks happen only when the window
  plausibly has space.
- mcache hits and pcache hits untouched; only the pcache FILL is routed.
- 1P alloc micro (standing +3.73% FAIL vs stock) is the hard gate, measured
  against STOCK on the combined tree; numa-dell 1P (confined, homing active,
  explicit-node refills) exercises the routed path.
- Off build: zero function diffs via DCE (all new code referenced only from
  gated call sites); `find`/`alloc`/`allocToCache` byte-identical (M5).
  Census verification is implementation step 1 after each wiring commit.

## 8. Tests

- Standing RED `TestNUMAPlacementRefillLocality` → GREEN ≥90%.
- Harness units (export_test.go `PageAlloc`, ~line 856; window-setter export so
  windows are exercised without real streams — m4): windowed alloc returns
  only in-window addresses; miss allocates nothing and sets the sentinel;
  sentinel re-arms on free/grow into the window; latch suppresses; findFrom ≡
  find equivalence over the existing find cases.
- Window computation: linux && goexperiment.numa unit over the REAL mallocinit
  outputs plus a pure-function test of the run-trimming over many simulated
  randomized prefixes (wrapped cases included) asserting pairwise disjointness
  and chunk alignment (C1).
- Hardware: locality probe sweep {2,8,32,128,256} + retain re-run (expect
  ≥90%); large-object locality via `NumaArenaNodeOfForTest(ptr)` comparing
  each large allocation's arena tag to the allocating P's home, bar ≥90%.
- Full battery: G2+G4 combined, one session, pre-registered bars plus G4-RSS.

## 9. Explicitly out of scope

Stack allocation and every non-heap span type (M4b), scavenger targeting,
user arenas, behavior when streams are disabled, pageCache drain/flush, chunk
metadata or summary format changes, any getcpu in allocSpan (M3).

---

## Review verdict — rev 1 (2026-08-26): REJECTED

Findings, all folded into rev 2 above: **C1** window claim false on the
default (randomizeHeapBase baseline-ON) layout — 256 GiB spacing, mod-256
prefix wrap breaks contiguity on ~25% of launches → windows now hint-derived
with longest-run trimming, invalid-window fallback, multi-prefix disjointness
tests. **C2** numaSearchAddr violated the searchAddr→inUse invariant (fault on
ungrown windows; windowHi clamp could cross into the next stream) → sentinel
discipline: init maxSearchAddr(), lower on grow/free, sentinel on
miss/exhaustion, fast path bounded by windowHi. **M1** grow-retry ambiguity →
grow-once/retry-once/unconditional-fallback + out-of-window latch. **M2** RSS
unmeasured → G4-RSS gate (peak-RSS ≤ +10%). **M3** Auto-caller keying would
add getcpu at allocSpan frequency → syscall-free keys only (explicit node or
P-home), Auto-without-placement stays lazy. **M4** locked-decision amendment
required for the grow-first order (done in the plan alongside this rev) and
spanAllocHeap-only routing. **M5** findFrom refactor would change the off
census → separate DCE'd function, find untouched, equivalence test.
m1 (8 TiB-aligned wording), m2 (aix 7-hint stream), m3 (sentinel-on-miss),
m4 (window state on pageAlloc, streams gate in callers, window-setter export),
m5 (free/grow scan gated on numaHeapHomingActive). Clean areas confirmed:
global searchAddr never raised is always-safe; pcache fill cannot cross
windows; 1P gate exercises the routed path; mcache/pcache-hit paths untouched.

## Review verdict — rev 2

(appended after re-review)
