// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package fipstls allows control over whether crypto/tls requires FIPS-approved settings.
// This package's effects are independent of the use of the BoringCrypto implementation.
package fipstls

import "sync/atomic"

var required uint32

// Force forces crypto/tls to restrict TLS configurations to FIPS-approved settings.
// By design, this call is impossible to undo (except in tests).
//
// Note that this call has an effect even in programs using
// standard crypto (that is, even when Enabled = false).
func Force() {
	atomic.StoreUint32(&required, 1)
}

// Abandon allows non-FIPS-approved settings.
// If called from a non-test binary, it panics.
func Abandon() {
	// Note: Not using boring.UnreachableExceptTests because we want
	// this test to happen even when boring.Enabled() = false.
	name := runtime_arg0()
	// Allow _test for Go command, .test for Bazel,
	// NaClMain for NaCl (where all binaries run as NaClMain),
	// and empty string for Windows (where runtime_arg0 can't easily find the name).
	// Since this is an internal package, testing that this isn't used on the
	// other operating systems should suffice to catch any mistakes.
	if !hasSuffix(name, "_test") && !hasSuffix(name, ".test") && name != "NaClMain" && name != "" {
		panic("fipstls: invalid use of Abandon in " + name)
	}
	atomic.StoreUint32(&required, 0)
}

// provided by runtime
func runtime_arg0() string

func hasSuffix(s, t string) bool {
	return len(s) > len(t) && s[len(s)-len(t):] == t
}

// Required reports whether FIPS-approved settings are required.
func Required() bool {
	return atomic.LoadUint32(&required) != 0
}
