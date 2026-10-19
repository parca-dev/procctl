#!/usr/bin/env bash
# Off-thread release tests. A = ctlthread (holder, released by its control
# thread at 2 s); B = pcprobe trying 200 ms windows every 500 ms from 3 s.
# B succeeding proves the GPU was really released.
set -u
cd "$(dirname "$0")"
make -s pcprobe ctlthread
R=results/ctl-$(date +%H%M%S); mkdir -p "$R"
echo "GPU procs: $(nvidia-smi --query-compute-apps=pid --format=csv,noheader | tr '\n' ' ')" | tee "$R/env.txt"
boot() { python3 -c 'import time;print(time.clock_gettime_ns(time.CLOCK_BOOTTIME))'; }

pair() { # name -- ctlthread args
  local n=$1; shift
  local d=$R/$n; mkdir -p "$d"
  local t0=$(( $(boot) + 4000000000 )); echo "$t0" > "$d/t0"
  echo "A: ctlthread $*" > "$d/cmd.txt"
  ./ctlthread --test release --name A --duration-s 8 --start-at-ns "$t0" "$@" --out "$d/A.jsonl" 2> "$d/A.stderr" &
  local a=$!
  ./pcprobe --name B --kernel 1 --duration-s 8 --start-at-ns "$t0" --enable-at-ms 500 \
    --win-offset-ms 3000 --win-len-ms 200 --win-period-ms 500 --out "$d/B.jsonl" 2> "$d/B.stderr" &
  local b=$!
  wait $a; echo "A exit $?" >> "$d/cmd.txt"; wait $b
  echo "== $n"
  grep -h '"ctl_release"\|"ctl_sync"\|"ev":"end"' "$d/A.jsonl" | sed 's/^/  A /' | cut -c1-400
  grep -h '"ev":"end"' "$d/B.jsonl" | python3 -c 'import json,sys
for l in sys.stdin:
  e=json.loads(l); print("  B starts ok/fail=%s/%s rc=%s samples=%s getdata_errs=%s" % (e["starts_ok"],e["starts_fail"],e["start_rc"],e["samples"],e["getdata_errs"]))'
  [[ -s "$d/A.stderr" ]] && { echo "  A stderr:"; head -5 "$d/A.stderr"; }
}

pair T1a-release-while-busy        --release-at-ms 2000
pair T1b-release-while-idle        --release-at-ms 2000 --idle-at-ms 1000
pair T1c-disable-without-stop-busy --release-at-ms 2000 --no-stop
pair T1d-sync-then-release-busy    --release-at-ms 2000 --sync
pair T1e-disable-without-stop-idle --release-at-ms 2000 --idle-at-ms 1000 --no-stop

echo "== T2-flush-period (busy 1 s, then idle; flush period 100 ms)"
./ctlthread --test flush --name F --duration-s 5 --idle-at-ms 1000 --flush-ms 100 --out "$R/T2.jsonl" 2> "$R/T2.stderr"
python3 - "$R/T2.jsonl" <<'EOF'
import json, sys
evs = [json.loads(l) for l in open(sys.argv[1]) if l.startswith("{")]
main_tid = next(e["tid"] for e in evs if e["ev"] == "start")
idle = next((e["ts"] for e in evs if e["ev"] == "app_idle"), None)
for e in evs:
    if e["ev"] in ("register", "activity_enable", "flush_period"):
        print(f"  {e['ev']}: rc={e['rc']} {e['err']}")
bc = [e for e in evs if e["ev"] == "buf_completed"]
busy = [e for e in bc if idle is None or e["ts"] < idle]
quiet = [e for e in bc if idle is not None and e["ts"] >= idle]
print(f"  bufferCompleted: {len(busy)} while busy, {len(quiet)} while idle")
print(f"  threads: {sorted(set(e['tid'] for e in bc))} (main tid {main_tid})")
print(f"  idle-phase calls with 0 records: {sum(1 for e in quiet if e['records'] == 0)}")
for e in quiet[:5]:
    print(f"    at {e['at_ms']} ms valid={e['valid']} records={e['records']} tid={e['tid']}")
EOF
echo "results -> $R"
