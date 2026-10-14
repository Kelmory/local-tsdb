"""InfluxQL-subset query language for the local TSDB.

Implements the standard time-series query surface (the InfluxDB *InfluxQL*
dialect) on top of :class:`~tsdb.query.QueryEngine`:

::

    SELECT <select_list> FROM <measurement>
        [WHERE <cond> (AND <cond>)*]
        [GROUP BY <group_item>, ...]
        [LIMIT <n>]

    SHOW MEASUREMENTS

* ``<select_list>`` is ``*``, bare field names, or aggregates
  ``count|sum|mean|min|max(field)``; a query containing any aggregate runs in
  *aggregation mode*.
* ``WHERE`` accepts time comparisons (``time > / >= / < / <=`` against an
  epoch integer, an RFC-3339 date string, or ``now()`` plus/minus durations
  such as ``1h``) and tag equality / inequality (``tag='v'``, ``tag!='v'``).
* ``GROUP BY`` accepts tag names and ``time(<duration>)`` buckets; combining
  both yields one row per (tag-group, time-bucket) pair.

The parser is a small tokenizer + recursive-descent parser; results are
returned as :class:`ResultSet` objects that render to InfluxDB-style CSV or
JSON for the HTTP API.
"""
from __future__ import annotations

import json
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Dict, List, Optional, Tuple

from .model import parse_series_key


class SqlError(ValueError):
    """Raised for any syntax or semantic error in a query."""


# --------------------------------------------------------------------------- #
# Tokenizer
# --------------------------------------------------------------------------- #
_PUNCT = ("!=", ">=", "<=", "(", ")", ",", "=", ">", "<", "+", "-", "*")
_KEYWORDS = {"select", "from", "where", "group", "by", "limit", "show",
            "measurements", "and", "time", "now"}


def _tokenize(text: str) -> List[Tuple[str, str]]:
    tokens: List[Tuple[str, str]] = []
    i, n = 0, len(text)
    while i < n:
        ch = text[i]
        if ch.isspace():
            i += 1
            continue
        if ch in "'\"":
            quote, i = ch, i + 1
            buf = []
            while i < n and text[i] != quote:
                if text[i] == "\\" and i + 1 < n:
                    buf.append(text[i + 1])
                    i += 2
                else:
                    buf.append(text[i])
                    i += 1
            if i >= n:
                raise SqlError("unterminated string")
            tokens.append(("str", "".join(buf)))
            i += 1
            continue
        if ch.isdigit():
            j = i
            while j < n and text[j].isdigit():
                j += 1
            tokens.append(("num", text[i:j]))
            i = j
            continue
        if ch.isalpha() or ch == "_":
            j = i
            while j < n and (text[j].isalnum() or text[j] in "_-"):
                j += 1
            word = text[i:j]
            tokens.append(("kw", word.lower()) if word.lower() in _KEYWORDS else ("ident", word))
            i = j
            continue
        for p in _PUNCT:
            if text.startswith(p, i):
                tokens.append(("punct", p))
                i += len(p)
                break
        else:
            raise SqlError("unexpected character {!r}".format(ch))
    return tokens


# --------------------------------------------------------------------------- #
# AST
# --------------------------------------------------------------------------- #
@dataclass
class SelectQuery:
    measurement: str = ""
    items: List[Tuple[str, Optional[str]]] = field(default_factory=list)  # ("field"|"agg", name/func, field?)
    start: Optional[int] = None
    end: Optional[int] = None
    tag_eq: Dict[str, str] = field(default_factory=dict)
    tag_neq: Dict[str, str] = field(default_factory=dict)
    group_tags: List[str] = field(default_factory=list)
    time_bucket: Optional[int] = None   # ns per bucket
    limit: Optional[int] = None


@dataclass
class ShowQuery:
    pass


# --------------------------------------------------------------------------- #
# Parser
# --------------------------------------------------------------------------- #
_AGG_FUNCS = {"count", "sum", "mean", "min", "max"}
# Durations tokenize as <num><unit-ident> (e.g. 1h); units in nanoseconds.
_DUR_NS = {"ns": 1, "us": 10**3, "ms": 10**6, "s": 10**9,
          "m": 60 * 10**9, "h": 3600 * 10**9, "d": 86400 * 10**9, "w": 7 * 86400 * 10**9}


