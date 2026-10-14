"""Data-block codec.

A *data block* is the on-disk unit for one time series over a contiguous run
of samples -- the analogue of an InfluxDB TSM block. It stores:

* the shared timestamp column (delta-of-delta compressed), and
* one column per field, each routed to the right codec by its type tag
  (float -> Gorilla XOR, int -> delta-of-delta, bool/str -> raw).

Blocks also carry ``min_ts`` / ``max_ts`` so the query engine can prune whole
blocks that fall outside a requested time range without decoding them.

The layout is self-describing and length-prefixed so it can be read back
without any out-of-band metadata:

::

    u16  nfields
    (name_len u16, name bytes, ftype u8) * nfields      # sorted by name
    i32  point_count
    i64  min_ts
    i64  max_ts
    u32  ts_bytes_len ; bytes ts_compressed            # encode_ints_dod
    (u32 col_bytes_len ; null_bitmap ; bytes col_data) * nfields
    # null_bitmap is ceil(point_count/8) bytes, LSB-first; bit i = 1 means
    # row i has a value, 0 means NULL (col_data holds a zero placeholder
    # there to keep the codec position-aligned).
"""
from __future__ import annotations

import struct
from typing import Dict, List, Optional

from .compression import decode_floats, decode_ints_dod, encode_floats, encode_ints_dod
from .model import FIELD_BOOL, FIELD_FLOAT, FIELD_INT, FIELD_STR, Point, field_type

# ftype byte values (kept distinct from the ASCII tags for on-disk clarity).
_TYPE_TAG = {FIELD_INT: 1, FIELD_FLOAT: 2, FIELD_BOOL: 3, FIELD_STR: 4}
_TAG_TYPE = {v: k for k, v in _TYPE_TAG.items()}

_ZERO = {FIELD_INT: 0, FIELD_FLOAT: 0.0, FIELD_BOOL: False, FIELD_STR: ""}


