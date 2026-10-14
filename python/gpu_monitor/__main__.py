"""Entry point: ``python -m gpu_monitor`` (from the project root).

Wires three pieces together:

1. the Go TSDB HTTP server (``ltsdb serve``) -- reused when something is
   already listening on ``--addr``, otherwise spawned as a child process;
2. a pynvml sampler that POSTs one line-protocol batch to ``/write`` every
   ``--interval`` seconds (the engine commits each batch durably, so even a
   hard kill only ever loses the in-flight tick);
3. a live ANSI dashboard rebuilt each tick from ``/query`` results.

Data accumulates without retention in the DB file (default ``gpu.ltdb``);
delete the file to start fresh.
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

# Allow running both as ``python -m gpu_monitor`` and as a script file.
_PACKAGE_DIR = os.path.dirname(os.path.abspath(__file__))
_PY_ROOT = os.path.dirname(_PACKAGE_DIR)      # python/
_REPO_ROOT = os.path.dirname(_PY_ROOT)        # repository root (holds ltsdb.exe)
if _PY_ROOT not in sys.path:
    sys.path.insert(0, _PY_ROOT)

from gpu_monitor.collector import GPUSampler  # noqa: E402
from gpu_monitor.view import LiveView  # noqa: E402


def server_alive(addr: str) -> bool:
    try:
        urllib.request.urlopen("http://{}/ping".format(addr), timeout=0.5)
        return True
    except Exception:
        return False


def ensure_server(addr: str, db_path: str, exe: str):
    """Return a spawned Popen handle, or None when reusing a live server."""
    if server_alive(addr):
        return None
    if not os.path.exists(exe):
        sys.exit("server binary not found: {}\n"
                 "build it first:  cd {} && go build -o ltsdb.exe ./cmd/ltsdb\n"
                 "or point --exe at an existing build".format(exe, _REPO_ROOT))
    proc = subprocess.Popen(
        [exe, "serve", "--db", db_path, "--addr", addr],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    deadline = time.time() + 5.0
    while time.time() < deadline:
        if server_alive(addr):
            return proc
        if proc.poll() is not None:
            sys.exit("ltsdb serve exited with code {}".format(proc.returncode))
        time.sleep(0.1)
    proc.terminate()
    sys.exit("server did not come up at {} in time".format(addr))


def query_rows(addr: str, sql: str) -> list[dict]:
    url = "http://{}/query?q={}".format(addr, urllib.parse.quote(sql))
    req = urllib.request.Request(url, headers={"Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=5.0) as resp:
        return json.loads(resp.read()).get("rows", [])


def write_lines(addr: str, lines: str) -> None:
    req = urllib.request.Request(
        "http://{}/write".format(addr), data=lines.encode(), method="POST")
    with urllib.request.urlopen(req, timeout=5.0) as resp:
        resp.read()


def main(argv=None) -> None:
    parser = argparse.ArgumentParser(
        description="Monitor GPU memory / CUDA usage into the local TSDB "
                    "and render a live terminal dashboard.")
    parser.add_argument("--db", default=os.path.join(_PROJECT_ROOT, "gpu.ltdb"),
                        help="database file for ltsdb (default: gpu.ltdb)")
    parser.add_argument("--addr", default="127.0.0.1:8086",
                        help="server address (default: 127.0.0.1:8086)")
    parser.add_argument("--interval", type=float, default=1.0,
                        help="sampling interval in seconds (default: 1)")
    parser.add_argument("--history", type=int, default=60,
                        help="samples kept for sparklines (default: 60)")
    parser.add_argument("--exe",
                        default=os.path.join(_REPO_ROOT, "ltsdb.exe"),
                        help="path to the ltsdb server binary")
    parser.add_argument("--keep-server", action="store_true",
                        help="leave the spawned server running on exit")
    args = parser.parse_args(argv)

    os.system("")  # enable ANSI escape handling on legacy Windows consoles
    server = ensure_server(args.addr, args.db, args.exe)
    try:
        sampler = GPUSampler()
    except Exception as exc:
        sys.exit("NVML init failed: {}".format(exc))

    view = LiveView((sampler.driver, sampler.cuda, sampler.names),
                    args.db, args.addr, args.history)
    print("\x1b[2J\x1b[?25l", end="")  # clear screen, hide cursor
    try:
        while True:
            tick_start = time.time()

            # 1) sample -> /write (the engine commits inside Ingest).
            try:
                lines = sampler.sample()
                if lines:
                    write_lines(args.addr, lines)
                    if not sampler.last_error:
                        view.last_error = ""
            except Exception as exc:
                view.last_error = "write: {}".format(exc)
            if sampler.last_error:
                view.last_error = sampler.last_error

            # 2) /query -> rebuild the frame from DB state.
            try:
                measurements = {row.get("name")
                                for row in query_rows(args.addr,
                                                      "SHOW MEASUREMENTS")}
                gpu_rows = query_rows(args.addr, "SELECT * FROM gpu") \
                    if "gpu" in measurements else []
                proc_rows = query_rows(args.addr, "SELECT * FROM gpu_proc") \
                    if "gpu_proc" in measurements else []
                view.ingest(gpu_rows, proc_rows)
            except Exception as exc:
                view.last_error = "query: {}".format(exc)

            sys.stdout.write(view.render())
            time.sleep(max(0.05, args.interval - (time.time() - tick_start)))
    except KeyboardInterrupt:
        pass
    finally:
        print("\x1b[?25h\x1b[0J", end="")
        sampler.shutdown()
        if server is not None and not args.keep_server:
            server.terminate()
        print("stopped. data kept in {}".format(args.db))


if __name__ == "__main__":
    main()
