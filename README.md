# procctl

A control channel between a privileged node agent (Go) and libraries running inside other processes (C).

- **Agent → process:** configuration and exclusive-resource grants travel through a sealed memfd. The process creates the memfd and maps it **read-only**.
- **Process → agent:** the process reports acks and state in a small block in its **own private memory**, which the agent reads with `process_vm_readv`.

**The process never writes shared memory.** The contract is [SPEC.md](SPEC.md); the roadmap is [plan.md](plan.md).

## Layout

| Path | What |
|---|---|
| `c/procctl.h` | The process side: one vendorable header (stb-style). No allocation, threads, constructors, or exit/fork hooks. |
| `*.go` | The agent side (`github.com/parca-dev/procctl`): `Scan`, `Attach`, `Client`, `Registry`. |
| `examples/echo` | A minimal consumer: a C client and a Go agent, plus `demo.sh`. |
| `spike/pcsampling` | The PC sampling experiments behind SPEC §9 (`FINDINGS.md`). |

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

/* hot path: one atomic load when nothing changed */
uint64_t gen; char buf[4096];
int n = procctl_poll(&ctx, buf, sizeof buf, &gen);
if (n > 0) { apply(buf, n); procctl_ack(&ctx, gen, PROCCTL_TRY); }
```

## Build and test

```
make test     # procctl.h in 12 compile configurations; C unit tests (ASan/UBSan); Go tests (-race, incl. C interop)
make demo     # the M1 single-host demo
```

**Requirements:**
- **Kernel:** Linux ≥ 5.4.
- **Agent permissions:** the agent needs ptrace-level access to the client (same user with Yama `ptrace_scope` 0, or `CAP_SYS_PTRACE`).

## Licence

Apache-2.0. `procctl.h` bundles SystemTap's `<sys/sdt.h>`, which is dedicated to the public domain (CC0).
