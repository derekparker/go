// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Central free lists.
//
// See malloc.go for an overview.
//
// The mcentral doesn't actually contain the list of free objects; the mspan does.
// Each mcentral is two lists of mspans: those with free objects (c->nonempty)
// and those that are completely allocated (c->empty).

package runtime

import (
	"internal/goexperiment"
	"internal/runtime/atomic"
	"internal/runtime/gc"
	"internal/runtime/sys"
)

// Central list of free objects of a given size.
type mcentral struct {
	_         sys.NotInHeap
	spanclass spanClass

	// partial and full contain two mspan sets each, per NUMA heap
	// arena stream (design §12.3/§12.4, task 9): one of swept in-use
	// spans, and one of unswept in-use spans, exactly as before
	// per-node routing existed -- now also indexed by the node a
	// span's home arena was grown for (heapArena.node, task 8), so
	// that a refill can search its own node's spans before any other
	// node's (see cacheSpan). These two roles still trade on each GC
	// cycle. The unswept set is drained either by allocation or by
	// the background sweeper in every GC cycle, so only two roles are
	// necessary.
	//
	// sweepgen is increased by 2 on each GC cycle, so the swept
	// spans are in partial[sweepgen/2%2] and the unswept spans are in
	// partial[1-sweepgen/2%2]. Sweeping pops spans from the
	// unswept set and pushes spans that are still in-use on the
	// swept set. Likewise, allocating an in-use span pushes it
	// on the swept set.
	//
	// Some parts of the sweeper can sweep arbitrary spans, and hence
	// can't remove them from the unswept set, but will add the span
	// to the appropriate swept list. As a result, the parts of the
	// sweeper and mcentral that do consume from the unswept list may
	// encounter swept spans, and these should be ignored.
	//
	// With the experiment off, numaMaxHeapNodes == 1 (I5) and every
	// indexed access below (via the accessor functions' node/idx
	// redirection, matching mheap.grow's own pattern) collapses to
	// the single unindexed set these fields held before task 9, both
	// in layout (a [1]spanSet array has the same size/alignment as a
	// bare spanSet) and in the literal-constant-0 indexing the
	// compiler generates for it.
	partial [2][numaMaxHeapNodes]spanSet // list of spans with a free object, per node
	full    [2][numaMaxHeapNodes]spanSet // list of spans with no free objects, per node
}

// Initialize a single central free list.
//
// Lock-rank note (I5): every per-node spanSet spine lock must be
// lockInit'd here -- 2 partial + 2 full sets x numaMaxHeapNodes, not
// just the four locks the pre-task-9 shape had -- or a
// staticlockranking build throws on the first uninitialized lock it
// sees. With the experiment off numaMaxHeapNodes == 1 and this is
// exactly the original four lockInit calls.
func (c *mcentral) init(spc spanClass) {
	c.spanclass = spc
	for i := range c.partial {
		for node := range c.partial[i] {
			lockInit(&c.partial[i][node].spineLock, lockRankSpanSetSpine)
		}
	}
	for i := range c.full {
		for node := range c.full[i] {
			lockInit(&c.full[i][node].spineLock, lockRankSpanSetSpine)
		}
	}
}

// partialUnswept returns the spanSet which holds partially-filled
// unswept spans for this sweepgen and NUMA node.
func (c *mcentral) partialUnswept(sweepgen uint32, node int32) *spanSet {
	idx := int32(0)
	if goexperiment.Numa {
		idx = node
	}
	return &c.partial[1-sweepgen/2%2][idx]
}

// partialSwept returns the spanSet which holds partially-filled
// swept spans for this sweepgen and NUMA node.
func (c *mcentral) partialSwept(sweepgen uint32, node int32) *spanSet {
	idx := int32(0)
	if goexperiment.Numa {
		idx = node
	}
	return &c.partial[sweepgen/2%2][idx]
}

// fullUnswept returns the spanSet which holds unswept spans without any
// free slots for this sweepgen and NUMA node.
func (c *mcentral) fullUnswept(sweepgen uint32, node int32) *spanSet {
	idx := int32(0)
	if goexperiment.Numa {
		idx = node
	}
	return &c.full[1-sweepgen/2%2][idx]
}

