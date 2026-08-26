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
// The allowed-nodes narrowing lives entirely here, in
// numaSetProcessBindAll, not in numaTopology/internal/runtime/numa: that
// package's Topology.NumAllowedNodes is never narrowed below NumNodes --
// the two are equal by construction for any Topology it produces (final
// review F5; see Topology.NumAllowedNodes's own doc comment). Instead,
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

// numaTopology is the machine's NUMA topology, discovered by
// numaInitTopology during schedinit. It is only populated when
// GOEXPERIMENT=numa is set; otherwise it is left at its zero value, which
// numaTopology.NumNodes == 0 correctly represents as "unknown" rather than
// silently claiming a single node.
//
// Layer 0: read-only after schedinit. Final review F4: stale comment
// corrected -- this was true only through the earliest Layer-0-only
// milestone; numaTopology is now the source topology reference for
// fill-one-socket confinement (numaShouldConfine/numaConfine), node-mask
// soft affinity (numaNoteSchedule), per-node heap arena stream homing
// (numaGrowNode/numaHeapHomingActive), and Topology.NumAllowedNodes
// (internal/runtime/numa/numa.go), in addition to diagnostics and tests.
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
// unpublished, so numaBindArena never spends a syscall on a policy the
// kernel rejected.
//
// Staleness: this function has exactly two call sites -- here (startup,
// from numaSchedinit) and numaStandDownWiden (a stand-down trigger). It is
// never called from the heap-growth path or on any timer, so
// numaAllowedNodemask is a snapshot of cpuset.mems as of one of those two
// moments, not a live view. A container that repartitions its cpuset
// mid-run is not observed until the next stand-down trigger (or process
// restart); see numaBindArena's doc comment for why re-checking more
// often is not worth it, and bind-all-policy.md's "Dynamic cpuset
// staleness" section for the full accepted-staleness model.
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

// numaConfineIfSmall applies fill-one-socket-first confinement (design
// §12.2) when the decision conditions hold. Called from schedinit after
// procresize, before any other runtime thread exists. Layer 1
// (numaSetProcessBindAll) has ALREADY run by this point and stays in
// force: confinement only narrows CPU affinity and replaces the task
// mempolicy; numaAllowedNodemask remains published and numaBindArena
// keeps stamping heap chunks with uniform BIND-all (locked decision 5).
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
// mask, locked decision 6's "operator placement wins" check) and
// numaDetectStartupAffinity (Task 10's independent, unconditional
// version of the same check, for node-mask soft affinity).
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
// by node id, matching numaShouldConfine's and numaNoteSchedule's own
// node<64 bound (numaMaxNode-derived). numaNodeCPUMaskCachedPopcount[id]
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
// 64 * numaCPUMaskBytes(1024) = 64KiB of static BSS. Build-tagged
// on-experiment only via this file's Linux-only, always-on-when-Linux
// nature is not the guard here (numa_linux.go builds regardless of the
// experiment) -- goexperiment.Numa gates numaBuildNodeCPUMaskCache's
// only call site (numaSchedinit), so with the experiment off this array
// is simply never populated (still statically allocated, same as
// numaScratch above, which accepts the same kind of always-present-but-
// only-used-on cost). Comparable in kind to Task 8's disclosed per-node
// array costs.
var numaNodeCPUMaskCache [64][numaCPUMaskBytes]byte
var numaNodeCPUMaskCachedPopcount [64]int32

// numaBuildNodeCPUMaskCache populates numaNodeCPUMaskCache for every
// node numaTopology discovered. Called once from numaSchedinit, after
// numaInitTopology. A single 8192-CPU scan per discovered node (not per
// getcpu-triggered node change) -- this is what turns the per-node mask
// from an 8192-iteration scan on every soft-affinity node change (the
// review's M3 finding) into an O(1) cache lookup.
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
// CPU-less memory-only node -- cannot be narrowed to). Shared by
// numaConfine (process-wide fill-one-socket confinement, once, from
// schedinit) and numaNoteSchedule (per-M soft affinity, design §12.4).
func numaNodeAffinityMask(node int32, mask *[numaCPUMaskBytes]byte) bool {
	if node < 0 || node >= 64 || numaNodeCPUMaskCachedPopcount[node] == 0 {
		return false
	}
	*mask = numaNodeCPUMaskCache[node]
	return true
}

