# procctl wire format, version 1

procctl lets a privileged node agent push configuration to processes on its
node and read their answers. The process side is a C header
([`c/procctl.h`](c/procctl.h)), the agent side a Go package. This document is
what both implement; the structs in `c/procctl.h` are the reference for every
field.

All integers are little-endian. "Process" is the program using `procctl.h`;
"agent" is the node agent.

## The region

The process creates the region:

1. `memfd_create("procctl:<consumer>-v1", MFD_CLOEXEC | MFD_ALLOW_SEALING | MFD_NOEXEC_SEAL)`
   (retry without `MFD_NOEXEC_SEAL` on `EINVAL`). `<consumer>` names the
   payload schema, `[a-z0-9_]{1,31}`; `1` is the wire format version.
2. Size it to one page for the header plus the agent area: the control block
   and the largest payload the process accepts, rounded up to whole pages.
3. Initialize its state block (below) in its own memory.
4. Write the header with `pwrite` at offset 0.
5. Seal it with `F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_SEAL`, never
   `F_SEAL_WRITE`. A sealed region therefore has a complete header and a size
   that can't change under the agent.
6. Map it `PROT_READ` and keep the fd open. The process never writes the
   region after sealing.

| Offset | Contents |
|---|---|
| 0 | Header (`struct procctl_header`, 80 bytes) |
| `page_size` | Control block (`struct procctl_ctrl`, 48 bytes), then the payload |

**Header** (written once): magic `PROC_CTL`, version `1`, `header_size`,
`page_size`, `total_size`, `consumer` (NUL-padded), `schema_version` (the
consumer's own), `state_size` and `state_addr` (the state block in the
process's address space) and a random non-zero `instance_id`.

**Control block** (written by the owning agent): `seq`, `owner_id`,
`heartbeat_ns`, `lease_ns`, `payload_len`, `payload_crc` (CRC32C of the
payload), and `flags` (bit 0, `RELEASED`).

## Configuration: agent to process

The owner publishes with a seqlock on `seq`:

1. Swap `seq` from even `s` to `s+1` with a compare-and-swap. If `seq` is
   odd, a previous writer died mid-write: swap it from `s` to `s+2`.
2. Write the payload (in 8-byte words), `payload_len` and `payload_crc`.
3. Swap `seq` from odd `t` to `t+1`. That even value is the new
   **generation**.

If either swap fails, another agent is writing and this write is abandoned.
`payload_len == 0` means "no configuration: use your defaults".

The process reads with `procctl_poll`: if `seq` is unchanged, there is nothing
to do (one atomic load). Otherwise, when `seq` is even, it copies the
payload, re-reads `seq`, and accepts the copy only if `seq` is unchanged and
the CRC matches. The CRC also catches a writer that resumed after losing
ownership and scribbled over its successor's payload; the owner republishes
when it sees its payload changed.

## Answers: process to agent

The **state block** lives in the process's own memory at `state_addr`
(`struct procctl_state`, 40 bytes, followed by up to `state_size - 40` bytes
of consumer state): magic `PROCSTAT`, `seq`, `instance_id` (equal to the
header's), `applied_gen`, `pid`, `len`.

The process updates it with a seqlock (`seq` odd while writing):
`procctl_ack(gen)` raises `applied_gen`; `procctl_state_set` replaces the
consumer state. `pid` is the writer's `getpid()`.

The agent reads it with `process_vm_readv`: `seq`, the block, `seq` again;
the copy is valid if both reads of `seq` are equal and even and match the
block's. Reads are bounded in time: a process can make its own memory slow
to read.

## Ownership and lease

At most one agent owns a region; only the owner writes it. An agent may claim
(compare-and-swap `owner_id` to its own random id) when:

- `owner_id` is 0, or
- `RELEASED` is set, or
- `heartbeat_ns` has not changed for `lease_ns`, by the agent's own clock.

On claim it clears `flags`, writes its `lease_ns`, and keeps the current
payload as its own. While it owns the region it changes `heartbeat_ns` about
every `lease_ns / 4`. On a clean shutdown it sets `RELEASED` (a successor
takes over at once and the process keeps its configuration), or, to
uninstall, sets `owner_id` to 0.

The process watches the heartbeat with `procctl_lease_expired`: with no owner,
or no heartbeat change for `lease_ns` by the process's clock, it reverts to
its defaults. When the lease recovers, the next poll delivers the current
configuration again.

## Discovery and validation (agent)

The agent finds regions by scanning `/proc/*/fd` for links to
`/memfd:procctl:*`, periodically and whenever something tells it about a new
process (`Registry.Notify`). For each, it:

1. pins the process with `pidfd_open`;
2. opens `/proc/<pid>/fd/<n>` read-write and checks it is a memfd on tmpfs of
   a sane size, with the three seals and no write seal;
3. maps it and validates a copy of the header: magic, version, `total_size`
   equal to the file size, a power-of-two `page_size` in [4096, 65536], a
   non-zero `instance_id`, an aligned non-zero `state_addr`, and a
   `state_size` in [40, 65536]; the consumer must match the memfd name.

Everything in the region and the state block comes from an untrusted process:
the agent bounds-checks it and never dereferences `state_addr` itself. A
process can only break its own channel.

A region is identified by its inode: a forked child that keeps the parent's
fd shares the region, and its copy of the state block reports the parent's
`pid`, so the agent treats it as stale. The child may not write it
(`-ECHILD`); `procctl_reinit` gives it a region of its own.

## Requirements

Linux 5.3 or later (`pidfd_open`; `MFD_NOEXEC_SEAL` is used on 6.3+). The
agent needs `CAP_SYS_PTRACE` to open other processes' fds and read their
memory, and must see their pid namespace (`hostPID` on Kubernetes).
