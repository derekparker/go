// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// NUMA-windowed page allocation (GOEXPERIMENT=numa, v4 stage 4; design:
// numa-design/v4-pagealloc-design.md).
//
// This file has no build tag: every entry point is referenced only from
// call sites behind the compile-time goexperiment.Numa constant (plus
// the test harness, which is excluded from the census baseline), so the
// linker's dead-code elimination strips all of it from an experiment-off
// binary -- the off census must show zero function diffs, which is the
// reason findFrom below DUPLICATES pageAlloc.find instead of find being
// refactored into a wrapper: find, alloc, and allocToCache must stay
// byte-identical (design review M5). findFrom's behavioral equivalence
// with find is locked by a harness test, not by inspection.

package runtime

import (
	"internal/runtime/sys"
)

// numaWindowSpan returns node's stream window, and whether the windowed
// path may be used for node at all (valid window, not latched).
func (p *pageAlloc) numaWindowSpan(node int32) (lo, hi offAddr, ok bool) {
	if node < 0 || node >= numaMaxHeapNodes || p.numaWindowLatch[node] {
		return offAddr{}, offAddr{}, false
	}
	w := p.numaWindows[node]
	if !w.lo.lessThan(w.hi) {
		return offAddr{}, offAddr{}, false // lo == hi: no valid window
	}
	return w.lo, w.hi, true
}

// numaWindowOf returns the node whose stream window contains addr, or
// -1. At most numaMaxHeapNodes compares; used by the free/grow
// searchAddr-lowering hooks (design §4), whose callers gate on
// numaHeapHomingActive so experiment-on single-node hosts never pay it.
func (p *pageAlloc) numaWindowOf(addr uintptr) int32 {
	a := offAddr{addr}
	for n := int32(0); n < numaMaxHeapNodes; n++ {
		w := p.numaWindows[n]
		if w.lo.lessThan(w.hi) && w.lo.lessEqual(a) && a.lessThan(w.hi) {
			return n
		}
	}
	return -1
}

// numaWindowLower lowers window n's searchAddr to base if base lies in
// window n and below it -- the windowed mirror of the global lowering
// free and grow already do (mpagealloc.go). This is also what ARMS an
// unarmed/exhausted window (sentinel -> real address), since the
// sentinel compares above everything.
func (p *pageAlloc) numaWindowLower(base uintptr) {
	n := p.numaWindowOf(base)
	if n < 0 {
		return
	}
	if b := (offAddr{base}); b.lessThan(p.numaSearchAddr[n]) {
		p.numaSearchAddr[n] = b
	}
}

// numaUpdateSearchAddr applies the miss/hit searchAddr rule for window
// n given findFrom's candidate (design §3, review NEW-2): a failed
// search for npages proves only that no free run of >= npages exists --
// NOT that the window is empty -- so the exhausted sentinel is set only
// when the candidate itself proves nothing free remains below windowHi
// (candidate >= hi, or addr == 0 in the caller, where findFrom returned
// maxSearchAddr()). Otherwise the searchAddr rises to the candidate,
// which is a valid searchAddr by findFrom's contract and prunes the
// next windowed search. Never touches the global p.searchAddr (review
// NEW-3).
func (p *pageAlloc) numaUpdateSearchAddr(node int32, candidate, hi offAddr) {
	if !candidate.lessThan(hi) {
		p.numaSearchAddr[node] = maxSearchAddr()
		return
	}
	if p.numaSearchAddr[node].lessThan(candidate) {
		p.numaSearchAddr[node] = candidate
	}
}

