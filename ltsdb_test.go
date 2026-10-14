package ltsdb_test

import (
	"context"
	"path/filepath"
	"testing"

	ltsdb "github.com/Kelmory/local-tsdb"
)

// TestPublicAPI exercises the importable surface the way an embedder would:
// open, write, commit, select and run InfluxQL.
func TestPublicAPI(t *testing.T) {
	db, err := ltsdb.Open(filepath.Join(t.TempDir(), "api.ltsdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	pts := []ltsdb.Point{{
		Measurement: "cpu",
		Tags:        map[string]string{"host": "a"},
		Fields:      map[string]any{"usage": 0.5},
		Timestamp:   1_700_000_000_000_000_000,
	}}
	if _, err := db.WritePoints(pts); err != nil {
		t.Fatal(err)
	}
	if err := db.Commit(); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Select(context.Background(), ltsdb.SelectQuery{Measurement: "cpu"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}

	rs, err := ltsdb.Exec(db, "SHOW MEASUREMENTS")
	if err != nil {
		t.Fatal(err)
	}
	if rs.NRows() != 1 {
		t.Fatalf("SHOW MEASUREMENTS returned %d rows, want 1", rs.NRows())
	}
}
