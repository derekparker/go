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

// Mempolicy constants used by numaConfine and numaTaskPolicyIsDefault.
//
// Linux get_nodes, shared by set_mempolicy and mbind, decrements maxnode
// before copying that many bits from the user mask. Consequently the ABI
// argument for node IDs [0, numa.MaxNodes) is numa.MaxNodes+1, while the
// user buffer itself contains exactly numa.MaxNodes bits.
const numaMaxNode = numa.MaxNodes + 1

// numaCPUMaskBytes is the byte length of the CPU affinity mask passed to
// sched_setaffinity(2) (via numaSetThreadAffinity) and sched_getaffinity.
// 8192 CPUs / 8 bits per byte, matching numa.Topology's CPUToNode
// capacity so any CPU id Topology can represent also fits this mask.
const numaCPUMaskBytes = 8192 / 8

const (
	_MPOL_PREFERRED  = 1
	_MPOL_MODE_FLAGS = 0xe000 // MPOL_F_NUMA_BALANCING (1<<13) | MPOL_F_RELATIVE_NODES (1<<14) | MPOL_F_STATIC_NODES (1<<15): optional flag bits get_mempolicy may OR into its returned mode

	numaNodemaskBits = 8 * unsafe.Sizeof(uintptr(0))
	// One word on 64-bit platforms, two on 32-bit.
	numaNodemaskWords = (numa.MaxNodes + numaNodemaskBits - 1) / numaNodemaskBits
)

// numaNodemask is the on-the-wire nodemask buffer for set_mempolicy.
// word[0] holds node ids [0, numaNodemaskBits);
// on 32-bit platforms only, word[1] holds node ids [numaNodemaskBits,
// 64). Word order matches the kernel's ulong-array indexing (low bits
// first), so this is correct independent of byte endianness.
type numaNodemask [numaNodemaskWords]uintptr

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
	numaDetectStartupAffinity()
	if debug.numa > 0 {
		println("numa: nodes", numaTopology.NumNodes)
	}
}

// numaInitTopology fills numaTopology by reading the machine's NUMA
// topology from sysfs. If topology discovery fails (e.g. no NUMA sysfs, or
// a sysfs file too large for numaScratch), numaTopology is reset to its
// zero value and then set to a single-node topology: NumNodes is set to
// 1, and every CPU maps to node 0 (NodeOfCPU
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

// numaTaskPolicyIsDefault reports whether the calling thread has no memory
// policy set. Confinement must not replace a policy selected by numactl or
// another launcher.
func numaTaskPolicyIsDefault() bool {
	var mode int32
	_, _, errno := linux.Syscall6(linux.SYS_GET_MEMPOLICY, uintptr(unsafe.Pointer(&mode)), 0, 0, 0, 0, 0)
	return errno == 0 && mode&^_MPOL_MODE_FLAGS == 0
}

// numaConfined is true while single-node confinement is
// active. Set once in numaConfineIfSmall (m0 is the only runtime
// thread); cleared only by numaStandDownIfNeeded.
var numaConfined atomic.Bool

// numaStoodDown is declared in numa_standdown.go, not here: stopm's hook
// (proc.go) inlines numaStoodDown.Load() directly so the steady-state
// (never confined, or confined-but-never-stood-down) cost at every park
// is one inlined atomic load, not a call into this Linux-only file. See
// numa_standdown.go's doc comment.

var numaConfinedNodeCPUs int32

