// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package numa

import (
	"internal/runtime/syscall/linux"
	"internal/strconv"
)

const nodeDir = "/sys/devices/system/node/"

// ReadTopology fills t with the machine's NUMA topology, read from
// /sys/devices/system/node.
//
// scratch is used as I/O scratch space and must have length >= ScratchSize;
// it is not retained after ReadTopology returns. ReadTopology performs no
// heap allocation.
//
// Layer 0 policy: every online node is allowed, including nodes with
// memory but no CPUs (e.g. CXL/HBM). Narrowing this to the process's actual
// memory policy is deferred to a later layer, which will use
// get_mempolicy(MPOL_F_MEMS_ALLOWED) instead of parsing cpusets.
//
// ReadTopology does not populate t.Distance: nothing consumes inter-node
// distance yet. Use ParseDistance directly on a nodeN/distance file if a
// caller needs it.
//
// Returns ErrNoTopology if the machine has no NUMA sysfs (e.g. a kernel
// built without CONFIG_NUMA). Returns an error if scratch cannot hold the
// full contents of a sysfs file: a large non-contiguous cpulist can exceed
// ScratchSize.
func ReadTopology(t *Topology, scratch []byte) error {
	if len(scratch) < ScratchSize {
		return errBufferTooSmall
	}
	scratch = scratch[:ScratchSize]

	// Entries not written below must read as "unknown" (-1), never the
	// zero value, which would misleadingly claim node 0.
	for i := range t.CPUToNode {
		t.CPUToNode[i] = -1
	}
	t.NumNodes = 0
	t.NumAllowedNodes = 0
	t.TruncatedNodes = false

	n, err := readSysfsFile([]byte(nodeDir+"online\x00"), scratch)
	if err != nil {
		return err
	}

	var nodeIDs [MaxNodes]int32
	numNodes, truncated, err := ParseNodeListTruncated(nodeIDs[:], scratch[:n])
	if err != nil {
		return err
	}
	t.TruncatedNodes = truncated

	var pathBuf [64]byte
	for i := 0; i < numNodes; i++ {
		id := nodeIDs[i]
		t.Nodes[i].ID = id

		pn := copy(pathBuf[:], nodeDir+"node")
		p := strconv.AppendInt(pathBuf[:pn], int64(id), 10)
		pn = len(p)
		pn += copy(pathBuf[pn:], "/cpulist\x00")
		path := pathBuf[:pn]

		cn, err := readSysfsFile(path, scratch)
		if err != nil {
			return err
		}

		numCPUs, err := parseCPUListIntoNodeMap(&t.CPUToNode, int8(id), scratch[:cn])
		if err != nil {
			return err
		}
		t.Nodes[i].NumCPUs = int32(numCPUs)
	}

	t.NumNodes = int32(numNodes)
	// Layer 0: every online node is allowed.
	t.NumAllowedNodes = int32(numNodes)

	return nil
}

// readSysfsFile reads the entire contents of the file at path (a
// NUL-terminated string) into buf, and returns the number of bytes read.
//
// Returns ErrNoTopology if path does not exist, and an error if the file's
// contents do not fit in buf.
func readSysfsFile(path []byte, buf []byte) (int, error) {
	fd, errno := linux.Open(&path[0], linux.O_RDONLY|linux.O_CLOEXEC, 0)
	if errno == linux.ENOENT {
		return 0, ErrNoTopology
	} else if errno != 0 {
		return 0, errSyscallFailed
	}

	n, errno := linux.Read(fd, buf)
	linux.Close(fd)
	if errno != 0 {
		return 0, errSyscallFailed
	}
	if n == len(buf) {
		// The read filled the buffer exactly; the file may be
		// larger than buf and we can't tell without another read.
		return 0, errBufferTooSmall
	}

	return n, nil
}
