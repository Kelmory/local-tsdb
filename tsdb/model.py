"""Core data models for the local time-series database.

The model mirrors InfluxDB's storage concepts:

* A **Point** is one measurement sample: a measurement name, an optional
  tag set (string key/value pairs), one or more typed fields and a
  nanosecond-precision timestamp.
* A **series key** is the canonical ``measurement[,tag1=v1][,tag2=v2]``
  string with tags sorted by name; it uniquely identifies a time series.
* Fields carry an explicit type tag so the compression layer can route each
  column to the right codec (integer vs float).
"""
from __future__ import annotations

import struct
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional, Tuple

# Field type tags. These drive both line-protocol parsing and compression.
FIELD_INT = "i"      # 64-bit signed integer
FIELD_FLOAT = "f"    # IEEE-754 double
FIELD_BOOL = "b"     # boolean
FIELD_STR = "s"      # string

# Numeric types that can be compressed with a numeric codec.
NUMERIC_TYPES = (FIELD_INT, FIELD_FLOAT)


@dataclass(frozen=True)
class Point:
    """A single measurement sample.

    ``tags`` and ``fields`` are plain dicts; the canonical series key is
    derived from them on demand so that dict ordering never affects identity.
    """

    measurement: str
    tags: Dict[str, str] = field(default_factory=dict)
    fields: Dict[str, Any] = field(default_factory=dict)
    timestamp: int = 0  # nanoseconds since epoch; 0 means "assign on write"

    def series_key(self) -> str:
        """Return the canonical InfluxDB-style series key for this point."""
        return series_key(self.measurement, self.tags)

    def field_names_sorted(self) -> Tuple[str, ...]:
        """Field names in stable (sorted) order."""
        return tuple(sorted(self.fields.keys()))


def series_key(measurement: str, tags: Dict[str, str]) -> str:
    """Build the canonical series key ``measurement[,tag1=v1][,tag2=v2]``.

    Tags are emitted sorted by name so that logically identical tag sets
    always produce one string regardless of insertion order.
    """
    if not tags:
        return measurement
    parts = [measurement] + ["{}={}".format(k, v) for k, v in sorted(tags.items())]
    return ",".join(parts)


def parse_series_key(key: str) -> Tuple[str, Dict[str, str]]:
    """Parse a series key back into ``(measurement, {tag: value})``."""
    if not key:
        raise ValueError("empty series key")
    parts = key.split(",")
    measurement = parts[0]
    tags: Dict[str, str] = {}
    for part in parts[1:]:
        name, sep, value = part.partition("=")
        if not sep:
            raise ValueError("malformed series key segment {!r}".format(part))
        tags[name] = value
    return measurement, tags


def field_type(value: Any) -> str:
    """Infer a field type tag from a Python value.

    ``bool`` is checked before ``int`` because ``bool`` is a subclass of
    ``int`` in CPython and would otherwise be misclassified as an integer.
    """
    if isinstance(value, bool):
        return FIELD_BOOL
    if isinstance(value, int):
        return FIELD_INT
    if isinstance(value, float):
        return FIELD_FLOAT
    if isinstance(value, str):
        return FIELD_STR
    raise TypeError("unsupported field value type: {!r}".format(type(value).__name__))


def coerce_field(value: Any, ftype: str) -> Any:
    """Coerce a raw parsed value into the Python type implied by ``ftype``."""
    if ftype == FIELD_INT:
        return int(value)
    if ftype == FIELD_FLOAT:
        return float(value)
    if ftype == FIELD_BOOL:
        return bool(value)
    if ftype == FIELD_STR:
        return str(value)
    raise ValueError("unknown field type {!r}".format(ftype))


def pack_field_name(name: str) -> bytes:
    """Length-prefix a UTF-8 name for on-disk storage."""
    raw = name.encode("utf-8")
    if len(raw) > 0xFFFF:
        raise ValueError("field/measurement name too long")
    return struct.pack(">H", len(raw)) + raw


def unpack_field_name(buf: bytes, offset: int):
    """Read a length-prefixed name; returns ``(name_bytes, next_offset)``."""
    (length,) = struct.unpack_from(">H", buf, offset)
    start = offset + 2
    return buf[start:start + length], start + length
