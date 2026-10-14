"""Unit tests for :mod:`tsdb.compression` (Gorilla-style codecs)."""
import math
import unittest

from tsdb import compression as c


class TestFloatXOR(unittest.TestCase):
    def test_roundtrip_basic(self):
        vals = [23.5, 23.5, 23.6, 24.0, 100.25]
        self.assertEqual(c.decode_floats(c.encode_floats(vals), len(vals)), vals)

    def test_single_value(self):
        vals = [3.14159]
        self.assertEqual(c.decode_floats(c.encode_floats(vals), 1), vals)

    def test_unchanged_run_collapses(self):
        # A long run of identical values should be far smaller than raw.
        vals = [1.25] * 1000
        enc = c.encode_floats(vals)
        self.assertLess(len(enc), len(vals) * 8)

    def test_empty(self):
        self.assertEqual(c.encode_floats([]), b"")
        self.assertEqual(c.decode_floats(b"", 0), [])

    def test_negative_zero_preserved(self):
        vals = [0.0, -0.0]
        out = c.decode_floats(c.encode_floats(vals), 2)
        self.assertEqual(math.copysign(1, out[0]), 1.0)   # +0.0
        self.assertEqual(math.copysign(1, out[1]), -1.0)  # -0.0

    def test_nan_preserved(self):
        vals = [float("nan"), 1.0]
        out = c.decode_floats(c.encode_floats(vals), 2)
        self.assertTrue(math.isnan(out[0]))
        self.assertEqual(out[1], 1.0)

    def test_large_magnitudes(self):
        vals = [1e30, -1e-30, 42.5]
        self.assertEqual(c.decode_floats(c.encode_floats(vals), len(vals)), vals)


class TestIntDeltaOfDelta(unittest.TestCase):
    def _rt(self, vals):
        return c.decode_ints_dod(c.encode_ints_dod(vals))

    def test_roundtrip_increasing(self):
        self.assertEqual(self._rt(list(range(100))), list(range(100)))

    def test_roundtrip_negative(self):
        vals = [-5, -3, -8, 10, -2, 0]
        self.assertEqual(self._rt(vals), vals)

    def test_single_value(self):
        self.assertEqual(self._rt([42]), [42])

    def test_empty(self):
        self.assertEqual(c.encode_ints_dod([]), b"")
        self.assertEqual(c.decode_ints_dod(b""), [])

    def test_steady_counter_is_tiny(self):
        # Constant step -> second differences are zero; encodes very compactly.
        vals = list(range(0, 1000, 7))
        enc = c.encode_ints_dod(vals)
        self.assertLess(len(enc), len(vals) * 2)

    def test_large_values(self):
        vals = [2**40, 2**40 + 1, 2**40 - 5]
        self.assertEqual(self._rt(vals), vals)


class TestVarintZigzag(unittest.TestCase):
    def test_zigzag_ordering(self):
        # Small magnitudes map to small non-negative codes.
        self.assertLess(c._zigzag(1), c._zigzag(100))
        self.assertEqual(c._unzigzag(c._zigzag(-7)), -7)

    def test_varint_roundtrip(self):
        for n in [0, 1, 127, 128, 300, 2**20, 2**40]:
            b = c._varint(n)
            out, pos = c._read_varint(b, 0)
            self.assertEqual(out, n)
            self.assertEqual(pos, len(b))


if __name__ == "__main__":
    unittest.main()
