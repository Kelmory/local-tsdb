# local-tsdb

A **single-file, zero-dependency time-series database** with an
**InfluxDB-compatible** write and query surface.

`local-tsdb` bundles everything a time-series store normally spreads across a
cluster — a page-oriented storage engine, a write-ahead log, Gorilla
compression, an inverted tag index and an InfluxQL subset — into **one file**
and **one static binary**, in the spirit of SQLite.

```
┌──────────────────────────────────────────────────────────────┐
│  HTTP API  /ping  /health  /write  /query     CLI: ltsdb      │
├──────────────────────────────────────────────────────────────┤
│  Line protocol (write)        InfluxQL subset (query)         │
├──────────────────────────────────────────────────────────────┤
│  Query engine: series match → block time-pruning → project/agg│
├──────────────────────────────────────────────────────────────┤
│  Index  ·  Columnar data blocks (Gorilla)  ·  Page store + WAL│
│                     one database file                         │
└──────────────────────────────────────────────────────────────┘
```

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
![Go](https://img.shields.io/badge/Go-1.21-00ADD8.svg)
![Python](https://img.shields.io/badge/Python-stdlib--only-3776AB.svg)

---

## Why

Most time-series databases are servers or multi-file directories (InfluxDB's
TSM, Prometheus blocks, VictoriaMetrics, QuestDB). `local-tsdb` is the
opposite: an **embedded, embedded-friendly** engine where the whole database is
a single file you can copy, ship or delete — while still speaking the
InfluxDB line protocol and a slice of InfluxQL, so existing tooling and clients
can talk to it unchanged.

Good fit for: local metrics collection, edge/IoT buffering, dev & test
environments, desktop telemetry, and offline analysis.

## What's in the repo

| Path | Language | Role |
|---|---|---|
| [`go/`](go) | Go 1.21, stdlib only | **Production engine** — the `ltsdb` binary: engine, InfluxQL, HTTP API, CLI |
| [`tsdb/`](tsdb) | Python 3, stdlib only | **Reference implementation** — same architecture, easy to read and audit |
| [`gpu_monitor/`](gpu_monitor) | Python + `pynvml` | **Real-world demo** — samples GPUs → `/write` → live ANSI dashboard |
| [`DESIGN.md`](DESIGN.md) | — | Architecture & on-disk format notes (reference engine) |

Both engines share the same design and the same on-disk ideas; the Go one is
what you run, the Python one is the readable specification you compare against.

## Features

- **Single-file storage** — one database file + an optional `.wal`, with a
  header page, a freelist for page reuse, overflow blob chains and a
  recover-then-checkpoint WAL (crash-safe, torn-frame tolerant).
- **InfluxDB data model** — `measurement + tags + fields + timestamp`, nanosecond
  timestamps, canonical series keys, per-field types (`i`/`f`/`b`/`s`).
- **Columnar compression** — Gorilla float XOR for gauges, delta-of-delta for
  timestamps and integer counters, zigzag varints.
- **Time-bound pruning** — every block carries `(locator, min_ts, max_ts)`;
  queries skip whole blocks outside the requested window instead of decoding
  them.
- **InfluxDB wire compatibility** — line protocol ingest and
  `GET|POST /query` returning CSV or JSON, plus `/ping` and `/health`.
- **InfluxQL subset** — `SELECT`/aggregations (`count|sum|mean|min|max`),
  `WHERE` on time and tags, `GROUP BY time(...)`/tags, `LIMIT`,
  `SHOW MEASUREMENTS`.
- **Zero third-party dependencies** — Go standard library only; the Python
  engine too (only the GPU demo needs `pynvml`).

## Quick start

### 1. Go engine (the one you run)

```sh
cd go
go build -o ltsdb ./cmd/ltsdb          # add .exe on Windows

# start an InfluxDB-compatible HTTP server
./ltsdb serve --db metrics.ltsdb --addr 127.0.0.1:8086
```

Write and query over HTTP:

```sh
curl -i -XPOST 'http://127.0.0.1:8086/write?precision=s' \
  --data-binary 'cpu,host=a,region=us usage=0.64'

curl -G 'http://127.0.0.1:8086/query' \
  --data-urlencode 'q=SELECT mean(usage) FROM cpu WHERE host='"'"'a'"'"''

# ask for JSON instead of CSV
curl -G 'http://127.0.0.1:8086/query' -H 'Accept: application/json' \
  --data-urlencode 'q=SHOW MEASUREMENTS'
```

CLI subcommands:

```sh
ltsdb serve   --db PATH [--addr 127.0.0.1:8086]
ltsdb query   --db PATH [--json] ("<SQL>" | --file Q.sql)   # CSV/JSON to stdout
ltsdb exec    --db PATH ("<SQL>" | --file Q.sql)            # row count to stderr
ltsdb import  --db PATH (--file lines.lp | -) [--precision ns|s|m|u] [--commit-every N]
ltsdb bench   --db PATH [--points 100000] [--hosts 8]
```

Exit codes: `0` success, `1` usage/syntax error, `2` runtime error.

> The Go module path is `github.com/Kelmory/local-tsdb`, so the command can also
> be installed with `go install github.com/Kelmory/local-tsdb/cmd/ltsdb@latest`.

### 2. Python reference engine

```sh
python demo/demo.py                     # end-to-end: API + line protocol + InfluxQL + HTTP
python -m pytest tests -q               # 139 tests
```

```python
from tsdb import TimeSeriesDB, Point

with TimeSeriesDB("cpu.ltdb") as db:
    db.write([Point("cpu", {"host": "a"}, {"usage": 0.67}, 1_700_000_000_000)])
    db.commit()
    rows = db.select(measurement="cpu", tag_filters={"host": "a"})
```

### 3. GPU monitor demo (Windows)

Reuses or spawns `ltsdb serve` and streams GPU utilisation / memory /
temperature / power plus per-process GPU memory into the database, then renders
a live ANSI dashboard rebuilt from `/query`:

```sh
pip install -r requirements.txt
python -m gpu_monitor --db gpu.ltdb --interval 1
```

Per-process GPU memory on Windows (WDDM) is read from a persistent PowerShell
`Get-Counter` child, since NVML does not expose it on current drivers.

## Benchmarks

Workload: 100 000 points across 8 series, ingested via line protocol.

| Metric | Python reference | Go engine | Go target |
|---|---:|---:|---:|
| Write throughput | ~44k pts/s | **~549k pts/s** | ≥300k |
| File size | 8.60 B/point | **9.26 B/point** | ≤10 |
| Full scan (100k rows) | ~1.12 s | **0.060 s** | ≤100 ms |
| Pruned range (8008 rows) | ~32 ms | **5.0 ms** | ≤5 ms |
| `mean` over 100k points | ~0.99 s | **0.015 s** | ≤100 ms |

Reproduce with `ltsdb bench --db tmp.ltsdb` (numbers vary by machine).

## Prior art & honest positioning

This project does **not** invent new algorithms. It is a clean-room
implementation that combines well-known, published designs, and it is
**largely AI-assisted code**:

- SQLite's single-file page model, freelist and write-ahead log;
- InfluxDB's storage model, TSM-style time-bounded columnar blocks and the
  Gorilla float-XOR / delta-of-delta codecs;
- the InfluxDB line protocol and InfluxQL as the external interface.

The value here is the *packaging* — a small, readable, single-file, embedded
time-series engine that is wire-compatible with InfluxDB — not novel research.
If you need scale, use the real InfluxDB / VictoriaMetrics / TimescaleDB; if
you need a single file you can drop on a disk, this is for you.

## Limitations

- **Single writer** — one in-process lock; no cross-process file lock. Not a
  multi-client server.
- **Append-only blocks** — each commit opens a new block per touched series; no
  compaction yet, so frequent commits grow the file.
- **No retention / downsampling / continuous queries** — time pruning is
  block-level only.
- **Aggregations are in-memory folds** — no materialised partial aggregates.
- **InfluxQL is a subset** — no subqueries, regex, or multi-measurement joins.

## Roadmap

- [ ] Block compaction / merging
- [ ] Retention policies & rollups
- [ ] Cross-platform GPU backend for the demo
- [ ] Benchmarks and CI on Linux/macOS
- [ ] More of InfluxQL (regex, `GROUP BY` without aggregates)

## License

[MIT](LICENSE).
