// Package bench generates the standard performance workload and measures
// write throughput, compression, scan latency and aggregation speed. It backs
// both the `ltsdb bench` CLI command and Go benchmarks.
//
// The workload mirrors the Python reference: `hosts` series writing whole
// 100-value cycles (usage = (j%100)/100), timestamped at 1ns intervals from
// BASE_TS, so the mean over any whole number of cycles is 0.495.
package bench

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Kelmory/local-tsdb/internal/engine"
	"github.com/Kelmory/local-tsdb/internal/lineproto"
	"github.com/Kelmory/local-tsdb/internal/model"
)

// BaseTS is the workload epoch: 2023-11-14T22:13:20Z in nanoseconds.
const BaseTS = int64(1_700_000_000) * 1_000_000_000

// Options configures one bench run.
type Options struct {
	DBPath string
	Points int // total points (default 100000)
	Hosts  int // number of series (default 8)
}

// Result holds the measured metrics.
type Result struct {
	NPoints         int
	Hosts           int
	Ticks           int
	WriteSeconds    float64
	FileSize        int64
	FullScanSeconds float64
	FullScanRows    int
	PruneSeconds    float64
	PruneRows       int
	MeanSeconds     float64
	Mean            float64
}

// Lines builds the workload as line protocol (parse path included in writes).
func Lines(hosts, ticks int) []string {
	lines := make([]string, 0, hosts*ticks)
	for i := 0; i < hosts; i++ {
		for j := 0; j < ticks; j++ {
			lines = append(lines, fmt.Sprintf("cpu,host=h%d usage=%.4f %d",
				i, float64(j%100)/100.0, BaseTS+int64(j)))
		}
	}
	return lines
}

// Run executes a full bench: fresh DB, timed parse+write+commit, file size,
// then timed full scan, pruned range scan ([+6000, +7000] ns) and mean
// aggregate on a reopened handle.
func Run(opts Options) (*Result, error) {
	if opts.Hosts <= 0 {
		opts.Hosts = 8
	}
	if opts.Points <= 0 {
		opts.Points = 100000
	}
	ticks := opts.Points / opts.Hosts
	if ticks < 7001 {
		return nil, fmt.Errorf("bench needs at least %d points per host (got %d)", 7001, ticks)
	}

	// Fresh database: remove any previous files.
	for _, p := range []string{opts.DBPath, opts.DBPath + ".wal"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}

	lines := Lines(opts.Hosts, ticks)
	eng, err := engine.Open(opts.DBPath)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	batch := make([]model.Point, 0, 4096)
	for _, line := range lines {
		p, perr := lineproto.ParseLine(line)
		if perr != nil {
			eng.Close()
			return nil, perr
		}
		batch = append(batch, p)
		if len(batch) >= 4096 {
			if _, werr := eng.WritePoints(batch); werr != nil {
				eng.Close()
				return nil, werr
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if _, werr := eng.WritePoints(batch); werr != nil {
			eng.Close()
			return nil, werr
		}
	}
	if err := eng.Commit(); err != nil {
		eng.Close()
		return nil, err
	}
	writeSeconds := time.Since(t0).Seconds()
	if err := eng.Close(); err != nil {
		return nil, err
	}

	fi, err := os.Stat(opts.DBPath)
	if err != nil {
		return nil, err
	}
	res := &Result{
		NPoints:      opts.Hosts * ticks,
		Hosts:        opts.Hosts,
		Ticks:        ticks,
		WriteSeconds: writeSeconds,
		FileSize:     fi.Size(),
	}

	// Reopen for the read benchmarks.
	eng, err = engine.Open(opts.DBPath)
	if err != nil {
		return nil, err
	}
	defer eng.Close()
	ctx := context.Background()

	t0 = time.Now()
	rows, err := eng.Select(ctx, engine.SelectQuery{Measurement: "cpu"})
	if err != nil {
		return nil, err
	}
	res.FullScanSeconds = time.Since(t0).Seconds()
	res.FullScanRows = len(rows)

	start, end := BaseTS+6000, BaseTS+7000
	t0 = time.Now()
	narrow, err := eng.Select(ctx, engine.SelectQuery{Measurement: "cpu", Start: &start, End: &end})
	if err != nil {
		return nil, err
	}
	res.PruneSeconds = time.Since(t0).Seconds()
	res.PruneRows = len(narrow)

	t0 = time.Now()
	agg, err := eng.Aggregate(ctx, engine.AggregateRequest{
		Measurement: "cpu", Field: "usage", Func: "mean",
	})
	if err != nil {
		return nil, err
	}
	res.MeanSeconds = time.Since(t0).Seconds()
	if f, ok := agg.Scalar.(float64); ok {
		res.Mean = f
	}
	return res, nil
}
