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

// Mempolicy constants. See numaSetProcessBindAll and numaBindArena.
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

// numaCPUMaskBytes is the byte length of the CPU affinity mask passed to
// sched_setaffinity(2) (via numaSetThreadAffinity) and sched_getaffinity.
// 8192 CPUs / 8 bits per byte, matching numa.Topology's CPUToNode
// capacity so any CPU id Topology can represent also fits this mask.
const numaCPUMaskBytes = 8192 / 8

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
	numaBuildNodeCPUMaskCache()
	numaSetProcessBindAll()
	numaDetectStartupAffinity()
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

// numaCurrentNode returns the id of the NUMA node the calling thread is
// currently running on, via the getcpu(2) syscall (through the
// numaGetCPUNode wrapper -- see numa_linux_getcpu.go for why this file
// never calls getcpu directly).
//
// It returns 0 if the experiment is off or if topology discovery found
// only one (or zero) nodes -- there is no other node it could report in
// either case. It returns -1 only if the getcpu syscall itself fails.
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
// Staleness: this function has exactly two call sites -- here (startup,
// from numaSchedinit) and numaStandDownWiden (a stand-down trigger). It is
// never called from the heap-growth path or on any timer, so
// numaAllowedNodemask is a snapshot of cpuset.mems as of one of those two
// moments, not a live view. A container that repartitions its cpuset
// mid-run is not observed until the next stand-down trigger (or process
// restart); see numaBindArena's doc comment for why re-checking more
// often is not worth it. This staleness is accepted by design.
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

// numaConfined is true while fill-one-socket-first confinement is
// active. Set once in numaConfineIfSmall (m0 is the only runtime
// thread); cleared only by numaStandDownIfNeeded.
var numaConfined atomic.Bool

// numaStoodDown is declared in numa_standdown.go, not here: stopm's hook
// (proc.go) inlines numaStoodDown.Load() directly so the steady-state
// (never confined, or confined-but-never-stood-down) cost at every park
// is one inlined atomic load, not a call into this Linux-only file. See
// numa_standdown.go's doc comment.

var (
	numaConfinedNode     int32
	numaConfinedNodeCPUs int32

	// numaSavedAffinity is the process's startup affinity mask, saved
	// by numaShouldConfine before any narrowing, and restored per
	// thread at/after stand-down.
	//
	// numaSavedAffinityLen is the byte length sched_getaffinity actually
	// reported for that save (numaShouldConfine's `r`); the stand-down
	// restore path (numaStandDownIfNeeded, numaFixThreadPlacement) never
	// reads it directly and instead passes the whole numaSavedAffinity
	// array to numaSetThreadAffinity/sched_setaffinity every time. That is
	// deliberately equivalent: numaSavedAffinity is package-level and
	// therefore zero-initialized, numaShouldConfine only ever narrows `r`
	// down from numaCPUMaskBytes (never grows it), and every byte at
	// index >= numaSavedAffinityLen is left at its zero value -- so
	// restoring the full numaCPUMaskBytes-sized array reproduces exactly
	// the saved mask, with no explicit truncation needed. The field is
	// kept as a documented invariant anchor (and for future diagnostics)
	// rather than removed.
	numaSavedAffinity    [numaCPUMaskBytes]byte
	numaSavedAffinityLen int32
)

// numaConfineIfSmall applies fill-one-socket-first
// confinement when the decision conditions hold. Called from schedinit after
// procresize, before any other runtime thread exists. The BIND-all task
// policy (numaSetProcessBindAll) has ALREADY run by this point and stays in
// force: confinement only narrows CPU affinity and replaces the task
// mempolicy; numaAllowedNodemask remains published and numaBindArena
// keeps stamping heap chunks with uniform BIND-all.
// Experiment off: caller never invokes it (goexperiment.Numa-guarded
// call site).
func numaConfineIfSmall(procs int32) {
	if node, ok := numaShouldConfine(procs); ok && numaConfine(node) {
		if debug.numa > 0 {
			println("numa: confined to node", node, "cpus", numaConfinedNodeCPUs)
		}
	}
}

