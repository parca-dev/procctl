#!/usr/bin/env bash
# Multi-process CUPTI PC sampling spike. Runs choreographed pcprobe processes
# against GPU 0 and writes JSONL logs to results/<run>/<scenario>/<proc>.jsonl.
#
#   ./run.sh                 # serialized + continuous groups
#   ./run.sh --mps           # also run the MPS group (starts a private MPS daemon)
#   ./run.sh --only S15      # run scenarios whose name starts with S15
#   KERNEL_US=500 ./run.sh   # shorter kernels
#
# PC sampling needs root, or the nvidia module loaded with
# NVreg_RestrictProfilingToAdminUsers=0. Run with sudo if needed.
set -euo pipefail
cd "$(dirname "$0")"

MPS=0
FOLLOWUP=0
ONLY=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --mps) MPS=1 ;;
    --followup) FOLLOWUP=1 ;;
    --only) ONLY="$2"; shift ;;
    *) echo "unknown arg $1" >&2; exit 1 ;;
  esac
  shift
done

KERNEL_US="${KERNEL_US:-2000}"
BARRIER_S="${BARRIER_S:-4}"
RUN="results/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RUN"
ln -sfn "$(basename "$RUN")" results/latest

make -s pcprobe

# ---------------------------------------------------------------- environment
{
  echo "## date";        date -Is
  echo "## uname";       uname -a
  echo "## os";          cat /etc/os-release 2>/dev/null | head -4
  echo "## nvidia-smi";  nvidia-smi 2>&1 || true
  echo "## nvidia-smi -q (compute mode, mig)"
  nvidia-smi -q 2>/dev/null | grep -iE "compute mode|mig mode|product name|driver version|cuda version" || true
  echo "## nvcc";        "${CUDA_HOME:-/usr/local/cuda}/bin/nvcc" --version 2>&1 | tail -2 || true
  echo "## profiling restriction"
  grep -i RmProfilingAdminOnly /proc/driver/nvidia/params 2>/dev/null || echo "(param not found)"
  echo "## euid";        id -u
  echo "## other GPU processes"
  nvidia-smi --query-compute-apps=pid,process_name,used_memory --format=csv 2>&1 || true
} > "$RUN/env.txt"
echo "environment -> $RUN/env.txt"

boot_ns() { python3 -c 'import time; print(time.clock_gettime_ns(time.CLOCK_BOOTTIME))'; }

# scenario NAME DURATION_S "PROC=ARGS" ...
#   PROC is a label (A, B, C); its kernel is spin_k0, k1, k2 in order.
scenario() {
  local name="$1" dur="$2"; shift 2
  if [[ -n "$ONLY" && "$name" != "$ONLY"* ]]; then return; fi
  local dir="$RUN/$name"
  mkdir -p "$dir"
  local t0=$(( $(boot_ns) + BARRIER_S * 1000000000 ))
  echo "$t0" > "$dir/t0"
  echo "== $name ($dur s)"
  local k=0 pids=() p
  for spec in "$@"; do
    local label="${spec%%=*}" args="${spec#*=}"
    echo "   $label: $args" | tee -a "$dir/cmd.txt"
    # shellcheck disable=SC2086
    ./pcprobe --name "$label" --kernel "$k" --duration-s "$dur" --start-at-ns "$t0" \
      --kernel-us "$KERNEL_US" $EXTRA $args --out "$dir/$label.jsonl" \
      2> "$dir/$label.stderr" &
    pids+=($!)
    k=$((k + 1))
  done
  local rc=0
  for p in "${pids[@]}"; do wait "$p" || rc=$?; done
  [[ $rc -ne 0 ]] && echo "   (a process exited $rc; see $dir/*.stderr)"
  sleep 1
}

NOSAMP="--enable-at-ms -1"
WHOLE="--enable-at-ms 0 --win-offset-ms 0 --win-len-ms -1 --win-period-ms 500"

