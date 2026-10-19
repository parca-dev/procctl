# procctl — Process Control Channel Specification

**Version:** 1.0-draft (revision 2)
**Status:** Draft. Field meanings may change until 1.0 is final.

This document defines how a privileged node **agent** sends configuration and exclusive-resource grants to a library running inside another **process**, and how the process reports back. It is independent of any implementation.

Configuration flows one way, agent → process, through a sealed shared-memory **region** that the process creates and maps **read-only**. The process reports back by keeping a **state block** in its own private memory, which the agent reads with `process_vm_readv`. **The process never writes shared memory.**

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "MAY", and "OPTIONAL" are to be interpreted as described in RFC 2119 and RFC 8174 when, and only when, they appear in all capitals.

---

## 1. Terms

| Term | Meaning |
|---|---|
| **Process** | The side running inside the target process (typically a vendored C library). It creates the region and owns the state block. |
| **Agent** | The privileged node-local side. It discovers regions, writes configuration and grants, and reads state blocks. |
| **Region** | One sealed memfd created by one process for one consumer: a header page and an agent area. |
| **Agent area** | The part of the region written by the owner agent and read by the process. |
| **State block** | A structure in the process's private memory, written by the process and read remotely by agents. |
| **Consumer** | A named schema family, for example `parcagpu`. |
| **Generation** | An even, non-zero value of the agent area's `seq`. It identifies one published agent payload. |
| **Owner** | The one agent currently allowed to write a region's agent area. |
| **Resource** | Something at most one process on the node may use at a time, for example PC sampling on one GPU. Identified by `(kind, resource_id)`. |
| **Grant** | Permission, written by the owner agent, for one process to use one resource. |
| **Arbiter** | The agent logic that issues and revokes grants. |

All integers are little-endian, unsigned and naturally aligned. "u32" and "u64" are 4- and 8-byte unsigned integers.

"Atomic" means a single-copy-atomic, naturally aligned access of the field's full width, with the stated memory ordering. "Relaxed", "acquire", "release" and "acq_rel" have their C11 meanings.

`CLOCK_BOOTTIME` is used for every timestamp. No rule in this specification compares one process's clock reading with another process's.

---

## 2. Versioning

1. A region carries `spec_major` and `spec_minor` (§4).
2. A reader MUST reject a region whose `spec_major` differs from the major version it implements. It MUST accept any `spec_minor`.
3. A minor revision MAY only give meaning to previously reserved fields or bits, and the zero value MUST mean the previous minor version's behaviour. Writers MUST zero reserved fields and bits. Readers MUST ignore reserved fields and bits they don't understand.
4. `spec_major` also appears in the memfd name (§3.1), so an agent can skip incompatible regions without mapping them.
5. `schema_version` belongs to the consumer and is never interpreted by the transport.
6. The spec version is independent of any implementation's release version.

---

## 3. Region creation (process)

### 3.1 Steps

A process creating a region MUST perform these steps in order:

1. **Create the memfd:**
   ```
   memfd_create("procctl:<consumer>-v<spec_major>", MFD_CLOEXEC | MFD_ALLOW_SEALING | MFD_NOEXEC_SEAL)
   ```
   On `EINVAL`, retry without `MFD_NOEXEC_SEAL`. `<consumer>` MUST match `[a-z0-9_]{1,31}`.
2. **Size it:** let `P` be the system page size, a power of two in [4096, 65536]. Then:
   - `agent_area_size` = 64 + `resource_slots` × 48 + the requested payload capacity, rounded up to a multiple of `P`;
   - `total_size` = `P + agent_area_size`.

   Call `ftruncate(fd, total_size)`.
3. **Initialize the state block** (§6) in memory the process owns for the region's whole lifetime, with static storage duration RECOMMENDED. Initialize it to a consistent, even-`seq` state.
4. **Write the header** (§4) with `pwrite(fd, …, 0)`. The agent area stays zero, since a new memfd is zero-filled.
5. **Seal it:** `fcntl(fd, F_ADD_SEALS, F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_SEAL)`. The process MUST NOT add `F_SEAL_WRITE` or `F_SEAL_FUTURE_WRITE`, because the agent needs a writable mapping.
6. **Map it:** `mmap(NULL, total_size, PROT_READ, MAP_SHARED, fd, 0)`. The process MUST NOT map the region writable at any point.
7. **Keep the fd open** for as long as the region is in use. Agents discover regions through the fd table (§10).
8. **Announce it** with the hello probe (§11).

