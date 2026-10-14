package lineproto

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/Kelmory/local-tsdb/internal/model"
)

func mustParse(t *testing.T, line string) model.Point {
	t.Helper()
	p, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine(%q) unexpected error: %v", line, err)
	}
	return p
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestParseMeasurementOnly(t *testing.T) {
	p := mustParse(t, "cpu usage=0.5")
	if p.Measurement != "cpu" {
		t.Errorf("measurement = %q, want cpu", p.Measurement)
	}
	if len(p.Tags) != 0 {
		t.Errorf("tags = %v, want empty", p.Tags)
	}
	if v, ok := p.Fields["usage"].(float64); !ok || !near(v, 0.5) {
		t.Errorf("fields[usage] = %#v, want 0.5", p.Fields["usage"])
	}
}

func TestParseTagsAndIntSuffix(t *testing.T) {
	p := mustParse(t, "cpu,host=a,region=us-east usage=0.67,idle=3i 1700000000000")
	wantTags := map[string]string{"host": "a", "region": "us-east"}
	if !reflect.DeepEqual(p.Tags, wantTags) {
		t.Errorf("tags = %v, want %v", p.Tags, wantTags)
	}
	idle, ok := p.Fields["idle"].(int64)
	if !ok {
		t.Fatalf("fields[idle] is %T, want int64", p.Fields["idle"])
	}
	if idle != 3 {
		t.Errorf("fields[idle] = %d, want 3", idle)
	}
	if v, ok := p.Fields["usage"].(float64); !ok || !near(v, 0.67) {
		t.Errorf("fields[usage] = %#v, want 0.67", p.Fields["usage"])
	}
	if p.Timestamp != 1700000000000 {
		t.Errorf("timestamp = %d, want 1700000000000", p.Timestamp)
	}
}

func TestParseNoTimestampDefaultsZero(t *testing.T) {
	p := mustParse(t, "m a=1")
	if p.Timestamp != 0 {
		t.Errorf("timestamp = %d, want 0", p.Timestamp)
	}
}

func TestParseBoolField(t *testing.T) {
	p := mustParse(t, "m ok=true,bad=false")
	if v, ok := p.Fields["ok"].(bool); !ok || !v {
		t.Errorf("fields[ok] = %#v, want true", p.Fields["ok"])
	}
	if v, ok := p.Fields["bad"].(bool); !ok || v {
		t.Errorf("fields[bad] = %#v, want false", p.Fields["bad"])
	}
}

func TestParseStringField(t *testing.T) {
	p := mustParse(t, "m note=hello")
	if p.Fields["note"] != "hello" {
		t.Errorf("fields[note] = %#v, want hello", p.Fields["note"])
	}
}

func TestParseEscapedSpaceInValue(t *testing.T) {
	p := mustParse(t, "cpu,host=a\\ b usage=1.0")
	if p.Tags["host"] != "a b" {
		t.Errorf("tags[host] = %q, want %q", p.Tags["host"], "a b")
	}
}

func TestParseEscapedCommaInTagValue(t *testing.T) {
	p := mustParse(t, "cpu,region=us\\,east a=1")
	if p.Tags["region"] != "us,east" {
		t.Errorf("tags[region] = %q, want %q", p.Tags["region"], "us,east")
	}
}

func TestParseQuotedStringWithUnescapedSpace(t *testing.T) {
	// Regression: the space inside the quoted value used to split the
	// fields section and lose everything after it.
	p := mustParse(t, `m s="hello world",n=1i 123`)
	if p.Fields["s"] != "hello world" {
		t.Errorf("fields[s] = %#v, want %q", p.Fields["s"], "hello world")
	}
	if p.Fields["n"] != int64(1) {
		t.Errorf("fields[n] = %#v, want 1", p.Fields["n"])
	}
	if p.Timestamp != 123 {
		t.Errorf("timestamp = %d, want 123", p.Timestamp)
	}
}

func TestParseSpaceSeparatedExtraSectionRejected(t *testing.T) {
	// Fields must be comma separated (InfluxDB rejects this too).
	if _, err := ParseLine(`m s="hello world" n=1i`); err == nil {
		t.Error("expected error for extra space-separated section")
	}
}

func TestParseQuotedStringWithComma(t *testing.T) {
	p := mustParse(t, `m s="a,b",n=2i`)
	if p.Fields["s"] != "a,b" {
		t.Errorf("fields[s] = %#v, want %q", p.Fields["s"], "a,b")
	}
	if p.Fields["n"] != int64(2) {
		t.Errorf("fields[n] = %#v, want 2", p.Fields["n"])
	}
}

func TestParseNegativeTimestamp(t *testing.T) {
	// Regression: a negative ts used to fail the isdigit check and be
	// misread as part of the fields section.
	p := mustParse(t, "m v=1.0 -100")
	if p.Timestamp != -100 {
		t.Errorf("timestamp = %d, want -100", p.Timestamp)
	}
}

func roundTrip(t *testing.T, p model.Point) model.Point {
	t.Helper()
	q, err := ParseLine(FormatPoint(p))
	if err != nil {
		t.Fatalf("round-trip error: %v (line: %s)", err, FormatPoint(p))
	}
	return q
}

func TestFormatRoundtripAllTypes(t *testing.T) {
	p := model.Point{
		Measurement: "cpu",
		Tags:        map[string]string{"host": "a b", "region": "us,east"},
		Fields: map[string]any{
			"load": 1.5, "n": int64(7), "ok": true, "note": "x y,z",
		},
		Timestamp: 999,
	}
	q := roundTrip(t, p)
	if q.Measurement != p.Measurement {
		t.Errorf("measurement = %q, want %q", q.Measurement, p.Measurement)
	}
	if !reflect.DeepEqual(q.Tags, p.Tags) {
		t.Errorf("tags = %v, want %v", q.Tags, p.Tags)
	}
	if !reflect.DeepEqual(q.Fields, p.Fields) {
		t.Errorf("fields = %v, want %v", q.Fields, p.Fields)
	}
	if q.Timestamp != p.Timestamp {
		t.Errorf("timestamp = %d, want %d", q.Timestamp, p.Timestamp)
	}
}

func TestFormatIntRenderedWithSuffix(t *testing.T) {
	line := FormatPoint(model.Point{Measurement: "m", Fields: map[string]any{"n": int64(5)}})
	if !strings.Contains(line, "n=5i") {
		t.Errorf("line %q does not contain n=5i", line)
	}
}

func TestErrorMissingFields(t *testing.T) {
	if _, err := ParseLine("cpu"); err == nil {
		t.Error("expected error for line without fields")
	}
}

func TestErrorBadTagSegment(t *testing.T) {
	if _, err := ParseLine("cpu,bogus a=1"); err == nil {
		t.Error("expected error for malformed tag segment")
	}
}

func TestErrorTagsWithoutFields(t *testing.T) {
	// Regression: "m,t=1 100" (timestamp but no fields section) must be
	// rejected with an error, not panic on the missing fields token.
	if _, err := ParseLine("m,t=1 100"); err == nil {
		t.Error("expected error for tags-only line with timestamp")
	}
}

func TestErrorEmptyString(t *testing.T) {
	if _, err := ParseLine(""); err == nil {
		t.Error("expected error for empty line")
	}
}