// numaShouldConfine reports whether fill-one-socket-first should engage
// (design §12.2): multi-node machine, representable topology, Layer 1
// actually engaged (published nodemask), an EXPLICITLY chosen GOMAXPROCS
// (locked decision 6 — without sched.customGOMAXPROCS, sysmon's ~1/sec
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
// documented staleness -- see bind-all-policy.md's "Dynamic cpuset
// staleness" section.
func numaShouldConfine(procs int32) (int32, bool) {
	// Final review F5: these first three conditions used to bail out
	// silently (a single bundled `if`, no numaConfineDeclined call) --
	// the only decline paths in this function that did not print under
	// GODEBUG=numa=1, contrary to this function's own doc comment above.
	// Split out and reported individually so a diagnostic run can tell
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
		// Layer 1 declined or failed; do not build confinement on top.
		numaConfineDeclined("layer1 inactive")
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
		// about to narrow (locked decision 6): only confine a process
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
	node := numaCurrentNode() // boot CPU's node (locked decision 2)
	// Final review F5: node < 0 (the getcpu syscall itself failed) and
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
// CPUs plus a task MPOL_PREFERRED policy for its memory (locked
// decision 1: PREFERRED, never single-node BIND — the OOM footgun).
// The PREFERRED set_mempolicy REPLACES the Layer-1 BIND-all task policy
// for this thread and, by clone inheritance, every later M. Runs while
// m0 is the only runtime thread.
//
// Heap-VMA exemption does not depend on this task policy (final review
// F4: corrected -- an earlier version of this comment said every chunk
// keeps uniform BIND-all unconditionally, which stopped being true once
// per-node heap arena stream homing landed, task 8): numaAllowedNodemask
// stays published either way, so mheap.grow's chunks keep getting an
// explicit VMA policy regardless of confinement, but which policy
// depends on numaHeapHomingActive (numa_linux.go) — with homing active
// (multi-node, streams enabled), numaBindGrowth stamps each chunk
// MPOL_PREFERRED to its own stream's node instead; with homing inactive
// (single-node, streams disabled, or topology not yet discovered),
// numaBindArena's uniform BIND-all applies exactly as originally
// described. Either way, the VMA-own policy is what holds against
// threads the task policy never reached (pre-runtime cgo threads;
// locked decision 1), and both a uniform BIND-all mask and per-node
// PREFERRED masks confined to mheap.grow's own disjoint per-node address
// partitioning VMA-merge cleanly, so neither case sees a Layer-2-style
// map blowup.
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
		// Layer-1 BIND-all policy that is already in force.
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
// confined (locked decisions 3-4, plus the SetDefaultGOMAXPROCS fix
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
// left "the operator explicitly chose a value" (locked decision 6, the
// same precondition numaShouldConfine required to confine in the first
// place) and returned to automatic mode, where confinement's premise no
// longer holds regardless of what the recomputed value happens to be.
//
// This function is deliberately syscall-free: the caller runs with
// mp.locks != 0 (acquirem, to pin the P across the resize) and the design's
// Forbidden list bans syscalls under sched.lock or with mp.locks != 0 (same
// rule as Task 10's schedule() hook). It only flips the one-way
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
	// Store numaStoodDown BEFORE numaConfined (Task 10 review I1):
	// numaNoteSchedule's reader checks numaConfined.Load() first, then
	// numaStoodDown.Load(), via a short-circuit ||. With the stores in
	// the other order, a reader could observe numaConfined already
	// false but numaStoodDown not yet true -- both gate checks false, so
	// node-mask soft affinity could engage and narrow an M's affinity
	// during the stand-down transition itself. Storing numaStoodDown
	// first closes that window: since these are sequentially consistent
	// atomics and numaStoodDown only ever goes false->true (never
	// back), by the time any reader observes numaConfined==false,
	// numaStoodDown==true is already guaranteed visible to that same
	// reader's subsequent load, regardless of interleaving.
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
func numaStandDownWiden() {
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
	// Best-effort re-read of the allowed-node mask before this thread's
	// own convergence: numaSetProcessBindAll re-issues get_mempolicy
	// (MPOL_F_MEMS_ALLOWED) and set_mempolicy, refreshing
	// numaAllowedNodemask -- this is the one point after startup where a
	// cpuset narrowed while running (a container's cpuset.mems/cpuset.cpus
	// edited mid-process) is picked up; see numaSetProcessBindAll's doc
	// comment and bind-all-policy.md's "Dynamic cpuset staleness" section
	// for why it is not re-read anywhere more often than this. Its own
	// failure (e.g. a cpuset narrowed to one node since Layer 1 ran) is
	// silently absorbed here: numaAllowedNodemask simply keeps its prior
	// value, and numaFixThreadPlacement below -- not this call -- is what
	// actually converges (and correctly retries at the next park on
	// failure) this M's affinity and task policy.
	numaSetProcessBindAll()
	numaFixThreadPlacement()
}

// numaFixThreadPlacement converges the calling M's placement after
// stand-down: restores BOTH its CPU affinity (self-call, no tid
// needed) AND its task mempolicy to Layer-1 BIND-all (set_mempolicy is
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
	// (its "layer1 inactive" decline check) -- and nothing in this file
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
	// Latch only after every syscall succeeded (I4): latching first
	// would permanently strand a thread that raced a transient failure.
	mp.numa.setPlacementDone()
}

// numaBindArena sets MPOL_BIND, over the same allowed-node mask
// numaSetProcessBindAll computed, as the VMA policy for the heap arena
// range [addr, addr+size). No span bookkeeping, no mcache/m.numaNode
// tracking, no per-M node lookup: one uniform BIND-all mbind per chunk,
// unconditionally.
//
// A per-chunk MPOL_PREFERRED refinement (keyed off the growing M's
// current node via getcpu(2)) previously ran here as a second mbind call
// after this one -- Layer 2. It is gone: its IMC locality gate failed
// (remote-DRAM share unchanged, +0.10%/-0.07% vs a required >=10% drop)
// and a single-session three-arm sweep proved it behaviorally inert
// (C-full vs C-L1, p=0.838) once fill-one-socket-first confinement was in
// place. It was also the sole cause of the ~34x per-chunk VMA
// fragmentation seen at Layer 2: adjacent chunks preferring different
// nodes could not VMA-merge, where this function's single uniform BIND-all
// mask merges cleanly. See task-6-report.md and RESULTS.md's "Layer 2
// removal" section for the removal evidence.
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
// becomes available: see task-6-report.md for that scope decision.
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
// by design, not a bug. See bind-all-policy.md's "Dynamic cpuset
// staleness" section.
//
// mbind errors are ignored: this is deliberate (see design), not a
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

// Per-node heap arena stream homing (design §12.3, task 8). numaBindArena
// above stays exactly as it is -- Layer 1's uniform BIND-all, unconditionally
// balancer-exempt against every thread (decision 5). The functions below add
// per-node memory homing on top of it, active only when per-node arena
// streams are real (numaHeapHomingActive) and always mbind'ing a range that
// mheap.grow's address partitioning (h.arenaHints[node]/h.curArena[node])
// already keeps disjoint from every other node's range -- so, unlike the
// removed Layer 2, adjacent chunks always share the same node's policy and
// merge cleanly (see numaBindArenaHome's doc comment).

