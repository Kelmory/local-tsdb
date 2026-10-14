"""Unit tests for :mod:`tsdb.index`."""
import unittest

from tsdb import Index


class TestIndex(unittest.TestCase):
    def _seed(self):
        idx = Index()
        k1 = idx.add_series("cpu", {"host": "a", "region": "us"})
        k2 = idx.add_series("mem", {"host": "a"})
        idx.add_block(k1, 10, 100, 200)
        idx.add_block(k1, 11, 300, 400)
        return idx, k1, k2

    def test_add_series_canonical_key(self):
        idx = Index()
        self.assertEqual(idx.add_series("cpu", {"b": "2", "a": "1"}), "cpu,a=1,b=2")

    def test_blocks_list(self):
        idx, k1, _ = self._seed()
        self.assertEqual(idx.blocks(k1), [(10, 100, 200), (11, 300, 400)])

    def test_series_for_tags_single(self):
        idx, k1, k2 = self._seed()
        self.assertEqual(idx.series_for_tags({"host": "a"}), {k1, k2})
        self.assertEqual(idx.series_for_tags({"region": "us"}), {k1})

    def test_series_for_tags_intersection(self):
        idx, k1, _ = self._seed()
        self.assertEqual(idx.series_for_tags({"host": "a", "region": "us"}), {k1})
        self.assertEqual(idx.series_for_tags({"host": "z"}), set())

    def test_series_for_tags_no_filter_returns_all(self):
        idx, k1, k2 = self._seed()
        self.assertEqual(idx.series_for_tags({}), {k1, k2})

    def test_measurements_sorted(self):
        idx, _, _ = self._seed()
        self.assertEqual(idx.measurements(), ["cpu", "mem"])

    def test_add_block_unknown_series_raises(self):
        idx = Index()
        with self.assertRaises(KeyError):
            idx.add_block("nope", 1, 0, 0)

    def test_persistence_roundtrip(self):
        idx, k1, k2 = self._seed()
        restored = Index.from_bytes(idx.to_bytes())
        self.assertEqual(restored.blocks(k1), [(10, 100, 200), (11, 300, 400)])
        # Derived structures rebuilt from parsed keys.
        self.assertEqual(restored.measurements(), ["cpu", "mem"])
        self.assertEqual(restored.series_for_tags({"host": "a"}), {k1, k2})
        self.assertEqual(restored.all_series(), sorted([k1, k2]))

    def test_empty_index_roundtrip(self):
        restored = Index.from_bytes(Index().to_bytes())
        self.assertEqual(restored.measurements(), [])


if __name__ == "__main__":
    unittest.main()
