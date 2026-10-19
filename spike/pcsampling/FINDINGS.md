# PC sampling across processes: findings

**Setup:** DGX Spark (GB10, sm_121, 48 SMs), driver 580.178.04, CUDA 13.0, CUPTI 130001, profiling allowed for non-root users. One GPU, no MIG. Probes run a 2 ms busy kernel in a loop; each process has its own kernel name.

**Runs:**
- `results/20261019-105019`: main matrix of 15 scenarios, each in serialized mode (no prefix), continuous mode (`C-`), and under MPS (`M-`). The directory names were repaired after a run.sh scoping bug.
- `results/20261019-111837`: follow-ups S40–S44.
- `results/20261019-112119` and `results/20261019-112259`: S45–S46, serialized (`F-`) and continuous (`FC-`).

## Answers

**F2: what holds the GPU.**
- The **first `cuptiPCSamplingStart`** on the GPU claims PC sampling for that process, and it keeps the claim **until `cuptiPCSamplingDisable`**.
  - `Stop` does **not** release it (S12, S40, S44).
  - `Enable` without `Start` claims nothing (S11: B samples normally while A sits enabled and never starts).
- Hand-off works only through **Disable by the holder, then Start by the next process** (S13, S41, S46). Same in serialized, continuous and MPS.

**F1: what a second process's Start does while another process holds the GPU.** It depends on the mode.

| Mode | 1st Start | Kernels in that window | Afterwards | Recovery |
|---|---|---|---|---|
| KERNEL_SERIALIZED (parcagpu today), also under MPS | **returns `CUPTI_SUCCESS`** (silent) | **about 180× slower** (2 ms → 350–870 ms) | `GetData` → `CUPTI_ERROR_UNKNOWN`; every later Start → `CUPTI_ERROR_UNKNOWN` | **none**: Disable → UNKNOWN and re-Enable → INVALID_PARAMETER, even after the holder has disabled (S45) |
| CONTINUOUS | `CUPTI_ERROR_HARDWARE_BUSY` (clean) | normal | 1× UNKNOWN, then `CUPTI_ERROR_INVALID_OPERATION` | **yes**: after the holder disables, Disable + Enable recovers (S45: 10/10 starts) |

**One collision is enough.** In every overlapping scenario (S10, S14–S16, S30, S44), the loser's sampling died at its first overlap and never came back for the rest of the run. That includes time-slot schedules: windows can only close at a kernel boundary, and serialized kernels take 17–40 ms while sampling, so windows overran into the next process's slot within the first second.

**F3: attribution.** Zero samples from another process's kernel in any scenario.

**F5: impact on processes that aren't sampling.** None measurable (S20, S21: B's kernel time is unchanged). B even gets more GPU, because A's sampled kernels take longer.

**F6: cost of a Start, and of a hand-off.**
- Serialized: a failed Start costs about 3 µs.
- Continuous: the **first Start after Enable blocks for about 320 ms**, HARDWARE_BUSY included. Later Starts take about 70 µs.
- Serialized: the first window after Enable runs kernels at about 170 ms, against about 16–25 ms in later windows (S46).

So each hand-off costs the receiving application roughly a 0.3 s stall.

**Overhead to the sampled process.**
- Serialized: kernels run **8–13× slower** inside a window (2 ms → 16–26 ms). Outside windows they're normal.
- Continuous: **no measurable kernel slowdown** (2.005 ms vs 2.004 ms). Very long windows hit `GetData` `OUT_OF_MEMORY` (S02, S10); 30–400 ms windows didn't.

**MPS:** behaves the same as serialized without MPS.

**Holder death** (`results/holder-death-124755`, `holder_death.sh`; GPU verified otherwise idle first). A holds the GPU inside a whole-run window; B first tries 1 s after A is killed.

| Case | B afterwards |
|---|---|
| H1: A SIGKILLed (serialized) | 14/14 starts ok: **the driver releases the claim when the holder process exits** |
| H2: A SIGKILLed (continuous) | 14/14 ok |
| H3: A SIGSTOPped (hung but alive), killed 3 s later | B's attempt while A was frozen **wedged B for good**, even after A died |