// numaGrowNode returns the NUMA node mheap.grow should grow into (stream),
// and whether that is a genuine per-node reading safe to home memory to
// (homed) -- review I1.
//
// stream is always a valid heap arena stream index ([0, numaMaxHeapNodes)),
// even when homed is false: stream 0's address space always exists and is
// always safe to grow into, regardless of whether this call can vouch for
// it being where the calling thread actually runs. homed is false, and
// stream is unconditionally 0, in every case where collapsing to node 0
// would otherwise be mistaken for a genuine node-0 reading:
//
//   - streams are disabled entirely (numaHeapStreamsEnabled -- experiment
//     off, race build, tight-VA riscv64, or 32-bit; review C1/C2);
//   - the running node could not be determined (a failed getcpu) -- a
//     syscall failure says nothing about which node is actually running,
//     least of all that it's node 0;
//   - the running node is >= numaMaxHeapNodes (I5: "not node % N sharing" --
//     a host with more real nodes than streams does not get false locality
//     for the nodes it has no stream for, and must not be homed to node 0
//     on their behalf either -- sparse/high node ids exist even on hosts
//     with few total nodes, e.g. CXL memory nodes).
//
// Called from allocSpan (via numaGrowNodeArg), at heap-growth frequency
// only (mheap.grow itself never calls getcpu -- the v2/v3 forbidden list's
// "no getcpu on a malloc fast path, refill/grow/scheduler-pass frequency
// only" rule). With streams disabled this returns before any syscall.
func numaGrowNode() (stream int32, homed bool) {
	if !numaHeapStreamsEnabled {
		return 0, false
	}
	node := numaCurrentNode()
	if node < 0 || node >= numaMaxHeapNodes {
		return 0, false
	}
	return node, true
}

// numaHeapHomingActive reports whether mheap.grow should home newly mapped
// chunks to a specific NUMA node (numaBindArenaHome, MPOL_PREFERRED) instead
// of Layer 1's uniform BIND-all (numaBindArena). True only when per-node
// arena streams are real and safe to home into:
//
//   - the experiment is on and numaMaxHeapNodes > 1 (I5 -- both a compile-time
//     constant check, so this whole function folds to "return false" with the
//     experiment off, same as numaMaxHeapNodes's own collapse);
//   - the machine actually has more than one node;
//   - numaAllowedNodemask has been published -- the same init-order guard
//     numaBindArena uses, since mheap.grow can run before numaSchedinit.
//
// False collapses grow's homing call to exactly today's numaBindArena
// BIND-all: single-node hosts and hosts where topology discovery hasn't run
// yet. (numaGrowNode's own homed=false cases -- disabled streams, failed
// getcpu, node >= numaMaxHeapNodes -- are handled by numaBindGrowth passing
// node < 0 through, not by this function.)
//
//go:nosplit
func numaHeapHomingActive() bool {
	return goexperiment.Numa && numaMaxHeapNodes > 1 && numaTopology.NumNodes > 1 && numaAllowedNodemask.Load() != 0
}

// numaBindGrowth is mheap.grow's single call site for arena VMA policy: it
// dispatches to numaBindArenaHome (per-node PREFERRED) only when node is a
// genuine per-node reading (node >= 0 -- see numaGrowNode/numaGrowNodeArg
// for the sentinel, review I1) AND per-node homing is active AND node is
// actually one of this process's allowed NUMA nodes (review I2: node's bit
// must be set in numaAllowedNodemask, the same mask numaBindArena's
// BIND-all already restricts itself to -- a cpuset can exclude a node that
// still fits in numaMaxHeapNodes's index range, e.g. a container pinned to
// nodes {0,3} on an 8-node host; mbind'ing MPOL_PREFERRED to an excluded
// node either fails outright or, worse, silently succeeds and leaves the
// range with no VMA policy at all once it hits a kernel that permits it,
// losing the balancer exemption decision 1 relies on). Every other case
// falls back to numaBindArena (uniform BIND-all) -- one atomic load and one
// AND to check the nodemask bit, no syscall spent on a policy the kernel
// would reject or that would silently under-exempt the range.
//
// Same nosplit-under-h.lock contract as numaBindArena -- see that
// function's doc comment for the full rationale (init-order guard,
// scavenger interaction, ignored mbind errors); it is not repeated here.
//
//go:nosplit
func numaBindGrowth(addr unsafe.Pointer, size uintptr, node int32) {
	if node >= 0 && numaHeapHomingActive() && numaAllowedNodemask.Load()&(uintptr(1)<<uint(node)) != 0 {
		numaBindArenaHome(addr, size, node)
		return
	}
	numaBindArena(addr, size)
}

