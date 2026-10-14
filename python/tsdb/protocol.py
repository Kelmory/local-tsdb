"""InfluxDB *line protocol* parser and serializer.

The line protocol is the de-facto text wire format for time-series ingestion::

    measurement[,tag1=v1][,tag2=v2] field1=fval[,field2=fval]... [timestamp]

where:

* the first space separates the (measurement + tag) key from the fields;
* tags are comma-separated ``name=value`` pairs before that space;
* each field value is typed by a trailing suffix -- an integer ends in ``i``
  (e.g. ``5i``), booleans are ``true``/``false``, and anything else numeric is a
  float; untyped non-numeric values are strings;
* the optional final token is a nanosecond timestamp.

Backslash escaping of space/comma/equals keeps names and values unambiguous,
mirroring InfluxDB's own rules so lines we emit parse back identically.
"""
from __future__ import annotations

from typing import Any, Dict, Optional, Tuple

from .model import FIELD_BOOL, FIELD_FLOAT, FIELD_INT, FIELD_STR, Point, field_type


class LineProtocolError(ValueError):
    """Raised when a line cannot be parsed as valid line protocol."""


# Characters that must be escaped in names / tag values.
_ESCAPABLE = {" ", ",", "="}


def _escape(s: str) -> str:
    out = []
    for ch in s:
        if ch in _ESCAPABLE or ch == "\\":
            out.append("\\" + ch)
        else:
            out.append(ch)
    return "".join(out)


def _unescape(s: str) -> str:
    out = []
    i = 0
    while i < len(s):
        c = s[i]
        if c == "\\" and i + 1 < len(s):
            out.append(s[i + 1])
            i += 2
        else:
            out.append(c)
            i += 1
    return "".join(out)


def _split_unescaped(s: str, sep: str, quotes: bool = False) -> list:
    """Split on ``sep`` ignoring backslash-escaped occurrences.

    With ``quotes=True`` a ``sep`` inside an unescaped double-quoted string
    does not split either -- needed for field values like ``s="a b"``.
    """
    parts = []
    cur = ""
    i = 0
    in_quote = False
    while i < len(s):
        c = s[i]
        if c == "\\" and i + 1 < len(s):
            cur += s[i:i + 2]
            i += 2
            continue
        if quotes and c == '"':
            in_quote = not in_quote
            cur += c
            i += 1
            continue
        if c == sep and not in_quote:
            parts.append(cur)
            cur = ""
        else:
            cur += c
        i += 1
    parts.append(cur)
    return parts


def _parse_value(s: str) -> Tuple[Any, str]:
    """Parse a single field value into ``(value, type_tag)``."""
    if s.endswith("i") and (s[:-1]).lstrip("-").isdigit():
        return int(s[:-1]), FIELD_INT
    if s == "true":
        return True, FIELD_BOOL
    if s == "false":
        return False, FIELD_BOOL
    try:
        return float(s), FIELD_FLOAT
    except ValueError:
        pass
    if len(s) >= 2 and s[0] == '"' and s[-1] == '"':
        s = _unescape(s[1:-1])
    else:
        s = _unescape(s)
    return s, FIELD_STR


def parse_line(line: str) -> Point:
    """Parse one line-protocol string into a :class:`Point`."""
    if not isinstance(line, str):
        raise LineProtocolError("line must be a string")
    text = line.strip()
    if not text or text.startswith("#"):
        raise LineProtocolError("empty line")

    # 1) Top-level split on *unescaped* spaces outside quotes:
    #    [head, fields(, ts)].
    tokens = _split_unescaped(text, " ", quotes=True)
    if len(tokens) < 2:
        raise LineProtocolError("missing fields section")
    ts = 0
    last = tokens[-1]
    if last.isdigit() or (last.startswith("-") and last[1:].isdigit()):
        ts = int(tokens.pop())
    # InfluxDB rejects extra space-separated sections (fields are comma
    # separated; a space means the next token would be a second timestamp).
    if len(tokens) > 2:
        raise LineProtocolError(
            "unexpected extra token after fields: {!r}".format(tokens[2]))
    head, tail = tokens[0], tokens[1]

    # 2) Split measurement / tags from the head.
    segs = _split_unescaped(head, ",")
    measurement = _unescape(segs[0])
    tags: Dict[str, str] = {}
    for seg in segs[1:]:
        name, _, value = seg.partition("=")
        if not _:
            raise LineProtocolError("bad tag {!r}".format(seg))
        tags[_unescape(name)] = _unescape(value)

    # 3) Fields (commas inside quoted string values do not split).
    fields: Dict[str, Any] = {}
    for seg in _split_unescaped(tail, ",", quotes=True):
        name, _, value = seg.partition("=")
        if not _:
            raise LineProtocolError("bad field {!r}".format(seg))
        val, _ftype = _parse_value(value)
        fields[_unescape(name)] = val

    return Point(measurement=measurement, tags=tags, fields=fields, timestamp=ts)


def format_point(point: Point) -> str:
    """Serialise a :class:`Point` back to line protocol."""
    head = _escape(point.measurement)
    for name, value in sorted(point.tags.items()):
        head += "," + _escape(name) + "=" + _escape(value)
    field_parts = []
    for name in sorted(point.fields):
        val = point.fields[name]
        ftype = field_type(val)
        if ftype == FIELD_INT:
            rendered = "{}i".format(int(val))
        elif ftype == FIELD_FLOAT:
            rendered = repr(float(val))
        elif ftype == FIELD_BOOL:
            rendered = "true" if val else "false"
        else:
            rendered = _escape(str(val))
        field_parts.append(_escape(name) + "=" + rendered)
    line = head + " " + ",".join(field_parts)
    if point.timestamp:
        line += " {}".format(point.timestamp)
    return line
