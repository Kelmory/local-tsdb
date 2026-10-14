"""ANSI live terminal rendering for the GPU monitor.

The view keeps a bounded rolling window per GPU (fed from ``SELECT * FROM
gpu`` result rows) and the newest row per (gpu, pid) from ``cuda_proc``, so
each frame is rebuilt from DB state rather than from the sampler directly.
"""
from __future__ import annotations

import time
from collections import deque

SPARK_CHARS = "▁▂▃▄▅▆▇█"
BAR_WIDTH = 26


def _num(value):
    """Row cell -> number, tolerating None / string encodings."""
    if value is None:
        return None
    try:
        return int(value)
    except (TypeError, ValueError):
        try:
            return float(value)
        except (TypeError, ValueError):
            return None


def _spark(values, width: int = 44) -> str:
    recent = list(values)[-width:]
    if not recent:
        return "(no data)"
    lo, hi = min(recent), max(recent)
    if hi <= lo:
        hi = lo + 1
    span = hi - lo
    return "".join(
        SPARK_CHARS[min(len(SPARK_CHARS) - 1,
                        int((v - lo) * (len(SPARK_CHARS) - 1) / span))]
        for v in recent)


def _bar(frac: float, width: int = BAR_WIDTH) -> str:
    frac = max(0.0, min(1.0, frac))
    filled = int(round(frac * width))
    return "|" + "█" * filled + "░" * (width - filled) + "|"


class LiveView:
    """Builds one self-refreshing ANSI frame from DB query rows."""

    def __init__(self, static_info, db_path: str, addr: str,
                 history: int = 60) -> None:
        self.driver, self.cuda, self.names = static_info
        self.db_path = db_path
        self.addr = addr
        self.history = history
        self.series: dict[int, deque] = {}     # gpu index -> sample dicts
        self._last_ts: dict[int, int] = {}     # gpu index -> newest ts ingested
        self.procs: dict[tuple[int, int], dict] = {}  # (gpu, pid) -> latest
        self.ticks = 0
        self.last_error = ""

    # -- ingest -------------------------------------------------------------- #

    def ingest(self, gpu_rows: list[dict], proc_rows: list[dict]) -> None:
        """Fold /query result rows into the rolling window."""
        for row in gpu_rows:
            gpu = int(_num(row.get("gpu")) or 0)
            ts = int(_num(row.get("time")) or 0)
            if ts <= self._last_ts.get(gpu, -1):
                continue  # already in the window (full history is re-read)
            self._last_ts[gpu] = ts
            self.series.setdefault(
                gpu, deque(maxlen=self.history)).append({
                    "ts": ts,
                    "mem_used": _num(row.get("mem_used")),
                    "mem_total": _num(row.get("mem_total")),
                    "util": _num(row.get("util_gpu")),
                    "temp": _num(row.get("temp")),
                    "power": _num(row.get("power")),
                })
        for row in proc_rows:
            key = (int(_num(row.get("gpu")) or 0),
                   int(_num(row.get("pid")) or 0))
            ts = int(_num(row.get("time")) or 0)
            current = self.procs.get(key)
            if current is None or ts >= current["ts"]:
                self.procs[key] = {
                    "proc": str(row.get("proc") or ""),
                    "kind": str(row.get("kind") or ""),
                    "mem": int(_num(row.get("mem")) or 0),
                    "ts": ts,
                }
        self.ticks += 1

    # -- render ---------------------------------------------------------------- #

    def render(self) -> str:
        out = [
            "\x1b[1m GPU Monitor \x1b[0m  db={}  server={}".format(
                self.db_path, self.addr),
            "driver {}  CUDA {}  tick #{}  {}".format(
                self.driver, self.cuda, self.ticks,
                time.strftime("%H:%M:%S")),
        ]
        if self.last_error:
            out.append("\x1b[33m last error: {}\x1b[0m".format(
                self.last_error))
        out.append("")
        for gpu, name in enumerate(self.names):
            out.append("\x1b[1mGPU{}  {}\x1b[0m".format(gpu, name))
            out.extend(self._gpu_block(gpu))
            out.append("")
        out.append("Ctrl+C 退出")
        # Move home, draw the frame, clear the remainder of the screen.
        return "\x1b[H" + "\n".join(out) + "\n\x1b[0J"

    def _gpu_block(self, gpu: int) -> list[str]:
        window = self.series.get(gpu)
        if not window:
            return ["  (等待采样...)"]
        current = window[-1]
        used = current["mem_used"] or 0
        total = current["mem_total"] or 0
        frac = used / total if total else 0.0
        lines = [
            "  mem {} {:.1f}%  {:,} / {:,} MiB".format(
                _bar(frac), frac * 100, used, total),
        ]
        mems = [s["mem_used"] or 0 for s in window]
        utils = [s["util"] or 0 for s in window]
        lines.append("  mem  history {}  min {:,} | now {:,} | max {:,} MiB"
                     .format(_spark(mems), min(mems), used, max(mems)))
        lines.append("  util history {}  min {}% | now {}% | max {}%"
                     .format(_spark(utils), min(utils),
                             current["util"] or 0, max(utils)))
        extras = []
        if current["temp"] is not None:
            extras.append("temp {}°C".format(current["temp"]))
        if current["power"] is not None:
            extras.append("power {:.1f} W".format(current["power"]))
        if extras:
            lines.append("  " + "   ".join(extras))

        procs = [(pid, info) for (g, pid), info in sorted(self.procs.items())
                 if g == gpu]
        if procs:
            lines.append("  GPU processes:")
            lines.append("    {:>8}  {:<6} {:<30} {:>10}".format(
                "PID", "KIND", "NAME", "MEM(MiB)"))
            for pid, info in procs:
                lines.append("    {:>8}  {:<6} {:<30} {:>10,}".format(
                    pid, info["kind"][:6], info["proc"][:30], info["mem"]))
        else:
            lines.append("  GPU processes: (none)")
        return lines