// numaBindArenaHome sets MPOL_PREFERRED(node), as the VMA policy for the
// heap arena range [addr, addr+size), homing that range's physical memory to
// node while still allowing it to spill gracefully if node fills (never
// MPOL_BIND to a single node -- the OOM footgun bind-all-policy.md forbids
// for the whole process; here it is a graceful-spill VMA policy over one
// preferred node, not a hard process-wide bind).
//
// Unlike numaBindArena's MPOL_BIND (which restricts allocation to
// numaAllowedNodemask -- every node this process is actually allowed to use,
// per its cpuset), MPOL_PREFERRED(node) drops that restriction at the VMA
// level: if node fills, the kernel spills to *any* node, not just the
// process's allowed set. numaBindGrowth's caller (review I2) is what keeps
// this safe -- node is only ever passed here after confirming node's own
// bit is set in numaAllowedNodemask, so preferring it is never itself a
// cpuset violation -- but the range's *spill* target is not cpuset-bounded
// by this VMA policy the way BIND-all's was. In practice the process's
// cpuset (cgroup cpuset.mems) still bounds actual page allocation
// independently of mempolicy -- the kernel enforces cpuset membership as a
// hard ceiling beneath any mempolicy, including PREFERRED's spill target --
// so this is not an escape from the cpuset, just a weaker *preference*
// restriction than BIND-all carried.
//
// Unlike the removed Layer 2 (which keyed a per-chunk PREFERRED off
// whichever thread happened to be growing the heap's single shared address
// stream at that moment, so adjacent chunks could end up preferring
// different nodes and could never VMA-merge -- see task-6-report.md), every
// chunk mbind'd here belongs to h.curArena[node]'s own address-partitioned
// stream (>=1 TiB apart from every other node's stream, design §12.3): all
// growth into one node's stream always gets that same node's PREFERRED
// policy, so adjacent chunks within a stream merge cleanly, the same way
// numaBindArena's uniform BIND-all does today.
//
// An explicit VMA policy -- BIND-all or, as set here, a node-specific
// PREFERRED -- carries no MPOL_F_MOF, so it exempts its range from the
// kernel's NUMA balancer against any thread that scans it (decision 1),
// independent of which specific policy mode is used; homing via PREFERRED
// does not weaken that exemption.
//
//go:nosplit
func numaBindArenaHome(addr unsafe.Pointer, size uintptr, node int32) {
	var mask numaNodemask
	mask[0] = uintptr(1) << uint(node)
	linux.Syscall6(linux.SYS_MBIND, uintptr(addr), size, uintptr(_MPOL_PREFERRED), uintptr(unsafe.Pointer(&mask[0])), numaMaxNode, 0)
}

// Node-mask soft affinity from the scheduler (design §12.4, task 10,
// ingredient c). Ingredients a (numaBindArenaHome/numaGrowNode above) and
// b (mcentral per-node spanSets, numa_refill_test.go) home memory by
// node; this ingredient keeps the M that touches that memory running on
// (a CPU of) the same node, so the kernel's own NUMA balancer -- exempted
// from scanning our VMAs by decision 1's explicit mempolicies -- is not
// the only thing discouraging cross-node scheduling drift.

// numaStartupFullAffinity records whether this process began life with
// CPU affinity over every online CPU numaTopology found -- i.e. NOT
// narrowed by taskset, a narrowed cpuset, or any other operator
// placement. Computed once, unconditionally, by numaDetectStartupAffinity
// during numaSchedinit.
//
// This is deliberately independent of numaShouldConfine's own narrowed-
// affinity check (numa_linux.go, locked decision 6): that check only
// runs when sched.customGOMAXPROCS is set, because fill-one-socket
// confinement itself requires an explicitly chosen GOMAXPROCS. Node-mask
// soft affinity carries no such precondition -- it applies to any
// multi-node process, confined or not, with any GOMAXPROCS -- so its own
// "operator placement wins" stand-down rule (design §12.4) needs an
// answer that does not depend on customGOMAXPROCS. Both checks reuse the
// same detection mechanism (numaAffinityPopcount vs numaOnlineCPUCount);
// only the trigger conditions differ.
//
// Set once, single-threaded, from numaSchedinit -- schedinit runs before
// any other runtime thread exists, the same invariant numaConfine and
// numaSetProcessBindAll rely on -- so every M created afterward may read
// this as a plain bool with no atomics, exactly like numaTopology itself.
var numaStartupFullAffinity bool

// numaStartupAffinity is the raw CPU affinity mask numaDetectStartupAffinity
// read at startup (the same sched_getaffinity result numaStartupFullAffinity
// is derived from). Kept, not just the derived bool, because
// numaWidenBeforeClone needs the actual mask to restore a soft-affinity-
// narrowed M to before it forks (see that function's doc comment) --
// and unlike numaShouldConfine's own numaSavedAffinity, this is captured
// unconditionally (independent of sched.customGOMAXPROCS), so it is
// reliably populated whenever numaStartupFullAffinity is true.
var numaStartupAffinity [numaCPUMaskBytes]byte

// numaDetectStartupAffinity computes numaStartupFullAffinity and saves
// the mask it was computed from into numaStartupAffinity. Called once
// from numaSchedinit, after numaSetProcessBindAll. No-ops (leaving both
// at their zero values -- fail closed) on a single-node host or an arch
// without numaSetThreadAffinity, since neither can ever reach
// numaNoteSchedule's eligibility gate (numaSoftAffinityEligible)
// regardless of this value.
//
// NOTE (M6, copied from numaShouldConfine's own equivalent note):
// offline CPUs can make sysfs-online and the affinity popcount disagree
// -- a host with offline CPUs may have numaOnlineCPUCount() count a CPU
// this process's affinity mask never included, so the popcount
// comparison declines (numaStartupFullAffinity stays false) even though
// nothing actually narrowed this process's affinity. Conservative:
// offline-CPU hosts simply do not get soft affinity, the same
// consequence numaShouldConfine already accepts for confinement.
func numaDetectStartupAffinity() {
	if numaTopology.NumNodes < 2 || !numaHasSetAffinity {
		return
	}
	r := sched_getaffinity(0, uintptr(numaCPUMaskBytes), &numaStartupAffinity[0])
	if r <= 0 {
		return
	}
	numaStartupFullAffinity = numaAffinityPopcount(numaStartupAffinity[:r]) == numaOnlineCPUCount()
	if debug.numa > 0 {
		if numaStartupFullAffinity {
			println("numa: soft affinity eligible (process started with full affinity)")
		} else {
			println("numa: soft affinity declined: narrowed startup affinity")
		}
	}
}