### 3.2 Layout

| Offset | Size | Contents |
|---|---|---|
| 0 | `P` | Header page (§4). Bytes beyond `header_size` are zero. |
| `P` | `agent_area_size` | Agent area (§5) |

### 3.3 No fallback

There is no anonymous-memory fallback, because an agent needs an fd to map the region. If any creation step fails, the process MUST release what it created, MUST NOT announce a region, and continues on its own defaults.

---

## 4. Header

Written once with `pwrite` before sealing (§3.1 step 4). Never modified afterwards.

| Off | Size | Field | Definition |
|---:|---:|---|---|
| 0 | 8 | `signature` | ASCII `PROC_CTL`, not NUL-terminated. Readers compare exactly 8 bytes. |
| 8 | 2 | `spec_major` | `1` |
| 10 | 2 | `spec_minor` | `0` |
| 12 | 4 | `header_size` | `128`. Readers ignore bytes beyond the fields they know. |
| 16 | 8 | `total_size` | Size of the memfd in bytes |
| 24 | 4 | `page_size` | `P` |
| 28 | 4 | `flags` | Reserved, 0 |
| 32 | 32 | `consumer` | NUL-padded `[a-z0-9_]{1,31}`. MUST equal `<consumer>` in the memfd name. |
| 64 | 4 | `schema_version` | Consumer-defined |
| 68 | 4 | `creator_pid` | The process's pid in its own pid namespace. Informational only (§10.3). |
| 72 | 8 | `agent_area_off` | `P` |
| 80 | 8 | `agent_area_size` | A multiple of `P`, ≥ `P` |
| 88 | 8 | `state_addr` | Address of the state block in the process's address space; 8-byte aligned; 0 = no state block |
| 96 | 4 | `state_size` | Capacity of the state block in bytes: a multiple of 8, in [64, 65536]; 0 if `state_addr` is 0 |
| 100 | 2 | `resource_slots` | Number of grant-table and resource-table entries, 0..64 |
| 102 | 2 | reserved | 0 |
| 104 | 8 | `created_at_ns` | `CLOCK_BOOTTIME` at creation |
| 112 | 8 | `instance_id` | Random, non-zero. Distinguishes regions across pid reuse and fork. |
| 120 | 4 | `header_crc32c` | CRC32C (Castagnoli) of bytes [0, 120) |
| 124 | 4 | reserved | 0 |

`state_addr` is an address in another process. Agents use it **only** as the target of a remote read (§7.3), never as a pointer into their own memory.

---

## 5. Agent area

Written only by the region's owner agent (§8.1). The process maps it read-only.

| Offset in area | Size | Contents |
|---|---|---|
| 0 | 64 | Control block (§5.1) |
| 64 | `resource_slots` × 48 | Grant table (§5.2) |
| 64 + `resource_slots` × 48 | the rest | Consumer payload |

The **agent payload** is the grant table followed by `payload_len` bytes of consumer payload. The whole agent payload is protected by the control block's `seq` (§7.1).

### 5.1 Control block

| Off | Size | Field | Protected by `seq` | Definition |
|---:|---:|---|---|---|
| 0 | 8 | `seq` | — | Sequence counter (§7.1). 0 = never written. Its low 32 bits are the wake word (§8.4). |
| 8 | 4 | `payload_len` | yes | Length of the consumer payload |
| 12 | 4 | `flags` | yes | Bit 0 `PAYLOAD_VALID`: when clear, `payload_len` is 0 and the process uses its own defaults for consumer configuration. The grant table is valid regardless. |
| 16 | 8 | `writer_id` | — | Random, non-zero id of the owner agent instance; 0 = no owner (§8.1) |
| 24 | 8 | `heartbeat_ns` | — | Owner's `CLOCK_BOOTTIME` at its last heartbeat; strictly increasing while owned (§8.3) |
| 32 | 8 | `lease_ns` | — | Lease length; 0 = no lease (§8.3) |
| 40 | 4 | `writer_pid` | — | Owner's pid as seen from the owner's pid namespace |
| 44 | 4 | `writer_flags` | — | Bit 0 `RELEASED`: the owner is shutting down, so a successor may claim immediately |
| 48 | 8 | `writer_start` | — | Owner's process start time: field 22 of `/proc/<writer_pid>/stat` |
| 56 | 4 | `payload_crc32c` | yes | CRC32C of the agent payload: grant table plus `payload_len` consumer bytes |
| 60 | 4 | reserved | — | 0 |

