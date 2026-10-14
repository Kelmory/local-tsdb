"""Unit tests for :mod:`tsdb.sql` (InfluxQL-subset parser + executor)."""
import json
import unittest

from tsdb import Index, QueryEngine
from tsdb.block import DataBlock
from tsdb.model import Point
from tsdb.sql import ResultSet, SQLExecutor, SqlError, parse_query


def _seed_engine():
    index = Index()
    blocks = {}

    def add(key_tags, pts):
        key = index.add_series(pts[0].measurement, key_tags)
        blk = DataBlock.from_points(pts)
        loc = len(blocks) + 1
        blocks[loc] = blk
        index.add_block(key, loc, blk.min_ts, blk.max_ts)

    add({"host": "a"}, [Point("cpu", {"host": "a"}, {"usage": 1.0}, 100),
                        Point("cpu", {"host": "a"}, {"usage": 2.0}, 200),
                        Point("cpu", {"host": "a"}, {"usage": 3.0}, 300)])
    add({"host": "b"}, [Point("cpu", {"host": "b"}, {"usage": 10.0}, 150),
                        Point("cpu", {"host": "b"}, {"usage": 20.0}, 250)])
    add({}, [Point("mem", {}, {"used": 5}, 50)])
    return QueryEngine(index, blocks.get, {})


class TestParser(unittest.TestCase):
    def test_parse_basic(self):
        q = parse_query("SELECT * FROM cpu")
        self.assertEqual(q.measurement, "cpu")

    def test_strict_inequality_shifts_bound(self):
        q = parse_query("SELECT * FROM m WHERE time > 100 AND time < 300")
        self.assertEqual((q.start, q.end), (101, 299))

    def test_inclusive_bounds(self):
        q = parse_query("SELECT * FROM m WHERE time >= 100 AND time <= 300")
        self.assertEqual((q.start, q.end), (100, 300))

    def test_now_arithmetic(self):
        now = 5_000_000_000
        q = parse_query("SELECT * FROM m WHERE time > now() - 1h", now_ns=now)
        self.assertEqual(q.start, now - 3600 * 10**9 + 1)

    def test_now_plus(self):
        now = 5_000_000_000
        q = parse_query("SELECT * FROM m WHERE time < now() + 5m", now_ns=now)
        self.assertEqual(q.end, now + 300 * 10**9 - 1)

    def test_rfc3339_date(self):
        # 2024-01-01T00:00:00Z == 1704067200 s.
        q = parse_query("SELECT * FROM m WHERE time >= '2024-01-01T00:00:00Z'")
        self.assertEqual(q.start, 1704067200 * 10**9)

    def test_tag_eq_and_neq(self):
        q = parse_query("SELECT * FROM m WHERE host='a' AND region!='x'")
        self.assertEqual((q.tag_eq, q.tag_neq), ({"host": "a"}, {"region": "x"}))

    def test_group_by_time_and_tags(self):
        q = parse_query("SELECT max(v) FROM m GROUP BY time(1h), host LIMIT 5")
        self.assertEqual(q.time_bucket, 3600 * 10**9)
        self.assertEqual(q.group_tags, ["host"])
        self.assertEqual(q.limit, 5)

    def test_unknown_function_raises(self):
        with self.assertRaises(SqlError):
            parse_query("SELECT median(v) FROM m")

    def test_trailing_tokens_raise(self):
        with self.assertRaises(SqlError):
            parse_query("SELECT * FROM cpu WHERE host='a' AND")

    def test_empty_query_raises(self):
        with self.assertRaises(SqlError):
            parse_query("   ")


