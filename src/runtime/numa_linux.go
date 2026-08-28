// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goexperiment"
	"internal/runtime/atomic"
	"internal/runtime/numa"
	"internal/runtime/syscall/linux"
	"unsafe"
)

// Mempolicy constants. See numaSetProcessBindAll.
//
// The allowed-nodes narrowing lives entirely here, in
// numaSetProcessBindAll, not in numaTopology/internal/runtime/numa: that
// package's Topology.NumAllowedNodes is never narrowed below NumNodes --
// the two are equal by construction for any Topology it produces
// (see Topology.NumAllowedNodes's own doc comment). Instead,
// numaSetProcessBindAll reads the process's real memory policy via
// get_mempolicy(MPOL_F_MEMS_ALLOWED) directly and publishes the result
// as numaAllowedNodemask, a value entirely separate from numaTopology.
//
// numaMaxNode is the maxnode argument passed to get_mempolicy,
// set_mempolicy, and mbind, on every architecture: a fixed 65. It is
// NEVER derived from numaNodemaskBits (which is 32 on 32-bit platforms:
// 386, arm, mips, mipsle) and never derived from numa.MaxNodes (64).
//
// The kernel's get_nodes()/copy_nodes_to_user() decrements maxnode and
// then sizes its destination write as BITS_TO_LONGS(maxnode-1) kernel
// ulongs; for maxnode=65 that's BITS_TO_LONGS(64), which is 8 bytes
// regardless of the calling process's own word size (2 32-bit ulongs or
// 1 64-bit ulong -- both 8 bytes). Passing maxnode=numaNodemaskBits+1
// (33, not 65) on a 32-bit arch would still make the kernel round its
// copy up to that same 8-byte length while our destination was a single
// 4-byte uintptr: a 4-byte out-of-bounds kernel write. numaNodemask below
// sizes the actual destination buffer to match this 8-byte requirement on
// every arch (numaNodemaskWords native-uintptr words: 1 on 64-bit, 2 on
// 32-bit).
const numaMaxNode = 65

const (
	_MPOL_PREFERRED      = 1
	_MPOL_BIND           = 2
	_MPOL_F_MEMS_ALLOWED = 4      // get_mempolicy flag: return the kernel's allowed-node mask
	_MPOL_MODE_FLAGS     = 0xe000 // MPOL_F_NUMA_BALANCING (1<<13) | MPOL_F_RELATIVE_NODES (1<<14) | MPOL_F_STATIC_NODES (1<<15): optional flag bits get_mempolicy may OR into its returned mode

	numaNodemaskBits = 8 * unsafe.Sizeof(uintptr(0))
	// numaNodemaskWords is the number of native uintptr words needed to
	// hold a numaMaxNode(65)-bit-capable nodemask, as required above: 1
	// word on 64-bit platforms, 2 on 32-bit.
	numaNodemaskWords = 64 / numaNodemaskBits
)

// numaNodemask is the on-the-wire nodemask buffer for get_mempolicy,
// set_mempolicy, and mbind. word[0] holds node ids [0, numaNodemaskBits);
// on 32-bit platforms only, word[1] holds node ids [numaNodemaskBits,
// 64). Word order matches the kernel's ulong-array indexing (low bits
// first), so this is correct independent of byte endianness.
type numaNodemask [numaNodemaskWords]uintptr

// numaNodemaskPopcount returns the number of set bits across all words of
// *m.
func numaNodemaskPopcount(m *numaNodemask) int {
	n := 0
	for _, w := range m {
		for w != 0 {
			w &= w - 1
			n++
		}
	}
	return n
}

// numaAllowedNodemask is the BIND-all nodemask published by
// numaSetProcessBindAll: bit 1<<id is set for each allowed NUMA node id
// in [0, numaNodemaskBits) -- all 64 representable ids on 64-bit
// platforms, but only ids 0-31 on 32-bit platforms (386, arm, mips,
// mipsle). This is a deliberate narrowing on 32-bit platforms only: the
// task-wide policy set by set_mempolicy in numaSetProcessBindAll always
// covers the full 64-id range via the (up to) 2-word numaNodemask
// buffer, but this single-word atomic cannot. A 32-bit host with more
// than 32 NUMA nodes is not a configuration the BIND-all policy targets.
//
// Zero until numaSetProcessBindAll both computes a mask with at least 2
// bits set (see the single-node and fallback-footgun guards there) and
// successfully calls set_mempolicy: a failed or skipped set_mempolicy
// never gets published.
var numaAllowedNodemask atomic.Uintptr

// numaTopology is the machine's NUMA topology, discovered by
// numaInitTopology during schedinit. It is only populated when
// GOEXPERIMENT=numa is set; otherwise it is left at its zero value, which
// numaTopology.NumNodes == 0 correctly represents as "unknown" rather than
// silently claiming a single node.
//
// Read-only after schedinit.
var numaTopology numa.Topology

