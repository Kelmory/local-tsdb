package influxql

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Kelmory/local-tsdb/internal/model"
)

// Row is one result row: an ordered map so JSON keys keep insertion order.
type Row struct {
	Keys []string
	vals map[string]any
}

func NewRow() *Row { return &Row{vals: map[string]any{}} }

// Set stores a value, keeping first-insertion key order.
func (r *Row) Set(key string, v any) {
	if _, ok := r.vals[key]; !ok {
		r.Keys = append(r.Keys, key)
	}
	r.vals[key] = v
}

func (r *Row) Get(key string) any { return r.vals[key] }

// ResultSet is the columnar result of a query, renderable to InfluxDB-style
// CSV or JSON.
type ResultSet struct {
	Columns []string
	Rows    []*Row
}

func NewResultSet(columns []string, rows []*Row) *ResultSet {
	return &ResultSet{Columns: append([]string(nil), columns...), Rows: rows}
}

func (rs *ResultSet) NRows() int { return len(rs.Rows) }

// ToCSV renders the result as CSV (header line first, LF line endings,
// trailing newline). Empty / missing cells render as quoted empty strings,
// floats use Python-repr notation, booleans render as True/False.
func (rs *ResultSet) ToCSV() string {
	var b strings.Builder
	for i, c := range rs.Columns {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(csvCell(c))
	}
	b.WriteByte('\n')
	for _, row := range rs.Rows {
		for i, c := range rs.Columns {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(csvCell(row.Get(c)))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func csvCell(v any) string {
	var s string
	switch x := v.(type) {
	case nil:
		s = ""
	case string:
		s = x
	case float64:
		s = model.FormatFloat(x)
	case bool:
		if x {
			s = "True"
		} else {
			s = "False"
		}
	case int64:
		s = strconv.FormatInt(x, 10)
	case int:
		s = strconv.Itoa(x)
	default:
		s = fmt.Sprint(v)
	}
	if strings.ContainsAny(s, ",\"\n") || s == "" {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// ToJSON renders {"columns": [...], "rows": [{...}, ...]} preserving row key
// insertion order. NaN/Infinity render like Python's json.dumps.
func (rs *ResultSet) ToJSON() string {
	var b strings.Builder
	b.WriteString(`{"columns": [`)
	for i, c := range rs.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		writeJSONStr(&b, c)
	}
	b.WriteString(`], "rows": [`)
	for i, row := range rs.Rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('{')
		for j, k := range row.Keys {
			if j > 0 {
				b.WriteString(", ")
			}
			writeJSONStr(&b, k)
			b.WriteString(": ")
			writeJSONVal(&b, row.Get(k))
		}
		b.WriteByte('}')
	}
	b.WriteString("]}")
	return b.String()
}

func writeJSONVal(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int:
		b.WriteString(strconv.Itoa(x))
	case float64:
		switch model.FormatFloat(x) {
		case "nan":
			b.WriteString("NaN")
		case "inf":
			b.WriteString("Infinity")
		case "-inf":
			b.WriteString("-Infinity")
		default:
			b.WriteString(model.FormatFloat(x))
		}
	case string:
		writeJSONStr(b, x)
	default:
		writeJSONStr(b, fmt.Sprint(v))
	}
}

func writeJSONStr(b *strings.Builder, s string) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil { // Encode appends a newline
		b.WriteString(`""`)
		return
	}
	b.WriteString(strings.TrimSuffix(sb.String(), "\n"))
}
