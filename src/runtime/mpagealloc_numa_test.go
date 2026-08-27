// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.numa

package runtime_test

import (
	. "runtime"
	"testing"
)

// Two synthetic stream windows over a test pageAlloc: window 0 covers
// chunks [b, b+2), window 1 covers chunks [b+64, b+66) -- disjoint,
// chunk-aligned, matching the real windows' invariants (design
// numa-design/v4-pagealloc-design.md §1).
func newWindowedPageAlloc(t *testing.T, chunks map[ChunkIdx][]BitRange) (*PageAlloc, func()) {
	t.Helper()
	p := NewPageAlloc(chunks, nil)
	b := BaseChunkIdx
	p.SetNUMAWindow(0, PageBase(b, 0), PageBase(b+2, 0))
	p.SetNUMAWindow(1, PageBase(b+64, 0), PageBase(b+66, 0))
	return p, func() { FreePageAlloc(p) }
}

func TestPageAllocAllocNode(t *testing.T) {
	b := BaseChunkIdx
	p, free := newWindowedPageAlloc(t, map[ChunkIdx][]BitRange{
		b:      {}, // fully free
		b + 64: {}, // fully free
	})
	defer free()
	// Unarmed windows: sentinel => miss, nothing allocated.
	if _, _, ok := p.AllocNode(1, 0); ok {
		t.Fatal("AllocNode succeeded on an unarmed window")
	}
	if got := p.NUMASearchAddr(0); got != MaxSearchAddrForTest() {
		t.Fatalf("unarmed window searchAddr = %#x, want sentinel", got)
	}
	// Arm both windows (what the grow/free hooks do in real code).
	p.NUMAWindowLower(PageBase(b, 0))
	p.NUMAWindowLower(PageBase(b+64, 0))

	// In-window allocation, per node.
	addr0, _, ok := p.AllocNode(1, 0)
	if !ok || addr0 < PageBase(b, 0) || addr0 >= PageBase(b+2, 0) {
		t.Fatalf("AllocNode(1, 0) = %#x ok=%v, want in window 0", addr0, ok)
	}
	addr1, _, ok := p.AllocNode(1, 1)
	if !ok || addr1 < PageBase(b+64, 0) || addr1 >= PageBase(b+66, 0) {
		t.Fatalf("AllocNode(1, 1) = %#x ok=%v, want in window 1", addr1, ok)
	}

	// Latch suppresses.
	p.SetNUMAWindowLatch(0, true)
	if _, _, ok := p.AllocNode(1, 0); ok {
		t.Fatal("AllocNode succeeded on a latched window")
	}
	p.SetNUMAWindowLatch(0, false)

	// Invalid node ids miss cleanly.
	if _, _, ok := p.AllocNode(1, -1); ok {
		t.Fatal("AllocNode succeeded on node -1")
	}
}

func TestPageAllocAllocNodeExhaustion(t *testing.T) {
	b := BaseChunkIdx
	p, free := newWindowedPageAlloc(t, map[ChunkIdx][]BitRange{
		b:      {{0, PallocChunkPages}}, // window 0: fully allocated
		b + 1:  {{0, PallocChunkPages}},
		b + 64: {}, // window 1: fully free
	})
	defer free()
	p.NUMAWindowLower(PageBase(b, 0))
	p.NUMAWindowLower(PageBase(b+64, 0))
	globalBefore := p.SearchAddr()

	// Window 0 has nothing free: miss, nothing allocated, and the
	// global searchAddr is untouched (review NEW-3). The windowed
	// search's result would land in window 1 -- allocNode must reject
	// it, not return node 1's memory as a node-0 hit.
	if addr, _, ok := p.AllocNode(1, 0); ok {
		t.Fatalf("AllocNode(1, 0) = %#x on an exhausted window", addr)
	}
	if got := p.SearchAddr(); got != globalBefore {
		t.Fatalf("global searchAddr moved on windowed miss: %#x -> %#x", globalBefore, got)
	}
	// Exhaustion sets the sentinel (candidate lands past windowHi):
	// the next miss is the O(1) fast-out.
	if got := p.NUMASearchAddr(0); got != MaxSearchAddrForTest() {
		t.Fatalf("exhausted window searchAddr = %#x, want sentinel", got)
	}
	// A free into the window re-arms it.
	p.Free(PageBase(b, 4), 1)
	p.NUMAWindowLower(PageBase(b, 4))
	addr, _, ok := p.AllocNode(1, 0)
	if !ok || addr != PageBase(b, 4) {
		t.Fatalf("AllocNode(1, 0) after re-arm = %#x ok=%v, want %#x", addr, ok, PageBase(b, 4))
	}
	// Window 1 was never touched by any of this.
	if a1, _, ok := p.AllocNode(1, 1); !ok || a1 < PageBase(b+64, 0) || a1 >= PageBase(b+66, 0) {
		t.Fatalf("AllocNode(1, 1) = %#x ok=%v after window-0 churn", a1, ok)
	}
}

