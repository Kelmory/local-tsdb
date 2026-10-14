package influxql

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Kelmory/local-tsdb/internal/block"
	"github.com/Kelmory/local-tsdb/internal/index"
	"github.com/Kelmory/local-tsdb/internal/model"
	"github.com/Kelmory/local-tsdb/internal/query"
)

func seedEngine(t *testing.T) *query.QueryEngine {
	t.Helper()
	ix := index.New()
	blocks := map[int]*block.Block{}
	add := func(tags map[string]string, pts ...model.Point) {
		t.Helper()
		key := ix.AddSeries(pts[0].Measurement, tags)
		blk, err := block.FromPoints(pts)
		if err != nil {
			t.Fatalf("FromPoints: %v", err)
		}
		loc := len(blocks) + 1
		blocks[loc] = blk
		minTs, _ := blk.MinTs()
		maxTs, _ := blk.MaxTs()
		if err := ix.AddBlock(key, index.BlockRef{Locator: loc, MinTs: minTs, MaxTs: maxTs}); err != nil {
			t.Fatalf("AddBlock: %v", err)
		}
	}
	cpu := func(host string, usage float64, ts int64) model.Point {
		return model.Point{
			Measurement: "cpu",
			Tags:        map[string]string{"host": host},
			Fields:      map[string]any{"usage": usage},
			Timestamp:   ts,
		}
	}
	add(map[string]string{"host": "a"}, cpu("a", 1.0, 100), cpu("a", 2.0, 200), cpu("a", 3.0, 300))
	add(map[string]string{"host": "b"}, cpu("b", 10.0, 150), cpu("b", 20.0, 250))
	add(map[string]string{}, model.Point{
		Measurement: "mem",
		Fields:      map[string]any{"used": int64(5)},
		Timestamp:   50,
	})
	return query.New(ix, func(loc int) (*block.Block, error) { return blocks[loc], nil },
		map[string][]model.Point{})
}

func newExecutor(t *testing.T) *Executor {
	t.Helper()
	qe := seedEngine(t)
	return &Executor{QE: qe, MeasurementsFn: qe.Index.Measurements}
}

func mustExec(t *testing.T, ex *Executor, sql string) *ResultSet {
	t.Helper()
	rs, err := ex.Execute(sql)
	if err != nil {
		t.Fatalf("Execute(%q) unexpected error: %v", sql, err)
	}
	return rs
}

func times(t *testing.T, rs *ResultSet) []int64 {
	t.Helper()
	out := make([]int64, 0, rs.NRows())
	for _, row := range rs.Rows {
		v, ok := row.Get("time").(int64)
		if !ok {
			t.Fatalf("row time is %T, want int64", row.Get("time"))
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// --- scan ------------------------------------------------------------------ //

func TestSelectStar(t *testing.T) {
	ex := newExecutor(t)
	r := mustExec(t, ex, "SELECT * FROM cpu")
	if r.NRows() != 5 {
		t.Fatalf("n_rows = %d, want 5", r.NRows())
	}
	want := []string{"name", "host", "time", "usage"}
	if !reflect.DeepEqual(r.Columns[:4], want) {
		t.Errorf("columns[:4] = %v, want %v", r.Columns[:4], want)
	}
}

func TestWhereTimeRangeStrict(t *testing.T) {
	ex := newExecutor(t)
	r := mustExec(t, ex, "SELECT * FROM cpu WHERE time > 100 AND time < 300")
	got := times(t, r)
	want := []int64{150, 200, 250}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("times = %v, want %v", got, want)
	}
}

func TestWhereTagEq(t *testing.T) {
	ex := newExecutor(t)
	r := mustExec(t, ex, "SELECT * FROM cpu WHERE host='a'")
	got := times(t, r)
	want := []int64{100, 200, 300}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("times = %v, want %v", got, want)
	}
}

func TestWhereTagNeq(t *testing.T) {
	ex := newExecutor(t)
	r := mustExec(t, ex, "SELECT * FROM cpu WHERE host!='a'")
	got := times(t, r)
	want := []int64{150, 250}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("times = %v, want %v", got, want)
	}
}

func TestLimit(t *testing.T) {
	ex := newExecutor(t)
	r := mustExec(t, ex, "SELECT * FROM cpu LIMIT 2")
	if r.NRows() != 2 {
		t.Errorf("n_rows = %d, want 2", r.NRows())
	}
}

func TestShowMeasurements(t *testing.T) {
	ex := newExecutor(t)
	r := mustExec(t, ex, "SHOW MEASUREMENTS")
	var names []string
	for _, row := range r.Rows {
		names = append(names, row.Get("name").(string))
	}
	want := []string{"cpu", "mem"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("measurements = %v, want %v", names, want)
	}
}

func TestGroupByRequiresAggregate(t *testing.T) {
	ex := newExecutor(t)
	if _, err := ex.Execute("SELECT * FROM cpu GROUP BY host"); err == nil {
		t.Error("expected error: GROUP BY requires an aggregate select list")
	}
}

// --- aggregate -------------------------------------------------------------- //

func TestScalarMean(t *testing.T) {
	ex := newExecutor(t)
	r := mustExec(t, ex, "SELECT mean(usage) FROM cpu")
	if !reflect.DeepEqual(r.Columns, []string{"mean_usage"}) {
		t.Errorf("columns = %v, want [mean_usage]", r.Columns)
	}
	if v, ok := r.Rows[0].Get("mean_usage").(float64); !ok || math_near(v, 7.2) == false {
		t.Errorf("mean_usage = %#v, want 7.2", r.Rows[0].Get("mean_usage"))
	}
}

