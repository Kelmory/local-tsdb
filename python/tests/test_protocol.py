"""Unit tests for :mod:`tsdb.protocol` (InfluxDB line protocol)."""
import unittest

from tsdb import model
from tsdb.protocol import LineProtocolError, format_point, parse_line


class TestParseLine(unittest.TestCase):
    def test_measurement_only(self):
        p = parse_line("cpu usage=0.5")
        self.assertEqual(p.measurement, "cpu")
        self.assertEqual(p.tags, {})
        self.assertAlmostEqual(p.fields["usage"], 0.5)

    def test_tags_and_int_suffix(self):
        p = parse_line("cpu,host=a,region=us-east usage=0.67,idle=3i 1700000000000")
        self.assertEqual(p.tags, {"host": "a", "region": "us-east"})
        self.assertEqual(p.fields["idle"], 3)
        self.assertIsInstance(p.fields["idle"], int)
        self.assertAlmostEqual(p.fields["usage"], 0.67)
        self.assertEqual(p.timestamp, 1700000000000)

    def test_no_timestamp_defaults_zero(self):
        p = parse_line("m a=1")
        self.assertEqual(p.timestamp, 0)

    def test_bool_field(self):
        # Fields are comma-separated within the fields section (line protocol spec).
        p = parse_line("m ok=true,bad=false")
        self.assertIs(p.fields["ok"], True)
        self.assertIs(p.fields["bad"], False)

    def test_string_field(self):
        p = parse_line("m note=hello")
        self.assertEqual(p.fields["note"], "hello")

    def test_escaped_space_in_value(self):
        p = parse_line('cpu,host=a\ b usage=1.0')
        self.assertEqual(p.tags["host"], "a b")

    def test_escaped_comma_in_tag_value(self):
        p = parse_line("cpu,region=us\,east a=1")
        self.assertEqual(p.tags["region"], "us,east")

    def test_quoted_string_with_unescaped_space(self):
        # Regression: the space inside the quoted value used to split the
        # fields section and lose everything after it.
        p = parse_line('m s="hello world",n=1i 123')
        self.assertEqual(p.fields["s"], "hello world")
        self.assertEqual(p.fields["n"], 1)
        self.assertEqual(p.timestamp, 123)

    def test_space_separated_extra_section_rejected(self):
        # Fields must be comma separated (InfluxDB rejects this too).
        with self.assertRaises(LineProtocolError):
            parse_line('m s="hello world" n=1i')

    def test_quoted_string_with_comma(self):
        p = parse_line('m s="a,b",n=2i')
        self.assertEqual(p.fields["s"], "a,b")
        self.assertEqual(p.fields["n"], 2)

    def test_negative_timestamp(self):
        # Regression: a negative ts used to fail the isdigit check and be
        # misread as part of the fields section.
        p = parse_line("m v=1.0 -100")
        self.assertEqual(p.timestamp, -100)


class TestFormatPoint(unittest.TestCase):
    def _rt(self, p):
        return parse_line(format_point(p))

    def test_roundtrip_all_types(self):
        p = model.Point("cpu", {"host": "a b", "region": "us,east"},
                        {"load": 1.5, "n": 7, "ok": True, "note": "x y,z"}, 999)
        q = self._rt(p)
        self.assertEqual(q.measurement, p.measurement)
        self.assertEqual(q.tags, p.tags)
        self.assertEqual(q.fields, p.fields)
        self.assertEqual(q.timestamp, p.timestamp)

    def test_int_rendered_with_suffix(self):
        line = format_point(model.Point("m", {}, {"n": 5}, 0))
        self.assertIn("n=5i", line)


class TestErrors(unittest.TestCase):
    def test_missing_fields_raises(self):
        with self.assertRaises(LineProtocolError):
            parse_line("cpu")

    def test_bad_tag_segment_raises(self):
        with self.assertRaises(LineProtocolError):
            parse_line("cpu,bogus a=1")

    def test_empty_string_raises(self):
        with self.assertRaises(LineProtocolError):
            parse_line("")


if __name__ == "__main__":
    unittest.main()