// numaOnlineCPUCount returns the total number of CPUs numaTopology found
// across every discovered node. Shared by numaShouldConfine (compared
// against the startup affinity mask's popcount to detect a narrowed
// mask, the "operator placement wins" check) and
// numaDetectStartupAffinity (an independent, unconditional
// version of the same check).
func numaOnlineCPUCount() int32 {
	online := int32(0)
	for i := int32(0); i < numaTopology.NumNodes; i++ {
		online += numaTopology.Nodes[i].NumCPUs
	}
	return online
}

// numaAffinityPopcount returns the number of set bits across mask, an
// affinity bitmask in sched_getaffinity's on-the-wire byte format.
func numaAffinityPopcount(mask []byte) int32 {
	pop := int32(0)
	for _, b := range mask {
		for b != 0 {
			b &= b - 1
			pop++
		}
	}
	return pop
}

// numaNodeCPUMaskCache holds, once built, the CPU affinity bitmask for
// every NUMA node id numaTopology could ever report -- indexed directly
// by node id, matching numaShouldConfine's own node<64 bound
// (numaMaxNode-derived). numaNodeCPUMaskCachedPopcount[id]
// is the same node's CPU count; 0 means "no cached mask" (either the id
// is not one of this host's real nodes, or its node is legitimately
// CPU-less -- both cases correctly report "cannot be narrowed to").
//
// Built once, eagerly, by numaBuildNodeCPUMaskCache, called from
// numaSchedinit while m0 is still the only runtime thread -- the same
// single-threaded invariant numaConfine/numaSetProcessBindAll/
// numaDetectStartupAffinity already rely on -- so every M thereafter
// reads it lock-free: numaTopology itself is documented read-only after
// schedinit, and this cache is a pure, deterministic function of it.
//
// 64 * numaCPUMaskBytes(1024) = 64KiB of static BSS. This file builds
// regardless of the experiment, so the guard is not a build tag:
// goexperiment.Numa gates numaBuildNodeCPUMaskCache's
// only call site (numaSchedinit), so with the experiment off this array
// is simply never populated (still statically allocated, same as
// numaScratch above, which accepts the same kind of always-present-but-
// only-used-on cost).
var numaNodeCPUMaskCache [64][numaCPUMaskBytes]byte
var numaNodeCPUMaskCachedPopcount [64]int32

// numaBuildNodeCPUMaskCache populates numaNodeCPUMaskCache for every
// node numaTopology discovered. Called once from numaSchedinit, after
// numaInitTopology. A single 8192-CPU scan per discovered node turns
// every later per-node mask lookup into an O(1) cache read.
func numaBuildNodeCPUMaskCache() {
	for i := int32(0); i < numaTopology.NumNodes; i++ {
		id := numaTopology.Nodes[i].ID
		if id < 0 || id >= 64 {
			continue
		}
		n := 0
		for cpu := 0; cpu < 8192; cpu++ {
			if numaTopology.NodeOfCPU(cpu) == id {
				numaNodeCPUMaskCache[id][cpu/8] |= 1 << (uint(cpu) % 8)
				n++
			}
		}
		numaNodeCPUMaskCachedPopcount[id] = int32(n)
	}
}

// numaNodeAffinityMask fills *mask with the cached CPU affinity bitmask
// for node (see numaNodeCPUMaskCache) and reports whether at least one
// CPU was found (a topology with no CPUs for this node -- e.g. a
// CPU-less memory-only node -- cannot be narrowed to). Used by
// numaConfine (process-wide fill-one-socket confinement, once, from
// schedinit).
func numaNodeAffinityMask(node int32, mask *[numaCPUMaskBytes]byte) bool {
	if node < 0 || node >= 64 || numaNodeCPUMaskCachedPopcount[node] == 0 {
		return false
	}
	*mask = numaNodeCPUMaskCache[node]
	return true
}

