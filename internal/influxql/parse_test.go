package influxql

import (
	"math"
	"testing"

	"github.com/Kelmory/local-tsdb/internal/model"
)

func mustParse(t *testing.T, sql string, nowNs int64) Query {
	t.Helper()
	q, err := ParseQuery(sql, nowNs)
	if err != nil {
		t.Fatalf("ParseQuery(%q) unexpected error: %v", sql, err)
	}
	return q
}

func TestParseBasic(t *testing.T) {
	q := mustParse(t, "SELECT * FROM cpu", 0)
	sel, ok := q.(*SelectQuery)
	if !ok {
		t.Fatalf("want *SelectQuery, got %T", q)
	}
	if sel.Measurement != "cpu" {
		t.Errorf("measurement = %q, want cpu", sel.Measurement)
	}
}

func TestStrictInequalityShiftsBound(t *testing.T) {
	q := mustParse(t, "SELECT * FROM m WHERE time > 100 AND time < 300", 0).(*SelectQuery)
	if q.Start == nil || *q.Start != 101 {
		t.Errorf("start = %v, want 101", q.Start)
	}
	if q.End == nil || *q.End != 299 {
		t.Errorf("end = %v, want 299", q.End)
	}
}

func TestInclusiveBounds(t *testing.T) {
	q := mustParse(t, "SELECT * FROM m WHERE time >= 100 AND time <= 300", 0).(*SelectQuery)
	if q.Start == nil || *q.Start != 100 {
		t.Errorf("start = %v, want 100", q.Start)
	}
	if q.End == nil || *q.End != 300 {
		t.Errorf("end = %v, want 300", q.End)
	}
}

func TestNowArithmetic(t *testing.T) {
	now := int64(5_000_000_000)
	q := mustParse(t, "SELECT * FROM m WHERE time > now() - 1h", now).(*SelectQuery)
	want := now - 3600*1000*1000*1000 + 1
	if q.Start == nil || *q.Start != want {
		t.Errorf("start = %v, want %d", q.Start, want)
	}
}

func TestNowPlus(t *testing.T) {
	now := int64(5_000_000_000)
	q := mustParse(t, "SELECT * FROM m WHERE time < now() + 5m", now).(*SelectQuery)
	want := now + 300*1000*1000*1000 - 1
	if q.End == nil || *q.End != want {
		t.Errorf("end = %v, want %d", q.End, want)
	}
}

func TestRFC3339Date(t *testing.T) {
	// 2024-01-01T00:00:00Z == 1704067200 s.
	q := mustParse(t, "SELECT * FROM m WHERE time >= '2024-01-01T00:00:00Z'", 0).(*SelectQuery)
	want := int64(1704067200) * 1000 * 1000 * 1000
	if q.Start == nil || *q.Start != want {
		t.Errorf("start = %v, want %d", q.Start, want)
	}
}

func TestTagEqAndNeq(t *testing.T) {
	q := mustParse(t, "SELECT * FROM m WHERE host='a' AND region!='x'", 0).(*SelectQuery)
	wantEq := map[string]string{"host": "a"}
	wantNeq := map[string]string{"region": "x"}
	if len(q.TagEq) != 1 || q.TagEq["host"] != "a" {
		t.Errorf("tag_eq = %v, want %v", q.TagEq, wantEq)
	}
	if len(q.TagNeq) != 1 || q.TagNeq["region"] != "x" {
		t.Errorf("tag_neq = %v, want %v", q.TagNeq, wantNeq)
	}
}

func TestGroupByTimeAndTags(t *testing.T) {
	q := mustParse(t, "SELECT max(v) FROM m GROUP BY time(1h), host LIMIT 5", 0).(*SelectQuery)
	if q.TimeBucket != 3600*1000*1000*1000 {
		t.Errorf("time_bucket = %d, want %d", q.TimeBucket, 3600*1000*1000*1000)
	}
	if len(q.GroupTags) != 1 || q.GroupTags[0] != "host" {
		t.Errorf("group_tags = %v, want [host]", q.GroupTags)
	}
	if q.Limit != 5 {
		t.Errorf("limit = %d, want 5", q.Limit)
	}
}

func TestUnknownFunctionRaises(t *testing.T) {
	if _, err := ParseQuery("SELECT median(v) FROM m", 0); err == nil {
		t.Error("expected error for unknown function")
	}
}

func TestTrailingTokensRaise(t *testing.T) {
	if _, err := ParseQuery("SELECT * FROM cpu WHERE host='a' AND", 0); err == nil {
		t.Error("expected error for trailing tokens")
	}
}

func TestEmptyQueryRaises(t *testing.T) {
	if _, err := ParseQuery("   ", 0); err == nil {
		t.Error("expected error for empty query")
	}
}

// FormatFloat behaviour is shared with CSV rendering; smoke-test the
// Python-repr boundaries here where the helper's callers live.
func TestFormatFloatParity(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{1.5, "1.5"},
		{0.67, "0.67"},
		{7.2, "7.2"},
		{1.0, "1.0"},
		{100.0, "100.0"},
		{1e15, "1000000000000000.0"},
		{1e16, "1e+16"},
		{1.5e16, "1.5e+16"},
		{1e-5, "1e-05"},
		{0.0001, "0.0001"},
		{math.NaN(), "nan"},
		{math.Inf(1), "inf"},
		{math.Inf(-1), "-inf"},
	}
	for _, c := range cases {
		if got := model.FormatFloat(c.in); got != c.want {
			t.Errorf("FormatFloat(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	// Go folds the -0.0 constant to +0, so exercise negative zero at runtime.
	negZero := math.Copysign(0, -1)
	if got := model.FormatFloat(negZero); got != "-0.0" {
		t.Errorf("FormatFloat(-0.0) = %q, want -0.0", got)
	}
}
