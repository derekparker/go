// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rand

import "syscall/js"

// The maximum buffer size for crypto.getRandomValues is 65536 bytes.
// https://developer.mozilla.org/en-US/docs/Web/API/Crypto/getRandomValues#exceptions
const maxGetRandomRead = 64 << 10

// read implements a pseudorandom generator
// using JavaScript crypto.getRandomValues method.
// See https://developer.mozilla.org/en-US/docs/Web/API/Crypto/getRandomValues.
var read = batched(getRandom, maxGetRandomRead)

var jsCrypto = js.Global().Get("crypto")
var uint8Array = js.Global().Get("Uint8Array")

func getRandom(b []byte) error {
	a := uint8Array.New(len(b))
	jsCrypto.Call("getRandomValues", a)
	js.CopyBytesToGo(b, a)
	return nil
}

// batched returns a function that calls f to populate a []byte by chunking it
// into subslices of, at most, readMax bytes.
func batched(f func([]byte) error, readMax int) func([]byte) error {
	return func(out []byte) error {
		for len(out) > 0 {
			read := len(out)
			if read > readMax {
				read = readMax
			}
			if err := f(out[:read]); err != nil {
				return err
			}
			out = out[read:]
		}
		return nil
	}
}