### 5.2 Grant table entry (48 bytes)

| Off | Size | Field | Definition |
|---:|---:|---|---|
| 0 | 16 | `resource_id` | Opaque id, e.g. a GPU UUID |
| 16 | 8 | `grant_id` | Non-zero id of this grant: the generation of the agent payload that first contained it (§9.3). 0 = empty slot. |
| 24 | 8 | `duration_ns` | How long the holder may use the resource, timed by the holder from when it first observes this grant (§9.1) |
| 32 | 4 | `kind` | Consumer-defined resource kind (e.g. 1 = PC sampling) |
| 36 | 4 | `flags` | Reserved, 0 |
| 40 | 8 | reserved | 0 |

An all-zero entry is an empty slot. A process holds a grant for `(kind, resource_id)` exactly while a slot with that `kind` and `resource_id` and a non-zero `grant_id` is present in its region's current agent payload.

---

## 6. State block (process private memory)

A process that reports state keeps a state block at `state_addr`, in its own private memory. It is never shared and never written by agents.

| Off | Size | Field | Definition |
|---:|---:|---|---|
| 0 | 8 | `magic` | ASCII `PROCSTAT` |
| 8 | 8 | `seq` | Sequence counter (§7.2); 0 = not yet initialized |
| 16 | 8 | `instance_id` | MUST equal the header's `instance_id` |
| 24 | 4 | `writer_pid` | `getpid()` of the process that last wrote the block (§12) |
| 28 | 4 | `state_len` | Length of the consumer state |
| 32 | 8 | `applied_gen` | Latest agent generation whose *consumer payload* the process has applied (§8.2) |
| 40 | 8 | `applied_writer_id` | `writer_id` read after that payload validated (§8.2) |
| 48 | 8 | `grants_seen_gen` | Generation of the latest grant table the process has fully evaluated (§9.1) |
| 56 | 4 | `crc32c` | CRC32C of the whole used block [0, 64 + `resource_slots` × 48 + `state_len`), computed with `seq` and this field set to 0 |
| 60 | 4 | reserved | 0 |
| 64 | `resource_slots` × 48 | resource table | §6.1 |
| 64 + `resource_slots` × 48 | `state_len` | consumer state | Consumer-defined |

### 6.1 Resource table entry (48 bytes)

| Off | Size | Field | Definition |
|---:|---:|---|---|
| 0 | 16 | `resource_id` | Resource this entry describes |
| 16 | 8 | `grant_id` | The grant this entry's `state` refers to; 0 if none |
| 24 | 8 | `last_activity_ns` | Process's `CLOCK_BOOTTIME` when it last used, or wanted, the resource. Informational; for arbiter policy only. |
| 32 | 4 | `kind` | Resource kind |
| 36 | 4 | `state` | 0 `IDLE`, 1 `ACQUIRING`, 2 `HOLDING`, 3 `RELEASED`, 4 `FAILED` |
| 40 | 4 | `flags` | Bit 0 `WANT`: the process would use this resource if granted |
| 44 | 4 | `error` | Consumer-defined error code, meaningful in `FAILED` |

`ACQUIRING` and `HOLDING` mean **may hold**. `IDLE`, `RELEASED` and `FAILED` mean **does not hold**. A process MUST only report a does-not-hold state when it really doesn't hold the resource, and will not use it again without a new grant.

---

## 7. Publication protocols

### 7.1 Agent area (seqlock; the owner agent writes, the process reads)

