package lineproto

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Kelmory/local-tsdb/internal/model"
)

// FormatPoint serialises a Point back to line protocol. Tags and fields are
// emitted sorted by name so identical points always round-trip to identical
// text.
func FormatPoint(p model.Point) string {
	var b strings.Builder
	b.WriteString(escape(p.Measurement))
	tagNames := make([]string, 0, len(p.Tags))
	for name := range p.Tags {
		tagNames = append(tagNames, name)
	}
	sort.Strings(tagNames)
	for _, name := range tagNames {
		b.WriteByte(',')
		b.WriteString(escape(name))
		b.WriteByte('=')
		b.WriteString(escape(p.Tags[name]))
	}
	fieldNames := make([]string, 0, len(p.Fields))
	for name := range p.Fields {
		fieldNames = append(fieldNames, name)
	}
	sort.Strings(fieldNames)
	for i, name := range fieldNames {
		if i == 0 {
			b.WriteByte(' ')
		} else {
			b.WriteByte(',')
		}
		b.WriteString(escape(name))
		b.WriteByte('=')
		b.WriteString(formatFieldValue(p.Fields[name]))
	}
	if p.Timestamp != 0 {
		b.WriteByte(' ')
		b.WriteString(strconv.FormatInt(p.Timestamp, 10))
	}
	return b.String()
}

func formatFieldValue(v any) string {
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10) + "i"
	case int:
		return strconv.Itoa(x) + "i"
	case float64:
		return model.FormatFloat(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case string:
		return escape(x)
	default:
		return escape(fmt.Sprint(v))
	}
}
