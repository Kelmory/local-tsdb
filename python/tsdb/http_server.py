"""Standard TSDB HTTP interface (InfluxDB-compatible), stdlib only.

Exposes the classic InfluxDB HTTP API surface over :mod:`http.server`:

* ``GET  /ping``   -- liveness probe, empty ``204``;
* ``GET  /health`` -- ``200 OK``;
* ``POST /write``  -- ingest a body of *line protocol*; optional
  ``?precision=s|m|u|ns`` (default ``ns``) selects the unit of any explicit
  timestamps, exactly like InfluxDB;
* ``GET/POST /query`` -- run an InfluxQL-subset query (see :mod:`tsdb.sql`);
  responds with InfluxDB-style CSV by default, or JSON when the client sends
  ``Accept: application/json``.

The server is single-process and guards all database access with one lock; it
is intended for local tooling / demos rather than multi-client serving.
"""
from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Optional
from urllib.parse import parse_qs, urlparse

from .model import Point
from .protocol import LineProtocolError, parse_line
from .sql import SqlError, SQLExecutor

_PRECISION_NS = {"s": 10**9, "m": 60 * 10**9, "u": 10**3, "ns": 1}


def ingest(db, text: str, precision: str = "ns") -> int:
    """Parse ``text`` as line protocol and buffer + commit it into ``db``.

    Returns the number of points written. Timestamps are interpreted in the
    requested unit (InfluxDB semantics) and normalised to nanoseconds.
    """
    if precision not in _PRECISION_NS:
        raise ValueError("unknown precision {!r}".format(precision))
    factor = _PRECISION_NS[precision]
    count = 0
    for raw in text.splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        p = parse_line(line)
        if p.timestamp:
            p = Point(p.measurement, dict(p.tags), dict(p.fields), p.timestamp * factor)
        db.write([p])
        count += 1
    db.commit()
    return count


class _Handler(BaseHTTPRequestHandler):
    """Request handler; the live DB + lock are attached to ``self.server``."""

    def log_message(self, fmt, *args):  # keep the demo quiet
        pass

    # -- helpers ------------------------------------------------------------ #
    @property
    def db(self):
        return self.server.tsdb

    def _send(self, code: int, body: bytes = b"", ctype: str = "text/plain; charset=utf-8") -> None:
        self.send_response(code)
        if body:
            self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if body:
            self.wfile.write(body)

    def _error(self, code: int, message: str) -> None:
        self._send(code, json.dumps({"error": message}).encode(), "application/json")

    def _query_params(self) -> dict:
        return {k: v[0] for k, v in parse_qs(urlparse(self.path).query).items()}

    # -- routes ------------------------------------------------------------- #
    def do_GET(self):
        try:
            path = urlparse(self.path).path.rstrip("/") or "/"
            if path == "/ping":
                self._send(204)
            elif path == "/health":
                self._send(200, b"OK")
            elif path == "/":
                self._send(200, json.dumps({"name": "local-tsdb", "version": 1}).encode(),
                           "application/json")
            elif path == "/query":
                self._handle_query()
            else:
                self._error(404, "no such endpoint")
        except Exception as e:  # never drop the connection without a response
            self._error(500, str(e))

    def do_POST(self):
        try:
            path = urlparse(self.path).path.rstrip("/") or "/"
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length) if length else b""
            if path == "/write":
                self._handle_write(body)
            elif path == "/query":
                # InfluxDB accepts the query in the URL *or* the POST body.
                params = self._query_params()
                if "q" not in params and body:
                    form = {k: v[0] for k, v in parse_qs(body.decode("utf-8", "replace")).items()}
                    params.update(form)
                self._handle_query(params=params)
            else:
                self._error(404, "no such endpoint")
        except Exception as e:  # never drop the connection without a response
            self._error(500, str(e))

    def _handle_write(self, body: bytes):
        try:
            text = body.decode("utf-8", "replace")
            precision = self._query_params().get("precision", "ns")
            with self.server.lock:
                ingest(self.db, text, precision)
            self._send(204)
        except (LineProtocolError, ValueError, TypeError) as e:
            self._error(400, str(e))

    def _handle_query(self, params: Optional[dict] = None):
        params = params if params is not None else self._query_params()
        q = params.get("q")
        if not q:
            self._error(400, "missing query parameter 'q'")
            return
        try:
            with self.server.lock:
                result = SQLExecutor(self.db._engine).execute(q)
        except SqlError as e:
            self._error(400, str(e))
            return
        accept = self.headers.get("Accept", "")
        if "application/json" in accept:
            self._send(200, result.to_json().encode(), "application/json")
        else:
            self._send(200, result.to_csv().encode(), "text/csv; charset=utf-8")


class TSDBHTTPServer:
    """Runs the HTTP API for a :class:`~tsdb.core.TimeSeriesDB` in a thread."""

    def __init__(self, db, host: str = "127.0.0.1", port: int = 8086):
        self.db = db
        self.httpd = ThreadingHTTPServer((host, port), _Handler)
        self.httpd.tsdb = db
        self.httpd.lock = threading.Lock()
        self._thread: Optional[threading.Thread] = None

    @property
    def address(self):
        return self.httpd.server_address[:2]

    @property
    def port(self) -> int:
        return self.httpd.server_address[1]

    def start(self) -> "TSDBHTTPServer":
        if self._thread is None:
            self._thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
            self._thread.start()
        return self

    def stop(self) -> None:
        if self._thread is not None:
            self.httpd.shutdown()
            self._thread.join(timeout=5)
            self.httpd.server_close()
            self._thread = None

    def __enter__(self):
        return self.start()

    def __exit__(self, *exc):
        self.stop()
        return False
