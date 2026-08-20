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

// Layer 1 mempolicy constants. See numaSetProcessBindAll and numaBindArena.
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
	_MPOL_MODE_FLAGS     = 0xc000 // MPOL_F_STATIC_NODES | MPOL_F_RELATIVE_NODES: optional flag bits get_mempolicy may OR into its returned mode

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
// numaSetProcessBindAll, consumed by numaBindArena's nosplit fast path:
// bit 1<<id is set for each allowed NUMA node id in [0, numaNodemaskBits)
// -- all 64 representable ids on 64-bit platforms, but only ids 0-31 on
// 32-bit platforms (386, arm, mips, mipsle). This is a deliberate
// narrowing on 32-bit platforms only: the task-wide policy set by
// set_mempolicy in numaSetProcessBindAll always covers the full 64-id
// range via the (up to) 2-word numaNodemask buffer, but this single-word
// atomic cannot. A 32-bit host with more than 32 NUMA nodes is not a
// configuration Layer 1 targets; such a host still gets correct
// task-policy behavior, just without the (redundant, in that case) arena
// VMA policy for node ids >= 32.
//
// Zero until numaSetProcessBindAll both computes a mask with at least 2
// bits set (see the single-node and fallback-footgun guards there) and
// successfully calls set_mempolicy. numaBindArena relies on this as an
// init-order guard (mheap.grow can run before numaSchedinit) and it also
// means a failed or skipped set_mempolicy never gets published: every
// mheap.grow would otherwise pay a guaranteed-failing (or wrongly-scoped)
// mbind forever.
var numaAllowedNodemask atomic.Uintptr

// numaPreferredCalls counts every Layer 2 MPOL_PREFERRED mbind attempted by
// numaBindArena (attempted, not necessarily kernel-accepted -- mbind errors
// are ignored there just as they are for the Layer 1 MPOL_BIND-all call;
// see numaBindArena's doc comment). Exported read-only for tests via
// NumaPreferredBindCalls in export_numa_test.go.
var numaPreferredCalls atomic.Uint32

// numaTopology is the machine's NUMA topology, discovered by
// numaInitTopology during schedinit. It is only populated when
// GOEXPERIMENT=numa is set; otherwise it is left at its zero value, which
// numaTopology.NumNodes == 0 correctly represents as "unknown" rather than
// silently claiming a single node.
//
// Layer 0: read-only after schedinit; nothing consumes it yet besides
// diagnostics and tests.
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
// fallback: layers built on top of Layer 0 must treat "1 node" as "no
// NUMA-aware behavior available", never crash.
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

// numaCurrentNode returns the id of the NUMA node the calling thread is
// currently running on, via the getcpu(2) syscall (through the
// numaGetCPUNode wrapper -- see numa_linux_getcpu.go for why this file
// never calls getcpu directly).
//
// It returns -1 if the experiment is off, if topology discovery found only
// one (or zero) nodes, or if the getcpu syscall itself fails.
func numaCurrentNode() int32 {
	if !goexperiment.Numa || numaTopology.NumNodes < 2 {
		return 0
	}
	node, ok := numaGetCPUNode()
	if !ok {
		return -1
	}
	return int32(node)
}

// numaSetProcessBindAll sets this process's task memory policy to
// MPOL_BIND over every NUMA node it is currently allowed to allocate
// from, and publishes the resulting mask in numaAllowedNodemask for
// numaBindArena to consume.
//
// It is called once from numaSchedinit, after numaInitTopology. Layer 1
// only: BIND-all, never MPOL_PREFERRED, and no STW toggle.
//
// Global constraint: on a single-node host (or when topology discovery
// only ever found one node), runtime behavior must be identical to stock
// Go. numaSetProcessBindAll returns immediately in that case, before any
// syscall, leaving numaAllowedNodemask at zero so numaBindArena stays a
// no-op too.
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
// unpublished, so numaBindArena never spends a syscall on a policy the
// kernel rejected.
func numaSetProcessBindAll() {
	if numaTopology.NumNodes < 2 {
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
// range [addr, addr+size), then -- Layer 2 -- attempts to refine that to
// MPOL_PREFERRED for whichever single NUMA node the calling M is running
// on right now (via getcpu(2), through the numaGetCPUNode wrapper; see
// numa_linux_getcpu.go). No span bookkeeping, no mcache/m.numaNode
// tracking: the node comes from getcpu in this function only, once, at
// grow time.
//
// Kernel policy-replacement semantics (get this right in review): the
// second mbind call does not stack on top of the first. The kernel's
// vma_replace_policy REPLACES the VMA's policy outright, so a chunk that
// gets a successful PREFERRED call ends up PREFERRED-only, not
// BIND-then-PREFERRED. The BIND-all call is kept first regardless, purely
// as a fallback: it guarantees the chunk is never left with no VMA policy
// at all when the PREFERRED call is skipped (getcpu failure, or node >=
// 64 -- the max node id this function's single-word-derived nodemask can
// represent; see numaNodemask's doc comment). #14406 (suppressing the
// NUMA balancer) still holds even for a PREFERRED-only chunk: a plain
// mbind call (BIND or PREFERRED) never sets MPOL_F_MOF on the VMA, so the
// balancer skips it regardless of mode, and every VMA this function never
// reaches (or where PREFERRED is skipped) is still covered by the
// task-wide policy numaSetProcessBindAll installed.
//
// numaBindArena is called from mheap.grow, with h.lock held, immediately
// after each sysMap of newly-backed heap memory (mmap with MAP_FIXED
// resets any VMA policy the range previously had). It must stay
// nosplit-safe and add nothing slower than two mbind syscalls plus one
// getcpu syscall under that lock: no allocation, no lock acquisition, no
// other syscalls. mheap.grow's true granularity is a palloc chunk (~4
// MiB), not a 64 MiB arena, so this runs once per ~4 MiB of heap growth --
// rare relative to malloc, but far more often than "per arena".
//
// CONCERN (VMA growth): adjacent chunks PREFERRED to different nodes
// cannot VMA-merge, so worst case is approximately heap-size/4MiB VMAs
// (under vm.max_map_count's default of 65530 even at tens of GiB of heap,
// but not free -- extra kernel memory and fault-path cost). Tracked as an
// open concern in RESULTS.md, not mitigated in this function.
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
// becomes available: see task-6-report.md for that scope decision.
//
// mbind errors are ignored for both calls: this is deliberate (see
// design), not a silently-swallowed bug.
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

	node, ok := numaGetCPUNode()
	if !ok || node >= 64 {
		return
	}
	var pmask numaNodemask
	pmask[uintptr(node)/numaNodemaskBits] = 1 << (uintptr(node) % numaNodemaskBits)
	linux.Syscall6(linux.SYS_MBIND, uintptr(addr), size, uintptr(_MPOL_PREFERRED), uintptr(unsafe.Pointer(&pmask[0])), numaMaxNode, 0)
	numaPreferredCalls.Add(1)
}
