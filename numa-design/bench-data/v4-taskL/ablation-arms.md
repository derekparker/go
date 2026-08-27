# Task L ablation arms (all test binaries built locally with ONE compiler
# from the fec32e3892 tree, scp'd to numa-dell; edits applied and reverted
# per-arm, recorded here verbatim)

- A0: experiment OFF.
- A1: experiment ON, unmodified (final tree).
- A2: ON; numaArmWindows call in numaSchedinit disabled
  (`if false && numaHeapStreamsEnabled && ...`) — windowed page path never
  arms, streams/routing still on. DIAGNOSTIC-ONLY state; exposed the
  invalid-window grow-per-refill latent bug.
- A3: ON; `numaHeapStreamsEnabled = false` forced in mallocinit — no
  per-node streams, no routing, no windows at runtime.
- A4: A3 + `numaSetProcessBindAll` body disabled (no BIND-all task policy,
  no published nodemask ⇒ arena mbind inert).
- A5: A3 + `numaConfineIfSmall` body disabled (no confinement).
- A6: experiment ON at v3-final 273a776194 (pre-v4), built from a temp
  worktree sharing the same compiler binaries and generated zbootstrap/
  zversion files.

Sweeps: L1 n=20 off/on interleaved; L2 fixed-order n=10 (superseded — order
violation); L2r/L3 rotating n=12; L4 rotating n=12 three-arm.