// numaSoftAffinityEligible reports whether node-mask soft affinity
// (numaNoteSchedule) may ever engage for this process at all: the
// experiment is on, this arch implements numaSetThreadAffinity, the
// machine is multi-node, and this process started with full affinity
// (numaStartupFullAffinity -- operator placement always wins, same rule
// Layer 1/confinement follow). Every input is either a compile-time
// constant or a plain package var written once, single-threaded, during
// numaSchedinit -- so this whole function is a handful of loads, cheap
// enough for numaNoteSchedule's common (steady-state) schedule() path.
func numaSoftAffinityEligible() bool {
	return goexperiment.Numa && numaHasSetAffinity && numaTopology.NumNodes >= 2 && numaStartupFullAffinity
}

// numaSoftAffinityCheckInterval bounds how often numaNoteSchedule pays
// its getcpu(2) syscall, per M, via a nanotime()-gated throttle.
//
// The plan's Forbidden list allows getcpu at "scheduler-pass frequency"
// (the same class as Task 8/9's grow/refill-frequency calls), and an
// early version of this function took that literally -- one getcpu call
// on every schedule() pass. A real-2-node-hardware run of the existing
// BenchmarkPingPongHog (runtime, a tight goroutine ping-pong that calls
// schedule() on every hand-off) showed that costs +56.02% (p=0.000,
// n=10) on numa-dell: schedule() is called far more often than acquirep
// ever is (every blocking channel op, every GC assist yield, ...), so
// "same order as acquirep" and "every schedule() call" are not actually
// the same frequency, and a raw (non-vDSO; Task 14 is the profile-gated
// vDSO/rseq follow-up) getcpu syscall's few-hundred-ns cost, paid that
// often, is a real regression -- not the "cheap compare, no syscall"
// steady state the design requires.
//
// nanotime() is the fix: vDSO-backed on amd64/arm64 linux (not a
// syscall trap), so reading it every schedule() pass to decide whether
// getcpu is even due is itself cheap. getcpu only actually runs once
// per interval per M, regardless of how often schedule() is called in
// between -- in the PingPongHog benchmark's regime, that collapses the
// syscall rate by orders of magnitude. 4ms is not load-bearing (no gate
// pins it): it is short enough that a real cross-node migration is
// re-narrowed promptly relative to typical scheduling quanta, and long
// enough that even a schedule()-call rate in the millions/sec keeps the
// syscall rate in the hundreds/sec.
//
// M9: if Task 11's gates want a different tradeoff (faster convergence
// vs even less overhead), the natural next step is a GODEBUG=numasoft=N
// knob rather than re-tuning this literal -- not done here since no
// gate has asked for it yet.
const numaSoftAffinityCheckInterval = 4 * 1e6 // 4ms in nanotime() units

// numaNoteSchedule applies node-mask soft affinity to the current M
// (design §12.4, ingredient c): at most once every
// numaSoftAffinityCheckInterval per M (see that constant's doc comment
// for why), reads the M's current NUMA node via getcpu (numaCurrentNode)
// and, ONLY if it differs from the node this M's affinity was last
// narrowed to (mp.numa.softAffinityNode), sched_setaffinity's the M to
// that node's CPU mask -- narrowing which CPUs the M may run on without
// pinning to a single CPU, so the kernel keeps full scheduling freedom
// within the node. Steady state -- interval not yet elapsed, the
// overwhelmingly common case -- costs one nanotime() read plus a
// handful of loads and compares: no syscall at all.
//
// Called from schedule(), after findRunnable returns and before execute,
// where mp.locks == 0 is a scheduler invariant (findRunnable never
// returns otherwise -- execute assumes it too); asserted defensively
// below rather than relied upon. Must never be called from acquirep:
// procresize asserts sched.lock held on its call path and allocm holds
// allocmLock plus acquirem on its, so a syscall there would be a
// lock-ordering/latency hazard (design's I3 note) -- schedule() is where
// the M is about to run user code with no locks held, which is exactly
// why the hook lives here instead.
//
// Never engages (returns before any syscall, including nanotime, since
// the cheaper checks are ordered first) when: the machine is
// single-node; this arch cannot set thread affinity; the process itself
// started with narrowed CPU affinity (operator placement wins --
// numaSoftAffinityEligible's numaStartupFullAffinity check);
// fill-one-socket confinement is currently active (numaConfined -- the
// process is already single-node, nothing to do); or confinement has
// stood down (numaStoodDown -- the operator/API raised GOMAXPROCS past
// the node; soft affinity must not re-narrow threads stand-down just
// widened back to full). numaStoodDown is a one-way latch (see its doc
// comment in numa_standdown.go), so once it is set, soft affinity stays
// disabled for the rest of the process's life -- there is no path back
// from a stand-down to re-engaging either ingredient.
//
// Soft affinity has no stand-down of its own for a process that was
// NEVER confined (numaConfined never true, so numaStandDownIfNeeded's
// own precondition -- "if !numaConfined.Load() { return false }" --
// never even evaluates this process): if GOMAXPROCS is later raised on
// such a process, already-narrowed Ms simply stay narrowed to whichever
// node they were last observed on. This is judged benign by design, not
// an oversight: sched_setaffinity narrows a CPU *set* (the whole node),
// never a single CPU, so the kernel keeps full scheduling freedom within
// that node and can still migrate the OS thread under real pressure;
// work-stealing across Ps is untouched (Forbidden list: "no steal
// changes"); and any NEW M the larger GOMAXPROCS brings in finds its own
// node independently via its own first numaNoteSchedule pass (and, as of
// the C1 fix below, starts from a genuinely wide inherited mask, not a
// leaked narrow one). The net effect of a GOMAXPROCS raise is simply
// "more Ms, each independently node-sticky", which is the intended
// steady state -- not "the process reverts to full spread", which is
// what stand-down means for confinement specifically.
//
// The experiment-off case is not checked here: like
// numaConfineIfSmall/numaFixThreadPlacement, this function relies
// entirely on its call site (schedule(), proc.go) gating on
// goexperiment.Numa -- a compile-time constant -- so the call itself
// dead-code-eliminates out of an experiment-off binary instead of
// costing a function call that immediately returns.
func numaNoteSchedule() {
	mp := getg().m
	if mp.locks != 0 {
		// Defensive only -- see the doc comment above; findRunnable is
		// never supposed to return with locks held. A silent return
		// (skip this pass, retry next schedule()) fails safe rather
		// than crashing a process on an invariant this function does
		// not otherwise need to police.
		return
	}
	if !numaSoftAffinityEligible() || numaConfined.Load() || numaStoodDown.Load() {
		return
	}
	now := nanotime()
	if !mp.numa.softAffinityCheckDue(now) {
		return // steady state: throttled, no getcpu syscall this pass
	}
	mp.numa.armSoftAffinityCheck(now + numaSoftAffinityCheckInterval)
	node := numaCurrentNode()
	if node < 0 || node >= 64 {
		// getcpu failed, or reported a node id beyond what this
		// process's nodemask machinery can represent (numaMaxNode,
		// the same bound numaShouldConfine's own getcpu check uses).
		// Nothing to do this pass; retried at the next due check.
		return
	}
	if last, ok := mp.numa.softAffinityNode(); ok && int32(last) == node {
		return // already narrowed to this node
	}
	numaApplySoftAffinity(mp, node)
}

