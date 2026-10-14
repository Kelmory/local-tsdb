package influxql

import (
	"sort"
	"strconv"
	"strings"

	"github.com/Kelmory/local-tsdb/internal/model"
	"github.com/Kelmory/local-tsdb/internal/query"
)

// Executor executes parsed queries against a query engine. Dependencies are
// injected so tests can drive it with a fake-backed QueryEngine.
type Executor struct {
	QE *query.QueryEngine
	// MeasurementsFn returns every measurement name, sorted (SHOW MEASUREMENTS).
	MeasurementsFn func() []string
	// Now returns the current time in ns; nil = time.Now.
	Now func() int64
}

// Execute parses and runs one statement, returning a ResultSet.
func (ex *Executor) Execute(sql string) (*ResultSet, error) {
	var nowNs int64
	if ex.Now != nil {
		nowNs = ex.Now()
	}
	q, err := ParseQuery(sql, nowNs)
	if err != nil {
		return nil, err
	}
	switch qq := q.(type) {
	case *ShowQuery:
		rows := make([]*Row, 0)
		if ex.MeasurementsFn != nil {
			for _, m := range ex.MeasurementsFn() {
				r := NewRow()
				r.Set("name", m)
				rows = append(rows, r)
			}
		}
		return NewResultSet([]string{"name"}, rows), nil
	case *SelectQuery:
		return ex.runSelect(qq)
	}
	return nil, &Error{Msg: "unknown query"}
}

func (ex *Executor) runSelect(q *SelectQuery) (*ResultSet, error) {
	hasAgg, hasPlain := false, false
	for _, it := range q.Items {
		switch it.Kind {
		case "agg":
			hasAgg = true
		case "star", "field":
			hasPlain = true
		}
	}
	if hasAgg && hasPlain {
		return nil, &Error{Msg: "cannot mix aggregates with bare fields / *"}
	}
	if !hasAgg && (len(q.GroupTags) > 0 || q.TimeBucket != 0) {
		return nil, &Error{Msg: "GROUP BY requires an aggregate select list"}
	}
	if hasAgg {
		return ex.runAggregate(q)
	}
	return ex.runScan(q)
}

// runScan executes a non-aggregate SELECT.
func (ex *Executor) runScan(q *SelectQuery) (*ResultSet, error) {
	// With tag != filters the LIMIT must be applied *after* filtering, so
	// fetch unbounded here and slice below.
	limit := 0
	if len(q.TagNeq) == 0 {
		limit = q.Limit
	}
	points, err := ex.QE.Select(q.Measurement, q.TagEq, nil, q.Start, q.End, limit)
	if err != nil {
		return nil, err
	}
	if len(q.TagNeq) > 0 {
		filtered := points[:0]
		for _, p := range points {
			excluded := false
			for k, v := range q.TagNeq {
				if p.Tags[k] == v {
					excluded = true
					break
				}
			}
			if !excluded {
				filtered = append(filtered, p)
			}
		}
		points = filtered
		if q.Limit > 0 && len(points) > q.Limit {
			points = points[:q.Limit]
		}
	}
	tagSet := map[string]bool{}
	for _, p := range points {
		for k := range p.Tags {
			tagSet[k] = true
		}
	}
	tagCols := sortedKeys(tagSet)
	fieldSet := map[string]bool{}
	for _, p := range points {
		for k := range p.Fields {
			fieldSet[k] = true
		}
	}
	fieldNames := sortedKeys(fieldSet)
	columns := append([]string{"name"}, tagCols...)
	columns = append(columns, "time")
	columns = append(columns, fieldNames...)
	rows := make([]*Row, 0, len(points))
	for _, p := range points {
		r := NewRow()
		r.Set("name", p.Measurement)
		r.Set("time", p.Timestamp)
		for _, t := range tagCols {
			if v, ok := p.Tags[t]; ok {
				r.Set(t, v)
			}
		}
		for _, f := range fieldNames {
			if v, ok := p.Fields[f]; ok {
				r.Set(f, v)
			}
		}
		rows = append(rows, r)
	}
	return NewResultSet(columns, rows), nil
}

