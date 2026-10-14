// Package lineproto implements the InfluxDB *line protocol*: parsing text
// lines into points and serialising points back to canonical text.
//
// The line protocol is the de-facto text wire format for time-series
// ingestion:
//
//	measurement[,tag1=v1][,tag2=v2] field1=fval[,field2=fval]... [timestamp]
//
// Field values are typed by a trailing suffix (an integer ends in "i"),
// booleans are "true"/"false", anything else numeric is a float and untyped
// non-numeric values are strings. Backslash escaping of space/comma/equals
// keeps names and values unambiguous.
package lineproto

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Kelmory/local-tsdb/internal/model"
)

// Error reports a malformed line-protocol string.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errf(format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

// ParseLine parses one line-protocol string into a Point.
func ParseLine(line string) (model.Point, error) {
	text := strings.TrimSpace(line)
	if text == "" || strings.HasPrefix(text, "#") {
		return model.Point{}, errf("empty line")
	}

	// 1) Top-level split on *unescaped* spaces outside quotes:
	//    [head, fields(, ts)].
	tokens := splitUnescaped(text, ' ', true)
	if len(tokens) < 2 {
		return model.Point{}, errf("missing fields section")
	}
	var ts int64
	last := tokens[len(tokens)-1]
	if isTimestampToken(last) {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil {
			return model.Point{}, errf("invalid timestamp %q", last)
		}
		ts = n
		tokens = tokens[:len(tokens)-1]
	}
	// InfluxDB rejects extra space-separated sections (fields are comma
	// separated; a space means the next token would be a second timestamp).
	if len(tokens) > 2 {
		return model.Point{}, errf("unexpected extra token after fields: %q", tokens[2])
	}
	// A timestamp-only tail (e.g. "m,t=1 100" -- tags but no fields) must be
	// rejected, not indexed out of range.
	if len(tokens) < 2 {
		return model.Point{}, errf("missing fields section")
	}
	head, tail := tokens[0], tokens[1]

	// 2) Split measurement / tags from the head.
	segs := splitUnescaped(head, ',', false)
	measurement := unescape(segs[0])
	tags := make(map[string]string)
	for _, seg := range segs[1:] {
		name, value, ok := cutByte(seg, '=')
		if !ok {
			return model.Point{}, errf("bad tag %q", seg)
		}
		tags[unescape(name)] = unescape(value)
	}

	// 3) Fields (commas inside quoted string values do not split).
	fields := make(map[string]any)
	for _, seg := range splitUnescaped(tail, ',', true) {
		name, value, ok := cutByte(seg, '=')
		if !ok {
			return model.Point{}, errf("bad field %q", seg)
		}
		v, err := parseValue(value)
		if err != nil {
			return model.Point{}, err
		}
		fields[unescape(name)] = v
	}

	return model.Point{Measurement: measurement, Tags: tags, Fields: fields, Timestamp: ts}, nil
}

// cutByte splits s at the first occurrence of sep ("strings.Cut" semantics).
func cutByte(s string, sep byte) (before, after string, found bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// splitUnescaped splits s on sep ignoring backslash-escaped occurrences.
// With quotes a sep inside an unescaped double-quoted string does not split
// either -- needed for field values like s="a b".
func splitUnescaped(s string, sep byte, quotes bool) []string {
	parts := make([]string, 0, 4)
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			cur.WriteByte(c)
			cur.WriteByte(s[i+1])
			i++
			continue
		}
		if quotes && c == '"' {
			inQuote = !inQuote
			cur.WriteByte(c)
			continue
		}
		if c == sep && !inQuote {
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	return append(parts, cur.String())
}

func unescape(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte(s[i+1])
			i++
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func escape(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == ',' || c == '=' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

// isTimestampToken mirrors Python's "last.isdigit() or (starts with '-' and
// the rest isdigit())".
func isTimestampToken(s string) bool {
	if strings.HasPrefix(s, "-") {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isIntLiteral mirrors Python's (s[:-1]).lstrip("-").isdigit() test.
func isIntLiteral(body string) bool {
	body = strings.TrimLeft(body, "-")
	if body == "" {
		return false
	}
	for i := 0; i < len(body); i++ {
		if body[i] < '0' || body[i] > '9' {
			return false
		}
	}
	return true
}

// parseValue parses a single field value into a typed Go value: int64,
// float64, bool or string.
func parseValue(s string) (any, error) {
	if strings.HasSuffix(s, "i") && isIntLiteral(s[:len(s)-1]) {
		n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
		if err != nil {
			return nil, errf("invalid integer %q", s)
		}
		return n, nil
	}
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if f, ok := parseFloat(s); ok {
		return f, nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = unescape(s[1 : len(s)-1])
	} else {
		s = unescape(s)
	}
	return s, nil
}

// parseFloat accepts what Python's float() accepts: decimal / scientific
// notation and the inf/nan specials, but not Go-only hex or underscore forms.
func parseFloat(s string) (float64, bool) {
	body := s
	if len(body) > 0 && (body[0] == '+' || body[0] == '-') {
		body = body[1:]
	}
	if strings.HasPrefix(body, "0x") || strings.HasPrefix(body, "0X") ||
		strings.Contains(s, "_") {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