So process exit is a reliable release (pidfd exit means the GPU is free), but a hung holder keeps the claim. Anyone who tries during that time is permanently broken.

An earlier holder-death run was discarded. An unrelated CUDA process (`fsst_cuda`) was holding PC sampling on the GPU during it; even a lone probe failed until that process stopped.

**Releasing from a background thread** (`results/ctl-131315`, `ctlthread.cu`, `ctl_tests.sh`; GPU idle otherwise).

A's main thread enables PC sampling, opens a window and launches kernels. A's control thread (all signals blocked) releases at 2 s: `cuCtxSetCurrent`, then Stop, a GetData drain, and Disable, under a mutex that the main thread only `try_lock`s. B (pcprobe) then tries 200 ms windows every 500 ms from 3 s.

| Case | Stop | Disable | A's app thread after release | B |
|---|---|---|---|---|
| T1a: main thread busy | ok, 4.1 ms | ok, 61 ms | 0 launch errors; kernels back to 2.007 ms (max 2.27) | 10/10 starts, samples ok |
| T1b: main thread idle (no CUDA calls since 1 s) | ok, 17 µs | ok, 57 ms | n/a | 10/10 |
| T1c: busy, Disable without Stop | skipped | ok, 56 ms | 0 errors, 2.007 ms | 10/10 |
| T1d: busy, `cuCtxSynchronize` first (1.7 ms) | ok | ok, 60 ms | 0 errors | 10/10 |
| T1e: idle, Disable without Stop | skipped | ok, 54 ms | n/a | 10/10 |

So a thread other than the app's can release PC sampling **whether the app is busy or idle**, with no synchronize needed (Disable alone is enough). The app thread sees no errors and no stall: Disable's ~55–60 ms blocks only the control thread, and the app's kernel time returns to baseline immediately. Lock contention was under 1 µs.

**CUPTI flush-period thread as a zero-new-thread alternative** (T2). With `cuptiActivityFlushPeriod(100)` and a `CONCURRENT_KERNEL` activity enabled, `bufferCompleted` was **never called** during 1 s of busy work or 4 s of idle. It ran only once, from the final `cuptiActivityFlushAll`, on CUPTI's worker thread. So it isn't a usable periodic hook; as configured here, it only fires when buffers fill or on an explicit flush.

## Consequences

1. **Uncoordinated duty cycling is out.** That includes parcagpu's current random windows, and time slots that only *assume* windows don't overlap. Any overlap permanently disables the loser's PC sampling and gives it a window of ~180× slower kernels.
2. **Strict mutual exclusion per GPU, with a confirmed release,** is required. A process may Start only while it holds a grant. The next process may be granted only after the holder has **Disabled**, and the arbiter must *know* that has happened. That's the revoke → ack → grant handshake, where the ack means "I have called Disable".
3. **Hand-offs are expensive** (~0.3 s stall), so rotation is coarse: seconds per holder, not 30 ms slots.
4. **Without a grant, don't sample.** An unmanaged process that starts PC sampling can permanently break a managed one in serialized mode.
5. **parcagpu stays on KERNEL_SERIALIZED,** because attributing PC samples to kernels requires it. The unrecoverable collision behaviour is therefore the one we design for.
6. **Releases come from Disable or process exit, never from Stop.** The arbiter may hand over after a confirmed Disable, or after pidfd exit. A holder that is alive but unresponsive must keep the grant: re-granting would wedge the next process.
7. **The release confirmation has to be readable *state*, not just an event.** An agent that restarts mid-hand-off must be able to ask "is this process still holding the GPU?" A USDT event fired while no agent was attached is lost.
8. **parcagpu needs a small control thread** to release on revoke or deadline whatever the app is doing. The tests above show it's safe. CUPTI's own flush thread can't be used for this.

## Caveats

- **One machine and one GPU type:** a single GB10 with one driver version. Re-check the F1/F2 behaviour on a datacenter GPU (H100/B200) and its driver before relying on it there.
- **One workload shape:** a single kernel shape with a single context per process.
- **Recovery for serialized mode** was only tried with Disable + Enable on the same context. Destroying and recreating the context wasn't tried, and isn't an option for parcagpu inside a user's process anyway.