// numaApplySoftAffinity is numaNoteSchedule's slow path: it only runs on
// an actual node change (M2, review): computing/copying the per-node
// mask needs a [numaCPUMaskBytes]byte (1024-byte) local, and keeping
// that out of numaNoteSchedule's own frame keeps the throttled fast path
// (the overwhelmingly common call) a tiny frame -- go:noinline so the
// compiler cannot undo the split by inlining this back into its caller.
//
//go:noinline
func numaApplySoftAffinity(mp *m, node int32) {
	var mask [numaCPUMaskBytes]byte
	if !numaNodeAffinityMask(node, &mask) {
		return
	}
	if numaSetThreadAffinity(0, &mask) {
		mp.numa.setSoftAffinityNode(int8(node))
		if debug.numa > 0 {
			println("numa: soft affinity narrowed M to node", node)
		}
	}
}

// numaWidenBeforeClone undoes node-mask soft affinity's per-M CPU
// narrowing on the calling M just before it clones a new kernel thread
// -- via fork(2)+exec (os/exec, any goroutine on this M), via this
// runtime's own clone(2) call in newosproc (a new non-cgo M), or via
// pthread_create on a cgo build (asmcgocall(_cgo_thread_start, ...), a
// new cgo M) -- so the new thread does not inherit a scheduling HINT as
// if it were deliberate operator placement.
//
// sched_setaffinity's mask is inherited across fork(2), clone(2), AND
// pthread_create (which itself is built on clone(2) with the same
// inheritance semantics): without this, a thread created from a
// soft-affinity-narrowed M would start life with that narrowed mask as
// its OWN startup affinity -- and nothing about that mask distinguishes
// "the runtime narrowed the parent thread as a transient scheduling
// hint" from "an operator ran the parent under taskset". This has two
// distinct, both serious, consequences depending on which path leaked:
//
//   - via os/exec: a GOEXPERIMENT=numa child process reads the inherited
//     mask via its own numaDetectStartupAffinity/numaShouldConfine
//     checks and concludes "operator placement wins", silently declining
//     both confinement and its own soft affinity for its entire
//     lifetime -- design §12.4's stand-down rule, tripped by an internal
//     artifact instead of a real operator (found in this task's own
//     verification: broke 3 existing Workstream A tests when go test's
//     own soft-narrowed Ms spawned testprog subprocesses).
//   - via newm1's own new-M paths (review C1, the critical finding, and
//     NEW-1, the cgo-build gap in the first fix): every new M this
//     runtime itself creates inherits whichever node the CREATING M
//     happened to be soft-narrowed to. Since numaNoteSchedule has no
//     widening path of its own (it only ever narrows), and getcpu on an
//     already-kernel-narrowed thread can only ever report the node it is
//     confined to, that new M's own first numaNoteSchedule pass just
//     confirms the same inherited node instead of discovering its own --
//     a self-reinforcing cascade that collapses the entire process onto
//     whichever node the first M to narrow (typically m0, at its very
//     first schedule() pass, before any other M exists) happened to be
//     on. On numa-dell at GOMAXPROCS=256 this manifested as every M
//     pinned to one node's 128 CPUs -- 2x oversubscription, the other
//     node fully idle -- silently defeating the entire feature while
//     still passing every prior correctness test (which only checked
//     "is each M narrowed to *a* single node", never "do Ms collectively
//     span more than one"). The first fix for this only widened before
//     newosproc's clone(2) call, missing that newm1's cgo branch
//     (asmcgocall(_cgo_thread_start, ...) -> pthread_create) never
//     reaches newosproc at all -- so the exact same cascade was fully
//     intact on any cgo build. Fixed by hoisting the widen call up to
//     the top of newm1, before either branch.
//
// Widens back to numaStartupAffinity: this process's own true starting
// mask, which is always the full mask whenever the calling M could have
// been soft-affinity-narrowed in the first place (numaSoftAffinityEligible
// requires numaStartupFullAffinity, which is only ever true when
// numaStartupAffinity's popcount already equals numaOnlineCPUCount()).
// Also clears this M's cached softAffinityNode (and, per review M4, its
// nextCheck deadline -- see clearSoftAffinityNode): without that, the
// next numaNoteSchedule pass would see the same node as before (or, for
// the deadline, not check again for up to numaSoftAffinityCheckInterval)
// and either believe no change is needed or simply not look -- either
// way permanently or transiently leaving this M's real kernel affinity
// wide instead of promptly re-narrowing to wherever it actually lands.
// No separate restore is needed after the clone/fork/pthread_create call
// returns in the parent -- the clear alone makes the next schedule()
// pass self-heal, immediately (nextCheck==0 is always due). When it
// actually widens (mp was soft-narrowed), it also increments
// numaWidenCount -- a diagnostic-only counter (numaWidenCountForTest,
// export_numa_test.go) that gives automated tests a race-safe way to
// observe that this path fired at all, closing the coverage gap I2's
// -race skip leaves for the newm1/newosproc/cgo site specifically
// (review adjudication (b)).
//
// Called from three sites: syscall_runtime_BeforeFork (proc.go, the
// os/exec ForkExec path), syscall_runtime_BeforeExec (proc.go, the
// syscall.Exec direct-execve path -- fixed alongside this comment: an
// earlier version only widened before fork/clone, missing that execve
// also inherits -- preserves, really, since no new thread is created --
// the calling thread's affinity mask, and syscall.Exec reaches execve
// without ever going through ForkExec/BeforeFork at all), and newm1
// (proc.go, the runtime's own new-M path -- both its cgo and non-cgo
// branches, per NEW-1 above). The first two run immediately before the
// actual fork/clone/execve syscall each guards; newm1's runs before the
// clone/pthread_create call within it. The BeforeFork call site runs
// under the "no more allocation or calls of non-assembly functions"
// constraint syscall.forkAndExecInChild1 documents at its own
// runtime_BeforeFork call site, so this function -- and everything it
// calls -- must stay nosplit; neither the BeforeExec nor the newm1 call
// site has that constraint of its own, but nosplit is a strictly more
// restrictive property, so the same function is safe to call from all
// three.
//
// I5 ruling (review, reworded per NEW-2 to the reviewer's stronger
// ground): at the BeforeFork and newm1 call sites, this function's
// sched_setaffinity syscall runs at essentially the identical
// lock/signal state as the clone/fork/pthread_create call it
// immediately precedes within the same function -- mp.locks != 0
// throughout in both cases (acquirem, held across newm/newm1's entire
// body). An earlier version of this comment claimed "signals already
// blocked, no runtime lock held" for the newosproc call site
// specifically; that was wrong there (execLock is acquired, and
// sigprocmask blocks signals, both AFTER where that call used to run)
// and is moot now that the call lives at the top of newm1, before
// execLock.rlock() in either branch. The correct, simpler ground: this
// code region already has to tolerate one syscall right here, because
// it is about to make a far more consequential one (clone/pthread_create
// itself) a few lines later under the same lock state -- an extra
// sched_setaffinity call is strictly no worse than what is already
// sanctioned at that exact point. The BeforeExec call site differs --
// mp.locks is not necessarily nonzero there, but execLock is held
// write-locked across the whole BeforeExec-to-AfterExec window instead.
// Not a Forbidden-list violation at any of the three sites: that list's
// "no syscalls under sched.lock or with
// mp.locks != 0" rule targets scheduler-hook syscalls that could
// contend with concurrent scheduling state (numaNoteSchedule's own
// schedule()-hook rule) -- the same reasoning Task 9's own recorded
// ruling used to scope that Forbidden-list line to scheduler hooks
// specifically, not every mp.locks!=0 context in the runtime.
//
//go:nosplit
func numaWidenBeforeClone(mp *m) {
	if _, ok := mp.numa.softAffinityNode(); !ok {
		return // never soft-narrowed; nothing to undo
	}
	if numaSetThreadAffinity(0, &numaStartupAffinity) {
		mp.numa.clearSoftAffinityNode()
		numaWidenCount.Add(1)
	}
}