- **Even** `seq` is stable; **odd** means a write is in progress.
- Each completed write leaves a new, strictly greater even value: that payload's **generation**.

**Writer** (owner agent):
```
1. s = load(seq, acquire)
   t = (s is even) ? s + 1 : s + 2              // s + 2 adopts a cycle left by a dead writer
   if !CAS(seq, s → t, acq_rel): abort           // another writer is active (§7.4)
2. fence(release)
3. for each 8-byte word of the agent payload: store(word, relaxed)
   store(payload_len, relaxed); store(flags, relaxed); store(payload_crc32c, relaxed)
4. if !CAS(seq, t → t + 1, release): abort        // preempted (§7.4)
5. FUTEX_WAKE(&seq low 32 bits, INT_MAX)          // §8.4
   generation = t + 1
```
A grant newly added in this write carries `grant_id = t + 1` (§9.3).

**Reader** (process; any number of readers, each with its own `last_seen`):
```
1. s1 = load(seq, acquire)
   if s1 == last_seen: no change
   if s1 is odd or s1 == 0: not available now; try later
2. n = load(payload_len, relaxed); if 64 + slots × 48 + n > agent area size: torn
   c = load(payload_crc32c, relaxed); f = load(flags, relaxed)
   for each word: tmp[i] = load(word, relaxed)
3. fence(acquire)
4. s2 = load(seq, relaxed)
5. if s2 != s1 or crc32c(tmp) != c: torn; retry
   else: tmp is generation s1; last_seen = s1
```

**Reader requirements:**
- A reader MUST copy the agent payload into private memory before interpreting it.
- A reader MUST bound lengths with capacities it computed itself.
- A reader MUST bound its retries, and MUST NOT block or spin waiting for an even `seq`.

### 7.2 State block (seqlock; the process writes in its own memory)

The process is the only writer. A multi-threaded process MUST serialize its writers.
```
1. store(seq, seq + 1, relaxed); fence(release)       // odd
2. write the fields, the resource table and the consumer state; compute crc32c last
3. store(seq, seq + 1, release)                         // even
```
After an update the process MAY fire the `procctl:state` probe (§11).

### 7.3 Remote read of the state block (agent)

The agent reads with `process_vm_readv` (or `pread` on `/proc/<pid>/mem`), keyed to a pidfd-pinned process:
```
1. s1 = remote_read_u64(state_addr + 8);  if s1 is odd or 0: busy
2. buf = remote_read(state_addr, state_size)
3. s2 = remote_read_u64(state_addr + 8)
4. valid iff s1 == s2 == buf.seq, magic is "PROCSTAT", buf.instance_id == header.instance_id,
   state_len and resource_slots fit in state_size, and crc32c matches
```
- The agent MUST bound its retries (RECOMMENDED: 8) and report "busy" after that.
- **Every remote read MUST have a time bound** enforced by the agent (§13).
- **A failed read is never evidence that a resource has been released** (§9.2).

Ordering note (informative): the reader's two `seq` reads and its block read are separate system calls. Torn reads are caught by the `seq` comparison and by the CRC.

### 7.4 Dead and competing agent writers

- **A writer that died mid-write** leaves `seq` odd. The next legitimate writer adopts that cycle with `s + 2`.
- **Two writers at once** (a suspended former owner resuming after a successor claimed the region) are detected by the CAS steps:
  - a writer whose step-1 CAS fails MUST NOT write;
  - a writer whose step-4 CAS fails has been preempted, and MUST stop writing until it re-establishes ownership (§8.1).
- **A preempted writer may have overwritten payload bytes;** readers then see a CRC mismatch or that writer's complete payload. The owner MUST, at least once per heartbeat interval, compare `payload_crc32c` and `payload_len` with what it last published, and republish if they differ.

### 7.5 Memory model note (informative)

Neither the C11 nor the Go memory model formally covers memory shared across processes. This protocol relies on atomic accesses and fences lowering to the corresponding hardware instructions (x86-64 TSO; ARMv8 LDAR/STLR/LDAXR/STLXR/DMB).

