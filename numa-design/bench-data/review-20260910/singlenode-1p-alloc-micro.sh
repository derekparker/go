#!/usr/bin/env bash
S=$(dirname "$0")
: > $S/micro-off.out; : > $S/micro-on.out
for i in $(seq 20); do
  for arm in off on; do
    GOMAXPROCS=1 taskset -c 5 $S/malloc-$arm.test -test.run XXX_none -test.bench 'Malloc(8|16)$' -test.benchtime 1s -test.count 1 2>/dev/null | grep '^Benchmark' >> $S/micro-$arm.out
  done
done
echo MICRO_DONE
