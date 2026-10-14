"""End-to-end demo of the local time-series database.

Run with::

    python demo/demo.py

It exercises, in order:

1. writing points through the Python API (``Point`` objects) and through the
   InfluxDB *line protocol*;
2. reading them back with :meth:`TimeSeriesDB.select` / :meth:`~TimeSeriesDB.aggregate`;
3. querying with the InfluxQL subset (:meth:`TimeSeriesDB.sql`);
4. serving the standard TSDB HTTP interface (``/ping``, ``/write``, ``/query``)
   and talking to it over a real socket.

Everything runs against a throw-away database in a temp directory, so the demo
is safe to re-run any number of times.
"""
import json
import os
import shutil
import sys
import tempfile
import urllib.parse
import urllib.request

# Allow running as ``python demo/demo.py`` from the project root.
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from tsdb import Point, TimeSeriesDB, TSDBHTTPServer


def banner(title: str) -> None:
    print("\n" + "=" * 62)
    print(title)
    print("=" * 62)


def main() -> None:
    workdir = tempfile.mkdtemp(prefix="ltdb_demo_")
    path = os.path.join(workdir, "demo.ltdb")

    # ------------------------------------------------------------------ #
    banner("1. Write points (Python API + line protocol)")
    # ------------------------------------------------------------------ #
    db = TimeSeriesDB(path)
    base_ts = 1_700_000_000 * 10**9          # a fixed epoch, in nanoseconds

    # Two hosts reporting CPU usage once per second for ten seconds.
    points = [Point("cpu", {"host": h}, {"usage": (i % 5) / 10.0}, base_ts + i)
              for h in ("a", "b") for i in range(10)]
    db.write(points)

    # Memory + load measurements via the line protocol (InfluxDB wire format).
    lines = "".join("mem,host=a used={}i\n".format(i * 1024) for i in range(5))
    lines += "".join("load,host=b n=1i,v={:.6f} {}\n".format(i / 3.0, base_ts + i) for i in range(5))
    db.write_lines(lines)

    db.commit()
    print("wrote {} points into {}".format(len(points) + 10, path))
    print("file size: {} bytes".format(os.path.getsize(path)))
    print("measurements:", ", ".join(db.measurements()))

    # ------------------------------------------------------------------ #
    banner("2. Read back (select / aggregate)")
    # ------------------------------------------------------------------ #
    rows = db.select(measurement="cpu", tag_filters={"host": "a"})
    print("cpu/host=a rows:", len(rows), "| first usage:", rows[0].fields["usage"])

    avg = db.aggregate("cpu", "usage", func="mean")
    print("mean cpu usage over all hosts: {:.3f}".format(avg))

    # ------------------------------------------------------------------ #
    banner("3. InfluxQL-subset queries (db.sql)")
    # ------------------------------------------------------------------ #
    sql = db.sql()

    rs = sql.execute("SELECT * FROM cpu WHERE host='a' LIMIT 3")
    print("\nSELECT * FROM cpu WHERE host='a' LIMIT 3   -> CSV:")
    print(rs.to_csv())

    rs = sql.execute("SELECT mean(usage) FROM cpu GROUP BY time(100)")
    print("\nSELECT mean(usage) FROM cpu GROUP BY time(100):")
    for row in rs.rows:
        print("  bucket {} -> {}".format(row["time"], round(row["mean_usage"], 3)))

    rs = sql.execute("SHOW MEASUREMENTS")
    print("\nSHOW MEASUREMENTS ->", [r["name"] for r in rs.rows])

    # ------------------------------------------------------------------ #
    banner("4. Standard TSDB HTTP interface (InfluxDB-compatible)")
    # ------------------------------------------------------------------ #
    server = TSDBHTTPServer(db, host="127.0.0.1", port=0).start()
    base_url = "http://{}:{}".format(*server.address)

    def call(method, path, body=None, headers=None):
        req = urllib.request.Request(base_url + path, data=body, method=method,
                                     headers=headers or {})
        with urllib.request.urlopen(req) as resp:
            return resp.status, resp.read()

    status, _ = call("GET", "/ping")
    print("GET  /ping   ->", status)

    body = "net,iface=eth0 rx=1234i\n".encode()
    status, _ = call("POST", "/write?precision=ns", body=body)
    print("POST /write  -> {} (ingested net/iface=eth0)".format(status))

    status, payload = call("GET", "/query?q=" + urllib.parse.quote("SELECT count() FROM net"),
                           headers={"Accept": "application/json"})
    result = json.loads(payload)
    print("GET  /query  -> {} | columns={} rows={}".format(
        status, result["columns"], result["rows"]))

    server.stop()
    db.close()
    shutil.rmtree(workdir, ignore_errors=True)
    print("\ndone. (temp dir removed)")


if __name__ == "__main__":
    main()