group() {
  local p="$1"   # name prefix for this group
  # --- baselines -------------------------------------------------------------
  scenario "${p}S00-solo-baseline"   8  "A=$NOSAMP"
  scenario "${p}S01-pair-baseline"   8  "A=$NOSAMP" "B=$NOSAMP"
  scenario "${p}S02-solo-whole"      8  "A=$WHOLE"
  scenario "${p}S03-solo-windows"    8  "A=--enable-at-ms 0 --win-offset-ms 0 --win-len-ms 30 --win-period-ms 100"

  # --- F1: second process starts while the first is sampling -----------------
  scenario "${p}S10-overlap-A-first" 10 "A=$WHOLE" \
    "B=--enable-at-ms 2000 --win-offset-ms 3000 --win-len-ms 200 --win-period-ms 500"
  scenario "${p}S10b-overlap-B-enabled-first" 10 \
    "A=--enable-at-ms 500 --win-offset-ms 1000 --win-len-ms -1 --win-period-ms 500" \
    "B=--enable-at-ms 0 --win-offset-ms 3000 --win-len-ms 200 --win-period-ms 500"

  # --- F2: is the hardware held by Enable or by Start..Stop? -----------------
  scenario "${p}S11-enable-hold" 8 \
    "A=--enable-at-ms 0 --win-len-ms 0" \
    "B=--enable-at-ms 1000 --win-offset-ms 2000 --win-len-ms 200 --win-period-ms 500"
  scenario "${p}S12-handoff-after-stop" 12 \
    "A=--enable-at-ms 0 --win-offset-ms 0 --win-len-ms 5000 --win-period-ms 100000" \
    "B=--enable-at-ms 500 --win-offset-ms 1000 --win-len-ms 200 --win-period-ms 500"
  scenario "${p}S13-handoff-after-disable" 14 \
    "A=--enable-at-ms 0 --win-offset-ms 0 --win-len-ms 5000 --win-period-ms 100000 --disable-at-ms 9000" \
    "B=--enable-at-ms 500 --win-offset-ms 1000 --win-len-ms 200 --win-period-ms 500"

  # --- duty-cycle patterns (F1/F3 under realistic windows) -------------------
  scenario "${p}S14-tdma-2"        10 \
    "A=--enable-at-ms 0 --win-offset-ms 0  --win-len-ms 30 --win-period-ms 100" \
    "B=--enable-at-ms 0 --win-offset-ms 50 --win-len-ms 30 --win-period-ms 100"
  scenario "${p}S15-aligned-collide" 10 \
    "A=--enable-at-ms 0 --win-offset-ms 0 --win-len-ms 30 --win-period-ms 100" \
    "B=--enable-at-ms 0 --win-offset-ms 0 --win-len-ms 30 --win-period-ms 100"
  scenario "${p}S16-drift"         10 \
    "A=--enable-at-ms 0 --win-offset-ms 0 --win-len-ms 30 --win-period-ms 70" \
    "B=--enable-at-ms 0 --win-offset-ms 0 --win-len-ms 30 --win-period-ms 110"
  scenario "${p}S30-tdma-3"        10 \
    "A=--enable-at-ms 0 --win-offset-ms 0  --win-len-ms 25 --win-period-ms 100" \
    "B=--enable-at-ms 0 --win-offset-ms 33 --win-len-ms 25 --win-period-ms 100" \
    "C=--enable-at-ms 0 --win-offset-ms 66 --win-len-ms 25 --win-period-ms 100"

  # --- F5: does sampling in A slow down B? -----------------------------------
  scenario "${p}S20-impact-whole"   8 "A=$WHOLE" "B=$NOSAMP"
  scenario "${p}S21-impact-windows" 8 \
    "A=--enable-at-ms 0 --win-offset-ms 0 --win-len-ms 30 --win-period-ms 100" "B=$NOSAMP"
}

