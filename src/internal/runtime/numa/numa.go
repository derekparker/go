// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package numa discovers the NUMA topology of the machine the process is
// running on: the set of memory nodes and which node each CPU belongs to.
//
// This package only discovers topology. It does not change allocator or
// scheduler behavior, and it is safe to import and call unconditionally;
// callers gate use of the result on GOEXPERIMENT=numa.
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
// Topology is large (~8 KiB); callers must always pass it by pointer,
// never return it by value.
type Topology struct {
	// NumNodes is the number of valid entries in Nodes.
	NumNodes int32

	Nodes [MaxNodes]Node

	// TruncatedNodes reports whether the machine has more NUMA nodes
	// than Topology can represent: at least one online node id was >=
	// MaxNodes and was silently dropped from Nodes rather than written
	// out of range (see ParseNodeListTruncated).
	//
	// Callers that bind process or memory policy to "every allowed
	// node" must treat TruncatedNodes as a stand-down signal: binding
	// to only the nodes Nodes could represent would silently exclude
	// the dropped nodes' memory, which is worse than doing nothing.
	TruncatedNodes bool

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

	// The machine has a CPU id that Topology cannot represent.
	errTopologyTooLarge error = stringError("numa: topology too large")

	// A system call failed.
	errSyscallFailed error = stringError("numa: syscall failed")
)