It follows Boehm, "Can Seqlocks Get Along with Programming Language Memory Models?" (2012): payload words are accessed with relaxed atomics, not plain copies. In Go, `sync/atomic` satisfies every ordering above. A plain `copy()` between the two `seq` loads does **not**, on arm64.

---

## 8. Ownership, acknowledgement, lease and wake

### 8.1 Ownership (agents)

Normally one agent runs per node. When several do, behaviour MUST degrade gracefully: exactly one owner per region, no repeated ownership changes between healthy agents, and no reversion of the process's configuration during a hand-over.

**Claiming.** An agent MAY claim a region with `CAS(writer_id, observed → own_id, acq_rel)` only if, for the observed owner, one of these holds:
1. `writer_id == 0`;
2. `RELEASED` is set;
3. the owner process no longer exists: `writer_pid` doesn't exist in the claimant's pid namespace, or its start time ≠ `writer_start`. Only usable when the claimant shares the owner's pid namespace; agents SHOULD run in the host pid namespace;
4. the claimant has itself observed `heartbeat_ns` unchanged for longer than a non-zero `lease_ns`, on its own clock.

**After a successful claim,** before its first payload write, the new owner MUST:
- store `writer_pid`, `writer_start` and `writer_flags = 0`;
- store `lease_ns` and a fresh `heartbeat_ns`;
- **adopt the current grant table as live** (§9.2, rule A4).

**Passive agents.** An agent that can't claim MUST NOT write any field of the region. It MAY read the agent area and the state block.

**Losing ownership.** Before each payload write and each heartbeat, the owner MUST re-load `writer_id` (acquire). If it's not its own, the owner has lost the region and MUST become passive.

**Release.**
- **Shutting down for replacement:** the owner SHOULD set `RELEASED` and leave all other fields unchanged.
- **Being removed for good:** the owner MAY instead `CAS(writer_id, own_id → 0)`.

### 8.2 Acknowledging configuration (process)

After applying the consumer payload of generation `g`, the process:
1. loads `writer_id` (acquire) after `g` validated (§7.1 step 5);
2. updates its state block with `applied_writer_id` = that value, and `applied_gen` = max(`applied_gen`, `g`).

`applied_gen` MUST NOT decrease.

An agent considers `g` acknowledged when a valid state read (§7.3) shows `applied_gen ≥ g` and `applied_writer_id` equal to the agent's own id.

### 8.3 Lease

- **Owner:** an owner that uses a lease stores a non-zero `lease_ns` (RECOMMENDED 10 s) and updates `heartbeat_ns` at intervals no longer than `lease_ns / 4`. Each value is its `CLOCK_BOOTTIME`, or the previous value + 1 if that's greater.
- **When the lease is expired:** either `writer_id == 0`, or `lease_ns != 0` and the process has seen `heartbeat_ns` *unchanged* for longer than `lease_ns`. That is measured on the process's own clock: it remembers the last value and the local time it first saw it. The process MUST NOT compare `heartbeat_ns` with its own clock.
- `RELEASED` does not affect expiry.
- **While the lease is expired,** the process SHOULD use its own defaults for consumer configuration, and MUST release every held resource (§9.1).
- **When the lease goes from expired to not expired,** the process MUST treat the current consumer payload as new, so it's applied again.

### 8.4 Wake

After every agent payload write, the owner MUST issue `FUTEX_WAKE` (shared, i.e. without `FUTEX_PRIVATE_FLAG`) on the low 32 bits of `seq`. A process thread MAY block in `FUTEX_WAIT` on that word, in its read-only mapping, with the value it last read and a timeout.

Wakes are hints:
- a waiter MUST re-read the agent area after any return from the wait;
- heartbeats don't wake;
- a waiter that evaluates the lease MUST use a timeout no longer than `lease_ns / 4`.

---

## 9. Exclusive resources

An exclusive resource is something that **at most one process on the node may use at a time**. Using it concurrently, even briefly, may cause lasting harm; for example, two processes starting PC sampling on one GPU permanently breaks the second one's sampling.

Grants travel in the agent payload's grant table (§5.2). Holders report in their state block's resource table (§6.1).

