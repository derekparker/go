// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// TestNUMAGetcpu lives in its own file, separate from numa_linux_test.go,
// because it is the only test in that file that does not depend on
// numaSetThreadAffinity (see numa_linux_affinity.go /
// numa_linux_affinity_other.go): getcpu(2) now has an assembly
// implementation on every GOOS=linux architecture (see the getcpu
// wrapper in each sys_linux_*.s), but sched_setaffinity does not --
// numaHasSetAffinity is still amd64/arm64-only. Keeping this test in a
// widely-tagged file, while numa_linux_test.go stays restricted to
// amd64/arm64, lets getcpu get real test coverage on every linux
// architecture without also running the affinity-dependent tests
// somewhere they cannot pass.

//go:build linux && goexperiment.numa

package runtime_test

import (
	"runtime"
	"testing"
)

func TestNUMAGetcpu(t *testing.T) {
	node := runtime.NumaCurrentNodeForTest()
	if node < 0 {
		t.Fatal("getcpu failed")
	}
}
