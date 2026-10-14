// Package model defines the core data vocabulary of the database: points,
// series keys and field types. It has no dependencies on other packages.
package model

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// FieldType tags drive line-protocol parsing and compression routing.
type FieldType byte

const (
	TypeInt   FieldType = 1 // 64-bit signed integer
	TypeFloat FieldType = 2 // IEEE-754 double
	TypeBool  FieldType = 3 // boolean
	TypeStr   FieldType = 4 // UTF-8 string
)

func (t FieldType) String() string {
	switch t {
	case TypeInt:
		return "int"
	case TypeFloat:
		return "float"
	case TypeBool:
		return "bool"
	case TypeStr:
		return "str"
	default:
		return fmt.Sprintf("FieldType(%d)", byte(t))
	}
}

// Point is a single measurement sample. Timestamp is nanoseconds since the
// epoch; zero means "assign on write".
type Point struct {
	Measurement string
	Tags        map[string]string
	Fields      map[string]any
	Timestamp   int64
}

// SeriesKey returns the canonical InfluxDB-style series key for this point.
func (p Point) SeriesKey() string { return SeriesKey(p.Measurement, p.Tags) }

// SeriesKey builds the canonical "measurement[,tag1=v1][,tag2=v2]" string.
// Tags are emitted sorted by name so logically identical tag sets always
// produce one string regardless of insertion order.
func SeriesKey(measurement string, tags map[string]string) string {
	if len(tags) == 0 {
		return measurement
	}
	names := make([]string, 0, len(tags))
	for k := range tags {
		names = append(names, k)
	}
	sortStrings(names)
	out := measurement
	for _, k := range names {
		out += "," + k + "=" + tags[k]
	}
	return out
}

// ParseSeriesKey parses a series key back into (measurement, tags).
func ParseSeriesKey(key string) (string, map[string]string, error) {
	if key == "" {
		return "", nil, fmt.Errorf("empty series key")
	}
	parts := splitByte(key, ',')
	measurement := parts[0]
	tags := make(map[string]string)
	for _, part := range parts[1:] {
		eq := -1
		for i := 0; i < len(part); i++ {
			if part[i] == '=' {
				eq = i
				break
			}
		}
		if eq < 0 {
			return "", nil, fmt.Errorf("malformed series key segment %q", part)
		}
		tags[part[:eq]] = part[eq+1:]
	}
	return measurement, tags, nil
}

// FieldTypeOf infers the field type from a Go value. Bool is checked first
// for parity with the Python reference (where bool is an int subclass).
func FieldTypeOf(v any) (FieldType, error) {
	switch v.(type) {
	case bool:
		return TypeBool, nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return TypeInt, nil
	case float32, float64:
		return TypeFloat, nil
	case string:
		return TypeStr, nil
	default:
		return 0, fmt.Errorf("unsupported field value type: %T", v)
	}
}

// CoerceField converts a raw parsed value into the Go type implied by ft.
func CoerceField(v any, ft FieldType) (any, error) {
	switch ft {
	case TypeInt:
		switch x := v.(type) {
		case int:
			return int64(x), nil
		case int64:
			return x, nil
		case string:
			return parseInt64(x)
		}
	case TypeFloat:
		switch x := v.(type) {
		case float64:
			return x, nil
		case int:
			return float64(x), nil
		case int64:
			return float64(x), nil
		case string:
			return parseFloat64(x)
		}
	case TypeBool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
	case TypeStr:
		if s, ok := v.(string); ok {
			return s, nil
		}
	default:
		return nil, fmt.Errorf("unknown field type %d", byte(ft))
	}
	return nil, fmt.Errorf("cannot coerce %T to %s", v, ft)
}

// FormatFloat renders a float like Python's repr(): shortest round-trip
// digits, fixed notation for decimal exponents in [-4, 16), scientific
// notation otherwise (with a two-digit exponent minimum), and the specials
// "nan" / "inf" / "-inf". Used wherever floats cross a text boundary.
func FormatFloat(f float64) string {
	if math.IsNaN(f) {
		return "nan"
	}
	if math.IsInf(f, 0) {
		if f > 0 {
			return "inf"
		}
		return "-inf"
	}
	sign := ""
	if math.Signbit(f) {
		sign = "-"
		f = math.Abs(f)
	}
	if f == 0 {
		return sign + "0.0"
	}
	// Shortest round-trip digits via the 'e' form: d.dddde±XX.
	s := strconv.FormatFloat(f, 'e', -1, 64)
	e := strings.IndexByte(s, 'e')
	mant := strings.Replace(s[:e], ".", "", 1)
	exp, _ := strconv.Atoi(s[e+1:])
	if exp >= 16 || exp < -4 {
		out := mant[:1]
		if len(mant) > 1 {
			out += "." + mant[1:]
		}
		es := strconv.Itoa(exp)
		if exp < 0 {
			es = es[1:] // strip the minus; re-added below
		}
		if len(es) < 2 {
			es = "0" + es
		}
		if exp < 0 {
			return sign + out + "e-" + es
		}
		return sign + out + "e+" + es
	}
	if exp >= len(mant)-1 {
		// Integer-valued: pad zeros and force one decimal place.
		return sign + mant + strings.Repeat("0", exp-len(mant)+1) + ".0"
	}
	if exp >= 0 {
		return sign + mant[:exp+1] + "." + mant[exp+1:]
	}
	return sign + "0." + strings.Repeat("0", -exp-1) + mant
}

// PackName length-prefixes a UTF-8 name (u16 LE) for on-disk storage.
func PackName(name string) ([]byte, error) {
	if len(name) > 0xFFFF {
		return nil, fmt.Errorf("field/measurement name too long")
	}
	out := make([]byte, 2+len(name))
	binary.LittleEndian.PutUint16(out, uint16(len(name)))
	copy(out[2:], name)
	return out, nil
}

// UnpackName reads a length-prefixed name; returns (name, next offset).
func UnpackName(buf []byte, off int) (string, int, error) {
	if off+2 > len(buf) {
		return "", off, fmt.Errorf("truncated name prefix")
	}
	n := int(binary.LittleEndian.Uint16(buf[off:]))
	off += 2
	if off+n > len(buf) {
		return "", off, fmt.Errorf("truncated name")
	}
	return string(buf[off : off+n]), off + n, nil
}

func sortStrings(s []string) {
	// insertion sort is fine for tiny tag sets, but use a simple quicksort
	// wrapper to stay allocation-free for the common small cases.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func splitByte(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func parseInt64(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64) // base 10: rejects hex, underscores
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", s)
	}
	return n, nil
}

func parseFloat64(s string) (float64, error) {
	// Reject the Go-only forms Python's float() does not accept.
	low := s
	if len(low) > 0 && (low[0] == '+' || low[0] == '-') {
		low = low[1:]
	}
	if strings.HasPrefix(low, "0x") || strings.HasPrefix(low, "0X") ||
		strings.ContainsAny(s, "_") {
		return 0, fmt.Errorf("invalid float %q", s)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid float %q", s)
	}
	return f, nil
}
