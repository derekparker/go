// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && !(amd64 || arm64)

package runtime

// numaGetCPUNode is the fallback for Linux architectures without a
// getcpu(2) assembly stub -- see numa_linux_getcpu.go for the amd64/arm64
// implementation and why this split exists. It always reports failure, so
// numaCurrentNode always reports 0 here (no node it could otherwise report;
// see numaCurrentNode's doc comment) and fill-one-socket-first confinement
// (numaShouldConfine's getcpu check) never engages on these architectures.
// This is correct, conservative behavior on architectures this getcpu stub
// does not target -- not a bug to fix later.
//
//go:nosplit
func numaGetCPUNode() (node uint32, ok bool) {
	return 0, false
}
