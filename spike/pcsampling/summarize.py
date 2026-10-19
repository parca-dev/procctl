#!/usr/bin/env python3
"""Summarize a pcprobe spike run directory into markdown tables."""
import json
import os
import statistics
import sys
from collections import Counter, defaultdict


def load(path):
    out = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line.startswith("{"):
                try:
                    out.append(json.loads(line))
                except json.JSONDecodeError:
                    pass
    return out


def pct(xs, p):
    if not xs:
        return 0
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(round(p / 100 * (len(xs) - 1))))]


def us(ns):
    return f"{ns / 1000:.0f}"


def analyze_proc(evs, t0):
    p = {"name": evs[0]["name"] if evs else "?", "windows": [], "attempts": []}
    open_win = None
    for e in evs:
        ev = e["ev"]
        if ev == "start":
            p["start"] = e
        elif ev == "enable":
            p["enable"] = e
        elif ev == "cfg" and e.get("step") == "set_config":
            p["cfg"] = e
        elif ev == "win_start":
            p["attempts"].append(((e["ts"] - t0) / 1e6, e["rc"], e["err"], e["lat_ns"]))
            if e["rc"] == 0:
                open_win = e
        elif ev == "win_stop" and open_win is not None:
            p["windows"].append({"t_start": open_win["ts"], "t_stop": e["ts"], **e})
            open_win = None
        elif ev == "disable":
            p["disable"] = e
        elif ev == "reenable_disable":
            p["reenable"] = e
        elif ev == "end":
            p["end"] = e
        elif ev == "fatal":
            p["fatal"] = e
        elif ev == "getdata_err":
            p.setdefault("getdata_errs", Counter())[e["err"]] += 1
    p["tput"] = [e for e in evs if e["ev"] == "tput"]
    return p


def overlaps(a, b):
    return a["t_start"] < b["t_stop"] and b["t_start"] < a["t_stop"]