// runAggregate executes an aggregate SELECT with optional tag and/or time
// bucket grouping.
func (ex *Executor) runAggregate(q *SelectQuery) (*ResultSet, error) {
	type group struct {
		tagVals []string
		bucket  int64
		count   int64
		cols    map[string][]any
	}
	groups := make([]*group, 0)
	byKey := map[string]*group{}
	groupKey := func(tagVals []string, bucket int64) string {
		return strings.Join(tagVals, "\x1f") + "\x1f#" + strconv.FormatInt(bucket, 10)
	}
	for _, key := range ex.candidateSeries(q) {
		_, tags, err := model.ParseSeriesKey(key)
		if err != nil {
			return nil, err
		}
		gprefix := make([]string, len(q.GroupTags))
		for i, t := range q.GroupTags {
			gprefix[i] = tags[t]
		}
		rows, err := ex.QE.IterRows(key, q.Start, q.End)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			var bucket int64
			if q.TimeBucket != 0 {
				bucket = floorDiv(row.Ts, q.TimeBucket) * q.TimeBucket
			}
			gk := groupKey(gprefix, bucket)
			g, ok := byKey[gk]
			if !ok {
				g = &group{
					tagVals: append([]string(nil), gprefix...),
					bucket:  bucket,
					cols:    map[string][]any{},
				}
				byKey[gk] = g
				groups = append(groups, g)
			}
			g.count++
			for _, it := range q.Items {
				if it.Kind != "agg" || it.Field == "" {
					continue
				}
				if v := row.Fields[it.Field]; v != nil {
					g.cols[it.Field] = append(g.cols[it.Field], v)
				}
			}
		}
	}
	// Tuple order: group tag values elementwise, then bucket.
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		for k := range a.tagVals {
			if k >= len(b.tagVals) {
				return false
			}
			if a.tagVals[k] != b.tagVals[k] {
				return a.tagVals[k] < b.tagVals[k]
			}
		}
		return a.bucket < b.bucket
	})
	var columns []string
	if q.TimeBucket != 0 {
		columns = append(columns, "time")
	}
	columns = append(columns, q.GroupTags...)
	for _, it := range q.Items {
		columns = append(columns, aggCol(it))
	}
	rows := make([]*Row, 0, len(groups))
	for _, g := range groups {
		r := NewRow()
		if q.TimeBucket != 0 {
			r.Set("time", g.bucket)
		}
		for i, t := range q.GroupTags {
			r.Set(t, g.tagVals[i])
		}
		for _, it := range q.Items {
			var v any
			switch {
			case it.Func == "count" && it.Field == "":
				v = g.count // count() counts rows
			case it.Func == "count":
				v = int64(len(g.cols[it.Field])) // count(field) counts non-nulls
			default:
				folded, err := fold(it.Func, g.cols[it.Field])
				if err != nil {
					return nil, err
				}
				v = folded
			}
			r.Set(aggCol(it), v)
		}
		rows = append(rows, r)
	}
	return NewResultSet(columns, rows), nil
}

// candidateSeries resolves series keys for measurement + tag filters and
// applies tag != exclusions.
func (ex *Executor) candidateSeries(q *SelectQuery) []string {
	keys := ex.QE.MatchSeries(q.Measurement, q.TagEq)
	if len(q.TagNeq) == 0 {
		return keys
	}
	var out []string
	for _, key := range keys {
		_, tags, err := model.ParseSeriesKey(key)
		if err != nil {
			continue
		}
		skip := false
		for k, v := range q.TagNeq {
			if tags[k] == v {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, key)
		}
	}
	return out
}

// aggCol names an aggregate column: "count" or e.g. "mean_usage".
func aggCol(it SelectItem) string {
	if it.Field == "" {
		return "count"
	}
	return it.Func + "_" + it.Field
}

// fold folds values like Python's _fold: empty input -> nil, sum keeps int
// precision over pure-int inputs, min/max return the original value.
func fold(fn string, vals []any) (any, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	switch fn {
	case "sum":
		var isum int64
		var fsum float64
		sawFloat := false
		for _, v := range vals {
			switch x := v.(type) {
			case int64:
				isum += x
			case int:
				isum += int64(x)
			case float64:
				fsum += x
				sawFloat = true
			case bool:
				if x {
					isum++
				}
			default:
				return nil, errf("cannot sum %T values", v)
			}
		}
		if sawFloat {
			return fsum + float64(isum), nil
		}
		return isum, nil
	case "mean":
		var sum float64
		for _, v := range vals {
			sum += toFloat(v)
		}
		return sum / float64(len(vals)), nil
	case "min", "max":
		best := vals[0]
		for _, v := range vals[1:] {
			if fn == "min" && lessThan(v, best) {
				best = v
			}
			if fn == "max" && lessThan(best, v) {
				best = v
			}
		}
		return best, nil
	}
	return nil, errf("unknown aggregate %q", fn)
}

func lessThan(a, b any) bool {
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		return as < bs
	}
	if aok != bok {
		return false // incomparable types keep their order
	}
	return toFloat(a) < toFloat(b)
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	}
	return 0
}

// floorDiv is Python-style floor division (Go's / truncates toward zero).
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
