package engine

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/Kelmory/local-tsdb/internal/block"
	"github.com/Kelmory/local-tsdb/internal/model"
)

func openTemp(t *testing.T) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/engine.ltsdb"
	e, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return e, path
}

// mp builds a point: mp("cpu", map[string]string{"host":"a"}, map[string]any{"usage":1.0}, 100)
func mp(m string, tags map[string]string, fields map[string]any, ts int64) model.Point {
	return model.Point{Measurement: m, Tags: tags, Fields: fields, Timestamp: ts}
}

func write(t *testing.T, e *Engine, pts ...model.Point) {
	t.Helper()
	if _, err := e.WritePoints(pts); err != nil {
		t.Fatal(err)
	}
}

func TestWriteCommitSelect(t *testing.T) {
	e, _ := openTemp(t)
	defer e.Close()
	write(t, e,
		mp("cpu", map[string]string{"host": "a"}, map[string]any{"usage": 1.0}, 100),
		mp("cpu", map[string]string{"host": "a"}, map[string]any{"usage": 2.0}, 200),
		mp("cpu", map[string]string{"host": "b"}, map[string]any{"usage": 3.0}, 300),
	)
	if err := e.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := e.Select(context.Background(), SelectQuery{Measurement: "cpu"})
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{100, 200, 300}
	for i, r := range rows {
		if r.Timestamp != want[i] {
			t.Fatalf("i=%d ts=%d", i, r.Timestamp)
		}
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	e, path := openTemp(t)
	write(t, e,
		mp("cpu", map[string]string{"host": "a"}, map[string]any{"usage": 1.0}, 100),
		mp("cpu", map[string]string{"host": "a"}, map[string]any{"usage": 2.0}, 200),
		mp("cpu", map[string]string{"host": "b"}, map[string]any{"usage": 3.0}, 300),
	)
	if err := e.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	rows, _ := e2.Select(context.Background(), SelectQuery{Measurement: "cpu"})
	if len(rows) != 3 {
		t.Fatalf("len=%d", len(rows))
	}
	res, err := e2.Aggregate(context.Background(), AggregateRequest{Measurement: "cpu", Field: "usage", Func: "mean"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scalar != 2.0 {
		t.Fatalf("mean=%v", res.Scalar)
	}
}

func TestMemtableVisibleBeforeCommit(t *testing.T) {
	e, _ := openTemp(t)
	defer e.Close()
	write(t, e, mp("mem", map[string]string{"host": "a"}, map[string]any{"used": int64(5)}, 10))
	// Not committed yet: the memtable must still be queryable.
	rows, _ := e.Select(context.Background(), SelectQuery{Measurement: "mem"})
	if len(rows) != 1 || rows[0].Fields["used"] != int64(5) {
		t.Fatalf("rows=%v", rows)
	}
	if err := e.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestMeasurementsAndSeries(t *testing.T) {
	e, _ := openTemp(t)
	defer e.Close()
	write(t, e,
		mp("cpu", map[string]string{"host": "a"}, map[string]any{"usage": 1.0}, 100),
		mp("cpu", map[string]string{"host": "a"}, map[string]any{"usage": 2.0}, 200),
		mp("cpu", map[string]string{"host": "b"}, map[string]any{"usage": 3.0}, 300),
	)
	e.Commit()
	if ms := e.Measurements(); len(ms) != 1 || ms[0] != "cpu" {
		t.Fatalf("measurements=%v", ms)
	}
	s := e.Series("cpu")
	if len(s) != 2 || s[0] != "cpu,host=a" || s[1] != "cpu,host=b" {
		t.Fatalf("series=%v", s)
	}
}

func TestTagFilterSelect(t *testing.T) {
	e, _ := openTemp(t)
	defer e.Close()
	write(t, e,
		mp("cpu", map[string]string{"host": "a"}, map[string]any{"usage": 1.0}, 100),
		mp("cpu", map[string]string{"host": "a"}, map[string]any{"usage": 2.0}, 200),
		mp("cpu", map[string]string{"host": "b"}, map[string]any{"usage": 3.0}, 300),
	)
	e.Commit()
	rows, _ := e.Select(context.Background(), SelectQuery{
		Measurement: "cpu", TagFilters: map[string]string{"host": "a"}})
	want := []int64{100, 200}
	for i, r := range rows {
		if r.Timestamp != want[i] {
			t.Fatalf("i=%d ts=%d", i, r.Timestamp)
		}
	}
}

func ip(v int64) *int64 { return &v }

func TestTwoCommitsTwoBlocksAndPrune(t *testing.T) {
	e, _ := openTemp(t)
	defer e.Close()
	// First block: ts 1..10. Second block: ts 1001..1010.
	var pts []model.Point
	for i := int64(1); i <= 10; i++ {
		pts = append(pts, mp("cpu", map[string]string{"h": "a"}, map[string]any{"v": 1.5}, i))
	}
	write(t, e, pts...)
	e.Commit()
	pts = nil
	for i := int64(1); i <= 10; i++ {
		pts = append(pts, mp("cpu", map[string]string{"h": "a"}, map[string]any{"v": 2.5}, 1000+i))
	}
	write(t, e, pts...)
	e.Commit()

	refs := e.Index().Blocks(e.Index().AddSeries("cpu", map[string]string{"h": "a"}))
	if len(refs) != 2 {
		t.Fatalf("refs=%v", refs)
	}

	// A range that only overlaps the second block returns exactly its rows.
	rows, _ := e.Select(context.Background(), SelectQuery{Measurement: "cpu", Start: ip(1001), End: ip(1010)})
	if len(rows) != 10 {
		t.Fatalf("len=%d", len(rows))
	}
	// A range between the blocks returns nothing without decoding either.
	gap, _ := e.Select(context.Background(), SelectQuery{Measurement: "cpu", Start: ip(50), End: ip(99)})
	if len(gap) != 0 {
		t.Fatalf("gap=%v", gap)
	}
}

func TestOutOfOrderWritesKeepBlockBoundsCorrect(t *testing.T) {
	// Regression: a commit whose points arrive out of order used to store
	// first/last-written timestamps as block bounds, so range queries
	// silently pruned away rows that actually lived inside the block.
	e, _ := openTemp(t)
	defer e.Close()
	tsOrder := []int64{300, 100, 500, 200, 400}
	var pts []model.Point
	for i, ts := range tsOrder {
		pts = append(pts, mp("cpu", nil, map[string]any{"v": float64(i)}, ts))
	}
	write(t, e, pts...)
	e.Commit()

	refs := e.Index().Blocks(e.Index().AddSeries("cpu", nil))
	if len(refs) != 1 || refs[0].MinTs != 100 || refs[0].MaxTs != 500 {
		t.Fatalf("refs=%v", refs)
	}

	rows, _ := e.Select(context.Background(), SelectQuery{Measurement: "cpu"})
	want := []int64{100, 200, 300, 400, 500}
	for i, r := range rows {
		if r.Timestamp != want[i] {
			t.Fatalf("i=%d ts=%d", i, r.Timestamp)
		}
	}
	// Ranges at both ends of the block must see their rows.
	lo, _ := e.Select(context.Background(), SelectQuery{Measurement: "cpu", Start: ip(50), End: ip(150)})
	if len(lo) != 1 || lo[0].Timestamp != 100 {
		t.Fatalf("lo=%v", lo)
	}
	hi, _ := e.Select(context.Background(), SelectQuery{Measurement: "cpu", Start: ip(450), End: ip(600)})
	if len(hi) != 1 || hi[0].Timestamp != 500 {
		t.Fatalf("hi=%v", hi)
	}
}

func TestDuplicateTimestampsMergeAfterCommit(t *testing.T) {
	// Same series + timestamp: later points win field-by-field.
	e, _ := openTemp(t)
	defer e.Close()
	write(t, e,
		mp("m", nil, map[string]any{"a": 1.0}, 5),
		mp("m", nil, map[string]any{"b": int64(2)}, 5),
		mp("m", nil, map[string]any{"a": 9.0}, 5),
	)
	e.Commit()
	rows, _ := e.Select(context.Background(), SelectQuery{Measurement: "m"})
	if len(rows) != 1 {
		t.Fatalf("len=%d", len(rows))
	}
	if rows[0].Fields["a"] != 9.0 || rows[0].Fields["b"] != int64(2) {
		t.Fatalf("fields=%v", rows[0].Fields)
	}
}

func TestSparseFieldsNullSemantics(t *testing.T) {
	// Regression: missing fields used to be zero-filled into the block,
	// polluting mean(a) and count(a).
	e, _ := openTemp(t)
	defer e.Close()
	write(t, e,
		mp("m", nil, map[string]any{"a": 1.5}, 1),
		mp("m", nil, map[string]any{"b": 2.0}, 2),
	)
	e.Commit()
	res, err := e.Aggregate(context.Background(), AggregateRequest{Measurement: "m", Field: "a", Func: "mean"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scalar != 1.5 {
		t.Fatalf("mean=%v", res.Scalar)
	}
	res, _ = e.Aggregate(context.Background(), AggregateRequest{Measurement: "m", Field: "a", Func: "count"})
	if res.Scalar != int64(1) {
		t.Fatalf("count=%v", res.Scalar)
	}
}

func TestFieldTypeConflictRejectedAtCommit(t *testing.T) {
	e, path := openTemp(t)
	write(t, e, mp("m", nil, map[string]any{"v": int64(1)}, 1))
	write(t, e, mp("m", nil, map[string]any{"v": 1.5}, 2))
	err := e.Commit()
	if err == nil || !errors.Is(err, block.ErrFieldTypeConflict) {
		t.Fatalf("err=%v", err)
	}
	// Improvement over Python: the store must be untouched after the failed
	// commit — reopen sees zero data and a consistent header.
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	rows, _ := e2.Select(context.Background(), SelectQuery{Measurement: "m"})
	if len(rows) != 0 {
		t.Fatalf("rows after failed commit: %v", rows)
	}
}

func TestLimitAfterTagNeq(t *testing.T) {
	// LIMIT must apply after tag != filtering (SQL-layer behavior ported
	// down to the scan level).
	e, _ := openTemp(t)
	defer e.Close()
	var pts []model.Point
	for i := int64(0); i < 10; i++ {
		host := "b"
		if i%2 == 1 {
			host = "a"
		}
		pts = append(pts, mp("m", map[string]string{"h": host}, map[string]any{"v": float64(i)}, i))
	}
	write(t, e, pts...)
	e.Commit()
	rows, _ := e.Select(context.Background(), SelectQuery{Measurement: "m", Limit: 5})
	if len(rows) != 5 {
		t.Fatalf("len=%d", len(rows))
	}
}

func TestReopenAfterCheckpointHasNoWAL(t *testing.T) {
	e, path := openTemp(t)
	write(t, e, mp("m", nil, map[string]any{"v": 1.0}, 1))
	e.Commit()
	e.Close()
	if _, err := os.Stat(path + ".wal"); !os.IsNotExist(err) {
		t.Fatalf("wal should be gone, err=%v", err)
	}
}