// numaShouldConfine reports whether fill-one-socket-first should
// engage: multi-node machine, representable topology, the BIND-all policy
// actually engaged (published nodemask), an EXPLICITLY chosen GOMAXPROCS
// (without sched.customGOMAXPROCS, sysmon's ~1/sec
// defaultGOMAXPROCS recompute reads the narrowed mask and freezes procs
// at the node size, a real feedback loop), no pre-existing narrowed CPU
// affinity (operator placement always wins), a usable getcpu, and
// procs <= the boot node's CPU count.
// As a side effect it saves the startup affinity mask for stand-down.
// Every declined reason prints under GODEBUG=numa=1 (diagnosability).
//
// Both the published nodemask this checks and the affinity mask it saves
// are startup snapshots (see numaSetProcessBindAll's doc comment): a
// cpuset narrowed after this point is not detected here or anywhere else
// until a stand-down trigger re-reads it (numaStandDownWiden). Accepted,
// documented staleness (see numaSetProcessBindAll's doc comment).
func numaShouldConfine(procs int32) (int32, bool) {
	// The first three conditions are split out and reported
	// individually (rather than one bundled `if`) so every decline path
	// in this function prints under GODEBUG=numa=1, as this function's
	// doc comment promises, and a diagnostic run can tell
	// "not multi-node" apart from "topology discovery gave up partway"
	// apart from "this arch has no sched_setaffinity".
	if numaTopology.NumNodes < 2 {
		numaConfineDeclined("not multi-node")
		return 0, false
	}
	if numaTopology.TruncatedNodes {
		numaConfineDeclined("topology truncated")
		return 0, false
	}
	if !numaHasSetAffinity {
		numaConfineDeclined("no sched_setaffinity on this arch")
		return 0, false
	}
	if numaAllowedNodemask.Load() == 0 {
		// The BIND-all policy declined or failed; do not build
		// confinement on top.
		numaConfineDeclined("bind-all mempolicy inactive")
		return 0, false
	}
	// Read without sched.lock: numaShouldConfine runs from schedinit, before
	// any other runtime thread exists (numaConfineIfSmall's call site is
	// still single-threaded m0), so there is no concurrent writer to race.
	// Every other reader/writer of sched.customGOMAXPROCS (GOMAXPROCS,
	// SetDefaultGOMAXPROCS, sysmonUpdateGOMAXPROCS, updateMaxProcsGoroutine)
	// takes sched.lock because they run concurrently with other Ms.
	if !sched.customGOMAXPROCS {
		// Default GOMAXPROCS auto-updates from the affinity mask we are
		// about to narrow: only confine a process
		// whose P count was chosen explicitly.
		numaConfineDeclined("GOMAXPROCS not explicitly set")
		return 0, false
	}
	r := sched_getaffinity(0, uintptr(numaCPUMaskBytes), &numaSavedAffinity[0])
	if r <= 0 {
		numaConfineDeclined("sched_getaffinity failed")
		return 0, false
	}
	numaSavedAffinityLen = int32(r)
	if numaAffinityPopcount(numaSavedAffinity[:r]) != numaOnlineCPUCount() {
		// taskset / narrowed cpuset: operator placement wins. NOTE:
		// offline CPUs can also make sysfs-online and the affinity
		// popcount disagree; the check then declines — conservative
		// (offline-CPU hosts simply do not confine). Record if seen.
		numaConfineDeclined("affinity narrower than online CPUs")
		return 0, false
	}
	node := numaCurrentNode() // confine to the boot CPU's node
	// node < 0 (the getcpu syscall itself failed) and
	// node >= 64 (getcpu succeeded but returned an id past what the
	// 64-node nodemask/cache arrays this package uses can represent) are
	// different failure modes -- report them separately so a diagnostic
	// run does not conflate "no working getcpu on this host" with "this
	// host has an implausibly high node id".
	if node < 0 {
		numaConfineDeclined("getcpu failed")
		return 0, false
	}
	if node >= 64 {
		numaConfineDeclined("node id out of range")
		return 0, false
	}
	ncpus := numaNodeCPUCount(node)
	if ncpus <= 0 || procs > ncpus {
		numaConfineDeclined("GOMAXPROCS exceeds node")
		return 0, false
	}
	return node, true
}