// fullSwept returns the spanSet which holds swept spans without any
// free slots for this sweepgen and NUMA node.
func (c *mcentral) fullSwept(sweepgen uint32, node int32) *spanSet {
	idx := int32(0)
	if goexperiment.Numa {
		idx = node
	}
	return &c.full[sweepgen/2%2][idx]
}

// numaSpanRefillLocal and numaSpanRefillRemote back the
// /numa/span-refills/local:spans and /numa/span-refills/remote:spans
// runtime/metrics counters (design §12.4's in-vivo locality proxy;
// review M1 controller ruling: named with the dimension in the path
// and the unit in the unit slot, not /numa/span-refills:{local,remote}
// as the plan text originally had it -- metric names freeze once
// shipped, so the upstream-correct shape is used from the start).
// Incremented only in cacheSpan, at refill frequency -- never on a
// malloc fast path. Declared unconditionally (metrics.go registers
// their names on every build, like every other runtime/metrics
// counter -- none are goexperiment-gated), but only ever written to
// when goexperiment.Numa; see cacheSpan.
var (
	numaSpanRefillLocal  atomic.Uint64
	numaSpanRefillRemote atomic.Uint64
)

// numaRefillNode returns the NUMA node mcentral.cacheSpan should route
// this span refill to, and whether that is a genuine per-node reading
// (design §12.4's routing ingredient) -- forwards directly to
// numaGrowNode (task 8), which already has exactly the contract
// routing needs: node is always a valid index into the per-node
// spanSet arrays above, even when genuine is false (streams disabled,
// a failed getcpu, or node >= numaMaxHeapNodes -- see numaGrowNode's
// doc comment for the full enumeration). Routing and growth homing
// read the node the same way; they differ only in call frequency
// (refill here, heap growth there) and in what they do with a
// non-genuine reading (routing still searches node 0's sets; growth
// declines to home -- see mcentral.grow).
//
// Called ONCE per mcentral.cacheSpan call, i.e. once per refill --
// never from getMCache or a malloc fast path (v2/v3 forbidden list).
//
// Controller ruling on plan line 103 ("no syscalls under sched.lock or
// with mp.locks != 0"): that rule is scoped to Task 10's scheduler
// hooks (schedule()/acquirep), where a syscall risks a lock-ordering
// or latency hazard against sched.lock and friends. mallocgc's
// alloc-path callers of refill (and therefore of this function) hold
// mp.locks != 0 via acquirem for the whole fast path -- but this
// getcpu call runs under exactly the same shape of contract Task 8's
// numaGrowNode already established for mheap.grow (a getcpu call made
// while h.lock is about to be or already is held, at grow frequency):
// no scheduler-affecting lock is held, and the call site is
// deliberately infrequent (refill, not malloc, frequency). Accepted;
// this is not the hazard plan line 103 was written to prevent.
func numaRefillNode() (node int32, genuine bool) {
	return numaGrowNode()
}

