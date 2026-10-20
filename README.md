# procctl

A control channel between a privileged node agent (Go) and libraries running inside other processes (C).

- **Agent → process:** configuration and exclusive-resource grants travel through a sealed memfd. The process creates the memfd and maps it **read-only**.
- **Process → agent:** the process reports acks and state in a small block in its **own private memory**, which the agent reads with `process_vm_readv`.

**The process never writes shared memory.** The contract is [SPEC.md](SPEC.md); the roadmap is [plan.md](plan.md).

## How it works

```
 target process (e.g. a CUDA app)                     node agent (privileged, hostPID)
 ────────────────────────────────                     ────────────────────────────────
  libfoo + procctl.h                                   github.com/parca-dev/procctl

  memfd "procctl:foo-v1"  ◀──── same pages ────▶  mmap(MAP_SHARED, read/write)
  mapped read-only:                                    writes: config + grants (seqlock),
   ├ header page (written once, sealed)                        heartbeat, ownership
   └ agent area: control block │ grants │ payload       wakes:  FUTEX_WAKE on seq

  state block (private memory) ──────────────────▶  process_vm_readv
   seq │ acks │ resource table │ consumer state       reads: acks, holdings, status
```

The memfd is sealed against shrinking, so the agent can never SIGBUS on it. Each side treats everything the other writes as untrusted input.

**How the agent finds the state block.** The state block lives in caller-provided `static` storage. `procctl_init` writes its address and size into the header (`state_addr`, `state_size`) before sealing, and the header never changes after that.

The agent uses that address only as a `process_vm_readv` target in the target process, never as a pointer of its own. Every read must pass these checks:
- the magic (`PROCSTAT`);
- the header's random `instance_id`;
- an unchanged `seq` around the copy;
- the CRC.

A bad address just makes the read fail. A forked child's inherited copy (same address, parent's `writer_pid`) is recognized as stale.

### Lifecycle, side by side

| | Process side (C, `procctl.h`) | Agent side (Go, `procctl` package) |
|---|---|---|
| **Start** | `procctl_init` creates the memfd, writes the header, seals it, maps it read-only, keeps the fd open, and fires the `procctl:hello` USDT probe. | `Registry.Run` scans `/proc/*/fd` for `/memfd:procctl:*`: at startup, every 60 s, and immediately on `Registry.Notify(pid)`. The `hello` package calls it when the hello uprobe fires, or when a process maps a binary or library containing the probe (perf mmap2 events). |
| **Attach** | — | `Attach` pins the process with a pidfd, fetches the fd (`pidfd_getfd`, or `/proc/<pid>/fd/<n>`), checks it's a sealed tmpfs file of sane size, maps it, and validates a private copy of the header. It emits `Attached`. |
| **Ownership** | Sees `writer_id ≠ 0`, so `procctl_agent_attached` is true. | Claims the region (CAS on `writer_id`). If another healthy agent owns it, this one stays passive and only reads. |
| **Lease** | `procctl_lease_expired` is true if the heartbeat stops changing for `lease_ns`. Then: use defaults, release resources. | Heartbeats every `Lease/4`. |
| **Config** | `procctl_poll` (one atomic load when nothing changed) copies a new generation out and checks seq and CRC. The consumer applies it, then calls `procctl_ack(gen)`. | `Client.Write` / `WritePayload` publish a new generation (seqlock, CRC) and wake waiters. `Client.WaitAck(gen)` returns once the ack shows up in the state block. |
| **Status** | `procctl_state_set` writes the consumer's status into its private state block (seqlock, CRC). Rate-limit it: ≤ ~1 kHz. | `Client.ReadState` reads the state block with `process_vm_readv` (or `/proc/<pid>/mem` where that's refused), re-checks seq, and validates the CRC. Returns acks, the resource table and consumer bytes. |
| **Fork** | The child inherits a read-only view and a stale state block, and its state writes return `-ECHILD`. `procctl_reinit` gives it its own region. | Reads the parent's state; ignores stale blocks (wrong `writer_pid`). A child that reinits shows up as a new region. |
| **Exit** | Nothing to do: no exit hooks. | Process exit is seen via pidfd → `Detached`, mapping released. |
| **Agent shutdown** | Keeps its config until the lease runs out; reverts at once on an uninstall. | `Registry.Close()` sets RELEASED, so a successor takes over with no config flap. `Close(ReleaseLeases(true))` is an uninstall: clients revert immediately. |