// numaConfineDeclined prints the decline reason under GODEBUG=numa=1.
func numaConfineDeclined(reason string) {
	if debug.numa > 0 {
		println("numa: confinement declined:", reason)
	}
}

// numaNodeCPUCount returns node's CPU count from the topology. It is
// the single source for both the <= decision in numaShouldConfine and
// the numaConfinedNodeCPUs threshold the stand-down trigger compares
// against.
func numaNodeCPUCount(node int32) int32 {
	for i := int32(0); i < numaTopology.NumNodes; i++ {
		if numaTopology.Nodes[i].ID == node {
			return numaTopology.Nodes[i].NumCPUs
		}
	}
	return 0
}

// numaConfine confines the process to node: CPU affinity to that node's
// CPUs plus a task MPOL_PREFERRED policy for its memory
// (PREFERRED, never single-node BIND — the OOM footgun).
// The PREFERRED set_mempolicy REPLACES the BIND-all task policy
// for this thread and, by clone inheritance, every later M. Runs while
// m0 is the only runtime thread.
//
// Heap-VMA exemption does not depend on this task
// policy: numaAllowedNodemask
// stays published either way, so mheap.grow's chunks keep getting an
// explicit BIND-all VMA policy regardless of confinement. That VMA-own
// policy is what holds against threads the task policy never reached
// (pre-runtime cgo threads), and its single uniform mask keeps adjacent
// chunks VMA-merging cleanly (see numaBindArena).
func numaConfine(node int32) bool {
	var cpumask [numaCPUMaskBytes]byte
	if !numaNodeAffinityMask(node, &cpumask) {
		return false
	}
	if !numaSetThreadAffinity(0, &cpumask) {
		return false
	}
	var pmask numaNodemask
	pmask[uintptr(node)/numaNodemaskBits] = 1 << (uintptr(node) % numaNodemaskBits)
	if _, _, errno := linux.Syscall6(linux.SYS_SET_MEMPOLICY,
		uintptr(_MPOL_PREFERRED), uintptr(unsafe.Pointer(&pmask[0])), numaMaxNode, 0, 0, 0); errno != 0 {
		// Policy failed: undo the affinity narrowing and stay on the
		// BIND-all task policy that is already in force.
		numaSetThreadAffinity(0, &numaSavedAffinity)
		return false
	}
	// Publish the plain fields BEFORE the atomic flag: numaStandDownIfNeeded
	// and numaFixThreadPlacement below read numaConfinedNode/
	// numaConfinedNodeCPUs only after observing numaConfined (or
	// numaStoodDown) true, so this order plus the Store's release
	// semantics guarantee they never read them half-written.
	numaConfinedNode = node
	numaConfinedNodeCPUs = numaNodeCPUCount(node) // same source as the decision
	numaConfined.Store(true)
	return true
}