// numaConfineIfSmall applies single-node confinement when the decision
// conditions hold. It runs after procresize and before the runtime creates
// any other thread. Memory policies are established only after eligibility
// is known, so a process that does not confine remains policy-default.
func numaConfineIfSmall(procs int32) {
	node, ok := numaShouldConfine(procs)
	if !ok {
		return
	}
	if !numaConfine(node) {
		numaConfineDeclined("could not apply confinement")
		return
	}
	if debug.numa > 0 {
		println("numa: confined to node", node, "cpus", numaConfinedNodeCPUs)
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

// numaNodeAffinityMask fills mask with the CPUs belonging to node. This
// runs once, during confinement, so constructing the mask directly avoids
// a 64 KiB cache of masks for nodes the process will never select.
func numaNodeAffinityMask(node int32, mask *[numaCPUMaskBytes]byte) bool {
	if node < 0 || node >= 64 {
		return false
	}
	found := false
	for cpu := 0; cpu < len(numaTopology.CPUToNode); cpu++ {
		if numaTopology.NodeOfCPU(cpu) == node {
			mask[cpu/8] |= 1 << (uint(cpu) % 8)
			found = true
		}
	}
	return found
}

// numaShouldConfine reports whether single-node confinement should
// engage. It requires a complete multi-node topology, an explicitly chosen
// GOMAXPROCS no larger than the current node, default CPU and memory
// placement, and the required affinity and getcpu syscalls. Explicit
// operator placement always wins.
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
	// Read without sched.lock: numaShouldConfine runs from schedinit, before
	// any other runtime thread exists (numaConfineIfSmall's call site is
	// still single-threaded m0), so there is no concurrent writer to race.
	// Every other reader/writer of sched.customGOMAXPROCS (GOMAXPROCS,
	// SetDefaultGOMAXPROCS, sysmonUpdateGOMAXPROCS, updateMaxProcsGoroutine)
	// takes sched.lock because they run concurrently with other Ms.
	if !sched.customGOMAXPROCS {
		numaConfineDeclined("GOMAXPROCS not explicitly set")
		return 0, false
	}
	if !numaStartupFullAffinity {
		// taskset / narrowed cpuset: operator placement wins. NOTE:
		// offline CPUs can also make sysfs-online and the affinity
		// popcount disagree; the check then declines — conservative
		// (offline-CPU hosts simply do not confine). Record if seen.
		numaConfineDeclined("affinity narrower than online CPUs")
		return 0, false
	}
	if !numaTaskPolicyIsDefault() {
		numaConfineDeclined("memory policy already set")
		return 0, false
	}
	node := numaCurrentNode() // confine to the boot CPU's node
	// node < 0 (the getcpu syscall itself failed) and
	// node >= 64 (getcpu succeeded but returned an id past what the
	// 64-node nodemask this package uses can represent) are
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
// The preferred policy is inherited by every later runtime-created M.
// Threads created by C before runtime initialization are outside this
// mechanism and retain their existing affinity and memory policy.
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
		// A failed set_mempolicy leaves the default policy unchanged.
		numaSetThreadAffinity(0, &numaStartupAffinity)
		return false
	}
	// Publish the node size before the flag read by stand-down.
	numaConfinedNodeCPUs = numaNodeCPUCount(node) // same source as the decision
	numaConfined.Store(true)
	return true
}

// numaStandDownIfNeeded reports whether single-node confinement
// should stand down: the new GOMAXPROCS exceeds the confined node, or it
// is no longer explicitly selected. The latter closes a feedback loop in
// which the default GOMAXPROCS observes the affinity this experiment
// narrowed.
//
// This function is syscall-free because its caller has mp.locks != 0.
// numaStandDownWiden restores the calling M after the world restarts;
// every other M restores itself at its next park.
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
			println("numa: confinement stood down, GOMAXPROCS reverted to default")
		} else {
			println("numa: confinement stood down, GOMAXPROCS", procs, ">", numaConfinedNodeCPUs)
		}
	}
	return true
}

// numaStandDownWiden restores the calling M after a stand-down trigger.
// Other Ms restore themselves at their next park in
// numaFixThreadPlacement. Avoid walking allm and changing affinity by TID:
// an exiting M's stale TID may already have been reused by the kernel.
func numaStandDownWiden() {
	numaFixThreadPlacement()
}

// numaFixThreadPlacement converges the calling M's placement after
// stand-down: restores its startup CPU affinity and MPOL_DEFAULT task
// policy. Each M performs this once at its first park after stand-down.
func numaFixThreadPlacement() {
	if !numaStoodDown.Load() {
		return
	}
	mp := getg().m
	if mp.numa.placementDone() {
		return
	}
	if !numaSetThreadAffinity(0, &numaStartupAffinity) {
		return // retry at next park
	}
	if _, _, errno := linux.Syscall6(linux.SYS_SET_MEMPOLICY,
		0, 0, 0, 0, 0, 0); errno != 0 {
		return // retry at next park
	}
	// Latch only after every syscall succeeded: latching first
	// would permanently strand a thread that raced a transient failure.
	mp.numa.setPlacementDone()
}

// numaStartupFullAffinity records whether this process began life with
// CPU affinity over every online CPU numaTopology found -- i.e. NOT
// narrowed by taskset, a narrowed cpuset, or any other operator
// placement. Computed once, unconditionally, by numaDetectStartupAffinity
// during numaSchedinit.
//
// Set once, single-threaded, from numaSchedinit, so every M created
// afterward may read this as a plain bool.
var numaStartupFullAffinity bool

// numaStartupAffinity is the raw CPU affinity mask numaDetectStartupAffinity
// read at startup (the same sched_getaffinity result numaStartupFullAffinity
// is derived from). It is also the mask restored after stand-down.
var numaStartupAffinity [numaCPUMaskBytes]byte

// numaDetectStartupAffinity computes numaStartupFullAffinity and saves
// the mask it was computed from into numaStartupAffinity. Called once
// from numaSchedinit. No-ops (leaving both at their zero values -- fail
// closed) on a single-node host or an arch
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
