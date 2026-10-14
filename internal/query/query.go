// Package query implements the query engine: series resolution, block-level
// time pruning, memtable merge, projection and aggregation. It sits on top of
// an index, a block reader callback and the uncommitted memtable.
package query

import (
	"fmt"
	"sort"

	"github.com/Kelmory/local-tsdb/internal/block"
	"github.com/Kelmory/local-tsdb/internal/index"
	"github.com/Kelmory/local-tsdb/internal/model"
)

// Row is one decoded sample: a timestamp plus field values (nil = NULL).
type Row struct {
	Ts     int64
	Fields map[string]any
}

// QueryEngine resolves and folds rows. Dependencies are injected so it can be
// exercised against an in-memory fake block reader.
type QueryEngine struct {
	Index     *index.Index
	ReadBlock func(locator int) (*block.Block, error)
	Memtable  map[string][]model.Point
}

// New builds a query engine over the given dependencies.
func New(ix *index.Index, readBlock func(int) (*block.Block, error), memtable map[string][]model.Point) *QueryEngine {
	return &QueryEngine{Index: ix, ReadBlock: readBlock, Memtable: memtable}
}

// MatchSeries returns sorted series keys matching measurement + tag filters.
// A measurement matches by exact key or by "measurement," prefix.
func (qe *QueryEngine) MatchSeries(measurement string, tagFilters map[string]string) []string {
	if tagFilters == nil {
		tagFilters = map[string]string{}
	}
	keys := qe.Index.SeriesForTags(tagFilters)
	if len(keys) == 0 {
		return nil
	}
	var result []string
	for k := range keys {
		if measurement == "" || k == measurement || len(k) > len(measurement) && k[:len(measurement)+1] == measurement+"," {
			result = append(result, k)
		}
	}
	// If a specific measurement was given but no tag filter, restrict to that
	// measurement's series exactly (mirrors the Python reference).
	if measurement != "" && len(tagFilters) == 0 {
		result = nil
		for _, k := range qe.Index.AllSeries() {
			if k == measurement || len(k) > len(measurement) && k[:len(measurement)+1] == measurement+"," {
				result = append(result, k)
			}
		}
	}
	sort.Strings(result)
	return result
}

// IterRows yields (ts, fields) from disk blocks (pruned) then memtable.
func (qe *QueryEngine) IterRows(seriesKey string, start, end *int64) ([]Row, error) {
	var out []Row
	for _, ref := range qe.Index.Blocks(seriesKey) {
		if start != nil && ref.MaxTs < *start {
			continue // whole block before range
		}
		if end != nil && ref.MinTs > *end {
			continue // whole block after range
		}
		blk, err := qe.ReadBlock(ref.Locator)
		if err != nil {
			return nil, err
		}
		for _, row := range blk.Rows() {
			if (start == nil || row.Timestamp >= *start) && (end == nil || row.Timestamp <= *end) {
				out = append(out, Row{Ts: row.Timestamp, Fields: row.Fields})
			}
		}
	}
	for _, p := range qe.Memtable[seriesKey] {
		if (start == nil || p.Timestamp >= *start) && (end == nil || p.Timestamp <= *end) {
			fields := make(map[string]any, len(p.Fields))
			for k, v := range p.Fields {
				fields[k] = v
			}
			out = append(out, Row{Ts: p.Timestamp, Fields: fields})
		}
	}
	return out, nil
}

// Select returns matching points ordered by (series key, timestamp).
func (qe *QueryEngine) Select(measurement string, tagFilters map[string]string,
	fields []string, start, end *int64, limit int) ([]model.Point, error) {
	if tagFilters == nil {
		tagFilters = map[string]string{}
	}
	var out []model.Point
	for _, key := range qe.MatchSeries(measurement, tagFilters) {
		measurementName, tags := splitKey(key)
		rows, err := qe.IterRows(key, start, end)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			fvals := row.Fields
			if fields != nil {
				filtered := make(map[string]any, len(fields))
				for _, f := range fields {
					if v, ok := fvals[f]; ok {
						filtered[f] = v
					}
				}
				fvals = filtered
			}
			out = append(out, model.Point{
				Measurement: measurementName, Tags: tags,
				Fields: fvals, Timestamp: row.Ts,
			})
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
	}
	// Stable time ordering across series.
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := out[i].SeriesKey(), out[j].SeriesKey()
		if si != sj {
			return si < sj
		}
		return out[i].Timestamp < out[j].Timestamp
	})
	return out, nil
}