// numaScratch is I/O scratch space for numaInitTopology. It is only used
// during schedinit's single call to numaInitTopology, but is kept as a
// package-level array (rather than a stack allocation) since it is large
// (numa.ScratchSize == 8192 bytes) and schedinit runs before the stack
// growth machinery it would otherwise depend on is fully set up.
var numaScratch [numa.ScratchSize]byte

// numaSchedinit discovers the machine's NUMA topology. It is called once
// from schedinit, after debug vars have been parsed.
//
// numaSchedinit is a no-op unless GOEXPERIMENT=numa is set: with the
// experiment off, this function must not change observable runtime
// behavior at all.
func numaSchedinit() {
	if !goexperiment.Numa {
		return
	}
	numaInitTopology()
	numaSetProcessBindAll()
	if debug.numa > 0 {
		println("numa: nodes", numaTopology.NumNodes, "allowed", numaTopology.NumAllowedNodes)
	}
}

// numaInitTopology fills numaTopology by reading the machine's NUMA
// topology from sysfs. If topology discovery fails (e.g. no NUMA sysfs, or
// a sysfs file too large for numaScratch), numaTopology is reset to its
// zero value and then set to a single-node topology: NumNodes and
// NumAllowedNodes are set to 1, and every CPU maps to node 0 (NodeOfCPU
// returns 0), matching Topology's zero-value CPUToNode entries and the
// !linux stub's policy in internal/runtime/numa. This is a conservative
// fallback: everything built on top of topology discovery must treat
// "1 node" as "no NUMA-aware behavior available", never crash.
func numaInitTopology() {
	if err := numa.ReadTopology(&numaTopology, numaScratch[:]); err != nil {
		if debug.numa > 0 {
			println("numa: topology discovery failed:", err.Error())
		}
		numaTopology = numa.Topology{}
		numaTopology.NumNodes = 1
		numaTopology.NumAllowedNodes = 1
		numaTopology.Nodes[0].ID = 0
	}
}

// numaSetProcessBindAll sets this process's task memory policy to
// MPOL_BIND over every NUMA node it is currently allowed to allocate
// from, and publishes the resulting mask in numaAllowedNodemask.
//
// It is called once from numaSchedinit, after numaInitTopology.
// BIND-all only, never MPOL_PREFERRED, and no STW toggle.
//
// A bind-to-everything policy changes no placement decision -- the
// kernel may still allocate this process's pages from any node it was
// already allowed to use -- but an explicitly configured memory policy
// opts the memory it covers out of automatic NUMA balancing: the
// kernel's balancer only scans and migrates memory whose placement is
// still policy-default. The runtime is asserting: it will handle
// locality; stop paying hint faults on its behalf.
//
// Global constraint: on a single-node host (or when topology discovery
// only ever found one node), runtime behavior must be identical to stock
// Go. numaSetProcessBindAll returns immediately in that case, before any
// syscall, leaving numaAllowedNodemask at zero.
//
// Stand-down constraint: if topology discovery observed a node id >=
// numa.MaxNodes (64) -- numaTopology.TruncatedNodes -- this host has more
// NUMA nodes than Topology, and therefore this function's nodemask, can
// represent. numaSetProcessBindAll returns immediately in that case too,
// again before any syscall and leaving numaAllowedNodemask unpublished:
// binding the process to only nodes 0-63 would silently exclude the
// dropped nodes' memory from the policy, which is worse than leaving the
// NUMA experiment inactive on this host.
//
// The allowed-node mask is read from the kernel itself
// (get_mempolicy(MPOL_F_MEMS_ALLOWED)) rather than derived from
// numaTopology: this automatically respects cpusets (e.g. a container's
// cpuset.mems) with no parsing, and includes CPU-less memory nodes. If
// the get_mempolicy call fails, numaTopology's allowed-node list is used
// as a fallback -- but if topology discovery itself failed,
// numaInitTopology already installed a synthesized single-node topology,
// and the rebuilt mask would be exactly {node0}; BIND-ing a genuinely
// multi-node host down to one node would be actively harmful, not merely
// unhelpful. So regardless of which path produced it: if the final mask
// has fewer than 2 bits set, set_mempolicy is never called and
// numaAllowedNodemask is left unpublished. set_mempolicy's own result is
// checked too -- a failing set_mempolicy also leaves numaAllowedNodemask
// unpublished.
//
// Staleness: this function is called once, at startup. It is never
// called from the heap-growth path or on any timer, so
// numaAllowedNodemask is a snapshot of cpuset.mems as of that moment,
// not a live view. A container that repartitions its cpuset mid-run is
// not observed until a process restart; see numaBindArena's doc comment
// for why re-checking more often is not worth it. This staleness is
// accepted by design.
func numaSetProcessBindAll() {
	if numaTopology.NumNodes < 2 {
		return
	}
	if numaTopology.TruncatedNodes {
		// This host has NUMA nodes beyond what Topology (and this
		// function's nodemask) can represent. Stand down rather than
		// silently bind to a truncated subset of nodes -- see the
		// doc comment above.
		return
	}

	var mask numaNodemask
	_, _, errno := linux.Syscall6(linux.SYS_GET_MEMPOLICY, 0, uintptr(unsafe.Pointer(&mask[0])), numaMaxNode, 0, uintptr(_MPOL_F_MEMS_ALLOWED), 0)
	if errno != 0 {
		mask = numaNodemask{}
		for i := int32(0); i < numaTopology.NumAllowedNodes; i++ {
			id := numaTopology.AllowedNode(i)
			if id < 0 || uintptr(id) >= 64 {
				continue
			}
			mask[uintptr(id)/numaNodemaskBits] |= 1 << (uintptr(id) % numaNodemaskBits)
		}
	}

	if numaNodemaskPopcount(&mask) < 2 {
		return
	}

	if _, _, errno := linux.Syscall6(linux.SYS_SET_MEMPOLICY, uintptr(_MPOL_BIND), uintptr(unsafe.Pointer(&mask[0])), numaMaxNode, 0, 0, 0); errno != 0 {
		return
	}
	numaAllowedNodemask.Store(mask[0])
}

