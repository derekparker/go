// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package numa_test

import (
	"internal/runtime/numa"
	"testing"
)

func TestParseNodeList(t *testing.T) {
	var buf [numa.MaxNodes]int32
	n, err := numa.ParseNodeList(buf[:], []byte("0-1\n"))
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
	n, err := numa.ParseCPUList(buf[:], []byte("0,2,4,6\n"))
	if err != nil || n != 4 || buf[1] != 2 {
		t.Fatalf("%v %v", buf[:n], err)
	}
}

func TestParseNodeListSingle(t *testing.T) {
	var buf [numa.MaxNodes]int32
	n, err := numa.ParseNodeList(buf[:], []byte("0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || buf[0] != 0 {
		t.Fatalf("n=%d buf=%v", n, buf[:n])
	}
}

func TestParseNodeListMultiRange(t *testing.T) {
	var buf [numa.MaxNodes]int32
	n, err := numa.ParseNodeList(buf[:], []byte("0-1,3-4\n"))
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
	var buf [numa.MaxNodes]int32
	n, err := numa.ParseNodeList(buf[:], []byte("\n"))
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
	var buf [numa.MaxNodes]int32
	n, err := numa.ParseNodeList(buf[:], []byte("0,64,65,2\n"))
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
	n, err := numa.ParseCPUList(buf[:], []byte("0,8192,8193,5\n"))
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

func TestParseCPUListIntoNodeMapReportsTruncation(t *testing.T) {
	n, truncated, err := numa.ParseCPUListIntoNodeMap([]byte("0,8192\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || !truncated {
		t.Fatalf("n=%d truncated=%v, want 1, true", n, truncated)
	}
}

func TestParseListBoundsSkipHugeRange(t *testing.T) {
	// A range extending far past the bound must not hang or overflow;
	// it should stop recording once ids reach the limit.
	var buf [numa.MaxNodes]int32
	n, err := numa.ParseNodeList(buf[:], []byte("0-1000000000\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n != numa.MaxNodes {
		t.Fatalf("n=%d, want %d", n, numa.MaxNodes)
	}
	for i := int32(0); i < numa.MaxNodes; i++ {
		if buf[i] != i {
			t.Errorf("buf[%d] = %d, want %d", i, buf[i], i)
		}
	}
}

func TestParseListBufferTooSmall(t *testing.T) {
	var buf [2]int32
	_, err := numa.ParseNodeList(buf[:], []byte("0-5\n"))
	if err != numa.ErrBufferTooSmall {
		t.Fatalf("err = %v, want ErrBufferTooSmall", err)
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
			var buf [numa.MaxNodes]int32
			_, err := numa.ParseNodeList(buf[:], []byte(tc))
			if err == nil {
				t.Fatalf("ParseNodeList(%q) = nil error, want error", tc)
			}
		})
	}
}

func TestNodeOfCPU(t *testing.T) {
	var top numa.Topology
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

func TestParseNodeListTruncated(t *testing.T) {
	// Node ids >= MaxNodes must be silently skipped from dst, but must
	// also be reported via the truncated bool so ReadTopology can stand
	// down NUMA optimizations on a host with more nodes than Topology
	// can represent.
	var buf [numa.MaxNodes]int32
	n, truncated, err := numa.ParseNodeListTruncated(buf[:], []byte("0,64,65,2\n"))
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
	if !truncated {
		t.Error("truncated = false, want true")
	}
}

func TestParseNodeListTruncatedFalse(t *testing.T) {
	var buf [numa.MaxNodes]int32
	_, truncated, err := numa.ParseNodeListTruncated(buf[:], []byte("0-1,3-4\n"))
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("truncated = true, want false")
	}
}

// TestReadTopology is a portable sanity check of ReadTopology: on Linux it
// exercises the real sysfs reader, and on other platforms the stub. It only
// asserts invariants that hold on every machine and platform, since this
// package is tested on machines with differing NUMA topology (including
// single-node machines with no /sys/devices/system/node NUMA directories at
// all).
func TestReadTopology(t *testing.T) {
	var top numa.Topology
	var scratch [numa.ScratchSize]byte
	err := numa.ReadTopology(&top, scratch[:])
	if err == numa.ErrNoTopology {
		t.Skip("no NUMA topology available on this machine")
	}
	if err != nil {
		t.Fatal(err)
	}

	if top.NumNodes < 1 {
		t.Errorf("NumNodes = %d, want >= 1", top.NumNodes)
	}
}