// numaStandDownIfNeeded reports whether fill-one-socket-first confinement
// should stand down: either (a) the new GOMAXPROCS exceeds the confined
// node's CPU count, or (b) customGOMAXPROCS is false, i.e. the process
// just transitioned (or returned) to default-GOMAXPROCS mode while
// confined (see the SetDefaultGOMAXPROCS discussion
// below). Called from startTheWorldWithSema immediately after
// sched.lock is released, with procs and customGOMAXPROCS both captured
// under that same sched.lock critical section as the procs computation
// itself (lock state matters: see the caller).
//
// Why (b) is required, not optional: runtime.SetDefaultGOMAXPROCS sets
// sched.customGOMAXPROCS = false and computes its newprocs from
// defaultGOMAXPROCS(0), which reads the CURRENT (already node-narrowed by
// confinement) CPU affinity mask. So newprocs comes out equal to
// numaConfinedNodeCPUs, condition (a) never fires, and sysmon's
// ~1/sec automatic GOMAXPROCS recompute (sysmonUpdateGOMAXPROCS, also
// customGOMAXPROCS==false) then keeps recomputing from that same narrowed
// mask forever -- the process would stay confined and never stand down.
// Standing down unconditionally on any customGOMAXPROCS==false transition
// closes that loop: it is exactly the signal that GOMAXPROCS control just
// left "the operator explicitly chose a value" (the
// same precondition numaShouldConfine required to confine in the first
// place) and returned to automatic mode, where confinement's premise no
// longer holds regardless of what the recomputed value happens to be.
//
// This function is deliberately syscall-free: the caller runs with
// mp.locks != 0 (acquirem, to pin the P across the resize), and
// syscalls are banned under sched.lock or with mp.locks != 0. It only
// flips the one-way
// numaStoodDown latch and reports whether it just did so; the actual
// syscall-bearing work -- the eager allm affinity walk and this thread's
// own convergence -- is done by numaStandDownWiden, which the caller
// invokes separately once mp.locks is back to 0 and the world has
// restarted. NOTE stand-down does NOT guarantee every thread is restored
// by the time numaStandDownWiden returns: the world is already restarting
// (gcwaiting cleared; startTheWorldWithSema's own loop can newm,
// proc.go:1813), and Ms can be on allm before their procid is stored
// (mcommoninit runs before newosproc). The eager walk is a latency
// optimization; per-thread correctness is numaFixThreadPlacement, called
// from every M's next park (stopm).
func numaStandDownIfNeeded(procs int32, customGOMAXPROCS bool) bool {
	if !numaConfined.Load() {
		return false
	}
	if procs <= numaConfinedNodeCPUs && customGOMAXPROCS {
		return false
	}
	// Store numaStoodDown BEFORE numaConfined: readers that gate on the
	// pair check numaConfined first and numaStoodDown second, so with
	// the stores in the other order a reader could observe numaConfined
	// already false but numaStoodDown not yet true -- both gates false
	// -- during the transition itself. Since these are sequentially
	// consistent atomics and numaStoodDown only ever goes false->true
	// (never back), storing numaStoodDown first guarantees that by the
	// time any reader observes numaConfined==false,
	// numaStoodDown==true is already visible to that same reader's
	// subsequent load, regardless of interleaving.
	numaStoodDown.Store(true)
	numaConfined.Store(false)
	if debug.numa > 0 {
		if !customGOMAXPROCS {
			println("numa: confinement stood down, GOMAXPROCS reverted to default (customGOMAXPROCS=false)")
		} else {
			println("numa: confinement stood down, GOMAXPROCS", procs, ">", numaConfinedNodeCPUs)
		}
	}
	return true
}