## Using the C side

```c
#define _GNU_SOURCE
#define PROCCTL_IMPLEMENTATION      /* in exactly one translation unit */
#include "procctl.h"

static _Alignas(8) unsigned char state[PROCCTL_STATE_STORAGE(0, 256)];
static procctl_t ctx;

procctl_config_t cfg = { .consumer = "myconsumer", .schema_version = 1,
                         .payload_capacity = 4096, .state_capacity = 256,
                         .state_storage = state };
procctl_init(&ctx, &cfg);

/* Hot path: one atomic load when nothing changed. */
uint64_t gen; char buf[4096];
int n = procctl_poll(&ctx, buf, sizeof buf, &gen);
if (n > 0) { apply(buf, n); procctl_ack(&ctx, gen, PROCCTL_TRY); }
else if (n == -ENODATA) { use_defaults(); }       /* agent cleared the config */

/* Now and then (not on the hot path): */
if (procctl_lease_expired(&ctx, 0)) use_defaults();  /* agent gone or hung */
procctl_state_set(&ctx, status, status_len, PROCCTL_TRY);
```

`PROCCTL_TRY` makes state writes return `-EBUSY` instead of waiting, which is right for application threads. `examples/echo/client.c` is a complete client.

## Using the agent side

```go
reg, _ := procctl.NewRegistry(procctl.RegistryOptions{
    Consumer: "myconsumer", // only regions named procctl:myconsumer-v1
    Lease:    10 * time.Second,
})
go reg.Run(ctx) // discovery, attach, heartbeats, exit detection

for ev := range reg.Events() {
    if ev.Kind != procctl.Attached {
        continue // Detached: the process exited
    }
    c := ev.Client
    gen, err := c.WritePayload(config) // publish; wakes the process's waiters
    if err != nil {
        continue // ErrNotOwner: another agent owns this region
    }
    if err := c.WaitAck(ctx, gen); err != nil { /* not applied (yet) */ }
    st, _ := c.ReadState(ctx) // st.AppliedGen, st.Resources, st.Consumer
    _ = st
}

reg.Close() // hand-over: a successor agent takes over without a config flap
```

- **Per process:** each attached process is a `*procctl.Client`. `Client.Info()` gives its pid, its pid-namespace pids, and the validated header.
- **All processes:** `reg.Clients()` lists everything attached.
- **Without a Registry:** `procctl.Scan` and `procctl.Attach` are the building blocks.

`examples/echo/agent` is a complete agent.

## Exclusive resources

Some things may only be used by **one process on the node at a time**. For example, PC sampling on a GPU: if a second process starts it, that process's sampling breaks permanently (see `spike/pcsampling/FINDINGS.md`). procctl arbitrates these with grants (SPEC §9).

- **The agent decides:** `exclusive.Arbiter` grants a resource to one process at a time and rotates it (round robin, renewing a lone holder without a hand-off).
- **Releases are confirmed, never assumed:** the agent re-grants only after the holder's state block says `RELEASED`, or the holder has exited. A hung holder blocks the resource rather than risking two holders, and that's reported as blocked.
- **The holder releases from its own thread:** waiting on `procctl_wait`, not depending on the application calling in, so an idle application still gives the resource back on time.

Process side: a control thread runs the holder loop (`examples/token/client.c` is the reference):