def _pack_nulls(present: List[bool]) -> bytes:
    """LSB-first presence bitmap; bit i = 1 means value i is present."""
    out = bytearray((len(present) + 7) // 8)
    for i, ok in enumerate(present):
        if ok:
            out[i >> 3] |= 1 << (i & 7)
    return bytes(out)


class DataBlock:
    """An in-memory columnar block for a single series."""

    def __init__(self) -> None:
        self.timestamps: List[int] = []
        self._cols: Dict[str, list] = {}
        self._types: Dict[str, str] = {}
        self._nulls: Dict[str, List[bool]] = {}
        self._field_order: List[str] = []

    # -- construction ------------------------------------------------------- #
    @classmethod
    def from_points(cls, points: List[Point]) -> "DataBlock":
        """Build a block from same-series ``points``.

        Points are sorted by timestamp first (callers may ingest out of
        order), so blocks are always stored in time order and ``min_ts`` /
        ``max_ts`` are the true time bounds.  The sort is stable.

        Duplicate timestamps are merged into one row: later points win
        field-by-field and the field sets are unioned (InfluxDB-style
        last-write-wins).

        The union of field names becomes the column set.  A field absent on
        an individual point is stored as NULL (per-column presence bitmap),
        so aggregates see InfluxDB-style null semantics instead of zero
        fills.  Conflicting value types for one field (e.g. int then float)
        raise :class:`TypeError` rather than being silently coerced.
        """
        points = sorted(points, key=lambda p: p.timestamp)
        merged: List[Point] = []
        for p in points:
            if merged and merged[-1].timestamp == p.timestamp:
                prev = merged[-1]
                fields_ = dict(prev.fields)
                fields_.update(p.fields)
                merged[-1] = Point(prev.measurement, prev.tags, fields_, p.timestamp)
            else:
                merged.append(p)
        points = merged

        blk = cls()
        fields: List[str] = []
        for p in points:
            for name in p.fields:
                if name not in fields:
                    fields.append(name)
        fields.sort()
        # Infer a stable type per field from the first concrete value seen,
        # rejecting conflicting types on the way.
        for name in fields:
            ftype = None
            for p in points:
                v = p.fields.get(name)
                if v is None:
                    continue
                t = field_type(v)
                if ftype is None:
                    ftype = t
                elif t != ftype:
                    raise TypeError(
                        "field type conflict for {!r}: cannot mix {} and {} "
                        "values in one block".format(name, ftype, t))
            blk._types[name] = ftype or FIELD_STR
            blk._cols[name] = []
            blk._nulls[name] = []
        blk._field_order = fields

        for p in points:
            blk.timestamps.append(int(p.timestamp))
            for name in fields:
                val = p.fields.get(name)
                if val is None:
                    # Zero placeholder keeps the column codec position-
                    # aligned; the presence bitmap marks the slot NULL.
                    blk._nulls[name].append(True)
                    val = _ZERO[blk._types[name]]
                else:
                    blk._nulls[name].append(False)
                blk._cols[name].append(val)
        return blk

    # -- accessors ---------------------------------------------------------- #
    @property
    def field_names(self) -> List[str]:
        return list(self._field_order)

    def field_type(self, name: str) -> str:
        return self._types[name]

    def __len__(self) -> int:
        return len(self.timestamps)

    @property
    def min_ts(self) -> Optional[int]:
        return self.timestamps[0] if self.timestamps else None

    @property
    def max_ts(self) -> Optional[int]:
        return self.timestamps[-1] if self.timestamps else None

    # -- serialisation ------------------------------------------------------ #
    def to_bytes(self) -> bytes:
        out = bytearray()
        fields = self._field_order
        out += struct.pack(">H", len(fields))
        for name in fields:
            raw = name.encode("utf-8")
            out += struct.pack(">H", len(raw)) + raw + struct.pack("B", _TYPE_TAG[self._types[name]])
        count = len(self.timestamps)
        out += struct.pack(">i", count)
        if count:
            out += struct.pack("<qq", self.timestamps[0], self.timestamps[-1])
        else:
            out += struct.pack("<qq", 0, 0)
        # Timestamp column.
        ts_bytes = encode_ints_dod(self.timestamps)
        out += struct.pack(">I", len(ts_bytes)) + ts_bytes
        # Field columns in the same order as the descriptors.  Each column is
        # ``u32 len ; null bitmap (ceil(count/8) bytes) ; codec data``.
        for name in fields:
            body = _pack_nulls(self._nulls[name]) + self._encode_column(name, self._cols[name])
            out += struct.pack(">I", len(body)) + body
        return bytes(out)

    def _encode_column(self, name: str, values: list) -> bytes:
        ftype = self._types[name]
        if ftype == FIELD_FLOAT:
            return encode_floats([float(v) for v in values])
        if ftype == FIELD_INT:
            return encode_ints_dod([int(v) for v in values])
        # bool / str -> raw, length-prefixed per value.
        out = bytearray()
        for v in values:
            if ftype == FIELD_BOOL:
                out += struct.pack("B", 1 if v else 0)
            else:
                b = str(v).encode("utf-8")
                out += struct.pack(">I", len(b)) + b
        return bytes(out)

    @classmethod
    def from_bytes(cls, data: bytes) -> "DataBlock":
        off = 0

        def u16():
            nonlocal off
            v = struct.unpack_from(">H", data, off)[0]
            off += 2
            return v

        def i32():
            nonlocal off
            v = struct.unpack_from(">i", data, off)[0]
            off += 4
            return v

        nfields = u16()
        names: List[str] = []
        types: Dict[str, str] = {}
        for _ in range(nfields):
            nl = u16()
            name = data[off:off + nl].decode("utf-8")
            off += nl
            tag = struct.unpack_from("B", data, off)[0]
            off += 1
            names.append(name)
            types[name] = _TAG_TYPE[tag]
        count = i32()
        min_ts, max_ts = struct.unpack_from("<qq", data, off)
        off += 16

        ts_len = struct.unpack_from(">I", data, off)[0]
        off += 4
        ts_bytes = data[off:off + ts_len]
        off += ts_len
        timestamps = decode_ints_dod(ts_bytes)[:count] if count else []

        cols: Dict[str, list] = {}
        nulls: Dict[str, List[bool]] = {}
        for name in names:
            col_len = struct.unpack_from(">I", data, off)[0]
            off += 4
            chunk = data[off:off + col_len]
            off += col_len
            nbitmap = (count + 7) // 8
            bitmap, col_data = chunk[:nbitmap], chunk[nbitmap:]
            cols[name] = cls._decode_column(types[name], col_data, count)
            nulls[name] = [bool((bitmap[i >> 3] >> (i & 7)) & 1)
                           for i in range(count)]

        blk = cls()
        blk.timestamps = timestamps
        blk._field_order = names
        blk._types = types
        blk._cols = cols
        blk._nulls = nulls
        return blk

    @staticmethod
    def _decode_column(ftype: str, data: bytes, count: int) -> list:
        if ftype == FIELD_FLOAT:
            return decode_floats(data, count)
        if ftype == FIELD_INT:
            return decode_ints_dod(data)[:count]
        out = []
        off = 0
        for _ in range(count):
            if ftype == FIELD_BOOL:
                (b,) = struct.unpack_from("B", data, off)
                off += 1
                out.append(bool(b))
            else:
                (ln,) = struct.unpack_from(">I", data, off)
                off += 4
                out.append(data[off:off + ln].decode("utf-8"))
                off += ln
        return out

    # -- row access --------------------------------------------------------- #
    def rows(self):
        """Yield ``(timestamp, {field: value})`` tuples in time order.

        Absent fields come back as ``None`` (each column stores a zero
        placeholder at those positions to keep its codec position-aligned).
        """
        for i, ts in enumerate(self.timestamps):
            yield ts, {name: (None if self._nulls[name][i] else self._cols[name][i])
                       for name in self._field_order}