// AggregateRequest describes one fold over matching rows.
type AggregateRequest struct {
	Measurement string
	Field       string
	Func        string // count|min|max|sum|mean
	TagFilters  map[string]string
	Start, End  *int64
	GroupBy     string // tag name; "" = no grouping
}

// AggregateResult is scalar (Groups == nil) or per-tag-value.
type AggregateResult struct {
	Scalar any            // int64 for count, float64 otherwise; nil when empty
	Groups map[string]any // sorted key iteration handled by callers
}

var validFuncs = map[string]bool{"count": true, "min": true, "max": true, "sum": true, "mean": true}

// Aggregate folds matching rows into a scalar or per-tag-group result.
// NULL / absent fields never fold in.
func (qe *QueryEngine) Aggregate(req AggregateRequest) (AggregateResult, error) {
	if !validFuncs[req.Func] {
		return AggregateResult{}, fmt.Errorf("unknown aggregate %q", req.Func)
	}
	if req.TagFilters == nil {
		req.TagFilters = map[string]string{}
	}
	var values []any
	groups := map[string][]any{}
	for _, key := range qe.MatchSeries(req.Measurement, req.TagFilters) {
		_, tags := splitKey(key)
		var gval string
		if req.GroupBy != "" {
			gval = tags[req.GroupBy]
		}
		rows, err := qe.IterRows(key, req.Start, req.End)
		if err != nil {
			return AggregateResult{}, err
		}
		for _, row := range rows {
			v := row.Fields[req.Field]
			if v == nil {
				continue // NULL / absent fields never fold in
			}
			if req.GroupBy != "" {
				groups[gval] = append(groups[gval], v)
			} else {
				values = append(values, v)
			}
		}
	}
	if req.GroupBy != "" {
		res := AggregateResult{Groups: map[string]any{}}
		gkeys := make([]string, 0, len(groups))
		for g := range groups {
			gkeys = append(gkeys, g)
		}
		sort.Strings(gkeys)
		for _, g := range gkeys {
			res.Groups[g] = finish(req.Func, reduce(req.Func, groups[g]))
		}
		return res, nil
	}
	return AggregateResult{Scalar: finish(req.Func, reduce(req.Func, values))}, nil
}

func finish(fn string, acc any) any {
	if acc == nil {
		return nil
	}
	if fn == "mean" {
		if sc, ok := acc.(sumCount); ok {
			if sc.n == 0 {
				return nil
			}
			return sc.sum / float64(sc.n)
		}
		return acc
	}
	return acc
}

type sumCount struct {
	sum float64
	n   int64
}

func reduce(fn string, values []any) any {
	if fn == "count" {
		return int64(len(values))
	}
	if len(values) == 0 {
		if fn == "mean" {
			return sumCount{}
		}
		return nil
	}
	switch fn {
	case "min", "max":
		acc := values[0]
		for _, v := range values[1:] {
			if (fn == "min" && floatValue(v) < floatValue(acc)) ||
				(fn == "max" && floatValue(v) > floatValue(acc)) {
				acc = v
			}
		}
		return floatValue(acc)
	case "sum":
		var s float64
		for _, v := range values {
			s += floatValue(v)
		}
		return s
	default: // mean
		acc := sumCount{sum: floatValue(values[0]), n: 1}
		for _, v := range values[1:] {
			acc.sum += floatValue(v)
			acc.n++
		}
		return acc
	}
}

func floatValue(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	}
	return 0
}

func splitKey(key string) (string, map[string]string) {
	measurement := key
	var tags map[string]string
	for i := 0; i < len(key); i++ {
		if key[i] == ',' {
			measurement = key[:i]
			rest := key[i+1:]
			tags = map[string]string{}
			start := 0
			for j := 0; j <= len(rest); j++ {
				if j == len(rest) || rest[j] == ',' {
					part := rest[start:j]
					for x := 0; x < len(part); x++ {
						if part[x] == '=' {
							tags[part[:x]] = part[x+1:]
							break
						}
					}
					start = j + 1
				}
			}
			break
		}
	}
	if tags == nil {
		tags = map[string]string{}
	}
	return measurement, tags
}
