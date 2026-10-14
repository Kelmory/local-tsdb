"""Unit tests for :mod:`tsdb.block` (data-block codec)."""
import unittest

from tsdb import model
from tsdb.block import DataBlock


def _rt(points):
    return DataBlock.from_bytes(DataBlock.from_points(points).to_bytes())


class TestDataBlock(unittest.TestCase):
    def test_roundtrip_mixed_fields(self):
        pts = [
            model.Point("cpu", {"h": "1"}, {"v": 23.5, "n": 1}, 100),
            model.Point("cpu", {"h": "1"}, {"v": 23.6, "n": 2}, 200),
            model.Point("cpu", {"h": "1"}, {"v": 24.0, "n": 3}, 300),
        ]
        b = _rt(pts)
        self.assertEqual(len(b), 3)
        self.assertEqual((b.min_ts, b.max_ts), (100, 300))
        self.assertEqual(sorted(b.field_names), ["n", "v"])
        self.assertEqual(b.field_type("n"), model.FIELD_INT)
        self.assertEqual(b.field_type("v"), model.FIELD_FLOAT)
        rows = list(b.rows())
        self.assertEqual(rows[0], (100, {"n": 1, "v": 23.5}))
        self.assertEqual(rows[-1], (300, {"n": 3, "v": 24.0}))

    def test_missing_field_is_null_not_zero(self):
        # v2 format: absent fields are stored as NULL (presence bitmap),
        # not zero-filled, so aggregates see InfluxDB-style semantics.
        pts = [model.Point("m", {}, {"a": 1.5}, 1), model.Point("m", {}, {"b": 7}, 2)]
        b = _rt(pts)
        rows = list(b.rows())
        self.assertEqual(rows[0], (1, {"a": 1.5, "b": None}))
        self.assertEqual(rows[1], (2, {"a": None, "b": 7}))

    def test_field_type_conflict_raises(self):
        # Regression: an int value followed by a float for the same field
        # used to be silently truncated (1.5 -> 1); it must fail loudly.
        pts = [model.Point("m", {}, {"v": 1}, 1),
               model.Point("m", {}, {"v": 1.5}, 2)]
        with self.assertRaises(TypeError):
            DataBlock.from_points(pts)

    def test_bool_then_int_conflict_raises(self):
        pts = [model.Point("m", {}, {"v": True}, 1),
               model.Point("m", {}, {"v": 7}, 2)]
        with self.assertRaises(TypeError):
            DataBlock.from_points(pts)

    def test_duplicate_timestamps_merge_last_wins(self):
        pts = [model.Point("m", {}, {"a": 1.0}, 5),
               model.Point("m", {}, {"b": 2.0}, 5),
               model.Point("m", {}, {"a": 9.0}, 5)]
        b = _rt(pts)
        rows = list(b.rows())
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0], (5, {"a": 9.0, "b": 2.0}))

    def test_bool_and_str_fields(self):
        pts = [model.Point("m", {}, {"ok": True, "s": "hi"}, 1),
               model.Point("m", {}, {"ok": False, "s": "yo"}, 2)]
        b = _rt(pts)
        rows = list(b.rows())
        self.assertEqual(rows[0][1], {"ok": True, "s": "hi"})
        self.assertEqual(rows[1][1], {"ok": False, "s": "yo"})

    def test_out_of_order_points_are_sorted(self):
        # Regression: blocks used to keep insertion order, so min_ts/max_ts
        # were the first/last *written* points and range pruning lost rows.
        pts = [model.Point("m", {}, {"v": float(i)}, ts)
               for i, ts in enumerate([300, 100, 500, 200, 400])]
        b = _rt(pts)
        self.assertEqual((b.min_ts, b.max_ts), (100, 500))
        self.assertEqual([ts for ts, _ in b.rows()], [100, 200, 300, 400, 500])

    def test_empty_block(self):
        b = _rt([])
        self.assertEqual(len(b), 0)
        self.assertIsNone(b.min_ts)
        self.assertEqual(list(b.rows()), [])


if __name__ == "__main__":
    unittest.main()
