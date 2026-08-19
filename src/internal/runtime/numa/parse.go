// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package numa

import (
	"internal/bytealg"
	"internal/strconv"
)

// maxCPUs is the largest CPU id ParseCPUList and ReadTopology will record,
// matching the length of Topology.CPUToNode.
const maxCPUs = 8192

// ParseNodeList parses a Linux kernel list-format value naming NUMA node
// ids (e.g. the contents of /sys/devices/system/node/online, "0-1\n") into
// dst, and returns the number of ids written.
//
// Node ids >= MaxNodes are silently skipped rather than written out of
// range. If data contains more ids than fit in dst, ParseNodeList returns
// an error.
func ParseNodeList(dst []int32, data []byte) (int, error) {
	return parseList(dst, data, MaxNodes)
}

// ParseCPUList parses a Linux kernel list-format value naming CPU ids (e.g.
// the contents of /sys/devices/system/node/nodeN/cpulist, "0,2,4,6\n") into
// dst, and returns the number of ids written.
//
// CPU ids >= 8192 are silently skipped rather than written out of range. If
// data contains more ids than fit in dst, ParseCPUList returns an error.
func ParseCPUList(dst []int32, data []byte) (int, error) {
	return parseList(dst, data, maxCPUs)
}

// parseList parses a Linux kernel "list format" value: a comma-separated
// list of entries, each either a single id ("5") or an inclusive range
// ("2-7"), terminated by a single trailing newline. See cpuset(7) "Formats"
// for the format this mirrors (used throughout /sys/devices/system/node).
//
// Ids >= limit are skipped rather than written to dst.
func parseList(dst []int32, data []byte, limit int32) (int, error) {
	i := bytealg.IndexByte(data, '\n')
	if i < 0 {
		return 0, errMalformedFile
	}
	data = data[:i]

	n := 0
	for len(data) > 0 {
		var tok []byte
		if i := bytealg.IndexByte(data, ','); i >= 0 {
			tok = data[:i]
			data = data[i+1:]
		} else {
			tok = data
			data = nil
		}

		start, end, err := parseRange(tok)
		if err != nil {
			return 0, err
		}

		for v := start; v <= end; v++ {
			if v >= int64(limit) {
				// Ids only increase within a range, and ranges
				// are visited in increasing order, so nothing
				// past this point (in this range or any later
				// token) can be in range either... except a
				// later token isn't guaranteed to be
				// increasing, so only stop this range, not
				// the whole list.
				break
			}
			if n >= len(dst) {
				return 0, errBufferTooSmall
			}
			dst[n] = int32(v)
			n++
		}
	}

	return n, nil
}

// parseRange parses a single list entry, either "N" or "N-M" (inclusive,
// M >= N).
func parseRange(tok []byte) (start, end int64, err error) {
	dash := bytealg.IndexByte(tok, '-')
	if dash < 0 {
		v, err := parseUint(tok)
		if err != nil {
			return 0, 0, err
		}
		return v, v, nil
	}

	start, err = parseUint(tok[:dash])
	if err != nil {
		return 0, 0, err
	}
	end, err = parseUint(tok[dash+1:])
	if err != nil {
		return 0, 0, err
	}
	if end < start {
		return 0, 0, errMalformedFile
	}
	return start, end, nil
}

// parseUint parses tok as a non-negative base-10 integer.
func parseUint(tok []byte) (int64, error) {
	if len(tok) == 0 {
		return 0, errMalformedFile
	}
	// Neither cmd/compile nor gccgo allocates for this string
	// conversion, since it does not escape.
	v, err := strconv.ParseInt(string(tok), 10, 64)
	if err != nil || v < 0 {
		return 0, errMalformedFile
	}
	return v, nil
}

// ParseDistance parses a Linux kernel NUMA distance table row (e.g. the
// contents of /sys/devices/system/node/nodeN/distance, "10 20 20 30\n"), a
// space-separated list of distance values, into dst, and returns the
// number of values written.
//
// If data contains more values than fit in dst, ParseDistance returns an
// error.
func ParseDistance(dst []uint8, data []byte) (int, error) {
	i := bytealg.IndexByte(data, '\n')
	if i < 0 {
		return 0, errMalformedFile
	}
	data = data[:i]

	n := 0
	for len(data) > 0 {
		var tok []byte
		if i := bytealg.IndexByte(data, ' '); i >= 0 {
			tok = data[:i]
			data = data[i+1:]
		} else {
			tok = data
			data = nil
		}

		v, err := parseUint(tok)
		if err != nil || v > 0xff {
			return 0, errMalformedFile
		}

		if n >= len(dst) {
			return 0, errBufferTooSmall
		}
		dst[n] = uint8(v)
		n++
	}

	return n, nil
}