class TestExecutorScan(unittest.TestCase):
    def setUp(self):
        self.engine = _seed_engine()
        self.sql = SQLExecutor(self.engine)

    def _rows(self, sql):
        return self.sql.execute(sql)

    def test_select_star(self):
        r = self._rows("SELECT * FROM cpu")
        self.assertEqual(r.n_rows, 5)
        self.assertEqual(r.columns[:4], ["name", "host", "time", "usage"])

    def test_where_time_range_strict(self):
        r = self._rows("SELECT * FROM cpu WHERE time > 100 AND time < 300")
        self.assertEqual(sorted(row["time"] for row in r.rows), [150, 200, 250])

    def test_where_tag_eq(self):
        r = self._rows("SELECT * FROM cpu WHERE host='a'")
        self.assertEqual([row["time"] for row in r.rows], [100, 200, 300])

    def test_where_tag_neq(self):
        r = self._rows("SELECT * FROM cpu WHERE host!='a'")
        self.assertEqual(sorted(row["time"] for row in r.rows), [150, 250])

    def test_limit(self):
        r = self._rows("SELECT * FROM cpu LIMIT 2")
        self.assertEqual(r.n_rows, 2)

    def test_show_measurements(self):
        r = self._rows("SHOW MEASUREMENTS")
        self.assertEqual([row["name"] for row in r.rows], ["cpu", "mem"])

    def test_group_by_requires_aggregate(self):
        with self.assertRaises(SqlError):
            self.sql.execute("SELECT * FROM cpu GROUP BY host")


class TestExecutorAggregate(unittest.TestCase):
    def setUp(self):
        self.engine = _seed_engine()
        self.sql = SQLExecutor(self.engine)

    def test_scalar_mean(self):
        r = self.sql.execute("SELECT mean(usage) FROM cpu")
        self.assertEqual(r.columns, ["mean_usage"])
        self.assertAlmostEqual(r.rows[0]["mean_usage"], 7.2)

    def test_count_no_field(self):
        r = self.sql.execute("SELECT count() FROM cpu")
        self.assertEqual(r.rows[0]["count"], 5)

    def test_min_max_sum(self):
        self.assertEqual(self.sql.execute("SELECT min(usage) FROM cpu").rows[0]["min_usage"], 1.0)
        self.assertEqual(self.sql.execute("SELECT max(usage) FROM cpu").rows[0]["max_usage"], 20.0)
        self.assertAlmostEqual(self.sql.execute("SELECT sum(usage) FROM cpu").rows[0]["sum_usage"], 36.0)

    def test_group_by_tag(self):
        r = self.sql.execute("SELECT max(usage) FROM cpu GROUP BY host")
        self.assertEqual(r.columns, ["host", "max_usage"])
        self.assertEqual(r.rows, [{"host": "a", "max_usage": 3.0},
                                  {"host": "b", "max_usage": 20.0}])

    def test_group_by_time_bucket(self):
        # Buckets of 100ns: a@100/200/300, b@150->100, b@250->200.
        r = self.sql.execute("SELECT mean(usage) FROM cpu GROUP BY time(100)")
        self.assertEqual(r.columns, ["time", "mean_usage"])
        got = {row["time"]: row["mean_usage"] for row in r.rows}
        self.assertAlmostEqual(got[100], (1.0 + 10.0) / 2)
        self.assertAlmostEqual(got[200], (2.0 + 20.0) / 2)
        self.assertEqual(got[300], 3.0)

    def test_group_by_tag_and_time(self):
        r = self.sql.execute("SELECT max(usage) FROM cpu GROUP BY host, time(100)")
        got = {(row["host"], row["time"]): row["max_usage"] for row in r.rows}
        self.assertEqual(got[("a", 100)], 1.0)
        self.assertEqual(got[("b", 200)], 20.0)

    def test_aggregate_respects_where(self):
        r = self.sql.execute("SELECT max(usage) FROM cpu WHERE host='a'")
        self.assertEqual(r.rows[0]["max_usage"], 3.0)


class TestResultSetRendering(unittest.TestCase):
    def test_csv_shape(self):
        rs = ResultSet(["name", "time", "v"], [{"name": "cpu", "time": 1, "v": 1.5}])
        csv = rs.to_csv()
        self.assertEqual(csv.splitlines(), ["name,time,v", "cpu,1,1.5"])

    def test_json_shape(self):
        rs = ResultSet(["a"], [{"a": 1}, {"a": 2}])
        doc = json.loads(rs.to_json())
        self.assertEqual(doc["columns"], ["a"])
        self.assertEqual(len(doc["rows"]), 2)


if __name__ == "__main__":
    unittest.main()