Resources MUST be per-process: a holding is never inherited by a forked child.

### 9.1 Holder obligations (process)

- **H1 — acquire only under a live grant.** The process MAY start using resource R only when all of these hold:
  - its most recent valid read of the agent payload contains a grant for R with `grant_id = G`;
  - the process has not already released G;
  - less than `duration_ns` has passed since it first observed G;
  - the lease is not expired.
- **H2 — announce first.** Before starting to use R, the process MUST complete a state-block update showing R `ACQUIRING` with `grant_id = G`. When acquisition succeeds, R becomes `HOLDING`. When it fails, R becomes `FAILED`, and the process MUST ensure it doesn't hold R.
- **H3 — release when required.** The process MUST stop using R, and then report R `RELEASED` with `grant_id = G`, when any of these happens:
  - G is no longer present in a valid read of the agent payload (revoked or replaced);
  - `duration_ns` has elapsed since it first observed G;
  - the lease is expired.
- **H4 — evaluate promptly.** The process SHOULD re-evaluate grants on every wake (§8.4), and at least every min(`lease_ns` / 4, remaining duration).
  - The update that records the outcome of evaluating generation `g` MUST also set `grants_seen_gen = g`.
  - `grants_seen_gen` MUST NOT decrease.
- **H5 — express interest.** A process that would use R SHOULD list R in its resource table with `WANT` set and a current `last_activity_ns`. An arbiter only grants to processes that list R.
- **H6 — independent of the app's own threads.** Release (H3) MUST NOT depend on the application calling into the library. A consumer that can only release from application callbacks doesn't meet H3.

### 9.2 Arbiter obligations (agent)

- **A1 — one holder.** For each `(kind, resource_id)` on a node, *outstanding grants* MUST exist in at most one process's region. A grant is outstanding from the write that publishes it until its release is confirmed (A2). Outstanding grants for one process are allowed (renewal, §9.3).
- **A2 — confirmed release.** The release of grant G for R, issued to process X, is confirmed only by one of:
  - X has exited (pidfd);
  - a valid state read (§7.3) of X shows **both**:
    - `grants_seen_gen ≥` the generation of the agent payload that removed G, **and**
    - X's resource-table entry for R is absent or in a does-not-hold state (`IDLE`, `RELEASED`, `FAILED`);
  - a valid state read of X shows R `RELEASED` or `FAILED` with `grant_id = G`.
- **A3 — never by timeout.** Elapsed time, read failures, missing state, or a stale state block (§12) MUST NOT confirm a release. An arbiter that can't confirm a release MUST keep the grant outstanding and SHOULD report the resource as blocked.
- **A4 — adopt on claim.** An agent that claims a region (§8.1) MUST treat every grant in that region's current grant table as outstanding until it confirms the release.
- **A5 — discover before granting.** Before issuing any grant for R, the agent MUST have:
  - completed one full discovery pass (§10.1);
  - read the state of every attached process with R in its resource table, treating any such process whose entry shows may-hold, or whose state can't be read, as an outstanding holder.
- **A6 — deadline as a backstop only.** `duration_ns` lets a holder release by itself when the agent is gone. The arbiter MUST still apply A2 before re-granting R, even after `duration_ns` has passed.

### 9.3 Grant identity

A grant's `grant_id` is the generation of the agent payload in which it first appears in that region. Generations are strictly increasing for the region's lifetime, across agent instances (§7.4), so a `grant_id` is never reused within a region.

**Renewal.** An arbiter extends a holding by replacing grant G with a new grant G′ for the same resource in the **same** process's region, in a single write. G′ has a new `grant_id` and `duration_ns`.

A holder that sees, in one valid read, G removed and G′ present for a resource it is `HOLDING` under G MAY **transfer** the holding to G′ without releasing. It then reports `HOLDING` with `grant_id = G′` and times G′ from that observation.

For A2, the release of G is then confirmed by the holder reporting G′; G′ stays outstanding. A holder that doesn't transfer simply releases G (H3), and may then acquire under G′ (H1).

### 9.4 Hand-off sequence (informative)