def scenario_report(sdir):
    name = os.path.basename(sdir)
    t0 = int(open(os.path.join(sdir, "t0")).read().strip())
    procs = []
    for fn in sorted(os.listdir(sdir)):
        if fn.endswith(".jsonl"):
            evs = load(os.path.join(sdir, fn))
            if evs:
                procs.append(analyze_proc(evs, t0))
    lines = [f"### {name}", ""]
    cmd = os.path.join(sdir, "cmd.txt")
    if os.path.exists(cmd):
        lines += ["```", open(cmd).read().rstrip(), "```", ""]

    lines.append("| proc | enable | starts ok/fail | start rc | start lat µs ok p50/max | "
                 "start lat µs fail p50/max | samples | samples/win-ms | foreign samples | "
                 "dropped | getdata errs | launches/s | kern ms (all / in-win) |")
    lines.append("|" + "---|" * 13)
    for p in procs:
        if "fatal" in p:
            lines.append(f"| {p['name']} | FATAL {p['fatal'].get('err')} |" + " |" * 11)
            continue
        end = p.get("end", {})
        en = p.get("enable")
        en_s = "-" if en is None else ("ok" if en["rc"] == 0 else f"{en['rc']} {en['err']}")
        ok_lat = [a[3] for a in p["attempts"] if a[1] == 0]
        bad_lat = [a[3] for a in p["attempts"] if a[1] != 0]
        rcs = Counter(f"{a[1]}:{a[2]}" for a in p["attempts"])
        win_ms = sum((w["t_stop"] - w["t_start"]) / 1e6 for w in p["windows"])
        own = end.get("own_kernel", "")
        bf = end.get("by_func", {})
        foreign = sum(v for k, v in bf.items() if k != own)
        tp = [t for t in p["tput"] if t["secs"] > 0.05]
        lps = statistics.mean(t["lps"] for t in tp) if tp else 0
        win_k = [t["win_kern_ms_mean"] for t in tp if t["win_launches"]]
        lines.append(
            f"| {p['name']} | {en_s} | {end.get('starts_ok', 0)}/{end.get('starts_fail', 0)} | "
            f"{', '.join(f'{k}×{v}' for k, v in rcs.items()) or '-'} | "
            f"{us(pct(ok_lat, 50))}/{us(max(ok_lat, default=0))} | "
            f"{us(pct(bad_lat, 50))}/{us(max(bad_lat, default=0))} | "
            f"{end.get('samples', 0)} | "
            f"{(end.get('samples', 0) / win_ms) if win_ms else 0:.1f} | "
            f"{foreign}{' ' + json.dumps({k: v for k, v in bf.items() if k != own}) if foreign else ''} | "
            f"{end.get('dropped', 0)} | {end.get('getdata_errs', 0)} | {lps:.0f} | "
            f"{end.get('kern_ms_mean', 0):.3f} / {statistics.mean(win_k) if win_k else 0:.3f} |")
    lines.append("")

    # First successful Start per process (handoff scenarios).
    firsts = []
    for p in procs:
        ok = [a for a in p["attempts"] if a[1] == 0]
        fails = [a for a in p["attempts"] if a[1] != 0]
        if p["attempts"]:
            firsts.append(f"{p['name']}: first ok start at "
                          f"{ok[0][0]:.0f} ms" if ok else f"{p['name']}: never started")
            if fails:
                firsts[-1] += f"; failures from {fails[0][0]:.0f} to {fails[-1][0]:.0f} ms"
        if "disable" in p:
            firsts.append(f"{p['name']}: disable at {(p['disable']['ts'] - t0) / 1e6:.0f} ms "
                          f"rc={p['disable']['rc']}")
        if "reenable" in p:
            okafter = [a for a in p["attempts"] if a[1] == 0 and a[0] > (p["reenable"]["ts"] - t0) / 1e6]
            firsts.append(f"{p['name']}: re-enable at {(p['reenable']['ts'] - t0) / 1e6:.0f} ms "
                          f"(disable rc={p['reenable']['rc']}, {p['reenable']['lat_ns'] / 1e6:.1f} ms), "
                          f"ok starts after it: {len(okafter)}")
    if firsts:
        lines += ["Timeline: " + "; ".join(firsts), ""]

    # Concurrent successful windows across processes.
    if len(procs) >= 2:
        pairs = []
        for i in range(len(procs)):
            for j in range(i + 1, len(procs)):
                a, b = procs[i], procs[j]
                n = sum(1 for wa in a["windows"] for wb in b["windows"] if overlaps(wa, wb))
                pairs.append(f"{a['name']}∩{b['name']}={n}")
        # Yield of overlapped vs isolated windows.
        ov, iso = [], []
        for i, p in enumerate(procs):
            others = [w for j, q in enumerate(procs) if j != i for w in q["windows"]]
            for w in p["windows"]:
                ms = (w["t_stop"] - w["t_start"]) / 1e6
                if ms <= 0:
                    continue
                (ov if any(overlaps(w, o) for o in others) else iso).append(w["samples"] / ms)
        lines.append(f"Concurrently-open successful windows: {', '.join(pairs)}. "
                     f"Yield samples/ms: overlapped mean "
                     f"{statistics.mean(ov) if ov else 0:.1f} (n={len(ov)}), isolated mean "
                     f"{statistics.mean(iso) if iso else 0:.1f} (n={len(iso)})")
        lines.append("")
    for p in procs:
        if p.get("getdata_errs"):
            lines.append(f"{p['name']} GetData errors: {dict(p['getdata_errs'])}")
    return lines, procs


def main():
    run = sys.argv[1] if len(sys.argv) > 1 else "results/latest"
    out = [f"# PC sampling spike: {run}", ""]
    env = os.path.join(run, "env.txt")
    if os.path.exists(env):
        out += ["<details><summary>environment</summary>", "", "```",
                open(env).read().rstrip(), "```", "</details>", ""]
    for d in sorted(os.listdir(run)):
        sdir = os.path.join(run, d)
        if os.path.isdir(sdir) and os.path.exists(os.path.join(sdir, "t0")):
            lines, _ = scenario_report(sdir)
            out += lines
    print("\n".join(out))


if __name__ == "__main__":
    main()
