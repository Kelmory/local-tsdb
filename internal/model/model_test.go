package model

import (
	"bytes"
	"testing"
)

func TestSeriesKeyNoTags(t *testing.T) {
	if got := SeriesKey("cpu", nil); got != "cpu" {
		t.Fatalf("got %q", got)
	}
}

func TestSeriesKeySingleTag(t *testing.T) {
	if got := SeriesKey("cpu", map[string]string{"host": "a"}); got != "cpu,host=a" {
		t.Fatalf("got %q", got)
	}
}

func TestSeriesKeySortedCanonical(t *testing.T) {
	a := SeriesKey("cpu", map[string]string{"zeta": "1", "alpha": "2"})
	b := SeriesKey("cpu", map[string]string{"alpha": "2", "zeta": "1"})
	if a != b {
		t.Fatalf("insertion order mattered: %q vs %q", a, b)
	}
	if a != "cpu,alpha=2,zeta=1" {
		t.Fatalf("got %q", a)
	}
}

func TestPointSeriesKey(t *testing.T) {
	p := Point{Measurement: "m", Tags: map[string]string{"b": "2", "a": "1"}}
	if got := p.SeriesKey(); got != "m,a=1,b=2" {
		t.Fatalf("got %q", got)
	}
}

func TestParseSeriesKeyNoTags(t *testing.T) {
	m, tags, err := ParseSeriesKey("cpu")
	if err != nil || m != "cpu" || len(tags) != 0 {
		t.Fatalf("m=%q tags=%v err=%v", m, tags, err)
	}
}

func TestParseSeriesKeyWithTags(t *testing.T) {
	m, tags, err := ParseSeriesKey("cpu,host=a,region=us-east")
	if err != nil || m != "cpu" {
		t.Fatalf("m=%q err=%v", m, err)
	}
	if tags["host"] != "a" || tags["region"] != "us-east" || len(tags) != 2 {
		t.Fatalf("tags=%v", tags)
	}
}

func TestParseSeriesKeyMalformed(t *testing.T) {
	if _, _, err := ParseSeriesKey("cpu,bogus"); err == nil {
		t.Fatal("expected error")
	}
	if _, _, err := ParseSeriesKey(""); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestFieldTypeOf(t *testing.T) {
	cases := []struct {
		v    any
		want FieldType
	}{
		{true, TypeBool},
		{int64(5), TypeInt},
		{int(5), TypeInt},
		{1.5, TypeFloat},
		{"x", TypeStr},
	}
	for _, c := range cases {
		got, err := FieldTypeOf(c.v)
		if err != nil || got != c.want {
			t.Fatalf("FieldTypeOf(%v) = %v, %v; want %v", c.v, got, err, c.want)
		}
	}
	if _, err := FieldTypeOf([]int{1, 2}); err == nil {
		t.Fatal("expected error for unsupported type")
	}
}

func TestCoerceField(t *testing.T) {
	if v, err := CoerceField("7", TypeInt); err != nil || v.(int64) != 7 {
		t.Fatalf("v=%v err=%v", v, err)
	}
	if v, err := CoerceField("1.5", TypeFloat); err != nil || v.(float64) != 1.5 {
		t.Fatalf("v=%v err=%v", v, err)
	}
	if v, err := CoerceField(true, TypeBool); err != nil || v.(bool) != true {
		t.Fatalf("v=%v err=%v", v, err)
	}
	if _, err := CoerceField([]int{1}, TypeInt); err == nil {
		t.Fatal("expected error")
	}
}

func TestNamePackingRoundtrip(t *testing.T) {
	packed, err := PackName("temperature")
	if err != nil {
		t.Fatal(err)
	}
	name, off, err := UnpackName(packed, 0)
	if err != nil || name != "temperature" || off != len(packed) {
		t.Fatalf("name=%q off=%d err=%v", name, off, err)
	}
}

func TestNamePackingEmpty(t *testing.T) {
	packed, _ := PackName("")
	name, _, err := UnpackName(packed, 0)
	if err != nil || name != "" {
		t.Fatalf("name=%q err=%v", name, err)
	}
}

func TestNameTooLong(t *testing.T) {
	if _, err := PackName(string(bytes.Repeat([]byte("x"), 0x10000))); err == nil {
		t.Fatal("expected error for >0xFFFF name")
	}
}
