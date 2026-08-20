// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && !(amd64 || arm64)

package runtime

const numaHasSetAffinity = false

func numaSetThreadAffinity(tid int32, mask *[numaCPUMaskBytes]byte) bool {
	return false
}