func TestPageAllocAllocNodeCandidateOnMiss(t *testing.T) {
	// Review NEW-2: a failed search for npages > 1 must NOT set the
	// exhausted sentinel when smaller in-window runs remain -- the
	// searchAddr rises to the candidate instead, and a later smaller
	// request still hits in-window.
	b := BaseChunkIdx
	p, free := newWindowedPageAlloc(t, map[ChunkIdx][]BitRange{
		// Window 0: everything allocated except one free page at
		// page 100 of the first chunk.
		b:      {{0, 100}, {101, PallocChunkPages - 101}},
		b + 1:  {{0, PallocChunkPages}},
		b + 64: {}, // window 1 free, so the global search has somewhere to land
	})
	defer free()
	p.NUMAWindowLower(PageBase(b, 100))
	p.NUMAWindowLower(PageBase(b+64, 0))

	// No 4-page run exists in window 0: miss...
	if addr, _, ok := p.AllocNode(4, 0); ok {
		t.Fatalf("AllocNode(4, 0) = %#x, want miss (no 4-page run in window)", addr)
	}
	// ...but the single free page keeps the window armed (candidate,
	// not sentinel)...
	if got := p.NUMASearchAddr(0); got == MaxSearchAddrForTest() {
		t.Fatal("miss with a remaining in-window free page set the exhausted sentinel (NEW-2)")
	}
	// ...and a 1-page request finds it.
	addr, _, ok := p.AllocNode(1, 0)
	if !ok || addr != PageBase(b, 100) {
		t.Fatalf("AllocNode(1, 0) = %#x ok=%v, want %#x", addr, ok, PageBase(b, 100))
	}
}

func TestPageAllocAllocToCacheNode(t *testing.T) {
	b := BaseChunkIdx
	p, free := newWindowedPageAlloc(t, map[ChunkIdx][]BitRange{
		b:      {},
		b + 1:  {{0, PallocChunkPages}},
		b + 64: {},
	})
	defer free()
	p.NUMAWindowLower(PageBase(b, 0))
	p.NUMAWindowLower(PageBase(b+64, 0))
	globalBefore := p.SearchAddr()

	c := p.AllocToCacheNode(0)
	if c.Empty() {
		t.Fatal("AllocToCacheNode(0) empty on a free window")
	}
	if base := c.Base(); base < PageBase(b, 0) || base >= PageBase(b+2, 0) {
		t.Fatalf("AllocToCacheNode(0) base %#x outside window 0", base)
	}
	if got := p.SearchAddr(); got != globalBefore {
		t.Fatalf("global searchAddr moved on windowed cache fill: %#x -> %#x", globalBefore, got)
	}

	// Exhaust window 0's free chunk via the cache path until it
	// misses; the miss must not poison the global searchAddr (stock
	// allocToCache's find-failure poisoning must not be mirrored).
	for i := 0; i < PallocChunkPages/64+1; i++ {
		if c := p.AllocToCacheNode(0); c.Empty() {
			break
		}
	}
	if got := p.SearchAddr(); got != globalBefore {
		t.Fatalf("global searchAddr moved on windowed cache miss: %#x -> %#x", globalBefore, got)
	}
}

func TestPageAllocFindFromEquivalence(t *testing.T) {
	// findFrom is a deliberate near-duplicate of find (census
	// constraint); this test locks the pairing: findFrom(n,
	// searchAddr) must behave exactly like find(n) on varied heap
	// shapes. Any future edit to find that is not mirrored fails here.
	b := BaseChunkIdx
	configs := []map[ChunkIdx][]BitRange{
		{b: {}},
		{b: {{0, PallocChunkPages}}, b + 1: {}},
		{b: {{0, 1}, {2, 5}, {10, 100}}, b + 1: {{0, PallocChunkPages / 2}}},
		{b: {{0, PallocChunkPages}}, b + 64: {{5, 10}}},
		{b: {{0, PallocChunkPages - 1}}},
	}
	for ci, chunks := range configs {
		p := NewPageAlloc(chunks, nil)
		for _, npages := range []uintptr{1, 2, 3, 4, 7, 16, 64, 129, PallocChunkPages, PallocChunkPages + 1} {
			wantAddr, wantSA := p.Find(npages)
			gotAddr, gotSA := p.FindFrom(npages, p.SearchAddr())
			if gotAddr != wantAddr || gotSA != wantSA {
				t.Errorf("config %d npages %d: findFrom = (%#x, %#x), find = (%#x, %#x)",
					ci, npages, gotAddr, gotSA, wantAddr, wantSA)
			}
		}
		FreePageAlloc(p)
	}
}

