"""End-to-end tests for the InfluxDB-compatible HTTP API (:mod:`tsdb.http_server`).

Runs a real :class:`~tsdb.core.TimeSeriesDB` behind :class:`TSDBHTTPServer` on
an ephemeral port and exercises it over plain HTTP with stdlib ``urllib``.
Each test writes to its own measurement so cases stay independent.
"""
import json
import os
import shutil
import tempfile
import unittest
import urllib.error
import urllib.parse
import urllib.request

from tsdb import TimeSeriesDB, TSDBHTTPServer


class TestHTTPAPI(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dir = tempfile.mkdtemp(prefix="ltdb_http_")
        cls.db_path = os.path.join(cls.dir, "http.ltdb")
        cls.db = TimeSeriesDB(cls.db_path)
        cls.server = TSDBHTTPServer(cls.db, port=0).start()

    @classmethod
    def tearDownClass(cls):
        cls.server.stop()
        cls.db.close()
        shutil.rmtree(cls.dir, ignore_errors=True)

    def url(self, path):
        return "http://{}:{}{}".format(*self.server.address, path)

    def request(self, method, path, body=None, headers=None):
        req = urllib.request.Request(
            self.url(path), data=body.encode() if body is not None else None,
            method=method, headers=headers or {})
        try:
            with urllib.request.urlopen(req) as resp:
                return resp.status, resp.read(), dict(resp.headers)
        except urllib.error.HTTPError as e:
            return e.code, e.read(), {}

    def query(self, sql, accept_json=True):
        path = "/query?q=" + urllib.parse.quote(sql)
        hdrs = {"Accept": "application/json"} if accept_json else None
        status, body, headers = self.request("GET", path, headers=hdrs)
        return status, body, headers

    # -- probes ------------------------------------------------------------- #
    def test_ping_returns_204(self):
        status, _, _ = self.request("GET", "/ping")
        self.assertEqual(status, 204)

    def test_health_ok(self):
        status, body, _ = self.request("GET", "/health")
        self.assertEqual((status, body), (200, b"OK"))

    # -- write + query round trip ------------------------------------------- #
    def test_write_then_query_json(self):
        lines = ("cpu,host=a usage=1.5 1700000000000\n"
                "cpu,host=a usage=2.5 1700000060000\n")
        status, _, _ = self.request("POST", "/write?precision=ns", body=lines)
        self.assertEqual(status, 204)

        status, body, _ = self.query("SELECT * FROM cpu WHERE host='a'")
        self.assertEqual(status, 200)
        doc = json.loads(body)
        self.assertIn("columns", doc)
        self.assertEqual(len(doc["rows"]), 2)
        self.assertAlmostEqual(doc["rows"][0]["usage"], 1.5)

    def test_write_precision_seconds(self):
        # precision=s: the explicit timestamp is interpreted in *seconds*.
        status, _, _ = self.request(
            "POST", "/write?precision=s", body="temp,h=1 v=21.0 1700000000\n")
        self.assertEqual(status, 204)
        status, body, _ = self.query("SELECT * FROM temp")
        doc = json.loads(body)
        self.assertEqual(doc["rows"][0]["time"], 1700000000 * 10**9)

    def test_query_csv_default(self):
        # Own measurement so the count is deterministic.
        status, _, _ = self.request(
            "POST", "/write?precision=ns", body="load,a=1 v=1i\nload,a=2 v=2i\n")
        self.assertEqual(status, 204)
        status, body, headers = self.query("SELECT count() FROM load", accept_json=False)
        self.assertEqual(status, 200)
        text = body.decode()
        self.assertTrue(text.startswith("count\n"))
        self.assertIn("text/csv", (headers.get("Content-Type") or "").lower())

    def test_bad_write_line_returns_400(self):
        status, body, _ = self.request("POST", "/write", body="not a valid line")
        self.assertEqual(status, 400)
        self.assertIn("error", json.loads(body))

    def test_bad_query_returns_400(self):
        status, body, _ = self.query("SELECT FROM cpu")
        self.assertEqual(status, 400)

    def test_unknown_path_404(self):
        status, _, _ = self.request("GET", "/nope")
        self.assertEqual(status, 404)


if __name__ == "__main__":
    unittest.main()