```
agent:   write A's table without G_A               → generation r; FUTEX_WAKE
A:       wake → read table → stop using R → state {R: RELEASED, G_A}, grants_seen_gen = r
agent:   remote-read A's state: confirmed (A2)     → write B's table with {R, G_B = new generation}
B:       wake → read → state {R: ACQUIRING, G_B} → acquire → state {R: HOLDING, G_B}
```

---

## 10. Discovery and attach (agent)

### 10.1 Discovery

- **The fd scan.** An agent finds regions by reading the links in `/proc/<pid>/fd/`. A region's link target is `/memfd:procctl:<consumer>-v<major>`, optionally followed by ` (deleted)`. Agents MUST support this scan.
- **Faster sources.** Agents MAY use the hello probe (§11) or other event sources to find regions sooner, but MUST NOT rely on them alone.
- **Wrong major version.** Agents SHOULD skip regions whose `<major>` they don't implement, without mapping them.

### 10.2 Validation

Before using a region, an agent MUST perform these steps in order, and reject the region on any failure:

1. **Pin the process:** obtain a pidfd.
2. **Get the fd:**
   - `pidfd_getfd`; or
   - open `/proc/<pid>/fd/<n>` `O_RDWR | O_CLOEXEC`.

   Then confirm through the pidfd that the process still exists.
3. **Check the file** (`fstat`, `fstatfs`): a regular file on tmpfs (`TMPFS_MAGIC`), with `st_size` within the agent's limit. Agents MUST accept at least 4 MiB.
4. **Check the seals:** `F_GET_SEALS` includes `F_SEAL_SHRINK`, `F_SEAL_GROW` and `F_SEAL_SEAL`, and includes neither `F_SEAL_WRITE` nor `F_SEAL_FUTURE_WRITE`.
5. **Map** `st_size` bytes `MAP_SHARED`, `PROT_READ | PROT_WRITE`.
6. **Snapshot the header** into private memory and validate it:
   - `signature`, `spec_major`, `header_crc32c`;
   - `total_size == st_size`;
   - `page_size`;
   - `consumer` equal to the name suffix;
   - `agent_area_off == page_size`, and `agent_area_off + agent_area_size == total_size`;
   - `resource_slots ≤ 64`, and 64 + `resource_slots` × 48 ≤ `agent_area_size`;
   - if `state_addr ≠ 0`: it is 8-byte aligned, `state_size` is a multiple of 8 in [64, 65536], and 64 + `resource_slots` × 48 ≤ `state_size`.
7. **Use only the snapshot from then on.** The agent MUST NOT re-read the header from shared memory.
8. **De-duplicate** by `(st_dev, st_ino)`.

The agent SHOULD `mprotect` its view of the header page to `PROT_READ`, and SHOULD close the fd after mapping.

### 10.3 Identity

- The agent identifies the process by pid and pidfd, and maps the host pid to namespace pids with the `NSpid:` line of `/proc/<pid>/status`.
- `creator_pid` MUST NOT be trusted for identity. It MAY serve as a hint for which of several pids sharing one `(st_dev, st_ino)` to read state from: prefer the pid whose innermost `NSpid` equals `creator_pid`, otherwise the lowest host pid.
- The agent SHOULD detect process exit through the pidfd.

---

## 11. Probes

The process SHOULD define these USDT probes (SystemTap SDT notes in `.note.stapsdt`):

| Provider | Name | When | Arguments |
|---|---|---|---|
| `procctl` | `hello` | after creating a region (§3.1 step 8), including `reinit` (§12) | `arg1`: s32 region fd; `arg2`: pointer to the NUL-terminated memfd name; `arg3`: s32 `getpid()` |
| `procctl` | `state` | after each state-block update (OPTIONAL) | `arg1`: u64 state `seq`; `arg2`: u64 `grants_seen_gen` |

- Agents MAY find the probes by reading the ELF notes of mapped objects.
- Probe arguments are hints. An agent MUST still discover and validate regions through §10, and MUST still confirm releases through state reads (§9.2).

---

## 12. fork

- **No fork handlers are required.** A forked child inherits:
  - the region fd (`MFD_CLOEXEC` closes it only on exec);
  - the read-only mapping;
  - a private copy of the state block.

  Reading the parent's agent area is harmless.
