"""Query engine.

Sits on top of the :class:`~tsdb.index.Index`, a block reader and an optional
*memtable* (points written but not yet flushed to disk). It resolves which series
match a measurement name plus tag filters, prunes whole data blocks whose time
bounds fall outside the requested range (the TSM-style optimisation), decodes
only the surviving rows, merges in any uncommitted memtable points and applies
projection / aggregation.

The public surface is intentionally small: :meth:`QueryEngine.select` returns
points; :meth:`QueryEngine.aggregate` folds them into a scalar or per-tag-group
result.
"""
from __future__ import annotations

import math
from typing import Callable, Dict, Iterable, List, Optional

from .model import Point


class QueryEngine:
    def __init__(self, index, read_block: Callable[[int], object], memtable: dict):
        self.index = index
        self._read_block = read_block          # locator -> DataBlock
        self.memtable = memtable               # series_key -> list[Point]

    # -- series resolution -------------------------------------------------- #
    def match_series(self, measurement: Optional[str] = None, tag_filters: Optional[Dict[str, str]] = None) -> List[str]:
        """Public wrapper: sorted series keys matching ``measurement`` + tags."""
        return self._matching_series(measurement, tag_filters or {})

    def _matching_series(self, measurement: Optional[str], tag_filters: Dict[str, str]) -> List[str]:
        keys = self.index.series_for_tags(tag_filters or {})
        if not keys:
            return []
        result = [k for k in keys if (measurement is None or k == measurement or k.startswith(measurement + ","))]
        # If a specific measurement was given but no tag filter, restrict to that
        # measurement's series exactly.
        if measurement is not None and not tag_filters:
            result = [k for k in self.index.all_series() if k == measurement or k.startswith(measurement + ",")]
        return sorted(result)

    def iter_points(self, series_key: str, start: Optional[int] = None, end: Optional[int] = None):
        """Public wrapper over :meth:`_iter_rows` for external consumers."""
        yield from self._iter_rows(series_key, start, end)

    def _iter_rows(self, series_key: str, start: Optional[int], end: Optional[int]):
        """Yield ``(ts, fields)`` from disk blocks (pruned) then memtable."""
        for locator, bmin, bmax in self.index.blocks(series_key):
            if start is not None and bmax < start:
                continue                       # whole block before range
            if end is not None and bmin > end:
                continue                       # whole block after range
            blk = self._read_block(locator)
            for ts, fields in blk.rows():
                if (start is None or ts >= start) and (end is None or ts <= end):
                    yield ts, fields
        for p in self.memtable.get(series_key, ()):
            ts = p.timestamp
            if (start is None or ts >= start) and (end is None or ts <= end):
                yield ts, dict(p.fields)

    # -- select ------------------------------------------------------------- #
    def select(self, measurement: Optional[str] = None, tag_filters: Optional[Dict[str, str]] = None,
               fields: Optional[List[str]] = None, start: Optional[int] = None, end: Optional[int] = None,
               limit: Optional[int] = None) -> List[Point]:
        """Return matching points ordered by series then timestamp."""
        tag_filters = tag_filters or {}
        out: List[Point] = []
        for key in self._matching_series(measurement, tag_filters):
            measurement_name, tags = _split_key(key)
            for ts, fvals in self._iter_rows(key, start, end):
                if fields is not None:
                    fvals = {k: v for k, v in fvals.items() if k in fields}
                out.append(Point(measurement=measurement_name, tags=dict(tags), fields=fvals, timestamp=ts))
                if limit is not None and len(out) >= limit:
                    return out
        # Stable time ordering across series.
        out.sort(key=lambda p: (p.series_key(), p.timestamp))
        return out

    # -- aggregate ---------------------------------------------------------- #
    def aggregate(self, measurement: str, field: str, func: str = "count",
                  tag_filters: Optional[Dict[str, str]] = None, start: Optional[int] = None,
                  end: Optional[int] = None, group_by: Optional[str] = None):
        """Fold matching rows into a scalar (or per-tag-group dict).

        ``func`` is one of ``count|min|max|sum|mean``. With ``group_by`` set to
        a tag name the result maps each distinct value of that tag to its fold.
        """
        funcs = {"count", "min", "max", "sum", "mean"}
        if func not in funcs:
            raise ValueError("unknown aggregate {!r}".format(func))

        def _finish(acc):
            if acc is None:
                return None
            if func == "mean":
                s, n = acc
                return s / n if n else None
            return acc

        tag_filters = tag_filters or {}
        values: List[float] = []
        groups: Dict[str, list] = {}
        for key in self._matching_series(measurement, tag_filters):
            _, tags = _split_key(key)
            gval = tags.get(group_by, "") if group_by is not None else None
            for ts, fvals in self._iter_rows(key, start, end):
                v = fvals.get(field)
                if v is None:
                    continue           # NULL / absent fields never fold in
                if group_by is not None:
                    groups.setdefault(gval, []).append(v)
                else:
                    values.append(v)
        if group_by is not None:
            return {g: _finish(_reduce(func, vals)) for g, vals in sorted(groups.items())}
        return _finish(_reduce(func, values))


def _reduce(func, values):
    if func == "count":
        return len(values)
    it = iter(values)
    try:
        first = next(it)
    except StopIteration:
        return (0, 0) if func == "mean" else None
    acc = first if func in ("min", "max") else ((first, 1) if func == "mean" else first)
    for v in it:
        acc = _step(func, acc, v)
    return acc


def _step(func, acc, v):
    if func == "min":
        return min(acc, v)
    if func == "max":
        return max(acc, v)
    if func == "sum":
        return acc + v
    s, n = acc
    return (s + v, n + 1)


def _split_key(key: str):
    parts = key.split(",")
    measurement = parts[0]
    tags = {}
    for part in parts[1:]:
        name, _, value = part.partition("=")
        tags[name] = value
    return measurement, tags
