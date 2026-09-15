# Pinned-clock confined gate

Date: 2026-09-15

Tree: `ba2d41763e36a9f30af6813920791a5dae76daaa`

Gate: `confined-rebased-external-pin-0914`

Host: two-node, 256-CPU Intel Sapphire Rapids system

## Clock control

The enabled RHEL 10 repositories did not provide `msr-tools`. The same
root-only MSR interface was accessed directly through
`/dev/cpu/<cpu>/msr`.

Before the gate, all 256 CPUs reported identical values:

```
IA32_HWP_CAPABILITIES (0x771): 0x0000000005081327
IA32_HWP_REQUEST      (0x774): 0x000000005500ff01
```

Each CPU's original request was saved separately. The low 24 request bits
were replaced with minimum and maximum performance both set to the
guaranteed-performance value (`0x13`) and desired performance zero:

```
Pinned IA32_HWP_REQUEST: 0x0000000055001313
```

A detached watchdog restored the per-CPU values on request or after a
one-hour timeout. After the gate, all 256 CPUs were read back and verified
at `0x000000005500ff01`. Temporary helper and state files were removed.

## Result

Ten rotating recorded rounds after one warmup:

| Metric | Stock | NUMA | Delta | p |
|---|---:|---:|---:|---:|
| wall ns/op | 1.798 ms | 1.406 ms | −21.79% | 0.000 |
| cycles/run | 2.979 T | 2.480 T | −16.75% | 0.000 |
| instructions/run | 1.690 T | 1.669 T | −1.27% | 0.000 |
| STW/GC | 2.006 ms | 1.608 ms | −19.83% | 0.001 |
| user+sys/op | 148.2 ms | 124.1 ms | −16.30% | 0.000 |
| effective clock | 1.784 GHz | 1.802 GHz | +1.01% | 0.000 |
| CPUs busy | 42/128 | 40/128 | −4.93% | 0.000 |

Peak RSS was statistically unchanged.

Nine of ten NUMA runs recorded zero host-wide hint faults and migrations.
Run 6 recorded 183 hint faults and 182 migrations. Stock averaged 173,849
hint faults per run. The gate analyzer returned `IMPROVED`, but this is a
candidate rather than a final result until it reproduces in a pinned
session on another day. It also does not satisfy an
exactly-zero-in-every-run mechanism rule.