class Parser:
    def __init__(self, tokens: List[Tuple[str, str]], now_ns: int):
        self.toks = tokens
        self.pos = 0
        self.now_ns = now_ns

    # -- helpers ------------------------------------------------------------ #
    def peek(self) -> Optional[Tuple[str, str]]:
        return self.toks[self.pos] if self.pos < len(self.toks) else None

    def next(self):
        tok = self.peek()
        if tok is None:
            raise SqlError("unexpected end of query")
        self.pos += 1
        return tok

    def expect_punct(self, p: str) -> None:
        kind, val = self.next()
        if kind != "punct" or val != p:
            raise SqlError("expected {!r} after {!r}".format(p, val))

    def expect_kw(self, kw: str) -> None:
        kind, val = self.next()
        if kind != "kw" or val != kw:
            raise SqlError("expected keyword {}".format(kw))

    # -- entry --------------------------------------------------------------- #
    def parse(self):
        kind, val = self.peek()[0], self.peek()[1]
        if kind == "kw" and val == "show":
            self.next()
            self.expect_kw("measurements")
            self._eof()
            return ShowQuery()
        self.expect_kw("select")
        q = SelectQuery()
        # select list
        tok = self.peek()
        if tok[0] == "punct" and tok[1] == "*":
            self.next()
            q.items.append(("star", None))
        else:
            while True:
                kind, name = self.next()
                if kind not in ("ident",):
                    raise SqlError("expected field or aggregate, got {!r}".format(name))
                nxt = self.peek()
                if nxt and nxt[0] == "punct" and nxt[1] == "(":
                    # aggregate: func(field) -- count() may omit the field.
                    if name not in _AGG_FUNCS:
                        raise SqlError("unknown function {!r}".format(name))
                    self.next()  # (
                    fname = None
                    inner = self.peek()
                    if not (inner and inner[0] == "punct" and inner[1] == ")"):
                        fkind, fname = self.next()
                        if fkind != "ident":
                            raise SqlError("expected field name in {}()".format(name))
                    self.expect_punct(")")
                    q.items.append(("agg", name, fname))
                else:
                    q.items.append(("field", None, name))
                nxt = self.peek()
                if nxt and nxt[0] == "punct" and nxt[1] == ",":
                    self.next()
                    continue
                break
        # FROM measurement
        self.expect_kw("from")
        kind, val = self.next()
        if kind != "ident":
            raise SqlError("expected measurement name, got {!r}".format(val))
        q.measurement = val
        # optional WHERE / GROUP BY / LIMIT (WHERE first per InfluxQL)
        tok = self.peek()
        if tok and tok[0] == "kw" and tok[1] == "where":
            self.next()
            while True:
                self._parse_cond(q)
                tok = self.peek()
                if tok and tok[0] == "kw" and tok[1] == "and":
                    self.next()
                    continue
                break
        tok = self.peek()
        if tok and tok[0] == "kw" and tok[1] == "group":
            self.next()
            self.expect_kw("by")
            while True:
                kind, val = self.next()
                if kind == "ident":
                    q.group_tags.append(val)
                elif kind == "kw" and val == "time":
                    self.expect_punct("(")
                    q.time_bucket = self._read_duration()
                    self.expect_punct(")")
                else:
                    raise SqlError("bad GROUP BY item {!r}".format(val))
                tok = self.peek()
                if tok and tok[0] == "punct" and tok[1] == ",":
                    self.next()
                    continue
                break
        tok = self.peek()
        if tok and tok[0] == "kw" and tok[1] == "limit":
            self.next()
            kind, val = self.next()
            if kind != "num":
                raise SqlError("expected integer after LIMIT")
            q.limit = int(val)
        self._eof()
        return q

    def _eof(self):
        if self.peek() is not None:
            raise SqlError("trailing tokens {!r}".format(self.toks[self.pos:]))

    # -- WHERE conditions ----------------------------------------------------- #
    def _parse_cond(self, q: SelectQuery) -> None:
        kind, val = self.next()
        if kind == "kw" and val == "time":
            pkind, pop = self.next()
            if pkind != "punct" or pop not in (">", ">=", "<", "<="):
                raise SqlError("bad time comparison {!r}".format(pop))
            ts = self._parse_time_expr()
            # Strict inequalities over the integer-ns domain: shift by one tick.
            if pop == ">":
                lo, hi = ts + 1, None
            elif pop == ">=":
                lo, hi = ts, None
            elif pop == "<":
                lo, hi = None, ts - 1
            else:
                lo, hi = None, ts
            if lo is not None:
                q.start = lo if q.start is None else max(q.start, lo)
            if hi is not None:
                q.end = hi if q.end is None else min(q.end, hi)
        elif kind == "ident":
            pkind, pop = self.next()
            if pkind != "punct" or pop not in ("=", "!="):
                raise SqlError("expected = or != after tag name")
            skind, sval = self.next()
            if skind != "str":
                raise SqlError("tag value must be a string literal")
            (q.tag_eq if pop == "=" else q.tag_neq)[val] = sval
        else:
            raise SqlError("expected time or tag condition, got {!r}".format(val))

    def _parse_time_expr(self) -> int:
        kind, val = self.next()
        base: Optional[int] = None
        if kind == "num":
            return int(val)
        if kind == "str":
            # RFC-3339 date (optionally with fractional seconds / offset).
            try:
                dt = datetime.fromisoformat(val.replace("Z", "+00:00"))
            except ValueError:
                raise SqlError("bad date string {!r}".format(val))
            if dt.tzinfo is None:
                dt = dt.replace(tzinfo=timezone.utc)
            return int(dt.timestamp() * 10**9)
        if kind == "kw" and val == "now":
            self.expect_punct("(")
            self.expect_punct(")")
            base = self.now_ns
        else:
            raise SqlError("expected time expression, got {!r}".format(val))
        # optional +/- duration adjustments (e.g. now() - 1h)
        while True:
            tok = self.peek()
            if not tok or tok[0] != "punct" or tok[1] not in ("+", "-"):
                break
            sign = 1 if tok[1] == "+" else -1
            self.next()
            base += sign * self._read_duration()
        return base

    def _read_duration(self) -> int:
        """Consume ``<num> <unit>`` (e.g. ``1h``) and return nanoseconds."""
        kind, val = self.next()
        if kind != "num":
            raise SqlError("expected duration like 1h")
        unit = ""
        nxt = self.peek()
        if nxt and nxt[0] == "ident" and len(nxt[1]) <= 2:
            self.next()
            unit = nxt[1]
        unit = unit or "ns"  # a bare number is nanoseconds (e.g. time(100))
        if unit not in _DUR_NS:
            raise SqlError("bad duration unit {!r}".format(unit))
        return int(val) * _DUR_NS[unit]


