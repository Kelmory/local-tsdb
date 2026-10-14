package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Kelmory/local-tsdb/internal/engine"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	eng, err := engine.Open(t.TempDir() + "/http.ltsdb")
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	t.Cleanup(func() { eng.Close() })
	srv := New(eng, "127.0.0.1:0")
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func request(t *testing.T, ts *httptest.Server, method, path, body string, headers map[string]string) (int, []byte, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rd)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, data, resp.Header
}

func query(t *testing.T, ts *httptest.Server, sql string, acceptJSON bool) (int, []byte, http.Header) {
	t.Helper()
	hdrs := map[string]string{}
	if acceptJSON {
		hdrs["Accept"] = "application/json"
	}
	return request(t, ts, "GET", "/query?q="+url.QueryEscape(sql), "", hdrs)
}

// --- probes ----------------------------------------------------------------- //

func TestPingReturns204(t *testing.T) {
	ts := newTestServer(t)
	code, body, _ := request(t, ts, "GET", "/ping", "", nil)
	if code != 204 {
		t.Errorf("status = %d, want 204", code)
	}
	if len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}
}

func TestHealthOK(t *testing.T) {
	ts := newTestServer(t)
	code, body, _ := request(t, ts, "GET", "/health", "", nil)
	if code != 200 || string(body) != "OK" {
		t.Errorf("got (%d, %q), want (200, OK)", code, body)
	}
}

// --- write + query round trip ------------------------------------------------ //

func TestWriteThenQueryJSON(t *testing.T) {
	ts := newTestServer(t)
	lines := "cpu,host=a usage=1.5 1700000000000\ncpu,host=a usage=2.5 1700000060000\n"
	code, _, _ := request(t, ts, "POST", "/write?precision=ns", lines, nil)
	if code != 204 {
		t.Fatalf("write status = %d, want 204", code)
	}
	code, body, _ := query(t, ts, "SELECT * FROM cpu WHERE host='a'", true)
	if code != 200 {
		t.Fatalf("query status = %d, want 200", code)
	}
	var doc struct {
		Columns []string         `json:"columns"`
		Rows    []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(doc.Columns) == 0 {
		t.Error("missing columns")
	}
	if len(doc.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(doc.Rows))
	}
	if v, ok := doc.Rows[0]["usage"].(float64); !ok || v != 1.5 {
		t.Errorf("rows[0].usage = %#v, want 1.5", doc.Rows[0]["usage"])
	}
}

func TestWritePrecisionSeconds(t *testing.T) {
	// precision=s: the explicit timestamp is interpreted in *seconds*.
	ts := newTestServer(t)
	code, _, _ := request(t, ts, "POST", "/write?precision=s", "temp,h=1 v=21.0 1700000000\n", nil)
	if code != 204 {
		t.Fatalf("write status = %d, want 204", code)
	}
	_, body, _ := query(t, ts, "SELECT * FROM temp", true)
	var doc struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(doc.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(doc.Rows))
	}
	// Use a typed decode to keep int64 timestamp precision.
	var raw struct {
		Rows []struct {
			Time int64 `json:"time"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if raw.Rows[0].Time != 1700000000*1000*1000*1000 {
		t.Errorf("time = %d, want %d", raw.Rows[0].Time, 1700000000*1000*1000*1000)
	}
}

func TestQueryCSVDefault(t *testing.T) {
	ts := newTestServer(t)
	code, _, _ := request(t, ts, "POST", "/write?precision=ns", "load,a=1 v=1i\nload,a=2 v=2i\n", nil)
	if code != 204 {
		t.Fatalf("write status = %d, want 204", code)
	}
	code, body, hdrs := query(t, ts, "SELECT count() FROM load", false)
	if code != 200 {
		t.Fatalf("query status = %d, want 200", code)
	}
	text := string(body)
	if !strings.HasPrefix(text, "count\n") {
		t.Errorf("csv = %q, want prefix count\\n", text)
	}
	if !strings.Contains(strings.ToLower(hdrs.Get("Content-Type")), "text/csv") {
		t.Errorf("content-type = %q, want text/csv", hdrs.Get("Content-Type"))
	}
}

func TestBadWriteLineReturns400(t *testing.T) {
	ts := newTestServer(t)
	code, body, _ := request(t, ts, "POST", "/write", "not a valid line", nil)
	if code != 400 {
		t.Fatalf("status = %d, want 400", code)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil || doc["error"] == nil {
		t.Errorf("body = %q, want JSON with error key", body)
	}
}

func TestBadQueryReturns400(t *testing.T) {
	ts := newTestServer(t)
	code, _, _ := query(t, ts, "SELECT FROM cpu", true)
	if code != 400 {
		t.Errorf("status = %d, want 400", code)
	}
}

func TestUnknownPath404(t *testing.T) {
	ts := newTestServer(t)
	code, _, _ := request(t, ts, "GET", "/nope", "", nil)
	if code != 404 {
		t.Errorf("status = %d, want 404", code)
	}
}

func TestIngestRejectsUnknownPrecision(t *testing.T) {
	eng, err := engine.Open(t.TempDir() + "/p.ltsdb")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if _, err := Ingest(eng, "m v=1", "bogus"); err == nil {
		t.Error("expected error for unknown precision")
	}
}
