"""Unit tests for :mod:`tsdb.model`."""
import unittest

from tsdb import model


class TestSeriesKey(unittest.TestCase):
    def test_no_tags(self):
        self.assertEqual(model.series_key("cpu", {}), "cpu")

    def test_single_tag(self):
        self.assertEqual(model.series_key("cpu", {"host": "a"}), "cpu,host=a")

    def test_tags_sorted_canonical(self):
        # Insertion order must not matter.
        a = model.series_key("cpu", {"zeta": "1", "alpha": "2"})
        b = model.series_key("cpu", {"alpha": "2", "zeta": "1"})
        self.assertEqual(a, b)
        self.assertEqual(a, "cpu,alpha=2,zeta=1")

    def test_point_series_key(self):
        p = model.Point("m", {"b": "2", "a": "1"}, {}, 0)
        self.assertEqual(p.series_key(), "m,a=1,b=2")


class TestParseSeriesKey(unittest.TestCase):
    def test_roundtrip_no_tags(self):
        m, t = model.parse_series_key("cpu")
        self.assertEqual((m, t), ("cpu", {}))

    def test_roundtrip_with_tags(self):
        m, t = model.parse_series_key("cpu,host=a,region=us-east")
        self.assertEqual(m, "cpu")
        self.assertEqual(t, {"host": "a", "region": "us-east"})

    def test_malformed_segment_raises(self):
        with self.assertRaises(ValueError):
            model.parse_series_key("cpu,bogus")


class TestFieldType(unittest.TestCase):
    def test_bool_before_int(self):
        # bool is a subclass of int; must classify as bool, not int.
        self.assertEqual(model.field_type(True), model.FIELD_BOOL)

    def test_int(self):
        self.assertEqual(model.field_type(5), model.FIELD_INT)

    def test_float(self):
        self.assertEqual(model.field_type(1.5), model.FIELD_FLOAT)

    def test_str(self):
        self.assertEqual(model.field_type("x"), model.FIELD_STR)

    def test_unsupported_raises(self):
        with self.assertRaises(TypeError):
            model.field_type([1, 2])


class TestCoerceField(unittest.TestCase):
    def test_coerce_int(self):
        self.assertEqual(model.coerce_field("7", model.FIELD_INT), 7)

    def test_coerce_float(self):
        self.assertAlmostEqual(model.coerce_field("1.5", model.FIELD_FLOAT), 1.5)

    def test_coerce_bool(self):
        self.assertIs(model.coerce_field(True, model.FIELD_BOOL), True)


class TestNamePacking(unittest.TestCase):
    def test_roundtrip(self):
        packed = model.pack_field_name("temperature")
        name, off = model.unpack_field_name(packed, 0)
        self.assertEqual(name.decode("utf-8"), "temperature")
        self.assertEqual(off, len(packed))

    def test_empty_name(self):
        packed = model.pack_field_name("")
        name, _ = model.unpack_field_name(packed, 0)
        self.assertEqual(name, b"")


if __name__ == "__main__":
    unittest.main()
