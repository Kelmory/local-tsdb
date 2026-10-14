"""Public database facade.

:class:`TimeSeriesDB` wires the storage engine, index, data blocks and query
engine together behind a small, InfluxDB-flavoured API:

* :meth:`write` / :meth:`write_line` buffer points into an in-memory *memtable*;
* :meth:`commit` flushes each series' buffered points to a new on-disk data
  block (Gorilla-compressed), updates the index, and durably persists both via
  the page store's write-ahead log;
* :meth:`select` / :meth:`aggregate` read through the query engine.

Data blocks are immutable once written (new samples for a series open a *new*
block rather than rewriting an old one), so decoded blocks can be cached for the
life of the process without invalidation logic.
"""
from __future__ import annotations

import time
from typing import Dict, List, Optional

from .block import DataBlock
from .index import Index
from .model import Point
from .protocol import LineProtocolError, format_point, parse_line
from .query import QueryEngine
from .sql import SQLExecutor
from .storage import PageStore


class TimeSeriesDB:
    def __init__(self, path: str):
        self.path = path
        self._store = PageStore(path)
        root = self._store.header.root_index_page
        if root != -1 and root is not None:
            self.index = Index.from_bytes(self._store.read_blob(root))
        else:
            self.index = Index()
        self.memtable: Dict[str, List[Point]] = {}
        self._block_cache: Dict[int, DataBlock] = {}
        self._engine = QueryEngine(self.index, self._read_block, self.memtable)

    # -- block access ------------------------------------------------------- #
    def _read_block(self, locator: int) -> DataBlock:
        blk = self._block_cache.get(locator)
        if blk is None:
            blk = DataBlock.from_bytes(self._store.read_blob(locator))
            self._block_cache[locator] = blk
        return blk

    # -- writes ------------------------------------------------------------- #
    def write(self, points) -> int:
        """Buffer ``points`` (iterable of :class:`Point`) for the next commit."""
        now = time.time_ns()
        count = 0
        for p in points:
            ts = p.timestamp if p.timestamp else now
            pt = Point(p.measurement, dict(p.tags), dict(p.fields), int(ts))
            key = self.index.add_series(pt.measurement, pt.tags)
            self.memtable.setdefault(key, []).append(pt)
            count += 1
        return count

    def write_line(self, line: str) -> Point:
        """Parse one line-protocol string and buffer the resulting point."""
        p = parse_line(line)
        self.write([p])
        return p

    def write_lines(self, text: str) -> int:
        """Buffer every non-empty, non-comment line in ``text``."""
        count = 0
        for raw in text.splitlines():
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            self.write_line(line)
            count += 1
        return count

    def commit(self) -> None:
        """Flush the memtable to disk blocks and persist index + data atomically."""
        if not self.memtable:
            return
        store = self._store
        store.begin()
        # Free the previous index blob *first* so its pages can be reused by the
        # allocations below (staged as clean full-page images).
        old_root = store.header.root_index_page
        if old_root != -1 and old_root is not None:
            store.free_blob(old_root)
        for key, pts in self.memtable.items():
            blk = DataBlock.from_points(pts)
            locator = store.write_blob(blk.to_bytes())
            self.index.add_block(key, locator, blk.min_ts, blk.max_ts)
        new_root = store.write_blob(self.index.to_bytes())
        store.header.root_index_page = new_root
        store.commit()
        self.memtable.clear()

    # -- reads -------------------------------------------------------------- #
    def select(self, measurement: Optional[str] = None, tag_filters: Optional[Dict[str, str]] = None,
               fields: Optional[List[str]] = None, start: Optional[int] = None, end: Optional[int] = None,
               limit: Optional[int] = None) -> List[Point]:
        return self._engine.select(measurement=measurement, tag_filters=tag_filters,
                                   fields=fields, start=start, end=end, limit=limit)

    def aggregate(self, measurement: str, field: str, func: str = "count",
                  tag_filters: Optional[Dict[str, str]] = None, start: Optional[int] = None,
                  end: Optional[int] = None, group_by: Optional[str] = None):
        return self._engine.aggregate(measurement=measurement, field=field, func=func,
                                      tag_filters=tag_filters, start=start, end=end, group_by=group_by)

    def sql(self) -> SQLExecutor:
        """Return an InfluxQL-subset executor bound to this database."""
        return SQLExecutor(self._engine)

    def measurements(self) -> List[str]:
        return self.index.measurements()

    def series(self, measurement: Optional[str] = None) -> List[str]:
        keys = self.index.all_series()
        if measurement is None:
            return keys
        prefix = measurement + ","
        return [k for k in keys if k == measurement or k.startswith(prefix)]

    # -- lifecycle ---------------------------------------------------------- #
    def close(self) -> None:
        self._store.close()

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb):
        self.close()
        return False


def line_protocol(points: List[Point]) -> str:
    """Serialise many points to a multi-line line-protocol string."""
    return "\n".join(format_point(p) for p in points)
