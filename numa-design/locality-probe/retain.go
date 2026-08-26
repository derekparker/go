// retain mode: -retainmb keeps allocations live, forcing continuous
// fresh heap growth (which IS node-homed) instead of span recycling
// (which is supplied by the node-blind pages layer). Used once for the
// v4 stage-4 attribution: recycling-dominated workloads should show the
// diluted share, growth-dominated ones a near-perfect share.
package main

import "flag"

var retainMB = flag.Int("retainmb", 0, "retain up to this many MiB live (0 = recycle constantly)")