def parse_query(sql: str, now_ns: Optional[int] = None) -> SelectQuery:
    """Parse one InfluxQL-subset statement."""
    if not isinstance(sql, str):
        raise SqlError("query must be a string")
    tokens = _tokenize(sql.strip())
    if not tokens:
        raise SqlError("empty query")
    return Parser(tokens, now_ns if now_ns is not None else time.time_ns()).parse()


# --------------------------------------------------------------------------- #
# Result set
# --------------------------------------------------------------------------- #
class ResultSet:
    """Columnar result of a query; renders to InfluxDB-style CSV or JSON."""

    def __init__(self, columns: List[str], rows: List[Dict]):
        self.columns = list(columns)
        self.rows = [dict(r) for r in rows]

    @property
    def n_rows(self) -> int:
        return len(self.rows)

    def to_csv(self) -> str:
        out = [",".join(_csv_cell(c) for c in self.columns)]
        for row in self.rows:
            out.append(",".join(_csv_cell(row.get(c, "")) for c in self.columns))
        return "\n".join(out) + "\n"

    def to_json(self) -> str:
        return json.dumps({"columns": self.columns, "rows": self.rows}, default=str)


def _csv_cell(v) -> str:
    if v is None:
        return ""
    s = v if isinstance(v, str) else (repr(v) if isinstance(v, float) else str(v))
    if any(ch in s for ch in (",", '"', "\n")) or s == "":
        s = '"' + s.replace('"', '""') + '"'
    return s