func TestNUMALongestHintRun(t *testing.T) {
	const d = uintptr(1) << 38 // randomized-layout spacing on amd64
	mk := func(base uintptr, n int) []uintptr {
		out := make([]uintptr, n)
		for i := range out {
			out[i] = base + uintptr(i)*d
		}
		return out
	}
	tests := []struct {
		name      string
		addrs     []uintptr
		wantStart int
		wantLen   int
		wantD     uintptr
	}{
		{"full8", mk(0x1000000000, 8), 0, 8, d},
		{"aix7", mk(0x2000000000, 7), 0, 7, d},
		// Wrap after 3 hints: high-prefix tail wraps to low addresses.
		// The two runs are 3 and 5 long; longest wins.
		{"wrapAt3", append(mk(0xff00000000000, 3), mk(0x10000000000, 5)...), 3, 5, d},
		// Wrap after 6: longest is the leading 6.
		{"wrapAt6", append(mk(0xff00000000000, 6), mk(0x10000000000, 2)...), 0, 6, d},
		{"single", mk(0x1000000000, 1), 0, 1, 0},
		{"twoRunsTie", append(mk(0xff00000000000, 4), mk(0x10000000000, 4)...), 0, 4, d},
	}
	for _, tt := range tests {
		start, n, sp := NumaLongestHintRunForTest(tt.addrs)
		if start != tt.wantStart || n != tt.wantLen || sp != tt.wantD {
			t.Errorf("%s: got (start=%d len=%d d=%#x), want (start=%d len=%d d=%#x)",
				tt.name, start, n, sp, tt.wantStart, tt.wantLen, tt.wantD)
		}
	}
	// Simulated wrapped-prefix sweep (design C1): for every wrap point,
	// the derived window [addrs[start], addrs[start+len-1]+d) must be
	// disjoint from a neighboring stream's window built the same way.
	for wrap := 1; wrap < 8; wrap++ {
		s0 := append(mk(0xfff0000000000, wrap), mk(0x8000000000, 8-wrap)...)
		s1 := mk(0x8000000000+8*d, 8) // next stream, contiguous
		st0, n0, d0 := NumaLongestHintRunForTest(s0)
		st1, n1, d1 := NumaLongestHintRunForTest(s1)
		if n0 < 1 || n1 != 8 {
			t.Fatalf("wrap=%d: run lengths %d, %d", wrap, n0, n1)
		}
		lo0, hi0 := s0[st0], s0[st0+n0-1]+d0
		lo1, hi1 := s1[st1], s1[st1+n1-1]+d1
		if lo0 < hi1 && lo1 < hi0 {
			t.Errorf("wrap=%d: windows overlap: [%#x,%#x) vs [%#x,%#x)", wrap, lo0, hi0, lo1, hi1)
		}
	}
}

func TestPageAllocAllocNodeAfterCacheFlush(t *testing.T) {
	// Regression (v4 stage 4, hit on numa-dell): pageCache.flush frees
	// pages WITHOUT pageAlloc.free, so it must lower the windowed
	// searchAddr too. Without that, the windowed searchAddr goes
	// stale-high after a flush below it, and the next windowed search
	// -- whose summaries correctly promise the flushed-free run below
	// `from` -- throws "bad summary data".
	b := BaseChunkIdx
	p, free := newWindowedPageAlloc(t, map[ChunkIdx][]BitRange{
		b:      {},
		b + 1:  {},
		b + 64: {},
	})
	defer free()
	p.NUMAWindowLower(PageBase(b, 0))

	// Fill a cache from the window's low block, then allocate past it
	// so the windowed searchAddr moves up.
	c := p.AllocToCacheNode(0)
	if c.Empty() {
		t.Fatal("cache fill failed")
	}
	if _, _, ok := p.AllocNode(64, 0); !ok {
		t.Fatal("post-cache alloc failed")
	}
	nsaBefore := p.NUMASearchAddr(0)
	// Flush the cache: its pages (below the windowed searchAddr) go
	// free without passing through pageAlloc.free.
	c.Flush(p)
	if got := p.NUMASearchAddr(0); got >= nsaBefore {
		t.Fatalf("windowed searchAddr not lowered by cache flush: %#x -> %#x", nsaBefore, got)
	}
	// The windowed search must now see the flushed pages instead of
	// throwing on the summary/searchAddr mismatch.
	addr, _, ok := p.AllocNode(2, 0)
	if !ok || addr < PageBase(b, 0) || addr >= PageBase(b+2, 0) {
		t.Fatalf("AllocNode(2, 0) after flush = %#x ok=%v", addr, ok)
	}
}
