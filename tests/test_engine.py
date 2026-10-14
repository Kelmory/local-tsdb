"""End-to-end tests for the :class:`~tsdb.core.TimeSeriesDB` facade.

Covers the full write -> commit -> read -> reopen lifecycle on a real single-file
database, including multi-block series and block-level time pruning.
"""
import os
import shutil
import tempfile
import unittest

from tsdb import Point, TimeSeriesDB


class EngineTestBase(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp(prefix="ltdb_engine_")
        self.path = os.path.join(self.dir, "engine.ltdb")

    def tearDown(self):
        shutil.rmtree(self.dir, ignore_errors=True)


class TestLifecycle(EngineTestBase):
    LINES = (
        "cpu,host=a usage=1.0 100\n"
        "cpu,host=a usage=2.0 200\n"
        "cpu,host=b usage=3.0 300\n"
    )

    def test_write_commit_select(self):
        db = TimeSeriesDB(self.path)
        n = db.write_lines(self.LINES)
        self.assertEqual(n, 3)
        rows = db.select(measurement="cpu")
        self.assertEqual([r.timestamp for r in rows], [100, 200, 300])
        db.close()

    def test_persistence_across_reopen(self):
        db = TimeSeriesDB(self.path)
        db.write_lines(self.LINES)
        db.commit()
        self.assertEqual(db.aggregate("cpu", "usage", "max"), 3.0)
        db.close()

        with TimeSeriesDB(self.path) as db2:
            rows = db2.select(measurement="cpu")
            self.assertEqual([r.timestamp for r in rows], [100, 200, 300])
            self.assertEqual(db2.aggregate("cpu", "usage", "mean"), (1.0 + 2.0 + 3.0) / 3)

    def test_memtable_visible_before_commit(self):
        db = TimeSeriesDB(self.path)
        db.write_lines("mem,host=a used=5i 10\n")
        # Not committed yet: the memtable must still be queryable.
        rows = db.select(measurement="mem")
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0].fields["used"], 5)
        db.commit()
        db.close()

    def test_measurements_and_series(self):
        db = TimeSeriesDB(self.path)
        db.write_lines(self.LINES)
        db.commit()
        self.assertEqual(db.measurements(), ["cpu"])
        self.assertEqual(db.series("cpu"), ["cpu,host=a", "cpu,host=b"])
        db.close()

    def test_tag_filter_select(self):
        db = TimeSeriesDB(self.path)
        db.write_lines(self.LINES)
        db.commit()
        rows = db.select(measurement="cpu", tag_filters={"host": "a"})
        self.assertEqual([r.timestamp for r in rows], [100, 200])
        db.close()


class TestMultiBlock(EngineTestBase):
    def test_two_commits_make_two_blocks_and_prune(self):
        db = TimeSeriesDB(self.path)
        # First block: ts 1..10. Second block: ts 1001..1010.
        db.write_lines("".join("cpu,h=a v=1.5 {}\n".format(i) for i in range(1, 11)))
        db.commit()
        db.write_lines("".join("cpu,h=a v=2.5 {}\n".format(1000 + i) for i in range(1, 11)))
        db.commit()

        refs = db.index.blocks(db.index.add_series("cpu", {"h": "a"}))
        self.assertEqual(len(refs), 2)

        # A range that only overlaps the second block returns exactly its rows.
        rows = db.select(measurement="cpu", start=1001, end=1010)
        self.assertEqual([r.timestamp for r in rows], list(range(1001, 1011)))
        # A range between the blocks returns nothing without decoding either.
        self.assertEqual(db.select(measurement="cpu", start=50, end=99), [])
        db.close()

    def test_out_of_order_writes_keep_block_bounds_correct(self):
        # Regression: a commit whose points arrive out of order used to store
        # first/last-written timestamps as block bounds, so range queries
        # silently pruned away rows that actually lived inside the block.
        db = TimeSeriesDB(self.path)
        db.write_lines("".join("cpu v={} {}\n".format(i, ts)
                               for i, ts in enumerate([300, 100, 500, 200, 400])))
        db.commit()

        refs = db.index.blocks(db.index.add_series("cpu", {}))
        self.assertEqual([(b[1], b[2]) for b in refs], [(100, 500)])

        rows = db.select(measurement="cpu")
        self.assertEqual([r.timestamp for r in rows], [100, 200, 300, 400, 500])
        # Ranges at both ends of the block must see their rows.
        self.assertEqual(db.select(measurement="cpu", start=50, end=150)[0].timestamp, 100)
        self.assertEqual(db.select(measurement="cpu", start=450, end=600)[0].timestamp, 500)
        db.close()

    def test_duplicate_timestamps_merge_after_commit(self):
        # Same series + timestamp: later points win field-by-field (InfluxDB
        # last-write-wins), so one row survives the flush.
        db = TimeSeriesDB(self.path)
        db.write_lines("m a=1.0 5\nm b=2.0 5\nm a=9.0 5\n")
        db.commit()
        rows = db.select(measurement="m")
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0].fields, {"a": 9.0, "b": 2.0})
        db.close()

    def test_sparse_fields_null_semantics(self):
        # Regression: missing fields used to be zero-filled into the block,
        # polluting mean(a) (5.0 instead of 10.0) and count(a) (2 instead of 1).
        db = TimeSeriesDB(self.path)
        db.write_lines("m a=1.5 1\nm b=2.0 2\n")
        db.commit()
        self.assertEqual(db.aggregate("m", "a", "mean"), 1.5)
        self.assertEqual(db.aggregate("m", "a", "count"), 1)
        rs = db.sql().execute("SELECT count(a) FROM m")
        self.assertEqual(rs.rows[0]["count_a"], 1)
        db.close()

    def test_field_type_conflict_rejected_at_commit(self):
        db = TimeSeriesDB(self.path)
        db.write([Point("m", {}, {"v": 1}, 1)])
        db.write([Point("m", {}, {"v": 1.5}, 2)])
        with self.assertRaises(TypeError):
            db.commit()
        db.close()

    def test_limit_applies_after_tag_neq(self):
        # Regression: LIMIT used to cut the scan before tag != filtering,
        # returning fewer rows than existed.
        db = TimeSeriesDB(self.path)
        lines = "".join("m,h={} v={}.0 {}\n".format("a" if i % 2 else "b", i, i)
                        for i in range(10))
        db.write_lines(lines)
        db.commit()
        rs = db.sql().execute("SELECT * FROM m WHERE h!='b' LIMIT 5")
        self.assertEqual(rs.n_rows, 5)
        db.close()


if __name__ == "__main__":
    unittest.main()