// numaBindArena sets MPOL_BIND, over the same allowed-node mask
// numaSetProcessBindAll computed, as the VMA policy for the heap arena
// range [addr, addr+size). No span bookkeeping, no mcache/m.numaNode
// tracking, no per-M node lookup: one uniform BIND-all mbind per chunk,
// unconditionally.
//
// The task-wide policy set by numaSetProcessBindAll is per-thread state:
// it covers the thread that set it and every thread cloned from it
// afterwards -- which is every thread the runtime itself creates -- but
// it never reaches threads that already existed before the runtime
// initialized, such as C threads created by a cgo constructor. A VMA
// policy is per-range, not per-thread: it exempts [addr, addr+size)
// from automatic NUMA balancing against every thread that touches it,
// no matter where that thread came from. This call is what makes the
// balancer exemption hold process-wide.
//
// numaBindArena is called from mheap.grow, with h.lock held, immediately
// after each sysMap of newly-backed heap memory (mmap with MAP_FIXED
// resets any VMA policy the range previously had). It must stay
// nosplit-safe and add nothing slower than one mbind syscall under that
// lock: no allocation, no lock acquisition, no other syscalls. mheap.grow's
// true granularity is a palloc chunk (~4 MiB), not a 64 MiB arena, so this
// runs once per ~4 MiB of heap growth -- rare relative to malloc, but far
// more often than "per arena".
//
// Init-order guard: mheap.grow runs before numaSchedinit (goargs/goenvs
// allocate heap memory before finishDebugVarsSetup, which precedes
// numaSchedinit, in schedinit). Until numaSetProcessBindAll publishes a
// non-empty numaAllowedNodemask, numaBindArena no-ops rather than issuing
// an mbind with an empty mask (which the kernel would silently reject
// with EINVAL). Those early-grown ranges carry no VMA policy of their
// own; they are balancer-exempt only via the task-wide policy set by
// numaSetProcessBindAll, which is why that call is load-bearing and not
// just a belt-and-suspenders duplicate of the arena mbind.
//
// numaBindArena does not catch up already-mapped ranges once the mask
// becomes available: those early ranges stay covered by the task-wide
// policy alone (see above), and a catch-up walk was judged not worth
// its complexity.
//
// numaAllowedNodemask is read here, never refreshed: this function does
// not call numaSetProcessBindAll (or otherwise re-query cpuset.mems)
// itself. Two reasons. First, this is the heap-growth path -- it runs
// under h.lock, nosplit, once per ~4 MiB grow -- and adding a
// get_mempolicy/set_mempolicy syscall pair here to check whether the
// cpuset moved would put syscalls exactly where the design goes out of
// its way to avoid them. Second, there is no portable notification a
// cgroup's cpuset.mems/cpuset.cpus changed that the runtime could block
// on instead of polling; polling on every grow would just reintroduce the
// same cost. So a cpuset narrowed after this process's mask was last
// read is stale here until a process restart -- accepted by design, not
// a bug.
//
// mbind errors are ignored: this is deliberate (see the design
// rationale above -- the policy is an exemption hint, and there is no
// useful recovery from a failed mbind on this path), not a
// silently-swallowed bug.
//
// Scavenger interaction: VMA policies survive sysUnused (MADV_FREE /
// MADV_DONTNEED); pages that refault after being scavenged are re-placed
// under the surviving policy, so scavenged-and-reused ranges need no
// re-mbind here.
//
//go:nosplit
func numaBindArena(addr unsafe.Pointer, size uintptr) {
	w0 := numaAllowedNodemask.Load()
	if w0 == 0 {
		return
	}
	var mask numaNodemask
	mask[0] = w0
	linux.Syscall6(linux.SYS_MBIND, uintptr(addr), size, uintptr(_MPOL_BIND), uintptr(unsafe.Pointer(&mask[0])), numaMaxNode, 0)
}