// numaStandDownWiden performs the syscall-bearing half of a stand-down
// trigger: a best-effort eager affinity restore over allm (latency
// optimization only) plus this calling M's own convergence. Called from
// startTheWorldWithSema only after releasem (mp.locks == 0, sched.lock
// free, world fully restarted via worldStarted()) -- never from
// numaStandDownIfNeeded's detection site, which runs with mp.locks != 0
// and must not make syscalls (see that function's doc comment).
// numaWidenAllThreads is the eager, best-effort, affinity-only allm
// walk: it restores every
// reachable thread's kernel CPU mask to the saved startup-wide mask.
// It deliberately touches NOTHING else -- no mempolicy, no per-M caches
// (cross-M cache writes are races; owners converge themselves). See the
// in-loop comments for the lock-free walk's accepted residuals.
func numaWidenAllThreads() {
	// Atomic head load, matching the tree's other lock-free allm walkers
	// (e.g. NumCgoCall, totalMutexWaitTimeNanos in debug.go): allm is
	// written under sched.lock (mcommoninit's atomicstorep, mexit's
	// unlink) and read here without it.
	for mp := (*m)(atomic.Loadp(unsafe.Pointer(&allm))); mp != nil; mp = mp.alllink {
		if mp.freeWait.Load() == freeMWait {
			// freeMWait (2) is the value mexit stores into freeWait the
			// moment it removes mp from allm under sched.lock -- it means
			// "g0 is still in use, NOT yet safe to reap" (the opposite of
			// torn down). A normal, never-exited M's freeWait sits at its
			// zero value (freeMStack) for its entire alive lifetime;
			// freeMWait is a transient marker mexit sets only once it
			// starts tearing this M down. We only ever observe
			// freeWait==freeMWait here because our lock-free walk reached
			// mp via a stale alllink pointer read just before (or racing)
			// that removal. Skip it -- its procid is no longer
			// trustworthy -- a live replacement M (if any) converges
			// itself at its own first park via numaFixThreadPlacement.
			//
			// Residual, accepted: this check closes only the MID-exit
			// window above. It does not protect against a fully-exited M
			// -- one whose freeWait has already advanced past freeMWait to
			// its terminal freeMRef/freeMStack value by the time we read
			// it -- reached the same way, via a stale alllink pointer.
			// Such an M reads as "alive" here (freeWait != freeMWait) and
			// its long-stale procid is still used; on Linux that tid can
			// eventually be recycled by the kernel for a brand-new,
			// unrelated thread, so sched_setaffinity could misdirect one
			// setaffinity call to the wrong thread. The eager walk is a
			// latency optimization only (documented above), so this has
			// no correctness impact: it never affects a legitimate target
			// M's own convergence, which is guaranteed separately, and
			// correctly, at its next park.
			continue
		}
		if tid := atomic.Load64(&mp.procid); tid != 0 {
			numaSetThreadAffinity(int32(tid), &numaSavedAffinity)
		}
	}
}

func numaStandDownWiden() {
	numaWidenAllThreads()
	// Best-effort re-read of the allowed-node mask before this thread's
	// own convergence: numaSetProcessBindAll re-issues get_mempolicy
	// (MPOL_F_MEMS_ALLOWED) and set_mempolicy, refreshing
	// numaAllowedNodemask -- this is the one point after startup where a
	// cpuset narrowed while running (a container's cpuset.mems/cpuset.cpus
	// edited mid-process) is picked up; see numaSetProcessBindAll's doc
	// comment
	// for why it is not re-read anywhere more often than this. Its own
	// failure (e.g. a cpuset narrowed to one node since startup) is
	// silently absorbed here: numaAllowedNodemask simply keeps its prior
	// value, and numaFixThreadPlacement below -- not this call -- is what
	// actually converges (and correctly retries at the next park on
	// failure) this M's affinity and task policy.
	numaSetProcessBindAll()
	numaFixThreadPlacement()
}

