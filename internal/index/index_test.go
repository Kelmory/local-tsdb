package index

import (
	"reflect"
	"testing"
)

func seed(t *testing.T) (*Index, string, string) {
	t.Helper()
	ix := New()
	k1 := ix.AddSeries("cpu", map[string]string{"host": "a", "region": "us"})
	k2 := ix.AddSeries("mem", map[string]string{"host": "a"})
	if err := ix.AddBlock(k1, BlockRef{10, 100, 200}); err != nil {
		t.Fatal(err)
	}
	if err := ix.AddBlock(k1, BlockRef{11, 300, 400}); err != nil {
		t.Fatal(err)
	}
	return ix, k1, k2
}

func TestAddSeriesCanonicalKey(t *testing.T) {
	ix := New()
	if got := ix.AddSeries("cpu", map[string]string{"b": "2", "a": "1"}); got != "cpu,a=1,b=2" {
		t.Fatalf("got %q", got)
	}
}

func TestBlocksList(t *testing.T) {
	ix, k1, _ := seed(t)
	got := ix.Blocks(k1)
	want := []BlockRef{{10, 100, 200}, {11, 300, 400}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestSeriesForTagsSingle(t *testing.T) {
	ix, k1, k2 := seed(t)
	if got := ix.SeriesForTags(map[string]string{"host": "a"}); !got[k1] || !got[k2] || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	if got := ix.SeriesForTags(map[string]string{"region": "us"}); !got[k1] || len(got) != 1 {
		t.Fatalf("got %v", got)
	}
}

func TestSeriesForTagsIntersection(t *testing.T) {
	ix, k1, _ := seed(t)
	got := ix.SeriesForTags(map[string]string{"host": "a", "region": "us"})
	if len(got) != 1 || !got[k1] {
		t.Fatalf("got %v", got)
	}
	if got := ix.SeriesForTags(map[string]string{"host": "z"}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestSeriesForTagsNoFilterReturnsAll(t *testing.T) {
	ix, k1, k2 := seed(t)
	got := ix.SeriesForTags(nil)
	if len(got) != 2 || !got[k1] || !got[k2] {
		t.Fatalf("got %v", got)
	}
}

func TestMeasurementsSorted(t *testing.T) {
	ix, _, _ := seed(t)
	got := ix.Measurements()
	if len(got) != 2 || got[0] != "cpu" || got[1] != "mem" {
		t.Fatalf("got %v", got)
	}
}

func TestAddBlockUnknownSeriesRaises(t *testing.T) {
	ix := New()
	if err := ix.AddBlock("nope", BlockRef{1, 0, 0}); err == nil {
		t.Fatal("expected error")
	}
}

func TestPersistenceRoundtrip(t *testing.T) {
	ix, k1, k2 := seed(t)
	restored, err := FromBytes(ix.ToBytes())
	if err != nil {
		t.Fatal(err)
	}
	want := []BlockRef{{10, 100, 200}, {11, 300, 400}}
	if !reflect.DeepEqual(restored.Blocks(k1), want) {
		t.Fatalf("blocks %v", restored.Blocks(k1))
	}
	// Derived structures rebuilt from parsed keys.
	if ms := restored.Measurements(); len(ms) != 2 || ms[0] != "cpu" || ms[1] != "mem" {
		t.Fatalf("measurements %v", ms)
	}
	if got := restored.SeriesForTags(map[string]string{"host": "a"}); !got[k1] || !got[k2] {
		t.Fatalf("tags %v", got)
	}
	if all := restored.AllSeries(); len(all) != 2 {
		t.Fatalf("all_series %v", all)
	}
}

func TestEmptyIndexRoundtrip(t *testing.T) {
	restored, err := FromBytes(New().ToBytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Measurements()) != 0 {
		t.Fatalf("measurements %v", restored.Measurements())
	}
}

func TestCorruptIndexRejected(t *testing.T) {
	ix, _, _ := seed(t)
	data := ix.ToBytes()
	data[0] ^= 0xFF
	if _, err := FromBytes(data); err == nil {
		t.Fatal("expected checksum error")
	}
}
