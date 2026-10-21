# procctl

Lets a privileged node agent (Go) push configuration to libraries running
inside other processes (C), and read back what they did with it.

- **Agent → process:** the process creates a sealed memfd and maps it
  read-only; the agent writes configuration into it.
- **Process → agent:** the process acks each configuration and reports its
  state in a small block in its own memory, which the agent reads with
  `process_vm_readv`.

The process never writes shared memory, never blocks, and costs one atomic
load per check when nothing changed. If the agent goes away, the process
falls back to its defaults after a lease. The wire format is in
[SPEC.md](SPEC.md).

## Process side (C)

Vendor [`c/procctl.h`](c/procctl.h), a single header with no dependencies:

```c
#define _GNU_SOURCE
#define PROCCTL_IMPLEMENTATION   /* in exactly one translation unit */
#include "procctl.h"

static _Alignas(8) unsigned char state[PROCCTL_STATE_SIZE(256)];
static procctl_t ctx;

procctl_config_t cfg = {.consumer = "myconsumer", .schema_version = 1,
                        .payload_capacity = 4096, .state_capacity = 256,
                        .state_storage = state};
procctl_init(&ctx, &cfg);           /* on error: just run on your defaults */

/* Wherever it's convenient, even on a hot path: */
uint64_t gen;
char buf[4096];
int n = procctl_poll(&ctx, buf, sizeof buf, &gen);
if (n > 0) {
  apply(buf, n);
  procctl_ack(&ctx, gen);
} else if (n == -ENODATA) {         /* the agent cleared the configuration */
  use_defaults();
  procctl_ack(&ctx, gen);
}

/* Now and then: */
if (procctl_lease_expired(&ctx, 0)) use_defaults();  /* agent gone */
procctl_state_set(&ctx, &status, sizeof status);     /* what you are doing */
```

[`examples/echo/client.c`](examples/echo/client.c) is a complete client.

## Agent side (Go)

```go
reg, _ := procctl.NewRegistry(procctl.RegistryOptions{
	Consumer: "myconsumer",     // only regions named procctl:myconsumer-v1
	Lease:    10 * time.Second, // processes revert this long after the agent stops
})
go reg.Run(ctx) // discovery, attach, heartbeats, exit detection

for ev := range reg.Events() {
	if ev.Kind != procctl.Attached {
		continue // Detached: the process exited
	}
	c := ev.Client
	gen, err := c.Write(config)
	if err != nil {
		continue // ErrNotOwner: another agent owns this region
	}
	if err := c.WaitAck(ctx, gen); err == nil {
		st, _ := c.ReadState(ctx) // st.AppliedGen, st.Consumer
		_ = st
	}
}

reg.Close() // a successor agent takes over without the processes reverting
```

- `reg.Clients()` lists every attached process; `Client.Info()` gives its
  pid and header.
- New processes are found by a `/proc` scan every `RescanInterval`. An agent
  that already sees process starts (e.g. a profiler) can call
  `reg.Notify(pid)` to find them at once.
- `reg.Uninstall()` instead of `Close()` makes processes revert immediately.

[`examples/echo/agent`](examples/echo/agent) is a complete agent.

The agent needs `CAP_SYS_PTRACE` (or the same user with Yama `ptrace_scope`
0) and must see the processes' pid namespace (`hostPID` on Kubernetes).

## Payload and state schemas

procctl carries bytes; what they mean is up to the consumer (named by
`consumer`, versioned by `schema_version`). Define them as C structs with
fixed-width fields and explicit padding, and generate the Go side with
`go tool cgo -godefs`, as this repo does for its own wire structs
([`scripts/gen-types.sh`](scripts/gen-types.sh)) and for the e2e test client
([`testclient/testclient.h`](testclient/testclient.h)).

## Repository

| Path | |
|---|---|
| `c/procctl.h` | Process side |
| `*.go` | Agent side: `Registry`, `Client`, `Scan`, `Attach` |
| `examples/echo` | Minimal client and agent |
| `testclient`, `cmd/procctl-testagent`, `deploy`, `e2e` | End-to-end tests on Kubernetes |

```
make test       # C (gcc/clang, C/C++, ASan/UBSan) and Go (-race, incl. C interop and stress)
make bench      # poll() microbenchmark
make e2e-kind   # end-to-end scenarios on a local kind cluster
```

Linux ≥ 5.3, little-endian. Apache-2.0.