// Allocate a span to use in an mcache.
//
// Routes the refill by NUMA node (design §12.4): the current node is
// read ONCE per call via numaRefillNode (getcpu, at refill frequency
// only -- never from getMCache or a malloc fast path, v2/v3 forbidden
// list). The local node's sets are searched first, exactly the way
// this function's single set was always searched before per-node
// routing existed (see cacheSpanFromNode); only if the local node has
// nothing usable are the remaining nodes tried, in index order (up to
// numaGrowLoopBound, review I3); only if no node has anything does
// this fall through to mheap growth, homed to the local node when the
// reading is genuine (see grow).
//
// Review I1 controller ruling: whether a refill counts as "local" or
// "remote" (/numa/span-refills/{local,remote}:spans, design §12.4's
// in-vivo locality proxy) is decided by inspecting the ACTUAL span this
// function is about to return, not by which code path produced it.
// An earlier version assumed cacheSpanFromNode(node, ...) succeeding
// meant "found on the local node" and c.grow succeeding meant
// "freshly homed to the local node" -- both are wrong in general:
// cacheSpanFromNode's node parameter only selects which per-node
// SPANSET to search, and c.grow's mheap.alloc call frequently
// satisfies small requests from mheap's per-P page cache or an
// address-ordered pages.alloc lookup, neither of which is node-aware
// -- mheap.grow (the only place that actually homes memory to a node)
// only runs on allocSpan's base==0 fallthrough. Believing "just grew"
// meant "definitely local" systematically overcounted local refills,
// biasing this counter (and Task 11's own gate proxy, which reads it).
func (c *mcentral) cacheSpan() *mspan {
	// Deduct credit for this span allocation and sweep if necessary.
	spanBytes := uintptr(gc.SizeClassToNPages[c.spanclass.sizeclass()]) * pageSize
	deductSweepCredit(spanBytes, 0)

	traceDone := false
	trace := traceAcquire()
	if trace.ok() {
		trace.GCSweepStart()
		traceRelease(trace)
	}

	node, genuine := numaRefillNode()

	// Review NEW-2: probe the local node's partial-swept set BEFORE
	// ever registering as a sweeper, matching upstream's own
	// pre-routing behavior (the original single-set cacheSpan always
	// popped partialSwept first and only called sweep.active.begin()
	// on a miss). The I2 fix below (sharing one sweepLocker across the
	// whole search instead of one per node) initially hoisted begin()
	// to always run first, even when this free, no-sweep-cost probe
	// alone would have sufficed -- regressing the hot path relative to
	// upstream. This keeps I2's win (still at most one begin/end pair
	// per refill) without paying for it on the common no-sweep-needed
	// case.
	sg := mheap_.sweepgen
	s := c.partialSwept(sg, node).pop()

	if s == nil {
		// If we sweep spanBudget spans without finding any free
		// space, just allocate a fresh span. This limits the amount
		// of time we can spend trying to find free space and
		// amortizes the cost of small object sweeping over the
		// benefit of having a full free span to allocate from. By
		// setting this to 100, we limit the space overhead to 1%.
		//
		// This budget is shared across every node searched below (I5-style
		// off-build collapse aside, this is the same global bound the
		// pre-task-9 single-set search always had -- routing spreads the
		// same amount of sweep work across nodes rather than multiplying
		// it per node).
		//
		// TODO(austin,mknyszek): This still has bad worst-case
		// throughput. For example, this could find just one free slot
		// on the 100th swept span. That limits allocation latency, but
		// still has very poor throughput. We could instead keep a
		// running free-to-used budget and switch to fresh span
		// allocation if the budget runs low.
		spanBudget := 100

		// Review I2: begin the sweeper's process-global sweepLocker
		// ONCE for the rest of this refill and share it across every
		// node cacheSpanFromNode tries below (including a re-check of
		// the local node's own unswept sets), rather than once per
		// node (up to numaGrowLoopBound+1 begin/end pairs, each a CAS
		// pair on a process-global word -- real contention on the
		// exact box this workstream targets, under concurrent refills
		// from many Ps).
		sl := sweep.active.begin()

		// Review NEW-2 micro-nit: the local node's partial-swept set was
		// already probed (empty) just above, under the same sg -- skip
		// cacheSpanFromNode's own equivalent probe for node specifically,
		// rather than paying for an immediate, guaranteed-empty repeat of
		// the exact check this function just made. sg is threaded through
		// (not re-read inside cacheSpanFromNode) so every call below,
		// local and remote alike, agrees on the same sweepgen snapshot
		// this whole refill is operating under.
		s = c.cacheSpanFromNode(node, &spanBudget, sl, sg, true)
		if s == nil && goexperiment.Numa {
			hwm := numaGrowLoopBound()
			for other := int32(0); other <= hwm; other++ {
				if other == node {
					continue
				}
				// Review M2: this loop is not gated on remaining budget.
				// Every node's first probe (partialSwept.pop, inside
				// cacheSpanFromNode) costs no sweep budget at all --
				// stopping the whole fallback early because budget ran
				// out on an EARLIER node would skip that free check on
				// every later node for no reason. cacheSpanFromNode's own
				// internal loops still stop doing actual sweep work once
				// budget is spent.
				if s = c.cacheSpanFromNode(other, &spanBudget, sl, sg, false); s != nil {
					break
				}
			}
		}

		if sl.valid {
			sweep.active.end(sl)
		}

		if s == nil {
			trace = traceAcquire()
			if trace.ok() {
				trace.GCSweepDone()
				traceDone = true
				traceRelease(trace)
			}

			// We failed to get a span from the mcentral so get one from
			// mheap, homed to the node this refill is routing for (ties
			// task 8's per-node growth streams to this routing decision,
			// and reuses the numaRefillNode reading above instead of a
			// second getcpu call at grow time).
			s = c.grow(node, genuine)
			if s == nil {
				return nil
			}
		}
	}

	// Review I1 controller ruling (see this function's doc comment):
	// local is computed uniformly, right here at the actual success
	// exit, from the span that's actually about to be returned -- one
	// arena-metadata lookup (not getcpu), honest for every path that
	// reaches here (local pop, remote pop, or grow).
	local := !goexperiment.Numa || numaArenaNode(s.base()) == node
	if goexperiment.Numa {
		if local {
			numaSpanRefillLocal.Add(1)
		} else {
			numaSpanRefillRemote.Add(1)
		}
	}

	// At this point s is a span that should have free slots.
	if !traceDone {
		trace := traceAcquire()
		if trace.ok() {
			trace.GCSweepDone()
			traceRelease(trace)
		}
	}
	n := int(s.nelems) - int(s.allocCount)
	if n == 0 || s.freeindex == s.nelems || s.allocCount == s.nelems {
		throw("span has no free objects")
	}
	freeByteBase := s.freeindex &^ (64 - 1)
	whichByte := freeByteBase / 8
	// Init alloc bits cache.
	s.refillAllocCache(whichByte)

	// Adjust the allocCache so that s.freeindex corresponds to the low bit in
	// s.allocCache.
	s.allocCache >>= s.freeindex % 64

	return s
}