// numaFixThreadPlacement converges the calling M's placement after
// stand-down: restores BOTH its CPU affinity (self-call, no tid
// needed) AND its task mempolicy to BIND-all (set_mempolicy is
// per-thread and cannot be applied cross-thread). Called from stopm --
// parked-M path, never malloc, never steal; one atomic load when the
// experiment is on and stand-down has not happened, nothing when off.
// This is the correctness path: every M -- including ones the eager
// walk missed (unset procid, late clones), and every M allocm creates
// AFTER stand-down (a fresh mPadded is never recycled, so its m.numa
// starts at its zero value and this function still runs once for it at
// its first park -- a harmless, one-time redundant re-issue of an
// affinity/policy pair that was already correct at clone time) --
// converges at its first park after stand-down. That first park can
// itself be gcstopm (a GC STW): the two syscalls below then run once,
// inside that STW, for any M whose first post-stand-down park happens to
// be for GC rather than idling -- a bounded, one-time-per-M cost either
// way.
func numaFixThreadPlacement() {
	if !numaStoodDown.Load() {
		return
	}
	mp := getg().m
	if mp.numa.placementDone() {
		return
	}
	// Affinity first (idempotent if the eager walk already got us).
	if !numaSetThreadAffinity(0, &numaSavedAffinity) {
		return // retry at next park
	}
	// w0 is unconditionally non-zero here: reaching numaStoodDown==true
	// requires having been numaConfined, which requires numaShouldConfine
	// to have observed numaAllowedNodemask.Load() != 0 at confine time
	// (its "bind-all mempolicy inactive" decline check) -- and nothing in this file
	// ever stores zero back into numaAllowedNodemask once published, so
	// it cannot have reverted to zero since. The check remains as a
	// belt-and-suspenders guard (an unbounded, silent retry-forever if
	// it were ever somehow false) rather than a throw, since a defensive
	// return here is strictly safer than a crash on an invariant this
	// function does not otherwise need to re-verify.
	w0 := numaAllowedNodemask.Load()
	if w0 == 0 {
		return
	}
	var mask numaNodemask
	mask[0] = w0
	if _, _, errno := linux.Syscall6(linux.SYS_SET_MEMPOLICY,
		uintptr(_MPOL_BIND), uintptr(unsafe.Pointer(&mask[0])), numaMaxNode, 0, 0, 0); errno != 0 {
		return // retry at next park
	}
	// Latch only after every syscall succeeded: latching first
	// would permanently strand a thread that raced a transient failure.
	mp.numa.setPlacementDone()
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
// read is stale here until the next stand-down trigger (numaStandDownWiden,
// which does call numaSetProcessBindAll) or a process restart -- accepted
// by design, not a bug.
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

// numaStartupFullAffinity records whether this process began life with
// CPU affinity over every online CPU numaTopology found -- i.e. NOT
// narrowed by taskset, a narrowed cpuset, or any other operator
// placement. Computed once, unconditionally, by numaDetectStartupAffinity
// during numaSchedinit.
//
// This is deliberately independent of numaShouldConfine's own narrowed-
// affinity check: that check only runs when sched.customGOMAXPROCS is
// set, because fill-one-socket confinement itself requires an explicitly
// chosen GOMAXPROCS. An unconditional answer to "did an operator place
// this process" is needed regardless (diagnostics and the hardware test
// battery consume it). Both checks reuse the same detection mechanism
// (numaAffinityPopcount vs numaOnlineCPUCount); only the trigger
// conditions differ.
//
// Set once, single-threaded, from numaSchedinit -- schedinit runs before
// any other runtime thread exists, the same invariant numaConfine and
// numaSetProcessBindAll rely on -- so every M created afterward may read
// this as a plain bool with no atomics, exactly like numaTopology itself.
var numaStartupFullAffinity bool

// numaStartupAffinity is the raw CPU affinity mask numaDetectStartupAffinity
// read at startup (the same sched_getaffinity result numaStartupFullAffinity
// is derived from). Kept, not just the derived bool: unlike
// numaShouldConfine's own numaSavedAffinity, this is captured
// unconditionally (independent of sched.customGOMAXPROCS), so it is
// reliably populated whenever numaStartupFullAffinity is true.
var numaStartupAffinity [numaCPUMaskBytes]byte

// numaDetectStartupAffinity computes numaStartupFullAffinity and saves
// the mask it was computed from into numaStartupAffinity. Called once
// from numaSchedinit, after numaSetProcessBindAll. No-ops (leaving both
// at their zero values -- fail closed) on a single-node host or an arch
// without numaSetThreadAffinity, where nothing can consume the answer.
//
// NOTE (numaShouldConfine's equivalent check shares this caveat):
// offline CPUs can make sysfs-online and the affinity popcount disagree
// -- a host with offline CPUs may have numaOnlineCPUCount() count a CPU
// this process's affinity mask never included, so the popcount
// comparison declines (numaStartupFullAffinity stays false) even though
// nothing actually narrowed this process's affinity. Conservative: the
// same consequence numaShouldConfine already accepts for confinement.
func numaDetectStartupAffinity() {
	if numaTopology.NumNodes < 2 || !numaHasSetAffinity {
		return
	}
	r := sched_getaffinity(0, uintptr(numaCPUMaskBytes), &numaStartupAffinity[0])
	if r <= 0 {
		return
	}
	numaStartupFullAffinity = numaAffinityPopcount(numaStartupAffinity[:r]) == numaOnlineCPUCount()
}