// allocNode is pageAlloc.alloc constrained to node's stream window: it
// allocates npages only from [windowLo, windowHi), performing at most
// ONE windowed search per call (design P10). On any miss -- invalid or
// latched or unarmed window, or no in-window run of npages -- it
// returns ok == false WITHOUT allocating, and the caller falls back
// (homed grow, then unrestricted alloc; mheap.allocSpan, design §6).
//
// The global p.searchAddr is never written here, in either direction:
// windowed searches start at or above it and prove nothing about lower
// addresses (never raise), and a windowed miss proves nothing globally
// (never poison -- stock alloc's npages==1 poisoning must not be
// mirrored; review NEW-3).
//
// p.mheapLock must be held.
//
// Must run on the system stack because p.mheapLock must be held.
//
//go:systemstack
func (p *pageAlloc) allocNode(npages uintptr, node int32) (addr, scav uintptr, ok bool) {
	assertLockHeld(p.mheapLock)

	lo, hi, valid := p.numaWindowSpan(node)
	if !valid {
		return 0, 0, false
	}
	nsa := p.numaSearchAddr[node]
	if !nsa.lessThan(hi) {
		// Unarmed or exhausted (maxSearchAddr sentinel), or the last
		// candidate reached the window end: the O(1) steady-state miss.
		// Growth or a free into the window re-arms via numaWindowLower.
		return 0, 0, false
	}
	if chunkIndex(nsa.addr()) >= p.end {
		return 0, 0, false
	}

	// Chunk fast path, mirroring alloc: nsa satisfies the searchAddr
	// invariant (points into inUse), so its summary and chunk metadata
	// are mapped; windows are chunk-aligned, so the whole chunk is
	// in-window.
	searchAddr := minOffAddr
	if pallocChunkPages-chunkPageIndex(nsa.addr()) >= uint(npages) {
		i := chunkIndex(nsa.addr())
		if max := p.summary[len(p.summary)-1][i].max(); max >= uint(npages) {
			j, searchIdx := p.chunkOf(i).find(npages, chunkPageIndex(nsa.addr()))
			if j == ^uint(0) {
				print("runtime: max = ", max, ", npages = ", npages, "\n")
				print("runtime: searchIdx = ", chunkPageIndex(nsa.addr()), ", p.numaSearchAddr[node] = ", hex(nsa.addr()), "\n")
				throw("bad summary data")
			}
			addr = chunkBase(i) + uintptr(j)*pageSize
			searchAddr = offAddr{chunkBase(i) + uintptr(searchIdx)*pageSize}
			goto Found
		}
	}

	// Windowed slow path: one full search starting inside the window.
	{
		from := nsa
		if from.lessThan(lo) {
			from = lo
		}
		var candidate offAddr
		addr, candidate = p.findFrom(npages, from)
		if addr == 0 || (offAddr{addr + npages*pageSize - 1}).lessThan(hi) == false {
			// Miss: nothing allocated. addr == 0 makes candidate
			// maxSearchAddr() (findFrom's exhausted return), so the
			// NEW-2 rule below sets the sentinel; an out-of-window
			// result raises to the candidate or the sentinel as the
			// candidate dictates.
			p.numaUpdateSearchAddr(node, candidate, hi)
			return 0, 0, false
		}
		searchAddr = candidate
	}
Found:
	scav = p.allocRange(addr, npages)
	p.numaUpdateSearchAddr(node, searchAddr, hi)
	return addr, scav, true
}

// allocToCacheNode is pageAlloc.allocToCache constrained to node's
// stream window, with allocNode's miss semantics: an empty pageCache
// means "fall back to the plain allocToCache" and nothing was
// allocated. The global p.searchAddr is never written (in particular,
// allocToCache's find-failure poisoning at its slow path must not be
// mirrored -- review NEW-3).
//
// p.mheapLock must be held.
//
// Must run on the system stack because p.mheapLock must be held.
//
//go:systemstack
func (p *pageAlloc) allocToCacheNode(node int32) pageCache {
	assertLockHeld(p.mheapLock)

	lo, hi, valid := p.numaWindowSpan(node)
	if !valid {
		return pageCache{}
	}
	nsa := p.numaSearchAddr[node]
	if !nsa.lessThan(hi) {
		return pageCache{}
	}
	if chunkIndex(nsa.addr()) >= p.end {
		return pageCache{}
	}
	c := pageCache{}
	ci := chunkIndex(nsa.addr())
	var chunk *pallocData
	if p.summary[len(p.summary)-1][ci] != 0 {
		// Fast path: free pages at or near the windowed searchAddr.
		// The chunk is in-window (chunk-aligned windows + invariant).
		chunk = p.chunkOf(ci)
		j, _ := chunk.find(1, chunkPageIndex(nsa.addr()))
		if j == ^uint(0) {
			throw("bad summary data")
		}
		c = pageCache{
			base:  chunkBase(ci) + alignDown(uintptr(j), 64)*pageSize,
			cache: ^chunk.pages64(j),
			scav:  chunk.scavenged.block64(j),
		}
	} else {
		from := nsa
		if from.lessThan(lo) {
			from = lo
		}
		addr, candidate := p.findFrom(1, from)
		if addr == 0 || !(offAddr{addr}).lessThan(hi) {
			p.numaUpdateSearchAddr(node, candidate, hi)
			return pageCache{}
		}
		ci = chunkIndex(addr)
		chunk = p.chunkOf(ci)
		c = pageCache{
			base:  alignDown(addr, 64*pageSize),
			cache: ^chunk.pages64(chunkPageIndex(addr)),
			scav:  chunk.scavenged.block64(chunkPageIndex(addr)),
		}
	}
	cpi := chunkPageIndex(c.base)
	chunk.allocPages64(cpi, c.cache)
	chunk.scavenged.clearBlock64(cpi, c.cache&c.scav /* free and scavenged */)
	p.update(c.base, pageCachePages, false, true)
	p.scav.index.alloc(ci, uint(sys.OnesCount64(c.cache)))
	// Windowed mirror of allocToCache's final searchAddr update: last
	// page of the cached block (the block is chunk-contained, hence
	// in-window and mapped).
	p.numaUpdateSearchAddr(node, offAddr{c.base + pageSize*(pageCachePages-1)}, hi)
	return c
}