```c
for (;;) {
    procctl_wait(&ctx, 50 * 1000 * 1000);                 /* futex: woken by agent writes */
    procctl_grant_t g;
    procctl_grant_get(&ctx, KIND, id, &g);                /* is there a grant for me? */
    bool ok = g.grant_id && procctl_grant_usable(&ctx, &g, 0) && !procctl_lease_expired(&ctx, 0);
    if (holding && !ok)       { release(); report(RELEASED, held, g.seen_gen); }
    else if (!holding && ok)  { report(ACQUIRING, g.grant_id, g.seen_gen); acquire(); report(HOLDING, ...); }
    else                      { report(current state, ..., g.seen_gen); }
}   /* report() = procctl_resource_report(..., PROCCTL_RES_F_WANT, ...) */
```

Agent side:

```go
arb := exclusive.New(func() []exclusive.Target {
    var ts []exclusive.Target
    for _, c := range reg.Clients() {
        ts = append(ts, c)
    }
    return ts
}, exclusive.Options{
    Kind:   1,
    Policy: exclusive.RoundRobin{Hold: 10 * time.Second},
    Ready:  reg.FirstScanDone(), // never grant before a full discovery pass
})
go arb.Run(ctx)
arb.Snapshot() // holder, hold time, pending releases, blocked time per resource
```

## One definition of the wire layout

The wire structs are defined only in C, in `c/procctl.h`. The Go agent's versions are generated from that header, the way ebpf-profiler does it in `support/`:

```
types_def.go (//go:build ignore)          types_gen.go (committed, DO NOT EDIT)
  type ctrlBlock C.struct_procctl_ctrl  →   type ctrlBlock struct { Seq uint64; Payload_len uint32; ... }
  ResHolding = C.PROCCTL_RES_HOLDING    →   ResHolding = 0x2
```

- **Regenerate:** `make generate` (= `scripts/gen-types.sh`, which runs `go tool cgo -godefs`).
- **Check:** CI runs `scripts/gen-types.sh --check` and fails if `types_gen.go` is stale.
- **Who needs a C compiler:** only whoever regenerates. Users of the Go module don't.
- **Sizes:** `_Static_assert`s in `procctl.h` pin every struct to the size SPEC.md gives.

**Consumer schemas work the same way.** A consumer declares its payload and state as C structs in its own header (fixed-width fields, explicit padding, no bitfields or unions). Its agent-side Go types come from a `types_def.go` of its own run through `go tool cgo -godefs`. The Go side encodes and decodes them with `encoding/binary` (little-endian).

## Layout

| Path | What |
|---|---|
| `SPEC.md` | The wire contract. |
| `c/procctl.h` | The process side: one vendorable header (stb-style). No allocation, threads, constructors, or exit/fork hooks. |
| `*.go` | The agent side (`github.com/parca-dev/procctl`): `Registry`, `Client`, `Scan`, `Attach`. |
| `exclusive/` | The arbiter for exclusive resources, with its model tests. |
| `types_def.go` → `types_gen.go` | The Go wire types, generated from `c/procctl.h` with `go tool cgo -godefs` (`scripts/gen-types.sh`). |
| `examples/echo` | A config-only consumer: C client, Go agent, `demo.sh`. |
| `examples/token` | An exclusive-resource consumer: C holder with a control thread, Go agent with the arbiter. |
| `spike/pcsampling` | The PC sampling experiments behind SPEC §9 (`FINDINGS.md`). |

## Build and test

```
make test     # procctl.h compile matrix; C unit tests (ASan/UBSan); Go tests (-race), incl. C interop, stress, arbiter
make bench    # poll() microbenchmarks
make demo     # the single-host echo demo
```

**Requirements:**
- **Kernel:** Linux ≥ 5.4, little-endian.
- **Agent permissions:** the agent needs ptrace-level access to the client, for `pidfd_getfd` and `process_vm_readv`: the same user with Yama `ptrace_scope` 0, or `CAP_SYS_PTRACE`.

## Licence

Apache-2.0. `procctl.h` bundles SystemTap's `<sys/sdt.h>`, which is dedicated to the public domain (CC0).
