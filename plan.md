# procctl — implementation plan

`github.com/parca-dev/procctl` is a control channel between a privileged node agent (Go) and libraries running inside other processes (C).

- **Agent → process:** the agent pushes configuration and **exclusive-resource grants** through a sealed shared-memory region. The process creates the region and maps it **read-only**.
- **Process → agent:** the process publishes a small state block in its **own private memory**, which the agent reads with `process_vm_readv`. **The process never writes shared memory.**

procctl is the transport plus a generic grant protocol. Each consumer defines its own payload, state schema and resource kinds.

This plan covers building procctl and proving it on its own. The parcagpu and parca-agent integrations will be planned separately against the finished library, using the hand-off artifacts in §11.

- **Contract:** `SPEC.md` (1.0-draft, revision 2).
- **Evidence for the design:** `spike/pcsampling/FINDINGS.md`.
- **Source prompt:** `prompt.txt`.

---

## 0. Scope notes and decisions

Several lines in the prompt were commented out by accident. All of them are in scope: lease, handshake e2e, header-pid assertion, Go↔C stress, cgo offset checks. The last became moot once the Go wire types were generated from the C header (§5.5).

**Decisions:**
- **The client is read-only.** It maps the region `PROT_READ` and reports through a state block in its own memory, read remotely (SPEC §6, §7.3). This replaces the earlier process→agent shared area. It is driven by the "fly on the wall" goal: nothing in the user's process writes memory that anyone else maps.
- **Exclusive resources are part of the spec** (SPEC §9): a grant table in the agent area, a resource table in the state block, and hard holder and arbiter rules. They exist because of PC sampling (§2.4), but are generic.
- **Release never depends on the application's threads** (SPEC H6). procctl provides `procctl_wait()` (a futex wait on the agent area) for a consumer-owned control thread. procctl itself creates no threads.
- **No fork hooks.** Inherited state is detected as stale (SPEC §12). `procctl_reinit()` is available for a child that wants its own channel.
- **Clocks:** all `CLOCK_BOOTTIME`, as in OTEP 4719, and never compared across processes.
- **The memfd name** carries the spec major version: `procctl:<consumer>-v1`.
- **Sizes:** 64 KiB payload by default, 4 MiB region cap, a state block of at most 64 KiB, at most 64 resource slots.
- **Discovery:** mapping events on by default, falling back to scan-only.
- **Agents per node:** typically one; more must degrade gracefully (SPEC §8.1).
- **`memfd_create` flags:** `MFD_CLOEXEC | MFD_ALLOW_SEALING | MFD_NOEXEC_SEAL`, retried without `MFD_NOEXEC_SEAL` on kernels before 6.3. Seals need `MFD_ALLOW_SEALING`, which the prompt's `MFD_CLOEXEC` alone lacks.

---

## 1. Prior art and what we take from it

| Source | Take | Diverge |
|---|---|---|
| **OTEP 4719, Process Context** | 8-byte signature (`PROC_CTL`) and version; a memfd named after the protocol; "write the marker last, with a fence"; `CLOCK_BOOTTIME`. **The state block uses OTEP's own reading technique:** a struct in the publisher's private memory, read with `process_vm_readv`, then the marker re-checked. | The agent area is a `MAP_SHARED`, sealed memfd with its fd kept open, because the agent must write. No anonymous fallback. Discovery is by `/proc/<pid>/fd`. A sequence counter instead of a timestamp. |
| **ebpf-profiler `process/processcontext`** | Validate signature and version before trusting anything; the remote-memory abstraction (also used for our state reads). | — |
| **sig-profiling C publisher, Datadog `otel_process_ctx.rs`** | The `memfd_create` flag-fallback ladder. | They allocate per update; we don't. |
| **Polar Signals custom-labels** | A cheap-to-read region at an advertised address. That's exactly what `state_addr` in the header is. | — |

**Future upstream:** the header and state block are laid out so that a "process control" OTEP companion could adopt them with only the signature and consumer namespace changed.

---

## 2. Key design decisions

### 2.1 Agent area: 64-bit seqlock (chosen over OTEP's "0, then new timestamp")

**Why a seqlock** (SPEC §7.1):
- **Even values are generations,** which double as config-ack ids and as **grant ids** (SPEC §9.3): strictly increasing for the region's lifetime, across agent restarts.
- **No clock dependency.**
- **A dead writer's odd value is adopted** with `s + 2`.
- **The `poll()` fast path stays one acquire load.**

**Both `seq` transitions are CASes,** so a suspended former owner that wakes up can't silently interleave with its successor. A payload CRC catches torn bytes (SPEC §7.4).

### 2.2 Memory ordering

- **Agent area:** relaxed atomic word copies between an acquire load and an acquire fence on the reader side, and a release fence and release CAS on the writer side (Boehm 2012).
  - **Go writer:** `sync/atomic` over the mmapped bytes. A plain `copy()` is not allowed: on arm64 it isn't ordered against the second `seq` load.
  - **C reader:** retries are bounded.
- **State block:**
  - the C writer is single, serialized by an internal lock, and writes into its own memory;
  - the Go reader uses three remote reads (`seq`, block, `seq`) plus a CRC (SPEC §7.3).

  Syscall boundaries give no formal ordering guarantee, so the CRC is the backstop.
- **Cross-process memory model:** documented as an assumption about hardware lowering (SPEC §7.6) and exercised by the arm64 stress tests (§8).

### 2.3 Read-only client

**What the process does:**
1. `memfd_create`, `ftruncate`, write the header with `pwrite`, seal;
2. `mmap(PROT_READ, MAP_SHARED)`;
3. keep the fd;
4. fire `procctl:hello`.

After that it only *reads* the agent area. It acks and reports by updating its state block (SPEC §6) in its own memory.

**Why:**
- the user's process never has a writable mapping of shared memory, so a bug in our library can't corrupt anyone else's view;
- no process-side shared-memory writer protocol, no CAS, no fork re-creation;
- the agent's trust surface shrinks to one header snapshot plus remote reads it fully controls.

**Costs:**
- **Reading state needs ptrace-level access** (`process_vm_readv`). That's the same privilege as `pidfd_getfd`, which the agent already needs.
- **State changes reach the agent by polling** (a few µs per read; 5 ms cadence while a release is pending). That's fast compared with what a hand-off costs anyway (e.g. CUPTI Disable ~60 ms), so there's no event path. The opt-in `procctl:state` USDT probe is a debugging aid only (SPEC §11.2).
- **The state block's address is advertised** (`state_addr`). It's only ever used as a remote-read target.