- **The child's copy of the state block is stale:** its `writer_pid` is the parent's pid.
  - The process side MUST NOT update the state block from a process whose `getpid()` differs from the pid that initialized it. Implementations check this on their write paths.
  - Agents MUST treat a state block whose `writer_pid` doesn't equal the innermost `NSpid` of the process read as **stale**. A stale block reports no state and no holdings. Under A3, staleness never confirms a release; the child cannot hold the parent's resources (§9).
- **A child that wants its own channel** MAY re-initialize: close the inherited fd, unmap the region, create a new region and state block with a new `instance_id`, and announce it (§11).

---

## 13. Trust

Each side MUST treat the other as untrusted:

- Every length, offset and index MUST be checked against bounds the reader derived from its own validated snapshot.
- No value read from a region or state block is ever dereferenced as a pointer in the reader's address space. `state_addr` is only ever used as a remote-read target.
- Every read MUST complete in bounded time, whatever the peer does.
  - Remote reads (§7.3) MUST have a time bound enforced by the agent. A process can make its own memory slow or blocking to read, for example with userfaultfd or a mapping backed by a slow filesystem.
  - An agent MUST isolate a slow or failing process from all others.
- The seals plus the `st_size` check ensure that accessing a validated mapping can't raise SIGBUS.
- A process can only corrupt or misreport its own channel. A process that falsely reports *may hold* for a resource can block that resource on its node; arbiters SHOULD report such blocking.

---

## 14. Consumer schemas (informative)

The consumer payload and the consumer state are opaque to the transport. Consumers SHOULD define fixed-layout, little-endian structures with explicit reserved padding and their own `schema_version`. Resource `kind` values are consumer-defined.

---

## Appendix A. Kernel requirements (informative)

| Feature | Linux | Needed by | If missing |
|---|---|---|---|
| `memfd_create`, file sealing | 3.17 | process | procctl unavailable |
| `MFD_NOEXEC_SEAL` | 6.3 | process | retry without the flag |
| `process_vm_readv` | 3.2 | agent | `pread` on `/proc/<pid>/mem` |
| shared `FUTEX_WAIT` on a read-only `MAP_SHARED` mapping | to be verified per kernel | process thread | wait with a short timeout and no wake |
| `pidfd_open` | 5.3 | agent | detect exit by re-checking `/proc/<pid>/stat` start time |
| `pidfd_getfd` | 5.6 | agent | open `/proc/<pid>/fd/<n>` |

The reference implementations target Linux ≥ 5.4.

## Appendix B. Relation to OTEP 4719 (informative)

**Borrowed from the OpenTelemetry Process Context specification:**
- the 8-byte non-NUL-terminated signature and version;
- a memfd named after the protocol;
- "write the publication marker last, with a fence";
- `CLOCK_BOOTTIME`;
- for the state block, the same reading technique: a structure in the publisher's private memory, read remotely with `process_vm_readv` and validated by re-checking a marker.

**Different because agents must also write:**
- the agent area is a sealed `MAP_SHARED` memfd whose fd stays open;
- there is no anonymous fallback;
- discovery uses `/proc/<pid>/fd`;
- the publication marker is a sequence counter whose even values identify generations and grants.

## Appendix C. PC sampling as an exclusive resource (informative)

Measurements on a DGX Spark (GB10, driver 580.178, CUDA 13.0) motivated §9. See the spike's `FINDINGS.md`.

- **What claims the GPU:** the first `cuptiPCSamplingStart` claims PC sampling on the GPU until `cuptiPCSamplingDisable` or process exit. `Stop` doesn't release it.
- **A second process starting meanwhile** in KERNEL_SERIALIZED mode gets a silent success, about 180× slower kernels for that window, and permanently broken sampling.
- **Releasing from a non-application thread works,** busy or idle, with no app-visible stall.

Mapping onto this specification:
- `kind` = PC sampling, `resource_id` = GPU UUID;
- acquire = Enable + Start; release = Stop + Disable;
- H6 is met by a consumer-owned control thread waiting on the wake word (§8.4).
