"""Unit tests for :mod:`tsdb.query` (query engine)."""
import unittest

from tsdb import Index, QueryEngine
from tsdb.block import DataBlock
from tsdb.model import Point


def _block(points):
    return DataBlock.from_points(points)


class TestQueryEngine(unittest.TestCase):
    def setUp(self):
        self.index = Index()
        self.blocks = {}
        self.memtable = {}

        k1 = self.index.add_series("cpu", {"host": "a"})
        k2 = self.index.add_series("cpu", {"host": "b"})
        k3 = self.index.add_series("mem", {})

        b1 = _block([Point("cpu", {"host": "a"}, {"usage": 1.0}, 100),
                     Point("cpu", {"host": "a"}, {"usage": 2.0}, 200),
                     Point("cpu", {"host": "a"}, {"usage": 3.0}, 300)])
        b2 = _block([Point("cpu", {"host": "b"}, {"usage": 10.0}, 150),
                     Point("cpu", {"host": "b"}, {"usage": 20.0}, 250)])
        b3 = _block([Point("mem", {}, {"used": 5}, 50)])

        self.blocks[1] = b1
        self.blocks[2] = b2
        self.blocks[3] = b3
        self.index.add_block(k1, 1, 100, 300)
        self.index.add_block(k2, 2, 150, 250)
        self.index.add_block(k3, 3, 50, 50)

        self.k1 = k1
        self.engine = QueryEngine(self.index, lambda loc: self.blocks[loc], self.memtable)

    # -- select ------------------------------------------------------------- #
    def test_select_all_cpu(self):
        rows = self.engine.select(measurement="cpu")
        self.assertEqual(len(rows), 5)
        # Ordered by series key then timestamp.
        self.assertEqual([r.timestamp for r in rows], [100, 200, 300, 150, 250])

    def test_select_tag_filter(self):
        rows = self.engine.select(measurement="cpu", tag_filters={"host": "a"})
        self.assertEqual([r.timestamp for r in rows], [100, 200, 300])

    def test_select_time_range_pruning(self):
        rows = self.engine.select(measurement="cpu", tag_filters={"host": "a"}, start=200, end=300)
        self.assertEqual([r.timestamp for r in rows], [200, 300])

    def test_select_field_projection(self):
        rows = self.engine.select(measurement="mem", fields=["used"])
        self.assertEqual(rows[0].fields, {"used": 5})

    def test_select_limit(self):
        rows = self.engine.select(measurement="cpu", limit=2)
        self.assertEqual(len(rows), 2)

    # -- memtable visibility ------------------------------------------------- #
    def test_memtable_points_visible_before_flush(self):
        self.memtable[self.k1] = [Point("cpu", {"host": "a"}, {"usage": 9.0}, 400)]
        rows = self.engine.select(measurement="cpu", tag_filters={"host": "a"})
        self.assertEqual([r.timestamp for r in rows], [100, 200, 300, 400])

    # -- aggregate ---------------------------------------------------------- #
    def test_aggregate_count(self):
        self.assertEqual(self.engine.aggregate("cpu", "usage", "count"), 5)

    def test_aggregate_count_is_not_first_value_plus_n(self):
        # Regression: count used to start from the first field value, so
        # values [100, 200, 300] returned 102.0 instead of 3.
        k = self.index.add_series("big", {})
        self.index.add_block(k, 4, 10, 30)
        self.blocks[4] = _block([Point("big", {}, {"v": 100.0}, 10),
                                 Point("big", {}, {"v": 200.0}, 20),
                                 Point("big", {}, {"v": 300.0}, 30)])
        self.assertEqual(self.engine.aggregate("big", "v", "count"), 3)

    def test_aggregate_count_empty_is_zero(self):
        self.assertEqual(self.engine.aggregate("mem", "usage", "count"), 0)

    def test_aggregate_skips_null_values(self):
        # Regression: sparse fields used to be zero-filled, so mean(a) over
        # rows [a=10.0, b=1.0] came out 5.0 instead of 10.0.
        k = self.index.add_series("sparse", {})
        self.index.add_block(k, 4, 1, 2)
        self.blocks[4] = _block([Point("sparse", {}, {"a": 10.0}, 1),
                                 Point("sparse", {}, {"b": 1.0}, 2)])
        self.assertEqual(self.engine.aggregate("sparse", "a", "mean"), 10.0)
        self.assertEqual(self.engine.aggregate("sparse", "a", "count"), 1)

    def test_aggregate_min_max_sum_mean(self):
        self.assertEqual(self.engine.aggregate("cpu", "usage", "min"), 1.0)
        self.assertEqual(self.engine.aggregate("cpu", "usage", "max"), 20.0)
        self.assertAlmostEqual(self.engine.aggregate("cpu", "usage", "sum"), 36.0)
        self.assertAlmostEqual(self.engine.aggregate("cpu", "usage", "mean"), 7.2)

    def test_aggregate_tag_scoped(self):
        self.assertEqual(self.engine.aggregate("cpu", "usage", "max", tag_filters={"host": "a"}), 3.0)

    def test_aggregate_group_by(self):
        res = self.engine.aggregate("cpu", "usage", "max", group_by="host")
        self.assertEqual(res, {"a": 3.0, "b": 20.0})

    def test_aggregate_unknown_func_raises(self):
        with self.assertRaises(ValueError):
            self.engine.aggregate("cpu", "usage", "median")


if __name__ == "__main__":
    unittest.main()
