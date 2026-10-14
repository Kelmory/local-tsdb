package query

import (
	"testing"

	"github.com/Kelmory/local-tsdb/internal/block"
	"github.com/Kelmory/local-tsdb/internal/index"
	"github.com/Kelmory/local-tsdb/internal/model"
)

func mustBlock(t *testing.T, pts []model.Point) *block.Block {
	t.Helper()
	b, err := block.FromPoints(pts)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type fixture struct {
	ix       *index.Index
	blocks   map[int]*block.Block
	memtable map[string][]model.Point
	qe       *QueryEngine
	k1       string
}

func setup(t *testing.T) *fixture {
	f := &fixture{
		ix:       index.New(),
		blocks:   map[int]*block.Block{},
		memtable: map[string][]model.Point{},
	}
	k1 := f.ix.AddSeries("cpu", map[string]string{"host": "a"})
	k2 := f.ix.AddSeries("cpu", map[string]string{"host": "b"})
	k3 := f.ix.AddSeries("mem", nil)

	f.blocks[1] = mustBlock(t, []model.Point{
		{Measurement: "cpu", Tags: map[string]string{"host": "a"}, Fields: map[string]any{"usage": 1.0}, Timestamp: 100},
		{Measurement: "cpu", Tags: map[string]string{"host": "a"}, Fields: map[string]any{"usage": 2.0}, Timestamp: 200},
		{Measurement: "cpu", Tags: map[string]string{"host": "a"}, Fields: map[string]any{"usage": 3.0}, Timestamp: 300},
	})
	f.blocks[2] = mustBlock(t, []model.Point{
		{Measurement: "cpu", Tags: map[string]string{"host": "b"}, Fields: map[string]any{"usage": 10.0}, Timestamp: 150},
		{Measurement: "cpu", Tags: map[string]string{"host": "b"}, Fields: map[string]any{"usage": 20.0}, Timestamp: 250},
	})
	f.blocks[3] = mustBlock(t, []model.Point{
		{Measurement: "mem", Fields: map[string]any{"used": int64(5)}, Timestamp: 50},
	})
	if err := f.ix.AddBlock(k1, index.BlockRef{Locator: 1, MinTs: 100, MaxTs: 300}); err != nil {
		t.Fatal(err)
	}
	if err := f.ix.AddBlock(k2, index.BlockRef{Locator: 2, MinTs: 150, MaxTs: 250}); err != nil {
		t.Fatal(err)
	}
	if err := f.ix.AddBlock(k3, index.BlockRef{Locator: 3, MinTs: 50, MaxTs: 50}); err != nil {
		t.Fatal(err)
	}
	f.k1 = k1
	f.qe = New(f.ix, func(loc int) (*block.Block, error) { return f.blocks[loc], nil }, f.memtable)
	return f
}

func TestSelectAllCPU(t *testing.T) {
	f := setup(t)
	rows, err := f.qe.Select("cpu", nil, nil, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("len=%d", len(rows))
	}
	// Ordered by series key then timestamp.
	want := []int64{100, 200, 300, 150, 250}
	for i, r := range rows {
		if r.Timestamp != want[i] {
			t.Fatalf("i=%d ts=%d want %d", i, r.Timestamp, want[i])
		}
	}
}

func TestSelectTagFilter(t *testing.T) {
	f := setup(t)
	rows, _ := f.qe.Select("cpu", map[string]string{"host": "a"}, nil, nil, nil, 0)
	want := []int64{100, 200, 300}
	for i, r := range rows {
		if r.Timestamp != want[i] {
			t.Fatalf("i=%d ts=%d", i, r.Timestamp)
		}
	}
}

func TestSelectTimeRangePruning(t *testing.T) {
	f := setup(t)
	s, e := int64(200), int64(300)
	rows, _ := f.qe.Select("cpu", map[string]string{"host": "a"}, nil, &s, &e, 0)
	want := []int64{200, 300}
	for i, r := range rows {
		if r.Timestamp != want[i] {
			t.Fatalf("i=%d ts=%d", i, r.Timestamp)
		}
	}
}

func TestSelectFieldProjection(t *testing.T) {
	f := setup(t)
	rows, _ := f.qe.Select("mem", nil, []string{"used"}, nil, nil, 0)
	if len(rows) != 1 || rows[0].Fields["used"] != int64(5) || len(rows[0].Fields) != 1 {
		t.Fatalf("rows=%v", rows)
	}
}

func TestSelectLimit(t *testing.T) {
	f := setup(t)
	rows, _ := f.qe.Select("cpu", nil, nil, nil, nil, 2)
	if len(rows) != 2 {
		t.Fatalf("len=%d", len(rows))
	}
}

func TestMemtablePointsVisibleBeforeFlush(t *testing.T) {
	f := setup(t)
	f.memtable[f.k1] = []model.Point{
		{Measurement: "cpu", Tags: map[string]string{"host": "a"}, Fields: map[string]any{"usage": 9.0}, Timestamp: 400},
	}
	rows, _ := f.qe.Select("cpu", map[string]string{"host": "a"}, nil, nil, nil, 0)
	want := []int64{100, 200, 300, 400}
	for i, r := range rows {
		if r.Timestamp != want[i] {
			t.Fatalf("i=%d ts=%d", i, r.Timestamp)
		}
	}
}

func agg(t *testing.T, f *fixture, req AggregateRequest) any {
	t.Helper()
	res, err := f.qe.Aggregate(req)
	if err != nil {
		t.Fatal(err)
	}
	return res.Scalar
}

func TestAggregateCount(t *testing.T) {
	f := setup(t)
	if got := agg(t, f, AggregateRequest{Measurement: "cpu", Field: "usage", Func: "count"}); got != int64(5) {
		t.Fatalf("got %v", got)
	}
}

func TestAggregateCountIsNotFirstValuePlusN(t *testing.T) {
	// Regression: count used to start from the first field value, so
	// values [100, 200, 300] returned 102.0 instead of 3.
	f := setup(t)
	k := f.ix.AddSeries("big", nil)
	if err := f.ix.AddBlock(k, index.BlockRef{Locator: 4, MinTs: 10, MaxTs: 30}); err != nil {
		t.Fatal(err)
	}
	f.blocks[4] = mustBlock(t, []model.Point{
		{Measurement: "big", Fields: map[string]any{"v": 100.0}, Timestamp: 10},
		{Measurement: "big", Fields: map[string]any{"v": 200.0}, Timestamp: 20},
		{Measurement: "big", Fields: map[string]any{"v": 300.0}, Timestamp: 30},
	})
	if got := agg(t, f, AggregateRequest{Measurement: "big", Field: "v", Func: "count"}); got != int64(3) {
		t.Fatalf("got %v", got)
	}
}

func TestAggregateCountEmptyIsZero(t *testing.T) {
	f := setup(t)
	if got := agg(t, f, AggregateRequest{Measurement: "mem", Field: "usage", Func: "count"}); got != int64(0) {
		t.Fatalf("got %v", got)
	}
}

func TestAggregateSkipsNullValues(t *testing.T) {
	// Regression: sparse fields used to be zero-filled, so mean(a) over
	// rows [a=10.0, b=1.0] came out 5.0 instead of 10.0.
	f := setup(t)
	k := f.ix.AddSeries("sparse", nil)
	if err := f.ix.AddBlock(k, index.BlockRef{Locator: 4, MinTs: 1, MaxTs: 2}); err != nil {
		t.Fatal(err)
	}
	f.blocks[4] = mustBlock(t, []model.Point{
		{Measurement: "sparse", Fields: map[string]any{"a": 10.0}, Timestamp: 1},
		{Measurement: "sparse", Fields: map[string]any{"b": 1.0}, Timestamp: 2},
	})
	if got := agg(t, f, AggregateRequest{Measurement: "sparse", Field: "a", Func: "mean"}); got != 10.0 {
		t.Fatalf("mean got %v", got)
	}
	if got := agg(t, f, AggregateRequest{Measurement: "sparse", Field: "a", Func: "count"}); got != int64(1) {
		t.Fatalf("count got %v", got)
	}
}

func TestAggregateMinMaxSumMean(t *testing.T) {
	f := setup(t)
	if got := agg(t, f, AggregateRequest{Measurement: "cpu", Field: "usage", Func: "min"}); got != 1.0 {
		t.Fatalf("min %v", got)
	}
	if got := agg(t, f, AggregateRequest{Measurement: "cpu", Field: "usage", Func: "max"}); got != 20.0 {
		t.Fatalf("max %v", got)
	}
	if got := agg(t, f, AggregateRequest{Measurement: "cpu", Field: "usage", Func: "sum"}); got != 36.0 {
		t.Fatalf("sum %v", got)
	}
	if got := agg(t, f, AggregateRequest{Measurement: "cpu", Field: "usage", Func: "mean"}); got != 7.2 {
		t.Fatalf("mean %v", got)
	}
}

func TestAggregateTagScoped(t *testing.T) {
	f := setup(t)
	got := agg(t, f, AggregateRequest{Measurement: "cpu", Field: "usage", Func: "max",
		TagFilters: map[string]string{"host": "a"}})
	if got != 3.0 {
		t.Fatalf("got %v", got)
	}
}

func TestAggregateGroupBy(t *testing.T) {
	f := setup(t)
	res, err := f.qe.Aggregate(AggregateRequest{Measurement: "cpu", Field: "usage", Func: "max", GroupBy: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Groups["a"] != 3.0 || res.Groups["b"] != 20.0 || len(res.Groups) != 2 {
		t.Fatalf("groups %v", res.Groups)
	}
}

func TestAggregateUnknownFuncRaises(t *testing.T) {
	f := setup(t)
	if _, err := f.qe.Aggregate(AggregateRequest{Measurement: "cpu", Field: "usage", Func: "median"}); err == nil {
		t.Fatal("expected error")
	}
}