followup() {
  local p="$1"
  # Clean hand-off: B's first Start only after A has stopped (A stays enabled).
  scenario "${p}S40-clean-handoff-after-stop" 9 \
    "A=--enable-at-ms 0 --win-offset-ms 0 --win-len-ms 3000 --win-period-ms 100000" \
    "B=--enable-at-ms 500 --win-offset-ms 4000 --win-len-ms 200 --win-period-ms 500"
  # Clean hand-off after A disables.
  scenario "${p}S41-clean-handoff-after-disable" 9 \
    "A=--enable-at-ms 0 --win-offset-ms 0 --win-len-ms 3000 --win-period-ms 100000 --disable-at-ms 3500" \
    "B=--enable-at-ms 500 --win-offset-ms 4000 --win-len-ms 200 --win-period-ms 500"
  # B wedges by colliding at 1 s, A stops at 3 s, B disables+re-enables at 4 s.
  scenario "${p}S42-wedge-then-reenable" 9 \
    "A=--enable-at-ms 0 --win-offset-ms 0 --win-len-ms 3000 --win-period-ms 100000" \
    "B=--enable-at-ms 500 --win-offset-ms 1000 --win-len-ms 200 --win-period-ms 500 --reenable-at-ms 4000"
  # B wedges, then re-enables while A is still sampling.
  scenario "${p}S43-wedge-reenable-while-busy" 8 \
    "A=$WHOLE" \
    "B=--enable-at-ms 500 --win-offset-ms 1000 --win-len-ms 200 --win-period-ms 500 --reenable-at-ms 3000"
  # B wedges at 1 s; A disables at 3 s; B disables+re-enables at 4 s.
  scenario "${p}S45-wedge-holder-disables-then-reenable" 9 \
    "A=--enable-at-ms 0 --win-offset-ms 0 --win-len-ms 2500 --win-period-ms 100000 --disable-at-ms 3000" \
    "B=--enable-at-ms 500 --win-offset-ms 1000 --win-len-ms 200 --win-period-ms 500 --reenable-at-ms 4000"
  # Rotation by Enable/Disable: each holds the GPU for 400 ms then disables; the other enables 100 ms later.
  scenario "${p}S46-rotate-by-disable" 9 \
    "A=--enable-at-ms 0 --win-offset-ms 0 --win-len-ms 400 --win-period-ms 100000 --disable-at-ms 450" \
    "B=--enable-at-ms 550 --win-offset-ms 550 --win-len-ms 400 --win-period-ms 100000 --disable-at-ms 1000"
  # Repeated alternation with a generous guard band (400 ms windows, 100 ms gaps).
  scenario "${p}S44-alternate-guarded" 10 \
    "A=--enable-at-ms 0 --win-offset-ms 0   --win-len-ms 400 --win-period-ms 1000" \
    "B=--enable-at-ms 0 --win-offset-ms 500 --win-len-ms 400 --win-period-ms 1000"
}

if [[ $FOLLOWUP -eq 1 ]]; then
  EXTRA=""; followup "F-"
  EXTRA="--continuous"; followup "FC-"
  python3 summarize.py "$RUN" | tee "$RUN/summary.md"
  echo "done -> $RUN"
  exit 0
fi

EXTRA=""
group ""

EXTRA="--continuous"
group "C-"

if [[ $MPS -eq 1 ]]; then
  export CUDA_MPS_PIPE_DIRECTORY="$PWD/$RUN/mps-pipe"
  export CUDA_MPS_LOG_DIRECTORY="$PWD/$RUN/mps-log"
  mkdir -p "$CUDA_MPS_PIPE_DIRECTORY" "$CUDA_MPS_LOG_DIRECTORY"
  if nvidia-cuda-mps-control -d; then
    trap 'echo quit | nvidia-cuda-mps-control || true' EXIT
    sleep 1
    EXTRA=""
    group "M-"
    echo quit | nvidia-cuda-mps-control || true
    trap - EXIT
  else
    echo "MPS daemon failed to start; skipping MPS group" | tee "$RUN/mps-skipped.txt"
  fi
fi

python3 summarize.py "$RUN" | tee "$RUN/summary.md"
echo "done -> $RUN"
