#!/usr/bin/env bash
# Holder death: does the GPU's PC sampling claim survive the holder being killed?
set -u
cd ~/procctl-spike/pcsampling
R=results/holder-death-$(date +%H%M%S); mkdir -p $R
boot() { python3 -c 'import time;print(time.clock_gettime_ns(time.CLOCK_BOOTTIME))'; }
run() { # name mode sig
  local n=$1 mode=$2 sig=$3 d=$R/$1; mkdir -p $d
  local t0=$(( $(boot) + 4000000000 )); echo $t0 > $d/t0
  ./pcprobe --name A --kernel 0 --duration-s 10 --start-at-ns $t0 $mode \
    --enable-at-ms 0 --win-offset-ms 0 --win-len-ms -1 --win-period-ms 500 --out $d/A.jsonl 2>$d/A.stderr &
  local a=$!
  ./pcprobe --name B --kernel 1 --duration-s 10 --start-at-ns $t0 $mode \
    --enable-at-ms 500 --win-offset-ms 3000 --win-len-ms 200 --win-period-ms 500 --out $d/B.jsonl 2>$d/B.stderr &
  local b=$!
  # Kill A 2 s into the run, while it is inside its window.
  python3 -c "import time;t=$t0+2_000_000_000
while time.clock_gettime_ns(time.CLOCK_BOOTTIME)<t: time.sleep(0.001)"
  kill -$sig $a; sleep 0.3; [[ $sig == STOP ]] && { sleep 3; kill -KILL $a; }
  echo "A killed with $sig at +2s" > $d/cmd.txt
  wait $b; wait $a 2>/dev/null
}
run H1-sigkill "" KILL
run H2-sigkill-cont "--continuous" KILL
run H3-sigstop-then-kill "" STOP
python3 summarize.py $R | grep -vE '^```|^$|^\|---|^\| proc|environment|^</?details|^# '