// numaWidenCount counts every time numaWidenBeforeClone actually widened
// a narrowed M (review adjudication (b)): diagnostic-only, read by
// NumaWidenCountForTest (export_numa_test.go) so automated tests have a
// race-safe way to confirm the newm1/newosproc/cgo widen path fired at
// all during M-creation churn, without depending on the distinct-node
// spread TestNUMASoftAffinity's I2 check asserts (which is skipped under
// -race -- see that test's doc comment) or on any particular kernel
// scheduling outcome. Not read by any non-test runtime code.
var numaWidenCount atomic.Uint64

// ---- P/goroutine node placement (v4 stage 2) ----
//
// Design: numa-design/v4-placement-design.md. Each P gets a home node
// (numaAssignPHomes, proportional contiguous partition); the M running
// a P converges to that P's node (numaNoteSchedule's placement path);
// refill routing and heap-growth homing key off the same assignment
// (numaGrowNode). Every consumer checks numaPlacementActive first --
// the pairing rule: assignment may only be consumed while enforcement
// is active.

// numaPlacementEligible is the startup half of numaPlacementActive,
// computed ONCE by numaPlacementInit from schedinit, immediately after
// numaConfineIfSmall has made the confinement decision (single-threaded,
// m0 only -- the same window as every other numa startup decision, and
// deliberately AFTER confinement so mutual exclusivity holds by
// construction).
var numaPlacementEligible bool

// numaPlacementActive reports whether P-home placement is consumed
// anywhere. numaStoodDown can in fact never latch in a
// placement-eligible process (stand-down requires numaConfined, and a
// confined process is permanently placement-ineligible -- see
// numaPlacementInit); the term is defense-in-depth, not a live path.
func numaPlacementActive() bool {
	return numaPlacementEligible && !numaStoodDown.Load()
}

// numaPlacementDeclined prints the decline reason under GODEBUG=numa=1.
func numaPlacementDeclined(reason string) {
	if debug.numa > 0 {
		println("numa: placement declined:", reason)
	}
}