// cacheSpanFromNode searches node's partial/full spanSets for a span
// with free objects, sweeping unswept spans as needed via sl (owned
// and begun/ended once by the caller, cacheSpan -- review I2, not
// begun/ended per node here), spending at most *budget span-sweep
// attempts (decremented as it goes -- shared across every node
// cacheSpan tries, see there). Returns nil if node has nothing usable
// within the remaining budget.
//
// sg is the sweepgen every call cacheSpan makes across one refill
// shares (threaded through rather than re-read here, review NEW-2
// micro-nit) -- see below. skipSwept, when true, skips this
// function's own partialSwept probe entirely: cacheSpan's local-node
// fast path (see there) already made that exact check, under the
// same sg, immediately before calling here, so repeating it would
// just re-derive the same empty result. Only cacheSpan's very first
// call (for the local node) passes true; every remote-node fallback
// call passes false, since those nodes' partialSwept sets have not
// been probed yet.
//
// This is exactly the pre-task-9 (single, unindexed) cacheSpan search
// body, parameterized by which node's sets to search and given a
// shared sweepLocker instead of acquiring its own; see cacheSpan for
// the local-then-remote-then-grow routing order this is composed
// into.
func (c *mcentral) cacheSpanFromNode(node int32, budget *int, sl sweepLocker, sg uint32, skipSwept bool) *mspan {
	// Try partial swept spans first. This costs no sweep budget --
	// review M2 -- so it's always tried (unless skipSwept), even if
	// the shared budget is already exhausted from an earlier node.
	if !skipSwept {
		if s := c.partialSwept(sg, node).pop(); s != nil {
			return s
		}
	}

	if !sl.valid {
		return nil
	}

	// Now try partial unswept spans.
	for ; *budget >= 0; *budget-- {
		s := c.partialUnswept(sg, node).pop()
		if s == nil {
			break
		}
		if sl2, ok := sl.tryAcquire(s); ok {
			// We got ownership of the span, so let's sweep it and use it.
			sl2.sweep(true)
			return s
		}
		// We failed to get ownership of the span, which means it's being or
		// has been swept by an asynchronous sweeper that just couldn't remove it
		// from the unswept list. That sweeper took ownership of the span and
		// responsibility for either freeing it to the heap or putting it on the
		// right swept list. Either way, we should just ignore it (and it's unsafe
		// for us to do anything else).
	}
	// Now try full unswept spans, sweeping them and putting them into the
	// right list if we fail to get a span.
	for ; *budget >= 0; *budget-- {
		s := c.fullUnswept(sg, node).pop()
		if s == nil {
			break
		}
		if sl2, ok := sl.tryAcquire(s); ok {
			// We got ownership of the span, so let's sweep it.
			sl2.sweep(true)
			// Check if there's any free space.
			freeIndex := s.nextFreeIndex()
			if freeIndex != s.nelems {
				s.freeindex = freeIndex
				return s
			}
			// Add it to the swept list, because sweeping didn't give us any free space.
			c.fullSwept(sg, node).push(s)
		}
		// See comment for partial unswept spans.
	}
	return nil
}

