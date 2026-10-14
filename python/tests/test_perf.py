"""Performance / throughput smoke tests for :mod:`tsdb`.

These are deliberately *loose* benchmarks: they assert correctness and a very
generous wall-clock ceiling, and print timings (points/s, file size, scan
times) so regressions are visible whenever the suite runs.  The whole module
is meant to finish in low seconds.
"""
import os
import shutil
import tempfile
import time
import unittest

from tsdb import TimeSeriesDB

N_HOSTS = 8
N_TICKS = 12_500          # 8 * 12_500 == 100_000 points total
BASE_TS = 1_700_000_000 * 10**9


def _value(j: int) -> float:
    return (j % 100) / 100.0


class TestPerformance(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dir = tempfile.mkdtemp(prefix="ltdb_perf_")
        cls.path = os.path.join(cls.dir, "perf.ltdb")
        cls.n_points = N_HOSTS * N_TICKS

        # Build the whole payload as line protocol so the parse path is timed too.
        lines = []
        for i in range(N_HOSTS):
            host = "h{}".format(i)
            for j in range(N_TICKS):
                lines.append("cpu,host={} usage={:.4f} {}".format(host, _value(j), BASE_TS + j))
        text = "\n".join(lines)

        db = TimeSeriesDB(cls.path)
        t0 = time.perf_counter()
        db.write_lines(text)
        db.commit()
        cls.write_seconds = time.perf_counter() - t0
        db.close()

        size = os.path.getsize(cls.path)
        print("\n[perf] wrote {} points in {:.3f}s -> {:.0f} points/s".format(
            cls.n_points, cls.write_seconds, cls.n_points / max(cls.write_seconds, 1e-9)))
        print("[perf] file size: {} bytes ({:.2f} bytes/point; raw line protocol was "
              "{} bytes)".format(size, size / cls.n_points, len(text)))

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.dir, ignore_errors=True)

    # -- write -------------------------------------------------------------- #
    def test_write_throughput_ceiling(self):
        self.assertLess(self.write_seconds, 60.0)

    def test_compression_sanity(self):
        # Gorilla-encoded binary must beat the raw text payload by a wide margin.
        size = os.path.getsize(self.path)
        self.assertLess(size, 2_000_000)

    # -- reads -------------------------------------------------------------- #
    def test_full_scan_and_pruning(self):
        with TimeSeriesDB(self.path) as db:
            t0 = time.perf_counter()
            all_rows = db.select(measurement="cpu")
            full_s = time.perf_counter() - t0
            self.assertEqual(len(all_rows), self.n_points)

            # Inclusive range covering 1/12.5 of the written time span:
            # only blocks/ticks inside [6000, 7000] may be returned.
            t0 = time.perf_counter()
            narrow = db.select(measurement="cpu", start=BASE_TS + 6_000, end=BASE_TS + 7_000)
            prune_s = time.perf_counter() - t0
            self.assertEqual(len(narrow), N_HOSTS * 1_001)

        print("\n[perf] full scan of {} rows: {:.3f}s".format(self.n_points, full_s))
        print("[perf] pruned range (8% of the time span): {:.4f}s for {} rows".format(
            prune_s, len(narrow)))

    def test_aggregate_over_all_rows(self):
        with TimeSeriesDB(self.path) as db:
            t0 = time.perf_counter()
            mean = db.aggregate("cpu", "usage", func="mean")
            elapsed = time.perf_counter() - t0
        # Every series is (j % 100)/100 over a whole number of cycles -> 0.495.
        self.assertAlmostEqual(mean, 0.495, places=6)
        print("\n[perf] mean over {} points: {:.3f}s".format(self.n_points, elapsed))


if __name__ == "__main__":
    unittest.main()
