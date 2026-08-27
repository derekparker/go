// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Test exports for the NUMA-windowed page allocation core
// (mpagealloc_numa.go, v4 stage 4). Tagged goexperiment.numa: the tests
// need numaMaxHeapNodes > 1 windows, and with the experiment off these
// exports would be the only references keeping the windowed code out of
// linker deadcode (the off census must not see it).
//
//go:build goexperiment.numa

package runtime

// SetNUMAWindow sets node's stream window on a test PageAlloc and
// resets its windowed searchAddr to the unarmed sentinel -- the state
// mallocinit establishes for real windows. Tests arm the window with
// NUMAWindowLower, simulating the grow/free lowering hooks.
func (p *PageAlloc) SetNUMAWindow(node int32, lo, hi uintptr) {
	pp := (*pageAlloc)(p)
	pp.numaWindows[node].lo = offAddr{lo}
	pp.numaWindows[node].hi = offAddr{hi}
	pp.numaSearchAddr[node] = maxSearchAddr()
	// Arm the maintenance hooks (grow/free/flush/scavenge lowering) the
	// way numaSchedinit does for the real heap.
	pp.numaWindowsActive = true
}

// NUMAWindowLower simulates the free/grow windowed searchAddr lowering
// hook (numaWindowLower) on a test PageAlloc.
func (p *PageAlloc) NUMAWindowLower(base uintptr) {
	(*pageAlloc)(p).numaWindowLower(base)
}

// SetNUMAWindowLatch sets node's out-of-window growth latch.
func (p *PageAlloc) SetNUMAWindowLatch(node int32, v bool) {
	(*pageAlloc)(p).numaWindowLatch[node] = v
}

// NUMASearchAddr returns node's windowed searchAddr.
func (p *PageAlloc) NUMASearchAddr(node int32) uintptr {
	return (*pageAlloc)(p).numaSearchAddr[node].addr()
}

// SearchAddr returns the global searchAddr (for asserting the windowed
// paths never write it -- design review NEW-3).
func (p *PageAlloc) SearchAddr() uintptr {
	return (*pageAlloc)(p).searchAddr.addr()
}

// MaxSearchAddrForTest is the unarmed/exhausted sentinel value.
func MaxSearchAddrForTest() uintptr { return maxSearchAddr().addr() }

// AllocNode runs pageAlloc.allocNode on a test PageAlloc.
func (p *PageAlloc) AllocNode(npages uintptr, node int32) (addr, scav uintptr, ok bool) {
	pp := (*pageAlloc)(p)
	systemstack(func() {
		lock(pp.mheapLock)
		addr, scav, ok = pp.allocNode(npages, node)
		unlock(pp.mheapLock)
	})
	return
}

// AllocToCacheNode runs pageAlloc.allocToCacheNode on a test PageAlloc.
func (p *PageAlloc) AllocToCacheNode(node int32) PageCache {
	pp := (*pageAlloc)(p)
	var c PageCache
	systemstack(func() {
		lock(pp.mheapLock)
		c = PageCache(pp.allocToCacheNode(node))
		unlock(pp.mheapLock)
	})
	return c
}

// Find runs the stock pageAlloc.find, and FindFrom the windowed
// findFrom, for the equivalence test locking their pairing (see
// mpagealloc_numa.go's header comment).
func (p *PageAlloc) Find(npages uintptr) (addr, searchAddr uintptr) {
	pp := (*pageAlloc)(p)
	systemstack(func() {
		lock(pp.mheapLock)
		var sa offAddr
		addr, sa = pp.find(npages)
		searchAddr = sa.addr()
		unlock(pp.mheapLock)
	})
	return
}

func (p *PageAlloc) FindFrom(npages, from uintptr) (addr, searchAddr uintptr) {
	pp := (*pageAlloc)(p)
	systemstack(func() {
		lock(pp.mheapLock)
		var sa offAddr
		addr, sa = pp.findFrom(npages, offAddr{from})
		searchAddr = sa.addr()
		unlock(pp.mheapLock)
	})
	return
}

// NumaLongestHintRunForTest exports the pure stream-run finder used by
// numaInitStreamWindows (v4 stage 4 Task P2).
func NumaLongestHintRunForTest(addrs []uintptr) (start, n int, spacing uintptr) {
	return numaLongestHintRun(addrs)
}

// NumaStreamWindowForTest returns the REAL heap's stream window for
// node, as computed by mallocinit; lo == hi means no valid window.
func NumaStreamWindowForTest(node int32) (lo, hi uintptr) {
	w := mheap_.pages.numaWindows[node]
	return w.lo.addr(), w.hi.addr()
}

// NumaFirstArenaHintForTest returns the address of node's first arena
// hint (0 if none) -- the hint growth will consume first, which the
// NEW-1 reorder guarantees is in-window whenever a valid window exists.
func NumaFirstArenaHintForTest(node int32) uintptr {
	if h := mheap_.arenaHints[node]; h != nil {
		return h.addr
	}
	return 0
}

// PallocChunkBytesForTest exports the palloc chunk size for alignment
// assertions.
func PallocChunkBytesForTest() uintptr { return pallocChunkBytes }
