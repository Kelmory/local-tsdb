package compress

import (
	"bytes"
	"math"
	"testing"
)

func TestFloatRTBasic(t *testing.T) {
	vals := []float64{23.5, 23.5, 23.6, 24.0, 100.25}
	got, err := DecodeFloats(EncodeFloats(vals), len(vals))
	if err != nil {
		t.Fatal(err)
	}
	for i := range vals {
		if got[i] != vals[i] {
			t.Fatalf("i=%d got %v want %v", i, got[i], vals[i])
		}
	}
}

func TestFloatRTSingleValue(t *testing.T) {
	vals := []float64{3.14159}
	got, _ := DecodeFloats(EncodeFloats(vals), 1)
	if got[0] != vals[0] {
		t.Fatalf("got %v", got[0])
	}
}

func TestFloatUnchangedRunCollapses(t *testing.T) {
	vals := make([]float64, 1000)
	for i := range vals {
		vals[i] = 1.25
	}
	enc := EncodeFloats(vals)
	if len(enc) >= len(vals)*8 {
		t.Fatalf("not compressed: %d bytes", len(enc))
	}
	got, _ := DecodeFloats(enc, len(vals))
	for i, v := range got {
		if v != 1.25 {
			t.Fatalf("i=%d got %v", i, v)
		}
	}
}

func TestFloatEmpty(t *testing.T) {
	if got := EncodeFloats(nil); got != nil {
		t.Fatalf("want nil, got %v", got)
	}
	got, err := DecodeFloats(nil, 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestFloatNegativeZeroPreserved(t *testing.T) {
	vals := []float64{0.0, math.Copysign(0, -1)}
	got, _ := DecodeFloats(EncodeFloats(vals), 2)
	if math.Signbit(got[0]) {
		t.Fatalf("out[0] should be +0.0")
	}
	if !math.Signbit(got[1]) {
		t.Fatalf("out[1] should be -0.0")
	}
}

func TestFloatNaNPreserved(t *testing.T) {
	vals := []float64{math.NaN(), 1.0}
	got, _ := DecodeFloats(EncodeFloats(vals), 2)
	if !math.IsNaN(got[0]) {
		t.Fatalf("out[0] not NaN: %v", got[0])
	}
	if got[1] != 1.0 {
		t.Fatalf("out[1] = %v", got[1])
	}
}

func TestFloatLargeMagnitudes(t *testing.T) {
	vals := []float64{1e30, -1e-30, 42.5}
	got, _ := DecodeFloats(EncodeFloats(vals), len(vals))
	for i := range vals {
		if got[i] != vals[i] {
			t.Fatalf("i=%d got %v want %v", i, got[i], vals[i])
		}
	}
}

func TestFloatTruncatedIsError(t *testing.T) {
	enc := EncodeFloats([]float64{1.0, 2.5})
	if _, err := DecodeFloats(enc[:3], 2); err == nil {
		t.Fatal("expected error on truncated stream")
	}
}

func dodRT(t *testing.T, vals []int64) []int64 {
	t.Helper()
	got, err := DecodeIntsDod(EncodeIntsDod(vals))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDodRTIncreasing(t *testing.T) {
	vals := make([]int64, 100)
	for i := range vals {
		vals[i] = int64(i)
	}
	got := dodRT(t, vals)
	for i := range vals {
		if got[i] != vals[i] {
			t.Fatalf("i=%d got %v", i, got[i])
		}
	}
}

func TestDodRTNegative(t *testing.T) {
	vals := []int64{-5, -3, -8, 10, -2, 0}
	got := dodRT(t, vals)
	for i := range vals {
		if got[i] != vals[i] {
			t.Fatalf("i=%d got %v want %v", i, got[i], vals[i])
		}
	}
}

func TestDodRTSingleValue(t *testing.T) {
	if got := dodRT(t, []int64{42}); got[0] != 42 {
		t.Fatalf("got %v", got)
	}
}

func TestDodEmpty(t *testing.T) {
	if got := EncodeIntsDod(nil); got != nil {
		t.Fatalf("want nil, got %v", got)
	}
	got, err := DecodeIntsDod(nil)
	if err != nil || got != nil {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestDodSteadyCounterIsTiny(t *testing.T) {
	var vals []int64
	for v := int64(0); v < 1000; v += 7 {
		vals = append(vals, v)
	}
	enc := EncodeIntsDod(vals)
	if len(enc) >= len(vals)*2 {
		t.Fatalf("not compact: %d bytes for %d values", len(enc), len(vals))
	}
}

func TestDodLargeValues(t *testing.T) {
	vals := []int64{1 << 40, 1<<40 + 1, 1<<40 - 5}
	got := dodRT(t, vals)
	for i := range vals {
		if got[i] != vals[i] {
			t.Fatalf("i=%d got %v", i, got[i])
		}
	}
}

func TestDodInt64Extremes(t *testing.T) {
	vals := []int64{math.MaxInt64, math.MinInt64, 0, -1, 1}
	got := dodRT(t, vals)
	for i := range vals {
		if got[i] != vals[i] {
			t.Fatalf("i=%d got %v want %v", i, got[i], vals[i])
		}
	}
}

func TestDodTruncatedIsError(t *testing.T) {
	enc := EncodeIntsDod([]int64{1, 2, 3})
	if _, err := DecodeIntsDod(enc[:len(enc)-1]); err == nil {
		t.Fatal("expected error on truncated stream")
	}
}

func TestZigzagOrdering(t *testing.T) {
	if Zigzag(1) >= Zigzag(100) {
		t.Fatal("small magnitudes must map to small codes")
	}
	if got := Unzigzag(Zigzag(-7)); got != -7 {
		t.Fatalf("got %v", got)
	}
}

func TestVarintRoundtrip(t *testing.T) {
	for _, n := range []uint64{0, 1, 127, 128, 300, 1 << 20, 1 << 40, 1 << 62} {
		b := Varint(n)
		out, pos, err := ReadVarint(b, 0)
		if err != nil || out != n || pos != len(b) {
			t.Fatalf("n=%d out=%d pos=%d err=%v", n, out, pos, err)
		}
	}
}

func TestBitWriterMSBFirstParity(t *testing.T) {
	// Port of the Python bit-level contract: low bits appended MSB-first.
	w := &BitWriter{}
	w.Write(0b101, 3)
	w.Write(0b11, 2)
	if got := w.Flush(); !bytes.Equal(got, []byte{0b10111000}) {
		t.Fatalf("got %08b", got)
	}
}
