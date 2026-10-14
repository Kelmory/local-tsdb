"""Series / tag index.

The index is the in-memory map from a canonical series key to the list of data
blocks that hold its samples, plus derived lookup structures:

* ``measurements`` -- every measurement name ever written;
* an inverted *tag index* mapping each ``(tag_name, tag_value)`` pair to the
  set of series keys carrying it, enabling fast "which series have
  ``region=us-east``" lookups.

Only the ``series_key -> [block refs]`` table is persisted (a single blob); the
measurement and tag structures are rebuilt from the parsed series keys on load,
so there is exactly one source of truth for identity.
"""
from __future__ import annotations

import struct
from typing import Dict, List, Set, Tuple

from .model import parse_series_key, series_key as make_key

# A block reference: (locator page number, min_ts, max_ts).
BlockRef = Tuple[int, int, int]


class Index:
    def __init__(self) -> None:
        self.series_blocks: Dict[str, List[BlockRef]] = {}
        self._measurements: Set[str] = set()
        self._tags: Dict[Tuple[str, str], Set[str]] = {}

    # -- mutation ----------------------------------------------------------- #
    def add_series(self, measurement: str, tags: Dict[str, str]) -> str:
        key = make_key(measurement, tags)
        if key not in self.series_blocks:
            self.series_blocks[key] = []
            self._measurements.add(measurement)
            for name, value in tags.items():
                self._tags.setdefault((name, value), set()).add(key)
        return key

    def add_block(self, series_key: str, locator: int, min_ts: int, max_ts: int) -> None:
        if series_key not in self.series_blocks:
            raise KeyError("unknown series {!r}".format(series_key))
        self.series_blocks[series_key].append((locator, min_ts, max_ts))

    # -- lookups ------------------------------------------------------------ #
    def blocks(self, series_key: str) -> List[BlockRef]:
        return list(self.series_blocks.get(series_key, ()))

    def measurements(self) -> List[str]:
        return sorted(self._measurements)

    def all_series(self) -> List[str]:
        return sorted(self.series_blocks.keys())

    def series_for_tags(self, tag_filters: Dict[str, str]) -> Set[str]:
        """Series keys matching *every* supplied ``tag_name=tag_value`` pair."""
        if not tag_filters:
            return set(self.series_blocks.keys())
        result = None
        for name, value in tag_filters.items():
            s = self._tags.get((name, value), set())
            result = set(s) if result is None else (result & s)
        return result or set()

    # -- persistence -------------------------------------------------------- #
    def to_bytes(self) -> bytes:
        out = bytearray()
        items = sorted(self.series_blocks.items())
        out += struct.pack(">I", len(items))
        for key, blocks in items:
            kb = key.encode("utf-8")
            out += struct.pack(">H", len(kb)) + kb
            out += struct.pack(">I", len(blocks))
            for locator, min_ts, max_ts in blocks:
                out += struct.pack("<qqq", locator, min_ts, max_ts)
        return bytes(out)

    @classmethod
    def from_bytes(cls, data: bytes) -> "Index":
        idx = cls()
        if not data:
            return idx
        off = 0

        def u16():
            nonlocal off
            v = struct.unpack_from(">H", data, off)[0]
            off += 2
            return v

        count = struct.unpack_from(">I", data, off)[0]
        off += 4
        for _ in range(count):
            kl = u16()
            key = data[off:off + kl].decode("utf-8")
            off += kl
            measurement, tags = parse_series_key(key)
            idx.add_series(measurement, tags)
            bcount = struct.unpack_from(">I", data, off)[0]
            off += 4
            for _ in range(bcount):
                locator, min_ts, max_ts = struct.unpack_from("<qqq", data, off)
                off += 24
                idx.series_blocks[key].append((locator, min_ts, max_ts))
        return idx