# --------------------------------------------------------------------------- #
# Executor
# --------------------------------------------------------------------------- #
class SQLExecutor:
    """Executes parsed queries against a :class:`~tsdb.query.QueryEngine`."""

    def __init__(self, engine):
        self.engine = engine

    # -- public ------------------------------------------------------------- #
    def execute(self, sql: str) -> ResultSet:
        q = parse_query(sql)
        if isinstance(q, ShowQuery):
            return ResultSet(["name"], [{"name": m} for m in self.engine.index.measurements()])
        has_agg = any(it[0] == "agg" for it in q.items)
        if has_agg and any(it[0] == "star" or (it[0] == "field") for it in q.items):
            raise SqlError("cannot mix aggregates with bare fields / *")
        if not has_agg and (q.group_tags or q.time_bucket is not None):
            raise SqlError("GROUP BY requires an aggregate select list")
        return self._run_select(q, has_agg)

    # -- SELECT ------------------------------------------------------------- #
    def _candidate_series(self, q: SelectQuery) -> List[str]:
        keys = self.engine.match_series(q.measurement, q.tag_eq)
        if not q.tag_neq:
            return keys
        out = []
        for key in keys:
            _, tags = parse_series_key(key)
            if any(tags.get(k) == v for k, v in q.tag_neq.items()):
                continue
            out.append(key)
        return out

    def _run_select(self, q: SelectQuery, has_agg: bool) -> ResultSet:
        if not has_agg:
            return self._run_scan(q)
        return self._run_aggregate(q)

    def _run_scan(self, q: SelectQuery) -> ResultSet:
        # With tag != filters the LIMIT must be applied *after* filtering,
        # so fetch unbounded here and slice below.
        points = self.engine.select(measurement=q.measurement, tag_filters=q.tag_eq or None,
                                    start=q.start, end=q.end,
                                    limit=None if q.tag_neq else q.limit)
        if q.tag_neq:
            points = [p for p in points
                      if not any(p.tags.get(k) == v for k, v in q.tag_neq.items())]
            if q.limit is not None:
                points = points[:q.limit]
        tag_cols = self._tag_columns([p.series_key() for p in points])
        field_names = sorted({name for p in points for name in p.fields})
        columns = ["name"] + tag_cols + ["time"] + field_names
        rows = []
        for p in points:
            row = {"name": p.measurement, "time": p.timestamp}
            row.update(p.tags)
            row.update({k: v for k, v in p.fields.items() if k in field_names})
            rows.append(row)
        return ResultSet(columns, rows)

    def _run_aggregate(self, q: SelectQuery) -> ResultSet:
        # acc[(group_tuple, bucket)] = {field: [values]} plus row counts
        groups: Dict[Tuple, Dict[str, list]] = {}
        counts: Dict[Tuple, int] = {}
        for key in self._candidate_series(q):
            _, tags = parse_series_key(key)
            gprefix = tuple(tags.get(t, "") for t in q.group_tags)
            for ts, fvals in self.engine.iter_points(key, q.start, q.end):
                bucket = (ts // q.time_bucket * q.time_bucket) if q.time_bucket else 0
                gk = gprefix + (bucket,)
                counts[gk] = counts.get(gk, 0) + 1
                cols = groups.setdefault(gk, {})
                for it in q.items:
                    _, func, fname = it
                    if fname is not None:
                        v = fvals.get(fname)
                        if v is not None:      # count/sum/... skip NULLs
                            cols.setdefault(fname, []).append(v)
        # Build rows.
        tag_cols = list(q.group_tags)
        columns = (["time"] if q.time_bucket else []) + tag_cols + [_agg_col(it) for it in q.items]
        rows = []
        for gk in sorted(groups):
            row: Dict = {}
            if q.time_bucket:
                row["time"] = gk[-1]
                gtags = gk[:-1]
            else:
                gtags = gk
            for t, v in zip(q.group_tags, gtags):
                row[t] = v
            cols = groups[gk]
            for it in q.items:
                _, func, fname = it
                if func == "count":
                    # count() counts rows; count(field) counts non-null values.
                    rows_val = counts.get(gk, 0) if fname is None else len(cols.get(fname, ()))
                else:
                    vals = cols.get(fname, [])
                    rows_val = _fold(func, vals)
                row[_agg_col(("agg", func, fname))] = rows_val
            rows.append(row)
        return ResultSet(columns, rows)

    def _tag_columns(self, series_keys: List[str]) -> List[str]:
        names = set()
        for key in series_keys:
            _, tags = parse_series_key(key)
            names.update(tags.keys())
        return sorted(names)


def _agg_col(item) -> str:
    _, func, fname = item
    if fname is None or fname == "":
        return "count"
    return "{}_{}".format(func, fname)


def _fold(func: str, values: list):
    if not values:
        return None
    if func == "sum":
        return sum(values)
    if func == "mean":
        return sum(values) / len(values)
    if func == "min":
        return min(values)
    if func == "max":
        return max(values)
    raise SqlError("unknown aggregate {!r}".format(func))
