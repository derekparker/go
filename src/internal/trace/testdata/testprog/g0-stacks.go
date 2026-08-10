// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Tests that the tracer records stacks for events emitted while running on
// g0 with no user goroutine (getg().m.curg == nil). See go.dev/issue/68093.

//go:build ignore

package main

import (
	"log"
	"os"
	"runtime/trace"
	"sync"
	"time"
)

func afterFuncCallback() {}

func main() {
	if err := trace.Start(os.Stdout); err != nil {
		log.Fatalf("failed to start tracing: %v", err)
	}

	// AfterFunc creates a goroutine from a timer callback running on g0.
	// The GoCreate event's stack should include the timer path (time.goFunc).
	var wg sync.WaitGroup
	wg.Add(1)
	time.AfterFunc(time.Millisecond, func() {
		afterFuncCallback()
		wg.Done()
	})
	wg.Wait()

	// Timer/Ticker channel wakes also run sendTime on g0 and GoUnpark the
	// waiter. The GoUnblock stack should include time.sendTime.
	timer := time.NewTimer(time.Millisecond)
	<-timer.C

	ticker := time.NewTicker(time.Millisecond)
	<-ticker.C
	ticker.Stop()

	trace.Stop()
}
