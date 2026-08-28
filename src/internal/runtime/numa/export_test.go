// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Export guts for testing.
//
// This file is package numa, not numa_test: it exposes unexported
// package internals to parse_test.go (package numa_test) without giving
// parse_test.go itself access to package numa's internals directly.
//
// It deliberately does not import "testing" (or any other package):
// package runtime imports internal/runtime/numa, so a package-numa test
// file that imported testing would create an import cycle the moment
// "go test" builds this package's test binary (testing -> ... ->
// runtime -> internal/runtime/numa -> testing). See cgroup's
// export_test.go for the same pattern.
package numa

// ErrBufferTooSmall exports errBufferTooSmall for tests in package
// numa_test.
var ErrBufferTooSmall = errBufferTooSmall
