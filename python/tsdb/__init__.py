"""``tsdb`` -- a local, single-file time-series database.

A small, dependency-free (Python standard library only) implementation that
borrows three ideas:

* **SQLite's single-file model** -- one self-contained file of fixed-size pages
  with a header page, a freelist and a physical write-ahead log for durability;
* **InfluxDB's storage model** -- points are ``measurement + tags + fields +
  timestamp``; samples live in per-series columnar *data blocks* compressed with
  Gorilla-style codecs (float XOR, integer delta-of-delta) and pruned by time
  bounds like TSM blocks;
* **the standard TSDB interface** -- InfluxDB line-protocol ingestion plus a
  select/aggregate query surface.

Quick start::

    from tsdb import TimeSeriesDB, Point

    with TimeSeriesDB("cpu.ltdb") as db:
        db.write([Point("cpu", {"host": "a"}, {"usage": 0.67}, 1_700_000_000_000)])
        db.commit()
        rows = db.select(measurement="cpu", tag_filters={"host": "a"})

See :mod:`tsdb.core` for the full API and ``demo/demo.py`` for a worked example.
"""
from .block import DataBlock
from .compression import (decode_floats, decode_ints_dod, encode_floats, encode_ints_dod)
from .core import TimeSeriesDB, line_protocol
from .http_server import TSDBHTTPServer, ingest
from .index import Index
from .model import (FIELD_BOOL, FIELD_FLOAT, FIELD_INT, FIELD_STR, Point, field_type,
                    parse_series_key, series_key)
from .protocol import LineProtocolError, format_point, parse_line
from .query import QueryEngine
from .sql import ResultSet, SQLExecutor, SqlError, parse_query
from .storage import PAGE_SIZE, PageStore, StorageError

__all__ = [
    "TimeSeriesDB", "Point", "DataBlock", "Index", "QueryEngine", "PageStore",
    "parse_line", "format_point", "LineProtocolError", "line_protocol",
    "series_key", "parse_series_key", "field_type",
    "encode_floats", "decode_floats", "encode_ints_dod", "decode_ints_dod",
    "FIELD_INT", "FIELD_FLOAT", "FIELD_BOOL", "FIELD_STR",
    "PAGE_SIZE", "StorageError",
    "SQLExecutor", "parse_query", "SqlError", "ResultSet",
    "TSDBHTTPServer", "ingest",
]

__version__ = "1.0.0"
