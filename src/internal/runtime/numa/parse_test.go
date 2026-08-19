// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package numa

import "testing"

func TestParseNodeList(t *testing.T) {
	var buf [MaxNodes]int32
	n, err := ParseNodeList(buf[:], []byte("0-1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || buf[0] != 0 || buf[1] != 1 {
		t.Fatalf("n=%d buf=%v", n, buf[:n])
	}
}

func TestParseCPUListInterleave(t *testing.T) {
	// Shape of numa-dell node0: even CPUs. Use a short fixture.
	var buf [16]int32
	n, err := ParseCPUList(buf[:], []byte("0,2,4,6\n"))
	if err != nil || n != 4 || buf[1] != 2 {
		t.Fatalf("%v %v", buf[:n], err)
	}
}

func TestParseNodeListSingle(t *testing.T) {
	var buf [MaxNodes]int32
	n, err := ParseNodeList(buf[:], []byte("0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || buf[0] != 0 {
		t.Fatalf("n=%d buf=%v", n, buf[:n])
	}
}

func TestParseNodeListMultiRange(t *testing.T) {
	var buf [MaxNodes]int32
	n, err := ParseNodeList(buf[:], []byte("0-1,3-4\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []int32{0, 1, 3, 4}
	if n != len(want) {
		t.Fatalf("n=%d buf=%v", n, buf[:n])
	}
	for i, w := range want {
		if buf[i] != w {
			t.Errorf("buf[%d] = %d, want %d", i, buf[i], w)
		}
	}
}

func TestParseNodeListEmpty(t *testing.T) {
	var buf [MaxNodes]int32
	n, err := ParseNodeList(buf[:], []byte("\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("n=%d, want 0", n)
	}
}

func TestParseNodeListBoundsSkip(t *testing.T) {
	// Node ids >= MaxNodes must be silently skipped, never written out
	// of range.
	var buf [MaxNodes]int32
	n, err := ParseNodeList(buf[:], []byte("0,64,65,2\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []int32{0, 2}
	if n != len(want) {
		t.Fatalf("n=%d buf=%v", n, buf[:n])
	}
	for i, w := range want {
		if buf[i] != w {
			t.Errorf("buf[%d] = %d, want %d", i, buf[i], w)
		}
	}
}

func TestParseCPUListBoundsSkip(t *testing.T) {
	// CPU ids >= 8192 must be silently skipped.
	var buf [16]int32
	n, err := ParseCPUList(buf[:], []byte("0,8192,8193,5\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []int32{0, 5}
	if n != len(want) {
		t.Fatalf("n=%d buf=%v", n, buf[:n])
	}
	for i, w := range want {
		if buf[i] != w {
			t.Errorf("buf[%d] = %d, want %d", i, buf[i], w)
		}
	}
}

func TestParseListBoundsSkipHugeRange(t *testing.T) {
	// A range extending far past the bound must not hang or overflow;
	// it should stop recording once ids reach the limit.
	var buf [MaxNodes]int32
	n, err := ParseNodeList(buf[:], []byte("0-1000000000\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n != MaxNodes {
		t.Fatalf("n=%d, want %d", n, MaxNodes)
	}
	for i := int32(0); i < MaxNodes; i++ {
		if buf[i] != i {
			t.Errorf("buf[%d] = %d, want %d", i, buf[i], i)
		}
	}
}

func TestParseListBufferTooSmall(t *testing.T) {
	var buf [2]int32
	_, err := ParseNodeList(buf[:], []byte("0-5\n"))
	if err != errBufferTooSmall {
		t.Fatalf("err = %v, want errBufferTooSmall", err)
	}
}

func TestParseListMalformed(t *testing.T) {
	tests := []string{
		"",                         // no trailing newline
		"0-1",                      // no trailing newline
		"a\n",                      // not a number
		"0,,1\n",                   // empty token
		"0-\n",                     // dangling range
		"-1\n",                     // negative
		"5-2\n",                    // reversed range
		"0-1-2\n",                  // too many dashes
		"1000000000000000000000\n", // overflow
	}
	for _, tc := range tests {
		t.Run(tc, func(t *testing.T) {
			var buf [MaxNodes]int32
			_, err := ParseNodeList(buf[:], []byte(tc))
			if err == nil {
				t.Fatalf("ParseNodeList(%q) = nil error, want error", tc)
			}
		})
	}
}

func TestParseDistance(t *testing.T) {
	var buf [MaxNodes]uint8
	n, err := ParseDistance(buf[:], []byte("10 20 20 30\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []uint8{10, 20, 20, 30}
	if n != len(want) {
		t.Fatalf("n=%d buf=%v", n, buf[:n])
	}
	for i, w := range want {
		if buf[i] != w {
			t.Errorf("buf[%d] = %d, want %d", i, buf[i], w)
		}
	}
}

func TestParseDistanceMalformed(t *testing.T) {
	tests := []string{
		"",         // no trailing newline
		"10 20",    // no trailing newline
		"10  20\n", // double space -> empty token
		"10 abc\n", // not a number
		"10 -1\n",  // negative
		"10 256\n", // out of uint8 range
	}
	for _, tc := range tests {
		t.Run(tc, func(t *testing.T) {
			var buf [MaxNodes]uint8
			_, err := ParseDistance(buf[:], []byte(tc))
			if err == nil {
				t.Fatalf("ParseDistance(%q) = nil error, want error", tc)
			}
		})
	}
}

func TestParseDistanceBufferTooSmall(t *testing.T) {
	var buf [2]uint8
	_, err := ParseDistance(buf[:], []byte("10 20 30\n"))
	if err != errBufferTooSmall {
		t.Fatalf("err = %v, want errBufferTooSmall", err)
	}
}

func TestNodeOfCPU(t *testing.T) {
	var top Topology
	for i := range top.CPUToNode {
		top.CPUToNode[i] = -1
	}
	top.CPUToNode[3] = 1

	if got := top.NodeOfCPU(3); got != 1 {
		t.Errorf("NodeOfCPU(3) = %d, want 1", got)
	}
	if got := top.NodeOfCPU(4); got != -1 {
		t.Errorf("NodeOfCPU(4) = %d, want -1", got)
	}
	if got := top.NodeOfCPU(-1); got != -1 {
		t.Errorf("NodeOfCPU(-1) = %d, want -1", got)
	}
	if got := top.NodeOfCPU(len(top.CPUToNode)); got != -1 {
		t.Errorf("NodeOfCPU(len) = %d, want -1", got)
	}
}

func TestNodeAllowedAndAllowedNode(t *testing.T) {
	var top Topology
	top.NumNodes = 3
	top.NumAllowedNodes = 2
	top.Nodes[0] = Node{ID: 0, NumCPUs: 4}
	top.Nodes[1] = Node{ID: 2, NumCPUs: 4}
	top.Nodes[2] = Node{ID: 5, NumCPUs: 0} // not in the allowed prefix

	if !top.NodeAllowed(0) {
		t.Error("NodeAllowed(0) = false, want true")
	}
	if !top.NodeAllowed(2) {
		t.Error("NodeAllowed(2) = false, want true")
	}
	if top.NodeAllowed(5) {
		t.Error("NodeAllowed(5) = true, want false")
	}
	if top.NodeAllowed(99) {
		t.Error("NodeAllowed(99) = true, want false")
	}

	if got := top.AllowedNode(0); got != 0 {
		t.Errorf("AllowedNode(0) = %d, want 0", got)
	}
	if got := top.AllowedNode(1); got != 2 {
		t.Errorf("AllowedNode(1) = %d, want 2", got)
	}
	if got := top.AllowedNode(2); got != -1 {
		t.Errorf("AllowedNode(2) = %d, want -1", got)
	}
	if got := top.AllowedNode(-1); got != -1 {
		t.Errorf("AllowedNode(-1) = %d, want -1", got)
	}
}

// TestReadTopology is a portable sanity check of ReadTopology: on Linux it
// exercises the real sysfs reader, and on other platforms the stub. It only
// asserts invariants that hold on every machine and platform, since this
// package is tested on machines with differing NUMA topology (including
// single-node machines with no /sys/devices/system/node NUMA directories at
// all).
func TestReadTopology(t *testing.T) {
	var top Topology
	var scratch [ScratchSize]byte
	err := ReadTopology(&top, scratch[:])
	if err == ErrNoTopology {
		t.Skip("no NUMA topology available on this machine")
	}
	if err != nil {
		t.Fatal(err)
	}

	if top.NumNodes < 1 {
		t.Errorf("NumNodes = %d, want >= 1", top.NumNodes)
	}
	if top.NumAllowedNodes < 1 || top.NumAllowedNodes > top.NumNodes {
		t.Errorf("NumAllowedNodes = %d, want in [1, %d]", top.NumAllowedNodes, top.NumNodes)
	}

	// Every allowed node must resolve back to itself through AllowedNode.
	seen := make(map[int32]bool)
	for i := int32(0); i < top.NumAllowedNodes; i++ {
		id := top.AllowedNode(i)
		if id < 0 {
			t.Errorf("AllowedNode(%d) = %d, want >= 0", i, id)
		}
		if !top.NodeAllowed(id) {
			t.Errorf("NodeAllowed(%d) = false, want true (from AllowedNode(%d))", id, i)
		}
		if seen[id] {
			t.Errorf("duplicate allowed node id %d", id)
		}
		seen[id] = true
	}
}