### 2.4 Exclusive resources, driven by PC sampling

**What the spike found** on the DGX Spark (`spike/pcsampling/FINDINGS.md`):
- **What claims the GPU:** the first `cuptiPCSamplingStart` claims PC sampling on the GPU, holding it until `Disable` or process exit. `Stop` doesn't release; `Enable` alone claims nothing.
- **A collision is permanent for the loser.** In KERNEL_SERIALIZED mode, which parcagpu needs to attribute PC samples to kernels, a second process's Start silently succeeds. It then gets about 180× slower kernels for that window, and broken sampling for the rest of its life: Disable and re-Enable fail. Random or time-slotted duty cycles collided within a second (windows can only close at kernel boundaries).
- **Hand-off works:** Disable by the holder, or holder exit, frees the GPU. Each hand-off costs the incoming process about a 0.3 s stall, so rotation is coarse.
- **A hung holder (SIGSTOP) keeps the claim,** and anyone who tries meanwhile is permanently broken.
- **Releasing from a non-application thread works,** busy or idle, with no app-visible stall. CUPTI's flush-period worker thread does **not** call back when idle, so it can't serve as the release hook.

**What the spec does about it** (SPEC §9):
- **Strict mutual exclusion:** at most one process with outstanding grants per `(kind, resource_id)`.
- **Release is confirmed only by state reads or pidfd exit, never by timeout** (A2, A3).
- **The deadline is only a backstop** for a holder whose agent is gone (H3, A6).
- **A new agent adopts the outstanding grants it finds** (A4).
- **No grants before full discovery** (A5).
- **Renewal transfers a hold without a release** (§9.3), so a lone process never pays the hand-off stall.
- **Holders release from a thread of their own** (H6), waking on the futex word (SPEC §8.4).

**What procctl ships for it:**
- C helpers for holders (`procctl_grant_get`, `procctl_resource_report`, `procctl_wait`);
- a Go **arbiter** package (`procctl/exclusive`) that implements A1–A6 with pluggable policy (§4.2).

That way the test client, parcagpu and parca-agent all use one tested implementation.

**Default arbiter policy** (tunable; implemented in `exclusive`, configured by parca-agent):
- candidates are processes listing the resource with `WANT`;
- round-robin among candidates, 10 s grants;
- a lone candidate keeps renewing;
- `MinActivity` before a first grant is 0: bursty processes like a database building an index get a grant, even though a grant costs ~0.3 s (decided: worth it);
- a holder that won't release is reported as blocked and never preempted.

### 2.5 fork

