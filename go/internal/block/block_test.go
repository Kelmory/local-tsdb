package block

import (
	"testing"

	"github.com/Kelmory/local-tsdb/internal/model"
)

func pt(fields map[string]any, ts int64) model.Point {
	return model.Point{Measurement: "m", Fields: fields, Timestamp: ts}
}

func rt(t *testing.T, pts []model.Point) *Block {
	t.Helper()
	b, err := FromPoints(pts)
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromBytes(b.ToBytes())
	if err != nil {
		t.Fatal(err)
	}
	return back
}

func TestRTMixedFields(t *testing.T) {
	pts := []model.Point{
		{Measurement: "cpu", Tags: map[string]string{"h": "1"},
			Fields: map[string]any{"v": 23.5, "n": int64(1)}, Timestamp: 100},
		{Measurement: "cpu", Tags: map[string]string{"h": "1"},
			Fields: map[string]any{"v": 23.6, "n": int64(2)}, Timestamp: 200},
		{Measurement: "cpu", Tags: map[string]string{"h": "1"},
			Fields: map[string]any{"v": 24.0, "n": int64(3)}, Timestamp: 300},
	}
	b := rt(t, pts)
	if b.Len() != 3 {
		t.Fatalf("len=%d", b.Len())
	}
	if mn, _ := b.MinTs(); mn != 100 {
		t.Fatalf("min=%d", mn)
	}
	if mx, _ := b.MaxTs(); mx != 300 {
		t.Fatalf("max=%d", mx)
	}
	names := b.FieldNames()
	if len(names) != 2 || names[0] != "n" || names[1] != "v" {
		t.Fatalf("names=%v", names)
	}
	if b.FieldType("n") != model.TypeInt || b.FieldType("v") != model.TypeFloat {
		t.Fatalf("types n=%v v=%v", b.FieldType("n"), b.FieldType("v"))
	}
	rows := b.Rows()
	if rows[0].Timestamp != 100 || rows[0].Fields["n"] != int64(1) || rows[0].Fields["v"] != 23.5 {
		t.Fatalf("rows[0]=%v", rows[0])
	}
	if rows[2].Timestamp != 300 || rows[2].Fields["n"] != int64(3) || rows[2].Fields["v"] != 24.0 {
		t.Fatalf("rows[2]=%v", rows[2])
	}
}

func TestMissingFieldIsNullNotZero(t *testing.T) {
	pts := []model.Point{
		pt(map[string]any{"a": 1.5}, 1),
		pt(map[string]any{"b": int64(7)}, 2),
	}
	b := rt(t, pts)
	rows := b.Rows()
	if rows[0].Fields["a"] != 1.5 || rows[0].Fields["b"] != nil {
		t.Fatalf("rows[0]=%v", rows[0].Fields)
	}
	if rows[1].Fields["a"] != nil || rows[1].Fields["b"] != int64(7) {
		t.Fatalf("rows[1]=%v", rows[1].Fields)
	}
}

func TestTypeConflictIntThenFloat(t *testing.T) {
	pts := []model.Point{
		pt(map[string]any{"v": int64(1)}, 1),
		pt(map[string]any{"v": 1.5}, 2),
	}
	if _, err := FromPoints(pts); err == nil {
		t.Fatal("expected type conflict error")
	}
}

func TestTypeConflictBoolThenInt(t *testing.T) {
	pts := []model.Point{
		pt(map[string]any{"v": true}, 1),
		pt(map[string]any{"v": int64(7)}, 2),
	}
	if _, err := FromPoints(pts); err == nil {
		t.Fatal("expected type conflict error")
	}
}

func TestDupTimestampsMergeLastWins(t *testing.T) {
	pts := []model.Point{
		pt(map[string]any{"a": 1.0}, 5),
		pt(map[string]any{"b": int64(2)}, 5),
		pt(map[string]any{"a": 9.0}, 5),
	}
	b := rt(t, pts)
	rows := b.Rows()
	if len(rows) != 1 {
		t.Fatalf("len=%d", len(rows))
	}
	if rows[0].Fields["a"] != 9.0 || rows[0].Fields["b"] != int64(2) {
		t.Fatalf("row=%v", rows[0].Fields)
	}
}

func TestBoolAndStrFields(t *testing.T) {
	pts := []model.Point{
		pt(map[string]any{"ok": true, "s": "hi"}, 1),
		pt(map[string]any{"ok": false, "s": "yo"}, 2),
	}
	b := rt(t, pts)
	rows := b.Rows()
	if rows[0].Fields["ok"] != true || rows[0].Fields["s"] != "hi" {
		t.Fatalf("rows[0]=%v", rows[0].Fields)
	}
	if rows[1].Fields["ok"] != false || rows[1].Fields["s"] != "yo" {
		t.Fatalf("rows[1]=%v", rows[1].Fields)
	}
}

func TestOutOfOrderPointsAreSorted(t *testing.T) {
	tsOrder := []int64{300, 100, 500, 200, 400}
	pts := make([]model.Point, len(tsOrder))
	for i, ts := range tsOrder {
		pts[i] = pt(map[string]any{"v": float64(i)}, ts)
	}
	b := rt(t, pts)
	if mn, _ := b.MinTs(); mn != 100 {
		t.Fatalf("min=%d", mn)
	}
	if mx, _ := b.MaxTs(); mx != 500 {
		t.Fatalf("max=%d", mx)
	}
	rows := b.Rows()
	wantTs := []int64{100, 200, 300, 400, 500}
	for i, row := range rows {
		if row.Timestamp != wantTs[i] {
			t.Fatalf("i=%d ts=%d want %d", i, row.Timestamp, wantTs[i])
		}
	}
}

func TestEmptyBlock(t *testing.T) {
	b := rt(t, nil)
	if b.Len() != 0 {
		t.Fatalf("len=%d", b.Len())
	}
	if _, ok := b.MinTs(); ok {
		t.Fatal("empty block must not report min_ts")
	}
	if len(b.Rows()) != 0 {
		t.Fatal("rows must be empty")
	}
}

func TestCorruptBlobCRCRejected(t *testing.T) {
	b, err := FromPoints([]model.Point{pt(map[string]any{"v": 1.5}, 1)})
	if err != nil {
		t.Fatal(err)
	}
	data := b.ToBytes()
	data[0] ^= 0xFF // flip a byte inside the body
	if _, err := FromBytes(data); err == nil {
		t.Fatal("expected checksum error")
	}
}

func TestTruncatedBlobRejected(t *testing.T) {
	b, _ := FromPoints([]model.Point{pt(map[string]any{"v": 1.5}, 1)})
	data := b.ToBytes()
	if _, err := FromBytes(data[:len(data)-3]); err == nil {
		t.Fatal("expected error on truncated blob")
	}
}