// Return span from an mcache.
//
// s must have a span class corresponding to this
// mcentral and it must not be empty.
//
// Returns s to its home node's spanSet (design §12.4) -- NOT the
// freeing thread's current node. A span's home node is fixed at grow
// time (heapArena.node, design §12.3) and never changes, so returning
// it to its own node keeps every per-node set node-pure: a future
// refill on that node always finds spans that are actually local to
// it. Routing by the freeing thread's node instead would gradually
// mix every node's memory into whichever node happens to free the
// most, defeating the routing ingredient entirely -- and the freeing
// thread usually isn't even the one that allocated the span in the
// first place, so its current node says nothing about the span's
// address range. numaArenaNode is a plain arena-metadata lookup, not a
// getcpu call, so this has no bearing on the "getcpu only at refill"
// rule.
func (c *mcentral) uncacheSpan(s *mspan) {
	if s.allocCount == 0 {
		throw("uncaching span but s.allocCount == 0")
	}

	node := numaArenaNode(s.base())

	sg := mheap_.sweepgen
	stale := s.sweepgen == sg+1

	// Fix up sweepgen.
	if stale {
		// Span was cached before sweep began. It's our
		// responsibility to sweep it.
		//
		// Set sweepgen to indicate it's not cached but needs
		// sweeping and can't be allocated from. sweep will
		// set s.sweepgen to indicate s is swept.
		atomic.Store(&s.sweepgen, sg-1)
	} else {
		// Indicate that s is no longer cached.
		atomic.Store(&s.sweepgen, sg)
	}

	// Put the span in the appropriate place.
	if stale {
		// It's stale, so just sweep it. Sweeping will put it on
		// the right list.
		//
		// We don't use a sweepLocker here. Stale cached spans
		// aren't in the global sweep lists, so mark termination
		// itself holds up sweep completion until all mcaches
		// have been swept.
		ss := sweepLocked{s}
		ss.sweep(false)
	} else {
		if int(s.nelems)-int(s.allocCount) > 0 {
			// Put it back on the partial swept list.
			c.partialSwept(sg, node).push(s)
		} else {
			// There's no free space and it's not stale, so put it on the
			// full swept list.
			c.fullSwept(sg, node).push(s)
		}
	}
}

// grow allocates a new empty span from the heap and initializes it for
// c's size class, homed to node when genuine is true (design §12.4:
// ties task 8's per-node growth streams to this refill's routing
// decision, reusing the numaRefillNode reading cacheSpan already made
// rather than a second getcpu call at grow time).
//
// genuine mirrors numaGrowNode's own homed bool (task 8, review I1):
// false collapses the grow-homing argument to mheap's "don't home"
// sentinel, exactly as numaGrowNodeArg would from a fresh reading --
// without re-reading the node.
func (c *mcentral) grow(node int32, genuine bool) *mspan {
	npages := uintptr(gc.SizeClassToNPages[c.spanclass.sizeclass()])
	growNode := int32(numaAllocNodeNoHome)
	if genuine {
		growNode = node
	}
	s := mheap_.alloc(npages, c.spanclass, growNode)
	if s == nil {
		return nil
	}
	s.initHeapBits()
	return s
}
