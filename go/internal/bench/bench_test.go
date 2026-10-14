package bench

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/Kelmory/local-tsdb/internal/engine"
)

// TestBenchCorrectness runs a small workload and checks the row counts and
// the cycle-mean invariant (mean of (j%100)/100 over whole cycles = 0.495).
func TestBenchCorrectness(t *testing.T) {
	res, err := Run(Options{
		DBPath: filepath.Join(t.TempDir(), "perf.ltsdb"),
		Points: 64000, // 8 hosts x 8000 ticks (window [+6000,+7000] fits)
		Hosts:  8,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.NPoints != 64000 {
		t.Errorf("n_points = %d, want 64000", res.NPoints)
	}
	if res.FullScanRows != 64000 {
		t.Errorf("full scan rows = %d, want 64000", res.FullScanRows)
	}
	// [Base+6000, Base+7000] inclusive -> 1001 ticks per host.
	if res.PruneRows != 8*1001 {
		t.Errorf("pruned rows = %d, want %d", res.PruneRows, 8*1001)
	}
	if math.Abs(res.Mean-0.495) > 1e-9 {
		t.Errorf("mean = %v, want 0.495", res.Mean)
	}
	if res.WriteSeconds <= 0 || res.FullScanSeconds <= 0 {
		t.Error("timings not recorded")
	}
}

// openBenchDB prepares a 100k-point database for read benchmarks.
func openBenchDB(b *testing.B) (*engine.Engine, func()) {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, "s.ltsdb")
	if _, err := Run(Options{DBPath: path, Points: 100000, Hosts: 8}); err != nil {
		b.Fatal(err)
	}
	eng, err := engine.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	return eng, func() { eng.Close() }
}

func BenchmarkWritePoints(b *testing.B) {
	// One iteration = parse + buffer + commit of 8k points (fresh DB).
	const points = 8000
	for i := 0; i < b.N; i++ {
		if _, err := Run(Options{
			DBPath: filepath.Join(b.TempDir(), "b.ltsdb"),
			Points: points, Hosts: 8,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFullScan(b *testing.B) {
	eng, cleanup := openBenchDB(b)
	defer cleanup()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := eng.Select(ctx, engine.SelectQuery{Measurement: "cpu"})
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 100000 {
			b.Fatalf("rows = %d, want 100000", len(rows))
		}
	}
}

func BenchmarkPruneScan(b *testing.B) {
	eng, cleanup := openBenchDB(b)
	defer cleanup()
	ctx := context.Background()
	start, end := BaseTS+6000, BaseTS+7000
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := eng.Select(ctx, engine.SelectQuery{Measurement: "cpu", Start: &start, End: &end})
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 8*1001 {
			b.Fatalf("rows = %d, want %d", len(rows), 8*1001)
		}
	}
}

func BenchmarkMeanAggregate(b *testing.B) {
	eng, cleanup := openBenchDB(b)
	defer cleanup()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.Aggregate(ctx, engine.AggregateRequest{
			Measurement: "cpu", Field: "usage", Func: "mean",
		})
		if err != nil {
			b.Fatal(err)
		}
		if f, ok := res.Scalar.(float64); !ok || math.Abs(f-0.495) > 1e-9 {
			b.Fatalf("mean = %v, want 0.495", res.Scalar)
		}
	}
}
