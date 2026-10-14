"""Temporary CPU-monitor demo: sample CPU -> ltsdb -> interactive dashboard.

A throwaway showcase (not part of the engine) that exercises the real Go
binary end to end, the same way ``gpu_monitor`` does:

1. spawn ``ltsdb serve`` on a free port;
2. sample total CPU load once a second (or, with ``--backfill``, write a whole
   backdated history in one pass) and POST each reading to ``/write``;
3. serve a small interactive dashboard that issues **real, time-filtered
   InfluxQL queries** against the engine -- the range buttons (``last 30s`` /
   ``1m`` / ``5m`` / ``all``) run ``SELECT * FROM cpu WHERE time > now() - ...``
   over ``GET /query`` and the panel shows the statement, row count and latency.

Windows-only: CPU load comes from WMI (``Win32_Processor.LoadPercentage``).

Usage:
    python python/examples/cpu_dashboard.py                 # live sample, then serve
    python python/examples/cpu_dashboard.py --backfill 900  # 15 min of history, then serve

Open the printed URL, or screenshot it with a headless browser.
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
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
EXE = os.path.join(ROOT, "ltsdb.exe")


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


def ingest(addr: str, args) -> str:
    """Feed the database; return a human label for the dashboard subtitle."""
    if args.backfill:
        # Backdated ingestion: one point per second covering the last
        # ``--backfill`` seconds, plus a final point "now", all in a single
        # line-protocol batch (one commit). The engine accepts arbitrary past
        # nanosecond timestamps, so the chart is full immediately and no real
        # time has to pass.
        end = time.time_ns()
        lines = []
        for i in range(args.backfill):
            ts = end - (args.backfill - i) * 1_000_000_000
            lines.append("cpu,host=local usage={}i {}".format(synth(i), ts))
        lines.append("cpu,host=local usage={}i {}".format(synth(args.backfill), end))
        write_lines(addr, "\n".join(lines) + "\n")
        return "backfilled {}s (synthetic history)".format(args.backfill)

    # Idle baseline, then a burst of synthetic load in the middle, then cooldown.
    threading.Thread(target=burn_after,
                     args=(args.samples * 0.2, args.samples * 0.5),
                     daemon=True).start()
    base = time.time_ns()
    for i in range(args.samples):
        ts = base + i * 1_000_000_000
        write_lines(addr, "cpu,host=local usage={}i {}\n".format(cpu_load(), ts))
        time.sleep(max(0.0, 1.0 - (time.time_ns() - ts) / 1e9))
    return "live sampling"


# --------------------------------------------------------------- dashboard --- #

PAGE = """<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<title>local-tsdb - CPU monitor</title>
<style>
 :root{color-scheme:dark}
 body{margin:0;background:#0d1117;color:#c9d1d9;
   font:14px/1.5 -apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif}
 .wrap{max-width:1080px;margin:0 auto;padding:32px 28px}
 h1{font-size:20px;margin:0 0 2px}
 .sub{color:#8b949e;font-size:13px;margin-bottom:20px}
 .bar{display:flex;align-items:center;gap:8px;margin-bottom:14px;flex-wrap:wrap}
 .bar .lbl{color:#8b949e;font-size:12px;margin-right:2px}
 button{background:#161b22;color:#c9d1d9;border:1px solid #30363d;border-radius:7px;
   padding:5px 12px;font:inherit;font-size:13px;cursor:pointer}
 button:hover{border-color:#58a6ff}
 button.active{background:#1f6feb;border-color:#1f6feb;color:#fff}
 .q{font-family:SFMono-Regular,Consolas,monospace;font-size:12.5px;color:#8b949e;
   background:#161b22;border:1px solid #21262d;border-radius:8px;padding:8px 12px;
   margin-bottom:18px;display:flex;justify-content:space-between;gap:16px;flex-wrap:wrap}
 .q b{color:#7ee787;font-weight:600}
 .q .meta{color:#58a6ff;white-space:nowrap}
 .cards{display:flex;gap:16px;margin-bottom:22px}
 .card{flex:1;background:#161b22;border:1px solid #21262d;border-radius:10px;
   padding:14px 16px}
 .card .k{color:#8b949e;font-size:12px;text-transform:uppercase;letter-spacing:.06em}
 .card .v{font-size:26px;font-weight:600;color:#7ee787;margin-top:4px}
 .panel{background:#161b22;border:1px solid #21262d;border-radius:10px;
   padding:18px 18px 8px}
 svg{width:100%;height:auto;display:block}
 .grid{stroke:#21262d;stroke-width:1}
 .axis{fill:#6e7681;font-size:11px}
 .line{fill:none;stroke:#58a6ff;stroke-width:2}
 .area{fill:rgba(88,166,255,.15);stroke:none}
 .foot{color:#8b949e;font-size:12px;margin-top:16px}
 code{background:#161b22;border:1px solid #21262d;border-radius:5px;padding:1px 5px;
   font-size:12px}
 .err{color:#ff7b72}
</style></head><body><div class="wrap">
 <h1>CPU Monitor</h1>
 <div class="sub">__SUBTITLE__</div>

 <div class="bar">
  <span class="lbl">time range:</span>
  <button data-r="30s">last 30s</button>
  <button data-r="1m">last 1m</button>
  <button data-r="5m">last 5m</button>
  <button data-r="all">all</button>
 </div>

 <div class="q">
  <span id="qsql">SELECT * FROM cpu</span>
  <span class="meta" id="qmeta"></span>
 </div>

 <div class="cards">
  <div class="card"><div class="k">current</div><div class="v" id="c-cur">-</div></div>
  <div class="card"><div class="k">mean</div><div class="v" id="c-mean">-</div></div>
  <div class="card"><div class="k">peak</div><div class="v" id="c-peak">-</div></div>
  <div class="card"><div class="k">rows</div><div class="v" id="c-n">-</div></div>
 </div>

 <div class="panel"><svg id="chart" viewBox="0 0 940 300"
   preserveAspectRatio="none"></svg></div>

 <div class="foot">Each range button issues a real InfluxQL query
  (<code>SELECT * FROM cpu WHERE time &gt; now() - ...</code>) over
  <code>GET /query</code>, proxied to the engine at <code>__ADDR__</code>.
  <span id="err" class="err"></span></div>
</div>
<script>
const RANGES = { "30s":"30s", "1m":"1m", "5m":"5m", "all":"" };
let active = "1m";

function sqlFor(id){
  const d = RANGES[id];
  return "SELECT * FROM cpu" + (d ? " WHERE time > now() - " + d : "");
}

function drawChart(vals){
  const W=940,H=300,pad=36, svg=document.getElementById("chart");
  if(!vals.length){ svg.innerHTML='<text x="470" y="150" class="axis" text-anchor="middle">no rows in range</text>'; return; }
  const n=vals.length, iw=W-2*pad, ih=H-2*pad;
  const X=i=> pad + (n===1 ? iw/2 : iw*i/(n-1));
  const Y=v=> pad + ih*(1-v/100);
  const line = "M " + vals.map((v,i)=>X(i).toFixed(1)+" "+Y(v).toFixed(1)).join(" L ");
  const area = line + " L "+X(n-1).toFixed(1)+" "+(pad+ih)+" L "+X(0).toFixed(1)+" "+(pad+ih)+" Z";
  let grid="";
  for(const g of [0,25,50,75,100]){
    const y=Y(g).toFixed(1);
    grid += '<line x1="'+pad+'" y1="'+y+'" x2="'+(W-pad)+'" y2="'+y+'" class="grid"/>'
          + '<text x="'+(W-pad+6)+'" y="'+y+'" class="axis">'+g+'%</text>';
  }
  svg.innerHTML = grid + '<path d="'+area+'" class="area"/><path d="'+line+'" class="line"/>';
}

function setButtons(){
  document.querySelectorAll("button[data-r]").forEach(b=>
    b.classList.toggle("active", b.dataset.r===active));
}

async function load(id){
  active = id; setButtons();
  const sql = sqlFor(id);
  document.getElementById("qsql").textContent = sql;
  document.getElementById("qmeta").textContent = "querying...";
  document.getElementById("err").textContent = "";
  const t0 = performance.now();
  try{
    const r = await fetch("/query?q=" + encodeURIComponent(sql),
                          {headers:{"Accept":"application/json"}});
    const data = await r.json();
    const ms = performance.now() - t0;
    const rows = (data.rows||[]).slice().sort((a,b)=> (+a.time)-(+b.time));
    const vals = rows.map(x=> +x.usage);
    document.getElementById("qmeta").textContent =
      rows.length + " rows · " + ms.toFixed(1) + " ms";
    document.getElementById("c-cur").textContent  = vals.length ? vals[vals.length-1]+"%" : "-";
    document.getElementById("c-mean").textContent = vals.length ? (vals.reduce((a,b)=>a+b,0)/vals.length).toFixed(1)+"%" : "-";
    document.getElementById("c-peak").textContent = vals.length ? Math.max(...vals)+"%" : "-";
    document.getElementById("c-n").textContent    = rows.length;
    drawChart(vals);
  }catch(e){
    document.getElementById("err").textContent = "query failed: " + e;
  }
}

document.querySelectorAll("button[data-r]").forEach(b=>
  b.addEventListener("click", ()=>load(b.dataset.r)));
load(active);
</script></body></html>
"""


class _Handler(BaseHTTPRequestHandler):
    engine_addr = ""
    page = b""

    def _send(self, code: int, body: bytes, ctype: str) -> None:
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):  # noqa: N802 (stdlib naming)
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path == "/query":
            target = "http://{}/query?{}".format(self.engine_addr, parsed.query)
            req = urllib.request.Request(target, headers={"Accept": "application/json"})
            try:
                with urllib.request.urlopen(req, timeout=10) as resp:
                    self._send(200, resp.read(), "application/json")
            except Exception as exc:  # surface engine errors to the page
                self._send(502, json.dumps({"error": str(exc)}).encode(),
                           "application/json")
            return
        self._send(200, self.page, "text/html; charset=utf-8")

    def log_message(self, *args):  # keep the console quiet
        pass


def serve(port: int, engine_addr: str, subtitle: str) -> None:
    _Handler.engine_addr = engine_addr
    _Handler.page = PAGE.replace("__ADDR__", engine_addr) \
                        .replace("__SUBTITLE__", subtitle).encode("utf-8")
    httpd = ThreadingHTTPServer(("127.0.0.1", port), _Handler)
    print("dashboard: http://127.0.0.1:{}/  (Ctrl+C to stop)".format(port))
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        httpd.shutdown()


# ------------------------------------------------------------------- main --- #

def main(argv=None) -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--samples", type=int, default=30)
    ap.add_argument("--backfill", type=int, default=0, metavar="SECONDS",
                    help="write SECONDS of (synthetic) history ending at now in "
                         "a single pass, instead of sampling live -- shows that "
                         "timestamps may be backdated")
    ap.add_argument("--addr", default="127.0.0.1:18087", help="ltsdb HTTP address")
    ap.add_argument("--port", type=int, default=8902, help="dashboard HTTP port")
    args = ap.parse_args(argv)

    if not os.path.exists(EXE):
        sys.exit("build the engine first: go build -o ltsdb.exe ./cmd/ltsdb")

    db = os.path.join(os.path.dirname(os.path.abspath(__file__)), "cpu_demo.ltsdb")
    for suffix in ("", ".wal"):
        if os.path.exists(db + suffix):
            os.remove(db + suffix)

    engine = start_server(args.addr, db)
    try:
        mode = ingest(args.addr, args)
        subtitle = "host=local  -  {}  -  db={}  -  {}".format(
            mode, os.path.basename(db), time.strftime("%Y-%m-%d %H:%M"))
        serve(args.port, args.addr, subtitle)
    finally:
        if engine is not None:
            engine.terminate()


if __name__ == "__main__":
    main()