// findFrom is pageAlloc.find with the search's pruning start taken from
// the explicit `from` parameter instead of p.searchAddr. It is a
// DELIBERATE near-duplicate of find (see this file's header comment):
// find must stay byte-identical for the experiment-off census, so it is
// not refactored into a wrapper over this. Any change to find must be
// mirrored here; the harness equivalence test (findFrom(n, p.searchAddr)
// == find(n) over the find test cases) enforces the pairing.
//
// Like find: returns a base address of 0 on failure, in which case the
// returned candidate searchAddr is maxSearchAddr(); on success the
// candidate is a valid searchAddr per find's contract.
//
// p.mheapLock must be held.
func (p *pageAlloc) findFrom(npages uintptr, from offAddr) (uintptr, offAddr) {
	assertLockHeld(p.mheapLock)

	i := 0

	firstFree := struct {
		base, bound offAddr
	}{
		base:  minOffAddr,
		bound: maxOffAddr,
	}
	foundFree := func(addr offAddr, size uintptr) {
		if firstFree.base.lessEqual(addr) && addr.add(size-1).lessEqual(firstFree.bound) {
			firstFree.base = addr
			firstFree.bound = addr.add(size - 1)
		} else if !(addr.add(size-1).lessThan(firstFree.base) || firstFree.bound.lessThan(addr)) {
			print("runtime: addr = ", hex(addr.addr()), ", size = ", size, "\n")
			print("runtime: base = ", hex(firstFree.base.addr()), ", bound = ", hex(firstFree.bound.addr()), "\n")
			throw("range partially overlaps")
		}
	}

	lastSum := packPallocSum(0, 0, 0)
	lastSumIdx := -1

nextLevel:
	for l := 0; l < len(p.summary); l++ {
		entriesPerBlock := 1 << levelBits[l]
		logMaxPages := levelLogPages[l]

		i <<= levelBits[l]

		entries := p.summary[l][i : i+entriesPerBlock]

		j0 := 0
		if searchIdx := offAddrToLevelIndex(l, from); searchIdx&^(entriesPerBlock-1) == i {
			j0 = searchIdx & (entriesPerBlock - 1)
		}

		var base, size uint
		for j := j0; j < len(entries); j++ {
			sum := entries[j]
			if sum == 0 {
				size = 0
				continue
			}

			foundFree(levelIndexToOffAddr(l, i+j), (uintptr(1)<<logMaxPages)*pageSize)

			s := sum.start()
			if size+s >= uint(npages) {
				if size == 0 {
					base = uint(j) << logMaxPages
				}
				size += s
				break
			}
			if sum.max() >= uint(npages) {
				i += j
				lastSumIdx = i
				lastSum = sum
				continue nextLevel
			}
			if size == 0 || s < 1<<logMaxPages {
				size = sum.end()
				base = uint(j+1)<<logMaxPages - size
				continue
			}
			size += 1 << logMaxPages
		}
		if size >= uint(npages) {
			addr := levelIndexToOffAddr(l, i).add(uintptr(base) * pageSize).addr()
			return addr, p.findMappedAddr(firstFree.base)
		}
		if l == 0 {
			return 0, maxSearchAddr()
		}

		print("runtime: summary[", l-1, "][", lastSumIdx, "] = ", lastSum.start(), ", ", lastSum.max(), ", ", lastSum.end(), "\n")
		print("runtime: level = ", l, ", npages = ", npages, ", j0 = ", j0, "\n")
		print("runtime: from = ", hex(from.addr()), ", i = ", i, "\n")
		print("runtime: levelShift[level] = ", levelShift[l], ", levelBits[level] = ", levelBits[l], "\n")
		for j := 0; j < len(entries); j++ {
			sum := entries[j]
			print("runtime: summary[", l, "][", i+j, "] = (", sum.start(), ", ", sum.max(), ", ", sum.end(), ")\n")
		}
		throw("bad summary data")
	}

	ci := chunkIdx(i)
	j, searchIdx := p.chunkOf(ci).find(npages, 0)
	if j == ^uint(0) {
		sum := p.summary[len(p.summary)-1][i]
		print("runtime: summary[", len(p.summary)-1, "][", i, "] = (", sum.start(), ", ", sum.max(), ", ", sum.end(), ")\n")
		print("runtime: npages = ", npages, "\n")
		throw("bad summary data")
	}

	addr := chunkBase(ci) + uintptr(j)*pageSize

	searchAddr := chunkBase(ci) + uintptr(searchIdx)*pageSize
	foundFree(offAddr{searchAddr}, chunkBase(ci+1)-searchAddr)
	return addr, p.findMappedAddr(firstFree.base)
}
