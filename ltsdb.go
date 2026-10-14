// Package ltsdb is the importable API of the local-tsdb engine: a single-file,
// zero-dependency time-series database with an InfluxDB-compatible surface.
//
// The command in cmd/ltsdb is a thin wrapper around this package; embedders can
// skip the CLI and use the engine directly:
//
//	import ltsdb "github.com/Kelmory/local-tsdb"
//
//	db, err := ltsdb.Open("metrics.ltsdb")
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer db.Close()
//
//	db.WritePoints([]ltsdb.Point{{
//		Measurement: "cpu",
//		Tags:        map[string]string{"host": "a"},
//		Fields:      map[string]any{"usage": 0.64},
//	}})
//	if err := db.Commit(); err != nil {
//		log.Fatal(err)
//	}
package ltsdb

import (
	"github.com/Kelmory/local-tsdb/internal/engine"
	"github.com/Kelmory/local-tsdb/internal/influxql"
	"github.com/Kelmory/local-tsdb/internal/model"
)

// Engine is a single-file time-series database. It is safe for concurrent use.
type Engine = engine.Engine

// Point is one measurement sample. Timestamp is nanoseconds since the epoch;
// zero means "assign the current time on write".
type Point = model.Point

// SelectQuery describes a point scan. A nil Start/End means unbounded.
type SelectQuery = engine.SelectQuery

// AggregateRequest describes a fold over matching rows.
type AggregateRequest = engine.AggregateRequest

// AggregateResult is a scalar aggregate, or per-tag-value groups.
type AggregateResult = engine.AggregateResult

// ResultSet is a rendered InfluxQL query result. Use its ToCSV or ToJSON
// methods to serialise it.
type ResultSet = influxql.ResultSet

// Open creates or opens a single-file database (plus the path+".wal"
// write-ahead log).
func Open(path string) (*Engine, error) { return engine.Open(path) }

// Exec runs one InfluxQL statement against the engine.
func Exec(e *Engine, statement string) (*ResultSet, error) {
	e.RLock()
	defer e.RUnlock()
	ex := &influxql.Executor{
		QE:             e.QueryEngine(),
		MeasurementsFn: e.Index().Measurements,
	}
	return ex.Execute(statement)
}
