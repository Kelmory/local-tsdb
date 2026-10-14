"""Temporary CPU-monitor demo: sample CPU -> ltsdb -> HTML dashboard.

A throwaway showcase (not part of the engine) that exercises the real Go
binary end to end, the same way ``gpu_monitor`` does:

1. spawn ``ltsdb serve`` on a free port;
2. sample total CPU load once a second for ~30s, POST each reading to
   ``/write`` as line protocol (a synthetic load is ramped up mid-run so the
   curve is interesting);
3. read the series back from ``/query`` and render a self-contained HTML
   dashboard with an SVG chart, written to ``docs/demo/cpu_dashboard.html``.

Windows-only: CPU load comes from WMI (``Win32_Processor.LoadPercentage``).

Usage:
    python docs/demo/cpu_dashboard.py [--samples 30] [--out docs/demo/cpu_dashboard.html]
"""
from __future__ import annotations

import argparse
import json
import math
import os
import subprocess
import sys
import threading
import time
import urllib.parse
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
EXE = os.path.join(ROOT, "go", "ltsdb.exe")


# ---------------------------------------------------------------- sampling --- #

def cpu_load() -> int:
    """Total CPU load percentage via WMI (Windows)."""
    out = subprocess.run(
        ["powershell", "-NoProfile", "-Command",
         "(Get-CimInstance Win32_Processor | "
         "Measure-Object -Property LoadPercentage -Average).Average"],
        capture_output=True, text=True).stdout.strip()
    try:
        return max(0, min(100, int(round(float(out)))))
    except ValueError:
        return 0