There are no handlers, and no `MADV_DONTFORK`.
- **A child inherits** the fd, the read-only mapping and a stale copy of the state block (`writer_pid` = the parent's pid).
- **The library refuses state writes** from a process whose `getpid()` differs from the initializing pid. This is checked on write paths only, never in `poll()`.
- **Agents ignore stale blocks.** Under A3, staleness never confirms a release; the child can't hold the parent's resources.
- **`procctl_reinit()`** gives a child its own region and state identity. Consumers that want it call it from their own atfork handler.

For CUDA this is the right default: a child forked after CUDA init can't use CUDA anyway.

### 2.6 Discovery: mapping events + hello USDT + coarse `/proc/*/fd` scan

Unchanged from the previous revision. Three layers, each a fallback for the one above it:

1. **Mapping events.** One `PERF_COUNT_SW_DUMMY` perf event per CPU with `mmap2`, reusing ebpf-profiler's sideband approach (`internal/perfutil`, `tracer/process_events.go`).
   - On an executable mapping of a new `(dev, inode)`, the agent reads `.note.stapsdt` once with parca-dev/usdt.
   - If the file has `procctl:hello`, the agent **first** attaches the uprobe to that inode, and **then** scans that pid's fds once. Doing it in that order removes the race.
   - Cached per inode; `PERF_RECORD_LOST` triggers a full scan.
2. **Hello USDT.** Covers late region creation, and `reinit` in fork children; neither produces executable mappings.
3. **60 s `/proc/*/fd` scan.** The safety net, and the startup pass that A5 requires.

**Rejected: `mmap_data`.** It reports every data mapping on the node, which is too much volume.

**Packaging and privileges:**
- procctl carries an attributed copy of the sideband reader in `internal/perfsideband`.
- Inside parca-agent, ebpf-profiler's existing stream is plugged in through a `ProbeAttacher` instead.
- **Privileges (measured on 7.0):** mapping events need `CAP_PERFMON`; the hello program needs `CAP_BPF`; but the uprobe PMU refuses `CAP_PERFMON` alone and needs **`CAP_SYS_ADMIN`**.
  - Without `CAP_SYS_ADMIN`, discovery degrades to mapping events + rescans (`mmap-only`): a mapping of a file with hello sites still triggers a pid scan, but late `procctl_init` and fork `reinit` wait for the rescan.
  - Without `CAP_BPF`/`CAP_PERFMON`, the agent runs scan-only.
- **Pid namespaces:** `bpf_get_current_pid_tgid()` reports root-namespace pids, which only match `/proc` for an agent in the initial pid namespace (`hostPID` on a real node). When nested (kind: the node is a container), the program walks `current->group_leader->thread_pid->numbers[]`, with offsets from kernel BTF, and reports the pid whose `ns->ns.inum` is the agent's pid namespace. Root is detected by the fixed `PROC_PID_INIT_INO`; `NSpid` can't tell, since it is relative to the procfs mount.
- **Overlayfs:** every container's copy of a binary reports its own `(dev, inode)`, though the uprobe lands on the shared backing inode. So the cache holds one entry, with its own uprobes, per container. Consequences:
  - One hello call fires every duplicate; the reader coalesces repeated pids.
  - A GC pass (every 2 min, one `/proc/*/maps` sweep) drops entries no live process maps and closes their uprobes. A forked child keeps the mapping, so it keeps them alive.

### 2.7 Symbol prefix and packaging

- **Prefix:** `procctl_`, with `procctl__` for internals.
- **One stb-style header,** `procctl.h`. One TU defines `PROCCTL_IMPLEMENTATION`.
- **Dependencies:** libc and Linux headers only. The USDT macros come from SystemTap's `<sys/sdt.h>`, bundled inside `procctl.h` (CC0, public domain; two small modifications are noted in-file) and used only when the system has no `<sys/sdt.h>`.
- **Implementation TU:** needs `_GNU_SOURCE` (or `-std=gnu11`, or C++), like most single-header Linux libraries; there's a clear `#error` otherwise.
- **Hidden visibility by default,** so two consumers in one process can vendor different copies.

---

## 3. SPEC.md (the contract)

`SPEC.md` is the source of truth. Section map:
- §4 header
- §5 agent area and grant table
- §6 state block and resource table
- §7 publication and remote read
- §8 ownership, acks, lease, wake
- §9 exclusive resources (holder H1–H6, arbiter A1–A6, renewal)
- §10 discovery and validation
- §11 probes
- §12 fork
- §13 trust
- Appendix C: PC sampling mapping

**Changes in revision 2:**
- **Removed:** the process area, `publish_status`, process-side shared-memory writes, and the fork re-creation and `MADV_DONTFORK` rules.
- **Added:** the state block and remote-read protocol; the grant table, resource table, and holder and arbiter rules; the futex wake word; the opt-in `procctl:state` debugging probe; and the stale-state rule for fork.
- **Changed:**
  - the header's `proc_area_off/size` become `state_addr`, `state_size` and `resource_slots`;
  - acks move into the state block (`applied_gen`, `applied_writer_id`);
  - the region is created with `pwrite` and mapped `PROT_READ`.

---

## 4. APIs (proposed; frozen at the end of M1 for the transport, M2 for exclusive resources)

### 4.1 C (`procctl.h`)
```c
#define PROCCTL_SPEC_MAJOR 1
#define PROCCTL_SPEC_MINOR 0
#define PROCCTL_VERSION    "0.1.0"

/* Caller storage with static storage duration: it contains the state block
 * whose address is published in the header (SPEC §6). */
typedef struct procctl { _Alignas(64) unsigned char opaque[512]; } procctl_t;

typedef struct procctl_config {
    const char *consumer;          /* [a-z0-9_]{1,31} */
    uint32_t    schema_version;
    uint32_t    payload_capacity;  /* consumer payload bytes (agent → process) */
    uint16_t    resource_slots;    /* 0..64 */
    uint32_t    state_capacity;    /* consumer state bytes (process → agent) */
    void       *state_storage;     /* >= PROCCTL_STATE_STORAGE(resource_slots, state_capacity) bytes, static, 8-aligned */
    uint32_t    flags;             /* PROCCTL_F_NO_HELLO, PROCCTL_F_STATE_PROBE */
} procctl_config_t;

PROCCTL_DEF int  procctl_init(procctl_t *ctx, const procctl_config_t *cfg);  /* 0 or -errno; no allocation, no threads, no hooks */

/* --- configuration (hot path) --------------------------------------------- */
/* 0 = no change (one acquire load); >0 = bytes of consumer payload copied; -EAGAIN busy;
 * -E2BIG buffer too small (generation consumed); -ENODEV disabled. Exactly one concurrent
 * caller receives each new generation. */
static inline int procctl_poll(procctl_t *ctx, void *buf, size_t cap, uint64_t *gen);
PROCCTL_DEF int  procctl_ack(procctl_t *ctx, uint64_t gen, int flags);         /* SPEC §8.2; PROCCTL_TRY never blocks (-EBUSY) */
PROCCTL_DEF bool procctl_lease_expired(procctl_t *ctx, uint64_t now_ns);         /* SPEC §8.3; now_ns = 0 reads the clock */
PROCCTL_DEF bool procctl_agent_attached(const procctl_t *ctx);

/* --- consumer state (process → agent) -------------------------------------- */
PROCCTL_DEF int  procctl_state_set(procctl_t *ctx, const void *state, size_t len, int flags);

/* --- exclusive resources (SPEC §9) ------------------------------------------ */
typedef struct procctl_grant { uint64_t grant_id, duration_ns, seen_gen; uint32_t kind; } procctl_grant_t;
/* Stateless seqlock read of the grant table: safe from any thread, doesn't affect poll(). */
PROCCTL_DEF int  procctl_grant_get(procctl_t *ctx, uint32_t kind, const uint8_t id[16], procctl_grant_t *out);
PROCCTL_DEF int  procctl_resource_report(procctl_t *ctx, uint32_t kind, const uint8_t id[16],
                                         uint32_t state, uint64_t grant_id, uint32_t flags /* WANT */,
                                         uint32_t error, uint64_t grants_seen_gen, int lflags);
/* Helper implementing H1/H3 timing: tracks first-observation time per grant_id. */
PROCCTL_DEF bool procctl_grant_usable(procctl_t *ctx, const procctl_grant_t *g, uint64_t now_ns);

/* --- control-thread support ------------------------------------------------- */
/* FUTEX_WAIT on the wake word until it changes or timeout_ns passes (SPEC §8.4).
 * 0 = woken or spurious, -ETIMEDOUT. For a consumer-owned thread; never call on a hot path. */
PROCCTL_DEF int  procctl_wait(procctl_t *ctx, uint64_t timeout_ns);

/* --- lifecycle ---------------------------------------------------------------- */
PROCCTL_DEF int  procctl_reinit(procctl_t *ctx);  /* new region + state identity, e.g. from a consumer atfork child handler */
PROCCTL_DEF void procctl_close(procctl_t *ctx);   /* optional; never registered for exit */
```

**Contract:**
- **Thread safety:** all functions except `init`, `reinit` and `close` are safe from any thread.
  - State writes (`ack`, `state_set`, `resource_report`) are serialized internally. `PROCCTL_TRY` returns `-EBUSY` instead of waiting, for application threads; control threads may block.
  - The lock is held only for a memcpy and a CRC of at most a few KiB.
- **`poll()` fast path:** `if (load_acquire(ctx->seq) == ctx->last_seen) return 0;`. It never reads the clock or calls `getpid`.
- **No globals,** no constructors, no exit hooks.
- **State writes refuse** (`-ECHILD`) after fork, until `reinit`.

**Reference holder loop (informative), for a consumer's control thread:**
```c
for (;;) {
    procctl_wait(ctx, quarter_lease_or_remaining_grant);
    procctl_grant_t g; int have = procctl_grant_get(ctx, KIND, id, &g) == 0 && g.grant_id;
    bool ok = have && procctl_grant_usable(ctx, &g, 0) && !procctl_lease_expired(ctx, 0);
    if (holding && (!ok || g.grant_id != held)) {
        if (ok && can_transfer) { held = g.grant_id; report(HOLDING, held, g.seen_gen); }
        else                   { release(); report(RELEASED, held, g.seen_gen); holding = false; }
    } else if (!holding && ok && want) {
        report(ACQUIRING, g.grant_id, g.seen_gen); holding = acquire(); held = g.grant_id;
        report(holding ? HOLDING : FAILED, held, g.seen_gen);
    } else {
        report_seen(g.seen_gen);   /* keep grants_seen_gen current (H4) */
    }
}
```

### 4.2 Go (`github.com/parca-dev/procctl`)
```go
type Key struct{ Dev, Ino uint64 }

func Scan(ctx context.Context, opt ScanOptions) ([]Candidate, ScanStats, error)
func Attach(pid, fd int, opt AttachOptions) (*Client, error)

type Client struct{ /* concurrency-safe */ }
func (c *Client) Info() Info                                   // Key, HostPID, NSPid, Consumer, SchemaVersion, InstanceID, capacities, ResourceSlots
func (c *Client) Write(payload []byte, grants []Grant) (gen uint64, err error) // one seqlock write; FUTEX_WAKE; ErrTooLarge, ErrNotOwner, ErrPreempted
func (c *Client) Owned() bool
func (c *Client) Heartbeat() error
func (c *Client) ReadState(ctx context.Context) (State, error) // SPEC §7.3; ErrBusy, ErrStale, ErrUnreadable, ErrTimeout
func (c *Client) WaitAck(ctx context.Context, gen uint64) error
func (c *Client) Done() <-chan struct{}                        // pidfd exit
func (c *Client) Close() error

type State struct {
    Seq, AppliedGen, AppliedWriterID, GrantsSeenGen uint64
    WriterPID uint32; Stale bool
    Resources []ResourceEntry   // {Kind, ID, GrantID, State, Flags, Error, LastActivityNS}
    Consumer  []byte
}

type Registry struct{}  // discovery, attach, heartbeats, pidfd exit, mapping events, hello
func NewRegistry(opt RegistryOptions) *Registry
func (r *Registry) Run(ctx context.Context) error
func (r *Registry) Notify(pid int)
func (r *Registry) OnExecMapping(pid int, dev, ino uint64, open func() (io.ReaderAt, error))
func (r *Registry) Events() <-chan Event
func (r *Registry) Clients() []*Client
func (r *Registry) FirstScanDone() <-chan struct{}               // A5 gate
func (r *Registry) Close(opts ...CloseOption) error              // default: set RELEASED; ReleaseLeases(true): clear writer_id

// package exclusive — arbiter implementing SPEC §9.2 over a Registry.
type Arbiter struct{}
func New(r *procctl.Registry, kind uint32, p Policy, opt Options) *Arbiter
func (a *Arbiter) Run(ctx context.Context) error                 // one loop per kind; state polling cadence: Options.PollInterval (250 ms), 5 ms while confirming a release
func (a *Arbiter) Snapshot() []ResourceStatus                    // holder, grant, since, blocked-for, candidates
type Policy interface {                                          // pluggable; default RoundRobin{Grant: 10 * time.Second, MinActivity: 0}
    Next(res ResourceID, holder *Holder, candidates []Candidate, now time.Time) Decision // Keep | Renew | Revoke | Grant(to)
}
```
- Pure Go on `golang.org/x/sys/unix`. cgo only in `_test.go`.
- `exclusive` never confirms a release by time (A3). It takes a payload callback, so a consumer's own config and its grants are written together.

---

## 5. Repository layout, module, licence, versioning, vendoring, schemas

### 5.1 Layout
```
SPEC.md  LICENSE (Apache-2.0)  NOTICE  README.md  CHANGELOG.md  SECURITY.md
go.mod                   # module github.com/parca-dev/procctl
procctl.go client.go registry.go scan.go attach.go seqlock.go header.go state.go remote.go pidns.go
exclusive/               # arbiter (SPEC §9.2) + policies
hello/                   # USDT watcher (parca-dev/usdt + cilium/ebpf)
internal/perfsideband/   # mmap2 sideband reader (adapted from ebpf-profiler internal/perfutil)
internal/shm/            # atomic word access over mmapped bytes
c/procctl.h              # THE vendorable file, and the only definition of the wire structs
types_def.go             # cgo -godefs input (//go:build ignore)
types_gen.go             # Go wire types generated from c/procctl.h (committed)
scripts/gen-types.sh     # regenerate / --check (CI)
c/tests/                 # unit tests, interop and stress clients, libFuzzer harness, poll microbenchmarks
cmd/procctl-testagent/
testclient/              # C test client + Dockerfile
examples/echo/           # config-only example consumer
examples/token/          # exclusive-resource example consumer (control thread + holder loop)
spike/pcsampling/        # the PC sampling spike: code, results, FINDINGS.md (kept as evidence)
e2e/  deploy/  infra/eks/ infra/gke/  scripts/cloud-e2e.sh
.github/workflows/
docs/integration-guide.md docs/unsupported.md docs/kernel-floor.md
```

### 5.2 Licence
Apache-2.0. The bundled `<sys/sdt.h>` is SystemTap's (CC0). `internal/perfsideband` will be attributed to ebpf-profiler (Apache-2.0).

### 5.3 Versioning
- **Spec:** `spec_major.spec_minor` in SPEC.md and the header; the major also appears in the memfd name.
- **Release:** semver tags on the Go module. `PROCCTL_VERSION` must match the tag, which CI checks.
- **Before v1.0.0:** API changes are allowed, but not spec 1.x wire changes.

### 5.4 Vendoring the C code
- Copy `c/procctl.h` from a release tag (also a release asset, with its SHA-256).
- `scripts/vendor-procctl.sh <tag> <dest>` writes `procctl.VERSION`.
- Consumers add a CI hash check.

### 5.5 Wire structs and consumer schemas: C is the source, Go is generated
- **procctl's wire structs are defined only in `c/procctl.h`,** with `_Static_assert`s pinning each to its SPEC size. `types_gen.go` is generated from them with `go tool cgo -godefs` (`scripts/gen-types.sh`, the approach ebpf-profiler uses in `support/`), and committed. CI fails if it's stale.
- **Consumers do the same:**
  - they declare their payload and state as C structs in their own header (fixed-width fields, explicit padding, no bitfields or unions);
  - they generate the agent-side Go types with their own `types_def.go` and `cgo -godefs`;
  - they encode and decode with `encoding/binary`.
- **No schema language, no code generator of our own, no golden vectors:** with a single source there is nothing to cross-check, only staleness.
- **Who needs a C compiler:** only whoever regenerates.

---

## 6. Test images

### 6.1 `procctl-testclient` (C, unprivileged)
- **Config path:**
  - **payload:** `{seq_tag, agent_write_ns, pattern_len, crc32c, pattern[N]}`, checked for tearing;
  - **acking:** `procctl_ack` after applying;
  - **consumer state:** `{last_seq_tag, recv_ns, applied_ns, torn, polls, changes, busy, lease_expired_at_ns, lease_restored_at_ns, lease_state, getpid, start_time}`, published with `procctl_state_set`.

  Latency is `recv_ns − agent_write_ns`, both on `CLOCK_BOOTTIME`. It's marked invalid when a time namespace is detected.
- **`--exclusive=K`** declares K simulated resources (random ids), each `WANT`, and runs a control thread with the reference holder loop (§4.1). "Acquire" and "release" are simulated, with configurable costs (default 300 ms acquire, matching the PC sampling hand-off).
  - Every acquire and release is logged with `CLOCK_BOOTTIME` to **stdout as JSON**, which the e2e suite collects. Overlap checking never depends on the agent's view.
  - `--holder-hang` makes the control thread stop responding. `--no-ctl-thread` releases only from the main loop, to show what goes wrong without H6.
- **Other modes:**
  - `--poll-interval`, `--fork-after`, `--fork-workers`, `--fork-reinit`, `--exit-after`;
  - hostile modes: `no-seals`, `grow-seal-missing`, `wrong-size`, `spoof-name`, `spoof-pid`, `bad-crc`, `bad-offsets`, `huge-len`, `bad-state-addr`, `state-never-even`. Adversarial holders (lying about releases, deliberately slow state memory) are out of scope for v0.1 (§12).
- **Image:** distroless/static (musl), multi-arch, non-root, read-only root fs.

### 6.2 `procctl-testagent` (Go, DaemonSet, hostPID)
- **Runs:** `Registry` + `hello` + an `exclusive.Arbiter` for the test kind, pushing config at `--rate`.
- **Verifies:** acks; tearing; hostile rejections; ownership changes; fork staleness; arbiter invariants, from its own view and cross-checked by the e2e suite against the clients' logs.
- **Exposes:**
  - `/metrics`: push latency, `procctl_clients{owned}`, rejections by reason, scan cost, attach path, state-read latency and failures, wake-to-ack latency;
  - arbiter metrics: grants, renewals, release-confirm latency, blocked seconds per resource, holder exits;
  - `/summary`, and a `PROCCTL_SUMMARY` log line on SIGTERM.
- **Security context:** `hostPID`, `SYS_PTRACE` (covers `pidfd_getfd`, `/proc/<pid>/fd`, `process_vm_readv`), `BPF` + `PERFMON`, and `SYS_ADMIN` for the uprobe PMU (§2.6). Overlays add AppArmor and SELinux settings; `privileged` is the fallback.

---

## 7. E2E suite (`e2e/`, sigs.k8s.io/e2e-framework)

One suite takes any kubeconfig, with labels `env`/`os`/`arch`.

### 7.1 Scenarios

| # | Scenario | Pass criteria |
|---|---|---|
| S1 | Push latency under load (50 clients, 10 Hz, 4 KiB); cold discovery (new image; init delayed 30 s) | p50 < 1 ms, p99 < 50 ms (100 µs poll interval); process start to first apply < 1 s with the rescan disabled |
| S2 | Client starts before agent | all attached after one scan |
| S3 | Agent restart and DaemonSet rolling update (maxSurge 0 and 1) | no torn reads; generations monotonic; acks resume within 2 s |
| S4 | Client restart | detach via pidfd, attach the new one, no leaked mappings |
| S5 | Lease: agent SIGKILL, SIGSTOP, SIGCONT; rolling update | clients revert within lease + check interval; re-apply on recovery; no reversion during a rolling update |
| S6 | 64 and 200 clients per node | all attached; agent CPU, RSS and state-read cost recorded |
| S7 | Fork: `--fork-workers=8` with and without `--fork-reinit` | children with inherited state are read as stale and never counted as holders; reinit children get new regions, found via hello in < 1 s |
| S8 | Hostile clients (every mode) | each rejected or isolated with the expected reason; the agent never crashes; others unaffected |
| S9 | Scan cost: 5k processes × 200 fds, plus an exec storm of 1k exec/s | recorded, within budget |
| S10 | PSA `restricted`, RuntimeDefault seccomp, `hostUsers: false` | works, or skipped with a reason |
| S11 | Attach and read paths: `pidfd_getfd` vs `/proc/<pid>/fd`; `process_vm_readv` vs `/proc/<pid>/mem` | all combinations pass, including on Bottlerocket SELinux and COS AppArmor |
| S12 | Unsupported environments | graceful |
| S13 | Two agents on one node; SIGKILL and SIGSTOP the owner | exactly one owner; zero ownership changes while healthy; take-over as specified |
| S14 | **Exclusive resources:** `--exclusive` clients (2, 8, 50 per resource) under the default round-robin; holder SIGKILLed mid-grant; holder `--holder-hang`; holder SIGSTOP; agent SIGKILLed mid-hand-off; agent rolling update mid-grant; a lone holder for 10 min | **holding intervals never overlap, judged from the clients' own logs.** Killed holder: re-grant right after the pidfd exit. Hung or stopped holder: **no** re-grant while it's alive, and blocked time reported. Agent killed: the holder self-releases at its deadline or on lease expiry, and the new agent adopts (A4) and doesn't grant before confirmation. Lone holder: zero releases (renewal transfers). `--no-ctl-thread` clients produce blocked time, the documented failure mode |
| S15 | Header pid vs host pid, in every scenario, plus `spoof-pid` | NSPid matches the client's own `getpid()` |
| S16 | Wake latency: agent write → client control thread wakes → state shows `grants_seen_gen` | p50/p99 recorded (target p99 < 10 ms with the agent's 5 ms confirm polling); futex wait on a read-only mapping verified on every node OS |

Each test writes `artifacts/<env>/<scenario>.json`.

### 7.2 Unsupported (documented; fail gracefully)
- **GKE Autopilot:** the agent is rejected at admission.
- **EKS Fargate:** no DaemonSets.
- **gVisor, Kata:** processes aren't visible to the host.

In all of these, clients run on their defaults and never acquire exclusive resources, because they never see a grant.

---

## 8. Unit and conformance tests

1. **Golden vectors:** region images and state-block images in known states, including hostile ones. The C and Go readers must agree.
2. **Interop (cgo in tests):** Go writer → C `poll` and `grant_get`; C `state_set` and `resource_report` → Go `ReadState`, through a real memfd and a real second process. Covers ownership take-over, odd-`seq` adoption, the wake word, fork staleness and `reinit`.
3. **Stress, amd64 and arm64** (60 s on PRs, 15 min nightly):
   - **forward:** a Go writer at full rate with variable sizes, feeding N C reader processes;
   - **reverse:** a C state writer at full rate, read by a Go remote reader;
   - **failure:** any torn read, any decreasing generation, or any phantom ack;
   - **variants:** pinned to separate cores, and a hostile `seq` flipper.
4. **Arbiter model tests** (`exclusive`):
   - **randomized schedules:** a simulated cluster of holders (prompt, slow, hung, crashing, lying) and agents (crashing, restarting, two at once), with message delays and dropped wakes;
   - **checked properties:** never two may-hold holders per resource; no confirmation by time; progress when holders are prompt.

   Run for millions of steps in CI. Optionally, a small TLA+ model of H1–H6 and A1–A6, checked with TLC in CI.
5. **Fuzzing:** Go native fuzzing of the header parse, `ReadState` and the grant-table decode; C libFuzzer on the `poll`/`grant_get` slow paths. 60 s per target on PRs, 30 min nightly.
6. **Microbenchmarks:**
   - `poll()` with no change: target < 2 ns, and a 5 µs proxy-launch delta within noise;
   - `state_set` and `resource_report`: 4 KiB in under 2 µs;
   - Go `ReadState`: about 5 µs per read.
7. **Kernel and feature report** in every job, including a futex-on-read-only-mapping probe.
8. **Generated-types check** (in place of the originally planned cgo offset checks): `scripts/gen-types.sh --check` verifies that `types_gen.go` matches `c/procctl.h`. The layout has only one source, and `_Static_assert`s pin its sizes to SPEC.
9. **Static checks:**
   - C11 `-Werror` under gcc and clang, and as C++;
   - hidden symbols only;
   - no `malloc`/`free`/`pthread_create`/constructors/`atexit` in `procctl.h`;
   - `go vet`, staticcheck, `-race`.

---

## 9. Milestones

Durations assume 1 engineer and are rough.

### M1 — Spec and single-host prototype of the transport (≈4 weeks)

**Status (2026-10-19): implemented on one host, except the maintainer review.**
- **`c/procctl.h`:** all of the M1 API, plus the exclusive-resource helpers planned for M2.
  - Tested by `c/tests/unit_test.c` under ASan/UBSan with gcc and clang.
  - Compiles cleanly in 12 configurations (gcc/clang, C/C++, system/bundled `sdt.h`).
- **Go:** `Scan`, `Attach`, `Client`, `Registry` (leases, the A5 first-scan gate, pidfd exit via epoll, ownership rules 1–4, republish on drift).
  - Tested by `interop_test.go` against real C processes, under `-race`: round trip, uninstall, RELEASED hand-over, two agents, the stale-heartbeat claim, fork staleness, `reinit`, and a grant waking a C control thread (about 1.3 ms to `HOLDING`).
- **`examples/echo` + `demo.sh`:** the demo below runs as described. After `kill -9` the restarted agent claims the region immediately via rule 3.
- **Spec fix found while implementing:** the state-block CRC is defined with `seq` *and* `crc32c` zeroed (SPEC §6).
- **Not done yet:**
  - the SPEC review with the maintainers;
  - choosing which of several pids sharing a region to read from by `creator_pid` (it uses the lowest pid for now);
  - hardware CRC32C in C (it uses a table, which is fine for M1 sizes).

- **SPEC.md:** revision 2 reviewed with the parca-agent and parcagpu maintainers.
- **`procctl.h`:** init, poll, ack, state_set, lease, wait, reinit, the `hello` probe, and the opt-in `state` debugging probe.
- **Go:** Scan, Attach, Client (Write, ReadState, WaitAck, Heartbeat), Registry with leases and the A5 gate, without hello.
- **Example consumer:** `examples/echo`.
- **Demo:** on one host, the client starts first, the agent discovers it, pushes config and sees the ack in the state block. `kill -9` on the agent makes the client fall back to defaults within the lease. A forked child shows a stale state block. A `reinit` child gets a new region.

### M2 — Exclusive resources, conformance and kind CI (≈3 weeks)

**Status (2026-10-19, branch `m2`): done, except the kind smoke test, which moves to M3** (it needs the M3 container images).
- **`exclusive.Arbiter`:** implements A1–A6, RoundRobin, renewal by transfer, adoption, foreign-holder blocking, and blocked reporting. `Client` gained `WritePayload`/`WriteGrants`/`Grants`, and adopts the region's grants on claim.
- **Model tests** (`exclusive/model_test.go`): random schedules with slow, hung, crashing and unreadable holders; holders that act on stale grant-table snapshots; and agent crash/restart with adoption.
  - **They found a real spec hole,** now fixed: SPEC A4's post-claim `grants_seen_gen ≥ G0` barrier. Without it, a holder that stalled after reading a grant could act on it after an agent restart.
  - **Mutation testing:** 7 of 8 deliberate arbiter bugs are caught. The 8th is masked by the independent foreign-holder check.
  - `make model-long` runs 5,000 × 10,000 steps.
- **`examples/token`:** a C holder with the reference control-thread loop, plus a Go agent. End-to-end tests (`exclusive/token_test.go`) judge overlap from the clients' own logs:
  - rotation with no overlap;
  - a lone holder renewed with zero releases;
  - **hand-over about 50 ms after the holder is SIGKILLed;**
  - a SIGSTOPped holder blocks the resource, and it's reported.
- **Conformance:**
  - **wire layout defined once, in C:** the Go wire types are generated from `c/procctl.h` with `go tool cgo -godefs` (`types_gen.go`); CI fails if they're stale;
  - Go↔C stress: 30 s on arm64 (Grace) gave 26.5 billion polls, 237k generations, 45M grant reads, 1.8M state reads, **zero torn data accepted**;
  - Go fuzzing (header, state, agent area); C libFuzzer (2.6M runs, clean);
- **Benchmarks (`make bench`):**

  | | amd64 (i7-1370P) | arm64 (GB10) |
  |---|---|---|
  | no-change `poll()` | 0.6 ns | 1.6 ns |
  | 5 µs launch proxy delta | ~0 ns | ~1.4 ns |

  The new hardware CRC32C (SSE4.2 / ARMv8, chosen at runtime) took a 4 KiB config read from 8.7 µs to about 1 µs.
- **CI:** `.github/workflows/ci.yml` (amd64 + arm64: compile matrix, C tests, Go tests with a 60 s stress run, benchmarks, short fuzzing) and `nightly.yml` (15-minute stress, long model test, long fuzzing; opens an issue on failure).
- **Deviations:**
  - there's no schema code generator (`procctl-gen` was built, then dropped): wire structs and consumer schemas are C structs, with Go generated by `cgo -godefs` (§5.5);
  - so there are no cgo offset tests and no golden vectors, only a staleness check.

- **C side:** `grant_get`, `resource_report`, `grant_usable`, and the reference holder loop in `examples/token`.
- **Go side:** `exclusive.Arbiter` with RoundRobin, renewal-by-transfer, adoption and blocked reporting.
- **Tests:** arbiter model tests (§8.4), interop, stress, fuzzing, benchmarks, generated-types staleness check, static checks.
- **Tooling:** `scripts/gen-types.sh` (`cgo -godefs`); `ci.yml` on amd64 and arm64; a kind smoke test.
- **Demo:** `examples/token` with three processes rotating a simulated resource on one host. Killing the holder hands over immediately; SIGSTOPping it blocks without overlap. The model tests run green in CI.

### M3 — Test images and the e2e suite (≈3 weeks)

**Status (2026-10-20):**
- **Phase 1 done:** images, manifests, and S2–S6 (plus S15).
- **Phase 2 done:** fast discovery: `hello/` (USDT + uprobes, pid-namespace translation, GC), `internal/perfsideband` (mmap2), and S1 + S7 on kind.
  - S1: init → first apply in ~20–100 ms, whether init runs at exec or 2 s later. The rescan is effectively off, and the first apply waits for the agent's 10 Hz write cadence.
  - S7: 2 parents + 16 reinit children all attached, ~70–100 ms. Inherited-state children are never attached, and their writes are refused with `-ECHILD`.

- **Images and manifests:** testclient and testagent (§6); `deploy/` overlays.
- **The suite:** S1–S16 on kind, with S9 at reduced scale and S12 as far as kind allows.
- **CI:** `e2e-kind.yml` on every PR, amd64 and arm64, two Kubernetes versions.
- **Demo:** `make e2e-kind` runs in under 20 min and emits artifacts. The S14 overlap checker is green.

### M4 — EKS/GKE matrix, run by hand from a developer account (≈1.5 weeks)
- **Clusters:**
  - `infra/eks` (eksctl, 4 spot nodegroups: AL2023 and Bottlerocket on x86 and Graviton);
  - `infra/gke` (gcloud, 4 spot pools: COS and Ubuntu on x86 and T2A, plus an optional Sandbox pool).
- **The runner:** `scripts/cloud-e2e.sh <eks|gke>` creates, runs, collects, and deletes from an `EXIT` trap. `make e2e-eks`, `make e2e-gke` and `make cloud-sweep` wrap it.
- **Credentials:** from the CLIs. Images come from public GHCR, or `make images-push REGISTRY=…`.
- **Demo:** both matrices green from a laptop, with `artifacts/report.md`, and nothing left running.

### M5 — Hardening and v0.1.0 (≈2 weeks)
- **Hardening:** fix M4 findings; security review against the SPEC §13 threat model; two weeks of nightly fuzzing without new crashes.
- **Freeze:** SPEC 1.0 final, `PROCCTL_VERSION 0.1.0`, the integration guide, both examples polished.
- **Release:** the Go module, `procctl.h` with its SHA-256, and images.
- **Demo:** `v0.1.0` tagged, a manual cloud matrix green on the tag, hand-off artifacts delivered.

### M6 — Cloud e2e in GitHub CI (after v0.1.0)
- **The workflow:** `e2e-cloud.yml` (nightly, dispatch, tags) calls the same script.
- **Credentials:** OIDC to AWS and GCP, with bootstrap Terraform.
- **Cleanup:** a janitor job and budget alerts.
- **Needs:** a team-owned AWS account and GCP project (Q8).

**Total to v0.1.0:** about 13.5 weeks.

---

## 10. CI workflows, manifests, cost

### 10.1 Workflows

| Workflow | Trigger | Jobs |
|---|---|---|
| `ci.yml` | PR, push main | Go (vet, staticcheck, `-race`) and C (gcc/clang, C/C++, ASan/UBSan, symbol checks), each on amd64 and arm64; interop + offset checks; stress (60 s, both arches); arbiter model tests; fuzz (60 s/target); benchmarks → job summary; spec-check |
| `images.yml` | PR (build), main and tags (push) | multi-arch testagent and testclient; SBOM + cosign on tags |
| `e2e-kind.yml` | PR, push main | {amd64, arm64} × {oldest, latest k8s} |
| `fuzz-nightly.yml` | nightly | 30 min fuzzing + 15 min stress, both arches |
| `release.yml` | tag `v*` | version check, `procctl.h` + checksums, release notes |

**CI is kind-only.** EKS and GKE runs are manual (M4) until M6.

### 10.2 Manifests (`deploy/`)
- **Base:**
  - testagent: `procctl-system` namespace (PSA privileged), DaemonSet with `hostPID`, caps, and tolerations for all pools;
  - testclient: `procctl-clients` namespace (PSA restricted), non-root, RuntimeDefault seccomp, read-only root fs, optional `hostUsers: false`.
- **Overlays:**
  - platforms: `kind`, `eks-al2023`, `eks-bottlerocket`, `gke-cos`, `gke-ubuntu`;
  - workloads: `scale`, `hostile`, `fork`, `noise`, `exclusive` (S14 client mixes);
  - negative: `unsupported-autopilot`, `unsupported-gvisor`.

### 10.3 Cost (manual cloud runs; approximate spot list prices, to verify)

| Item | Per run (~75 min) |
|---|---|
| EKS control plane | $0.13 |
| EKS 4 spot nodes + disks | ~$0.20 |
| GKE zonal cluster fee (one free per billing account) | $0–0.13 |
| GKE 4 spot nodes + disks | ~$0.15 |
| Optional Sandbox pool | ~$0.04 |
| Egress / image pulls | < $0.05 |
| **Total** | **≈ $0.50–0.80** |
| M4–M5 manual runs (20–40) | **≈ $10–35 total** |
| Later, M6 nightly | ≈ $20–35/month |

**Leaked clusters** cost about $5/day. Protections: the `EXIT`-trap teardown, `make cloud-sweep`, and a budget alert on your own account. GitHub arm64 runners are free for public repos.

---

## 11. Hand-off artifacts (for the parcagpu and parca-agent plans)

1. **The contract:** SPEC.md 1.0 final, `docs/kernel-floor.md`.
2. **C API:** `procctl.h` v0.1.0 with its guarantees (no allocation, threads, constructors or exit hooks; one-load `poll`) and benchmark numbers.
3. **Go API:** `procctl`, `procctl/exclusive` and `procctl/hello`, with godoc. Plus the `cgo -godefs` recipe for consumer schemas (§5.5).
4. **Integration guide:**
   - **schema and vendoring:** schema files and vendoring;
   - **config path:** where to `poll()`, apply-then-ack;
   - **state:** `procctl_state_set`;
   - **holder:** the **control-thread holder loop**: lazy creation, signals blocked, a named thread, `procctl_wait`, H1–H6;
   - **agent:** wiring Registry + Arbiter + hello + mapping events (inside parca-agent: a `ProbeAttacher` on ebpf-profiler's mmap2 stream);
   - **deployment:** security contexts and metrics per platform.
5. **Examples:** `examples/echo` (config) and `examples/token` (exclusive resource): the templates.
6. **PC sampling requirements for parcagpu**, from `spike/pcsampling/FINDINGS.md`:
   - **modes and identity:**
     - stay on KERNEL_SERIALIZED;
     - resource id = GPU UUID (`cuDeviceGetUuid`), not the ordinal;
     - one resource entry per GPU with a context, `WANT` while active;
   - **acquire and release:** acquire = Enable + Start (`cuCtxSetCurrent` on the control thread is fine); release = Stop + Disable, from the control thread (measured safe, busy or idle);
   - **never sample without a grant,** including when no agent is present; this replaces today's always-on random windows;
   - **wedge detection:** after Start, `GetData` → `CUPTI_ERROR_UNKNOWN` means another sampler holds the GPU. Report `FAILED` with an error; don't retry for the life of the process;
   - **within a grant,** keep the duty-cycled windows to bound the 8–13× in-window kernel slowdown;
   - **the control thread:** created on the first grant only; blocks all signals; `pthread_setname_np("parcagpu-ctl")`; exits with the process, with no exit-time work beyond what parcagpu already does.
7. **Arbiter defaults for parca-agent:** RoundRobin with 10 s grants, renewal for a lone holder, `MinActivity` 0 (short bursts are granted), 5 ms confirm polling, blocked-time metrics.
8. **The cloud matrix report** for v0.1.0, and the known-limitations list (§12).

---

## 12. Open questions and risks

### Open questions
- **Q8. Cloud accounts for M6:** which AWS account and GCP project? Not needed before M6.

### Resolved
- **Q9. GPU hardware coverage:** procctl tests basic functionality locally: the spike on the DGX Spark, plus kind and cloud CPU nodes for the transport. Validation on expensive datacenter GPUs belongs to the parcagpu plan, which can reuse `spike/pcsampling` as-is.
- **Q10. Futex wait on a read-only shared mapping:** handled in S16. If it fails on a node OS, fall back to timeout-only waits there.
- **Q11. Short bursts:** granting is worth the ~0.3 s first-window stall; `MinActivity` is 0.

### Risks
- **R1. Cross-process memory model.** Mitigation: atomic word copies, CRCs, and arm64 stress tests and e2e runs.
- **R2. arm64-specific ordering bugs.** Mitigation: stress tests on GitHub arm64 runners for every PR, plus Graviton and T2A e2e runs.
- **R3. LSM denial of ptrace-class access** (`pidfd_getfd`, `/proc/<pid>/fd`, `process_vm_readv`) on Bottlerocket SELinux, COS AppArmor, or Yama ≥ 2. Mitigation: fallbacks, overlays, S11, and reason metrics. Yama 3 is unsupported.
- **R4. Scan cost** on dense nodes. Mitigation: mapping events, hello and per-inode caches; a BPF `task_file` iterator as future work.
- **R5. User namespaces:** gated by S10.
- **R6/R7. Adversarial holders: accepted, deferred past v0.1.** A malicious shim could make remote reads of its state slow, or claim to hold a resource forever and block it on its node; the latter is inherent to A3. v0.1 doesn't defend against adversarial shims beyond what the spec already requires: header validation, seals, bounded reads with a plain timeout, and blocked-time metrics. No isolation pool, no admin override, no adversarial test modes. Revisit if procctl gains untrusted consumers.
- **R8. Holders without a control thread** (a consumer ignoring H6) block resources while idle. Mitigation: the integration guide, `examples/token`, and S14 `--no-ctl-thread` showing the failure.
- **R9. Two vendored copies in one process:** hidden visibility, separate regions, keyed by `(dev, ino)`.
- **R10. Mapping-event volume** on exec-heavy nodes. Mitigation: caches, a lost-records rescan, metrics; S9.
- **R11. Cloud run flakiness:** retry the create; treat spot preemption as infra failure; run manually before every milestone and release until M6.
- **R12. Upstream divergence:** keep transport and schema separate; engage the sig-profiling group early.
