# PC sampling multi-process spike

This spike answers how CUPTI PC sampling behaves when several processes share one GPU. The answer decides how parcagpu coordinates sampling, and from that, what procctl needs to carry.

## Files

- `pcprobe.cu`: a single test process.
  - Runs a busy kernel `spin_k<N>`; each process gets a different N, so its samples can be attributed to it.
  - Enables PC sampling and opens sampling windows on a schedule anchored to a shared `CLOCK_BOOTTIME` start time.
  - Logs JSON lines: the return code and latency of every CUPTI call, each window's samples split by kernel function, and launches per second.
- `run.sh`: runs the scenarios below, once each in KERNEL_SERIALIZED mode (no prefix), CONTINUOUS mode (`C-`), and optionally under MPS (`M-`, enabled with `--mps`).
- `summarize.py`: produces `results/<run>/summary.md`.

## Run

```
make
sudo ./run.sh --mps      # about 15 min; sudo unless NVreg_RestrictProfilingToAdminUsers=0
less results/latest/summary.md
```

## Questions → scenarios

| Q | Question | Scenarios | Look at |
|---|---|---|---|
| F1 | B calls Start while A is inside a window: error, silent success, or hang? | S10, S10b, S15, S16 | B's start rc and latency; "concurrently-open successful windows"; overlapped vs isolated yield |
| F2 | Is the hardware held by Enable, or only by Start..Stop? | S11 (A enabled, never started), S12 (A stops at 5 s but stays enabled), S13 (A disables at 9 s) | the "Timeline" line: when B's first successful Start happens |
| F3 | Do one process's samples include another's kernels? | S14, S15, S16, S30 | "foreign samples" column |
| F5 | Does sampling in A slow down B? | S20, S21 vs S01 | B's launches/s and kernel ms |
| F6 | Cost of a failed Start | any collision scenario | "start lat µs fail" column |
| — | Does time-slotted (TDMA) sharing work? | S14 (2 procs), S30 (3 procs) | everyone's starts succeed, yield ≈ the solo S03 yield |
| — | Do any of the above differ under MPS or CONTINUOUS mode? | `M-*`, `C-*` | compare with the unprefixed runs |

The DGX Spark (GB10) has a single GPU and no MIG support, so F4 (MIG) is out of scope here.