def burn(duration: float) -> None:
    """Spin ``ncpu`` Python loops for ``duration`` seconds (background load)."""
    def spin():
        end = time.time() + duration
        while time.time() < end:
            pass

    threads = [threading.Thread(target=spin, daemon=True)
               for _ in range(os.cpu_count() or 4)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()


def burn_after(delay: float, duration: float) -> None:
    """Sleep, then generate background load (used to shape the demo curve)."""
    time.sleep(delay)
    burn(duration)


def synth(i: int) -> int:
    """Deterministic, plausible-looking load percentage (for --backfill)."""
    v = 18 + 22 * abs(math.sin(i / 7.0)) + 14 * abs(math.sin(i / 2.3))
    return max(0, min(100, int(v)))


# ------------------------------------------------------------ ltsdb server --- #

def server_alive(addr: str) -> bool:
    try:
        urllib.request.urlopen("http://{}/ping".format(addr), timeout=0.5)
        return True
    except Exception:
        return False


def start_server(addr: str, db: str):
    if server_alive(addr):
        return None
    proc = subprocess.Popen([EXE, "serve", "--db", db, "--addr", addr],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for _ in range(50):
        if server_alive(addr):
            return proc
        time.sleep(0.1)
    raise SystemExit("ltsdb serve did not come up at " + addr)


def write_lines(addr: str, lines: str) -> None:
    req = urllib.request.Request("http://{}/write".format(addr),
                                 data=lines.encode(), method="POST")
    urllib.request.urlopen(req, timeout=5.0).read()


def query(addr: str, sql: str) -> list[dict]:
    url = "http://{}/query?q={}".format(addr, urllib.parse.quote(sql))
    req = urllib.request.Request(url, headers={"Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=5.0) as resp:
        return json.loads(resp.read())["rows"]


# ----------------------------------------------------------------- render --- #

def _area_path(points, w, h, pad):
    inner_w, inner_h = w - pad * 2, h - pad * 2
    n = len(points)
    xy = [(pad + inner_w * i / (n - 1), pad + inner_h * (1 - v / 100.0))
          for i, v in enumerate(points)]
    line = "M " + " L ".join("{:.1f} {:.1f}".format(x, y) for x, y in xy)
    area = line + " L {:.1f} {:.1f} L {:.1f} {:.1f} Z".format(
        xy[-1][0], pad + inner_h, xy[0][0], pad + inner_h)
    return line, area, xy


def render_html(points: list[int], meta: dict) -> str:
    w, h, pad = 940, 300, 36
    line, area, xy = _area_path(points, w, h, pad)
    gridlines = "".join(
        '<line x1="{p}" y1="{y:.1f}" x2="{x2}" y2="{y:.1f}" class="grid"/>'
        '<text x="{tx}" y="{y:.1f}" class="axis">{v}%</text>'.format(
            p=pad, y=pad + (h - 2 * pad) * (1 - v / 100.0), x2=w - pad,
            tx=w - pad + 6, v=v)
        for v in (0, 25, 50, 75, 100))
    cur, mean = points[-1], sum(points) / len(points)
    dots = "".join('<circle cx="{:.1f}" cy="{:.1f}" r="2.4" class="dot"/>'.format(x, y)
                   for x, y in xy)
    return """<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<title>local-tsdb - CPU monitor</title>
<style>
 :root{{color-scheme:dark}}
 body{{margin:0;background:#0d1117;color:#c9d1d9;
   font:14px/1.5 -apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif}}
 .wrap{{max-width:1080px;margin:0 auto;padding:32px 28px}}
 h1{{font-size:20px;margin:0 0 2px}}
 .sub{{color:#8b949e;font-size:13px;margin-bottom:22px}}
 .cards{{display:flex;gap:16px;margin-bottom:22px}}
 .card{{flex:1;background:#161b22;border:1px solid #21262d;border-radius:10px;
   padding:14px 16px}}
 .card .k{{color:#8b949e;font-size:12px;text-transform:uppercase;
   letter-spacing:.06em}}
 .card .v{{font-size:26px;font-weight:600;color:#7ee787;margin-top:4px}}
 .panel{{background:#161b22;border:1px solid #21262d;border-radius:10px;
   padding:18px 18px 8px}}
 svg{{width:100%;height:auto;display:block}}
 .grid{{stroke:#21262d;stroke-width:1}}
 .axis{{fill:#6e7681;font-size:11px}}
 .line{{fill:none;stroke:#58a6ff;stroke-width:2}}
 .area{{fill:rgba(88,166,255,.15);stroke:none}}
 .dot{{fill:#58a6ff}}
 .foot{{color:#8b949e;font-size:12px;margin-top:16px}}
 code{{background:#161b22;border:1px solid #21262d;border-radius:5px;
   padding:1px 5px;font-size:12px}}
</style></head><body><div class="wrap">
 <h1>CPU Monitor</h1>
 <div class="sub">{subtitle}</div>
 <div class="cards">
  <div class="card"><div class="k">current</div><div class="v">{cur}%</div></div>
  <div class="card"><div class="k">mean</div><div class="v">{mean:.1f}%</div></div>
  <div class="card"><div class="k">peak</div><div class="v">{peak}%</div></div>
  <div class="card"><div class="k">samples</div><div class="v">{n}</div></div>
 </div>
 <div class="panel">
  <svg viewBox="0 0 {w} {h}" preserveAspectRatio="none">
   {grid}
   <path d="{area}" class="area"/>
   <path d="{line}" class="line"/>
   {dots}
  </svg>
 </div>
 <div class="foot">Stored and read back from <code>local-tsdb</code> over
  <code>POST /write</code> + <code>GET /query</code> (line protocol, nanosecond
  timestamps). Total CPU load sampled from WMI.</div>
</div></body></html>
""".format(subtitle=meta["subtitle"], cur=cur, mean=mean, peak=max(points),
           n=len(points), w=w, h=h, grid=gridlines, area=area, line=line, dots=dots)


# ------------------------------------------------------------------- main --- #

def main(argv=None) -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--samples", type=int, default=30)
    ap.add_argument("--backfill", type=int, default=0, metavar="SECONDS",
                    help="write SECONDS of (synthetic) history ending at now in "
                         "a single pass, instead of sampling live -- shows that "
                         "timestamps may be backdated")
    ap.add_argument("--addr", default="127.0.0.1:18087")
    ap.add_argument("--out", default=os.path.join(ROOT, "docs", "demo",
                                                  "cpu_dashboard.html"))
    args = ap.parse_args(argv)

    if not os.path.exists(EXE):
        sys.exit("build the engine first: cd go && go build -o ltsdb.exe ./cmd/ltsdb")

    db = os.path.join(ROOT, "docs", "demo", "_cpu_demo.ltsdb")
    for suffix in ("", ".wal"):
        if os.path.exists(db + suffix):
            os.remove(db + suffix)

    server = start_server(args.addr, db)
    if args.backfill:
        # Backdated ingestion: one sample per second covering the last
        # ``--backfill`` seconds, all written in a single pass -- the engine
        # accepts arbitrary (past) nanosecond timestamps, so no waiting is
        # needed and the chart is full immediately.
        end = time.time_ns()
        for i in range(args.backfill):
            ts = end - (args.backfill - 1 - i) * 1_000_000_000
            write_lines(args.addr,
                        "cpu,host=local usage={}i {}\n".format(synth(i), ts))
        tag = "backfilled {}s (synthetic history)".format(args.backfill)
    else:
        # Idle baseline, then a burst of synthetic load in the middle, then cooldown.
        threading.Thread(target=burn_after,
                         args=(args.samples * 0.2, args.samples * 0.5),
                         daemon=True).start()
        base = time.time_ns()
        for i in range(args.samples):
            ts = base + i * 1_000_000_000
            write_lines(args.addr,
                        "cpu,host=local usage={}i {}\n".format(cpu_load(), ts))
            time.sleep(max(0.0, 1.0 - (time.time_ns() - ts) / 1e9))
        tag = "live sampling"

    rows = query(args.addr, "SELECT * FROM cpu")
    points = [int(r["usage"]) for r in sorted(rows, key=lambda r: int(r["time"]))]
    mean = sum(points) / len(points) if points else 0
    html = render_html(points, {
        "subtitle": "host=local  -  {} samples  -  mean {:.1f}%  -  {}  -  "
                    "{}".format(len(points), mean, tag,
                                time.strftime("%Y-%m-%d %H:%M"))})
    with open(args.out, "w", encoding="utf-8") as fh:
        fh.write(html)
    print("wrote {} ({} points from ltsdb)".format(args.out, len(points)))

    if server is not None:
        server.terminate()


if __name__ == "__main__":
    main()