// numaPlacementInit computes numaPlacementEligible. Runs once from
// schedinit (via the goexperiment.Numa-gated call site there), after
// numaConfineIfSmall. The checks mirror numaShouldConfine's ordering
// and diagnostics.
func numaPlacementInit() {
	if numaTopology.NumNodes < 2 {
		// Single-node (or unknown) machine: silently ineligible, the
		// same no-print rule numaShouldConfine applies to this case.
		return
	}
	if numaTopology.TruncatedNodes {
		numaPlacementDeclined("topology truncated")
		return
	}
	if !numaHeapStreamsEnabled {
		numaPlacementDeclined("heap streams disabled")
		return
	}
	// Review C1: the per-node spanSet/arenaHints/curArena arrays are
	// sized numaMaxHeapNodes, and the placement key feeds them via
	// numaGrowNode. A CPU-bearing node with an id past that bound
	// (sparse/CXL ids, >8-node boxes) would home Ps to an index the
	// heap arrays do not have -- the whole feature declines instead.
	for i := int32(0); i < numaTopology.NumNodes; i++ {
		if numaTopology.Nodes[i].NumCPUs > 0 && numaTopology.Nodes[i].ID >= numaMaxHeapNodes {
			numaPlacementDeclined("CPU-bearing node id beyond heap streams")
			return
		}
	}
	if !numaHasSetAffinity {
		numaPlacementDeclined("no sched_setaffinity on this arch")
		return
	}
	if !numaStartupFullAffinity {
		numaPlacementDeclined("narrowed startup affinity")
		return
	}
	if numaConfined.Load() {
		numaPlacementDeclined("confined (fill-one-socket active)")
		return
	}
	numaPlacementEligible = true
	if debug.numa > 0 {
		println("numa: placement eligible")
	}
}

// numaPlacementQuotas computes per-node P quotas for nprocs Ps over the
// CPU-bearing nodes described by cpus (index = node id; 0 = no CPUs on
// that node), writing them to quotas (same indexing; len(cpus) ==
// len(quotas) <= numaMaxHeapNodes). Pure function: largest-remainder
// proportional split (int64 products -- review m3), remainder
// distributed one P per node in decreasing-remainder order with ties to
// the lower node id, then the >=1 rule: every CPU-bearing node gets at
// least one P while some node still has more than one to give.
func numaPlacementQuotas(nprocs int32, cpus, quotas []int32) {
	clear(quotas)
	var total int64
	for _, c := range cpus {
		total += int64(c)
	}
	if total == 0 || nprocs <= 0 {
		return
	}
	var assigned int32
	for i, c := range cpus {
		q := int32(int64(nprocs) * int64(c) / total)
		quotas[i] = q
		assigned += q
	}
	var bumped [numaMaxHeapNodes]bool
	for assigned < nprocs {
		best, bestRem := -1, int64(-1)
		for i, c := range cpus {
			if c == 0 || bumped[i] {
				continue
			}
			if rem := int64(nprocs) * int64(c) % total; rem > bestRem {
				best, bestRem = i, rem
			}
		}
		if best < 0 {
			break // unreachable: remainder count < CPU-bearing node count
		}
		bumped[best] = true
		quotas[best]++
		assigned++
	}
	for {
		zero := -1
		for i, c := range cpus {
			if c > 0 && quotas[i] == 0 {
				zero = i
				break
			}
		}
		if zero < 0 {
			return
		}
		donor, donorQuota := -1, int32(1)
		for i, q := range quotas {
			if q > donorQuota {
				donor, donorQuota = i, q
			}
		}
		if donor < 0 {
			return // nprocs < CPU-bearing node count: some nodes stay at 0
		}
		quotas[donor]--
		quotas[zero]++
	}
}

// numaAssignPHomes (re)assigns contiguous home-node ranges to
// allp[:nprocs]. Called from schedinit (right after numaPlacementInit;
// review M1 -- the bootstrap procresize runs BEFORE the placement
// decision, so waiting for "the next procresize" would leave every home
// unassigned until the first STW, exactly the ramp-up phase in which
// the heap gets laid out and homed) and from procresize under
// sched.lock with the world stopped -- so plain stores are race-free,
// and GOMAXPROCS changes recompute quotas by construction. When
// placement is inactive it clears every home, so a stale assignment can
// never be consumed after (defense-in-depth; see numaPlacementActive).
func numaAssignPHomes(nprocs int32) {
	if !numaPlacementActive() {
		for i := int32(0); i < nprocs; i++ {
			allp[i].numa.clearHome()
		}
		return
	}
	var cpus, quotas [numaMaxHeapNodes]int32
	for i := int32(0); i < numaTopology.NumNodes; i++ {
		id := numaTopology.Nodes[i].ID
		if id >= 0 && id < numaMaxHeapNodes {
			cpus[id] = numaTopology.Nodes[i].NumCPUs
		}
	}
	numaPlacementQuotas(nprocs, cpus[:], quotas[:])
	node, remaining := int32(0), quotas[0]
	for i := int32(0); i < nprocs; i++ {
		for remaining == 0 && node < numaMaxHeapNodes-1 {
			node++
			remaining = quotas[node]
		}
		if remaining == 0 {
			// Defensive only: numaPlacementQuotas distributes exactly
			// nprocs Ps whenever total CPUs > 0, which eligibility
			// guarantees. Leave the P unassigned rather than invent a
			// home (consumers fall back to getcpu).
			allp[i].numa.clearHome()
			continue
		}
		allp[i].numa.setHome(int8(node))
		remaining--
	}
	if debug.numa > 0 {
		print("numa: P homes:")
		for n := int32(0); n < numaMaxHeapNodes; n++ {
			if quotas[n] > 0 {
				print(" node", n, "=[", quotas[n], "]")
			}
		}
		println()
	}
}