func TestCountNoField(t *testing.T) {
	ex := newExecutor(t)
	r := mustExec(t, ex, "SELECT count() FROM cpu")
	if v, ok := r.Rows[0].Get("count").(int64); !ok || v != 5 {
		t.Errorf("count = %#v, want 5", r.Rows[0].Get("count"))
	}
}

func TestMinMaxSum(t *testing.T) {
	ex := newExecutor(t)
	if v := mustExec(t, ex, "SELECT min(usage) FROM cpu").Rows[0].Get("min_usage"); v != 1.0 {
		t.Errorf("min_usage = %#v, want 1.0", v)
	}
	if v := mustExec(t, ex, "SELECT max(usage) FROM cpu").Rows[0].Get("max_usage"); v != 20.0 {
		t.Errorf("max_usage = %#v, want 20.0", v)
	}
	if v := mustExec(t, ex, "SELECT sum(usage) FROM cpu").Rows[0].Get("sum_usage"); v != 36.0 {
		t.Errorf("sum_usage = %#v, want 36.0", v)
	}
}

func TestGroupByTag(t *testing.T) {
	ex := newExecutor(t)
	r := mustExec(t, ex, "SELECT max(usage) FROM cpu GROUP BY host")
	if !reflect.DeepEqual(r.Columns, []string{"host", "max_usage"}) {
		t.Errorf("columns = %v, want [host max_usage]", r.Columns)
	}
	want := []struct {
		host string
		max  float64
	}{{"a", 3.0}, {"b", 20.0}}
	if len(r.Rows) != len(want) {
		t.Fatalf("n_rows = %d, want %d", len(r.Rows), len(want))
	}
	for i, w := range want {
		row := r.Rows[i]
		if row.Get("host") != w.host {
			t.Errorf("row %d host = %#v, want %q", i, row.Get("host"), w.host)
		}
		if v, ok := row.Get("max_usage").(float64); !ok || v != w.max {
			t.Errorf("row %d max_usage = %#v, want %v", i, row.Get("max_usage"), w.max)
		}
	}
}

func TestGroupByTimeBucket(t *testing.T) {
	// Buckets of 100ns: a@100/200/300, b@150->100, b@250->200.
	ex := newExecutor(t)
	r := mustExec(t, ex, "SELECT mean(usage) FROM cpu GROUP BY time(100)")
	if !reflect.DeepEqual(r.Columns, []string{"time", "mean_usage"}) {
		t.Errorf("columns = %v, want [time mean_usage]", r.Columns)
	}
	want := map[int64]float64{100: 5.5, 200: 11.0, 300: 3.0}
	if len(r.Rows) != len(want) {
		t.Fatalf("n_rows = %d, want %d", len(r.Rows), len(want))
	}
	for _, row := range r.Rows {
		bucket, ok := row.Get("time").(int64)
		if !ok {
			t.Fatalf("row time is %T, want int64", row.Get("time"))
		}
		mean, ok := row.Get("mean_usage").(float64)
		if !ok {
			t.Fatalf("row mean_usage is %T, want float64", row.Get("mean_usage"))
		}
		if w, ok := want[bucket]; !ok || math_near(mean, w) == false {
			t.Errorf("bucket %d mean = %v, want %v", bucket, mean, w)
		}
	}
}

func TestGroupByTagAndTime(t *testing.T) {
	ex := newExecutor(t)
	r := mustExec(t, ex, "SELECT max(usage) FROM cpu GROUP BY host, time(100)")
	type key struct {
		host   string
		bucket int64
	}
	got := map[key]float64{}
	for _, row := range r.Rows {
		host, _ := row.Get("host").(string)
		bucket, _ := row.Get("time").(int64)
		max, _ := row.Get("max_usage").(float64)
		got[key{host, bucket}] = max
	}
	if v := got[key{"a", 100}]; v != 1.0 {
		t.Errorf("max(a,100) = %v, want 1.0", v)
	}
	if v := got[key{"b", 200}]; v != 20.0 {
		t.Errorf("max(b,200) = %v, want 20.0", v)
	}
}

func TestAggregateRespectsWhere(t *testing.T) {
	ex := newExecutor(t)
	r := mustExec(t, ex, "SELECT max(usage) FROM cpu WHERE host='a'")
	if v, ok := r.Rows[0].Get("max_usage").(float64); !ok || v != 3.0 {
		t.Errorf("max_usage = %#v, want 3.0", r.Rows[0].Get("max_usage"))
	}
}

// --- rendering -------------------------------------------------------------- //

func TestCSVShape(t *testing.T) {
	row := NewRow()
	row.Set("name", "cpu")
	row.Set("time", int64(1))
	row.Set("v", 1.5)
	rs := NewResultSet([]string{"name", "time", "v"}, []*Row{row})
	got := strings.Split(strings.TrimSuffix(rs.ToCSV(), "\n"), "\n")
	want := []string{"name,time,v", "cpu,1,1.5"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("csv = %q, want %q", got, want)
	}
}

func TestJSONShape(t *testing.T) {
	row1, row2 := NewRow(), NewRow()
	row1.Set("a", int64(1))
	row2.Set("a", int64(2))
	rs := NewResultSet([]string{"a"}, []*Row{row1, row2})
	var doc struct {
		Columns []string         `json:"columns"`
		Rows    []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(rs.ToJSON()), &doc); err != nil {
		t.Fatalf("ToJSON is not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(doc.Columns, []string{"a"}) {
		t.Errorf("columns = %v, want [a]", doc.Columns)
	}
	if len(doc.Rows) != 2 {
		t.Errorf("n_rows = %d, want 2", len(doc.Rows))
	}
}

func math_near(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-12
}
