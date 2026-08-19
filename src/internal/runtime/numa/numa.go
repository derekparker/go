// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package numa discovers the NUMA topology of the machine the process is
// running on: the set of memory nodes and which node each CPU belongs to.
//
// This package is Layer 0 of the NUMA support plan: it only discovers
// topology. It does not change allocator or scheduler behavior, and it is
// safe to import and call unconditionally; callers gate use of the result
// on GOEXPERIMENT=numa.
package numa

const (
	// MaxNodes is the largest number of NUMA nodes a Topology can record.
	// Node ids >= MaxNodes are dropped by ParseNodeList and ReadTopology
	// rather than written out of range.
	MaxNodes = 64

	// ScratchSize is the required minimum length of the scratch buffer
	// passed to ReadTopology.
	ScratchSize = 8192
)

// Node describes one NUMA node.
type Node struct {
	ID      int32
	NumCPUs int32
}

// Topology describes the NUMA topology of the machine: its nodes and the
// node each CPU belongs to.
//
// Topology is large (~13 KiB); callers must always pass it by pointer,
// never return it by value.
type Topology struct {
	// NumNodes is the number of valid entries in Nodes.
	NumNodes int32

	// NumAllowedNodes is the number of nodes this process may allocate
	// memory from; Nodes[0:NumAllowedNodes] are the allowed nodes.
	//
	// At Layer 0, every online node is allowed, including nodes with
	// memory but no CPUs (e.g. CXL/HBM): excluding CPU-less nodes here
	// would silently shrink the usable-memory mask. A later layer
	// narrows this to the process's actual memory policy via
	// get_mempolicy(MPOL_F_MEMS_ALLOWED).
	NumAllowedNodes int32

	Nodes [MaxNodes]Node

	// Distance is the inter-node distance table, indexed by node index
	// (not node id). It is not populated by ReadTopology: nothing
	// consumes inter-node distance yet. Use ParseDistance directly on a
	// nodeN/distance file if a caller needs it.
	Distance [MaxNodes][MaxNodes]uint8

	// CPUToNode maps a CPU id to the id of the node it belongs to, or -1
	// if unknown. This is diagnostic/test-only: no hot path does a
	// CPU->node lookup, since getcpu returns the node directly.
	CPUToNode [8192]int8
}

// NodeOfCPU returns the id of the node CPU cpu belongs to, or -1 if cpu is
// out of range or its node is unknown.
func (t *Topology) NodeOfCPU(cpu int) int32 {
	if cpu < 0 || cpu >= len(t.CPUToNode) {
		return -1
	}
	return int32(t.CPUToNode[cpu])
}

// NodeAllowed reports whether this process may allocate memory from NUMA
// node id node.
func (t *Topology) NodeAllowed(node int32) bool {
	for i := int32(0); i < t.NumAllowedNodes; i++ {
		if t.Nodes[i].ID == node {
			return true
		}
	}
	return false
}

// AllowedNode returns the id of the i'th node this process may allocate
// memory from, for i in [0, NumAllowedNodes). It returns -1 if i is out of
// that range.
func (t *Topology) AllowedNode(i int32) int32 {
	if i < 0 || i >= t.NumAllowedNodes {
		return -1
	}
	return t.Nodes[i].ID
}

// stringError is a trivial implementation of error, equivalent to
// errors.New, which cannot be imported from a runtime package.
type stringError string

func (e stringError) Error() string { return string(e) }

// All errors are explicitly converted to type error in global
// initialization to ensure that the linker allocates a static interface
// value. This is necessary because these errors may be used before the
// allocator is available.
var (
	// ErrNoTopology indicates the platform does not expose NUMA topology
	// information, e.g. no /sys/devices/system/node on Linux.
	ErrNoTopology error = stringError("numa: no topology information available")

	// The contents of a sysfs file did not match the expected format.
	errMalformedFile error = stringError("numa: malformed sysfs file")

	// A caller-provided buffer (scratch or a parser's dst) was too small
	// to hold the data.
	errBufferTooSmall error = stringError("numa: buffer too small")

	// A system call failed.
	errSyscallFailed error = stringError("numa: syscall failed")
)
