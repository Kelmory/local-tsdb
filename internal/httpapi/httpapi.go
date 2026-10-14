// Package httpapi exposes the InfluxDB-compatible HTTP API over the engine:
//
//	GET  /ping   -- liveness probe, empty 204
//	GET  /health -- 200 "OK"
//	POST /write  -- ingest line protocol (?precision=s|m|u|ns, default ns)
//	GET/POST /query -- run InfluxQL; CSV by default, JSON with
//	                   Accept: application/json
//
// Malformed input yields 400 with a JSON error body; unexpected failures are
// answered with 500 so a connection never drops without a response.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Kelmory/local-tsdb/internal/engine"
	"github.com/Kelmory/local-tsdb/internal/influxql"
	"github.com/Kelmory/local-tsdb/internal/lineproto"
	"github.com/Kelmory/local-tsdb/internal/model"
)

// precisionNs maps InfluxDB precision flags to per-unit nanoseconds.
var precisionNs = map[string]int64{
	"ns": 1, "u": 1000, "s": 1000 * 1000 * 1000, "m": 60 * 1000 * 1000 * 1000,
}

// Ingest parses text as line protocol, buffers the points and commits them
// into the engine. Timestamps are interpreted in the requested unit
// (InfluxDB semantics) and normalised to nanoseconds. All lines are parsed
// before anything is buffered, so a malformed line leaves the engine
// untouched. Returns the number of points written.
func Ingest(eng *engine.Engine, text string, precision string) (int, error) {
	factor, ok := precisionNs[precision]
	if !ok {
		return 0, fmt.Errorf("unknown precision %q", precision)
	}
	pts := make([]model.Point, 0, strings.Count(text, "\n")+1)
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := lineproto.ParseLine(line)
		if err != nil {
			return 0, err
		}
		if p.Timestamp != 0 {
			p.Timestamp *= factor
		}
		pts = append(pts, p)
	}
	if len(pts) > 0 {
		if _, err := eng.WritePoints(pts); err != nil {
			return 0, err
		}
	}
	if err := eng.Commit(); err != nil {
		return 0, err
	}
	return len(pts), nil
}

// Server serves the HTTP API for one open engine.
type Server struct {
	eng  *engine.Engine
	http *http.Server
}

// New builds a server listening on addr.
func New(eng *engine.Engine, addr string) *Server {
	s := &Server{eng: eng}
	s.http = &http.Server{Addr: addr, Handler: s}
	return s
}

// ListenAndServe runs the HTTP server (blocking).
func (s *Server) ListenAndServe() error { return s.http.ListenAndServe() }

// Shutdown gracefully drains connections.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}
	switch {
	case r.Method == http.MethodGet && path == "/ping":
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && path == "/health":
		s.send(w, http.StatusOK, []byte("OK"), "text/plain; charset=utf-8")
	case r.Method == http.MethodGet && path == "/":
		s.send(w, http.StatusOK,
			[]byte(`{"name": "local-tsdb", "version": 1}`), "application/json")
	case path == "/query" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		s.handleQuery(w, r)
	case r.Method == http.MethodPost && path == "/write":
		s.handleWrite(w, r)
	default:
		s.error(w, http.StatusNotFound, "no such endpoint")
	}
}

// send writes a response; Content-Type is only set for non-empty bodies,
// mirroring the reference implementation.
func (s *Server) send(w http.ResponseWriter, code int, body []byte, ctype string) {
	if len(body) > 0 {
		w.Header().Set("Content-Type", ctype)
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(code)
	if len(body) > 0 {
		w.Write(body)
	}
}

func (s *Server) error(w http.ResponseWriter, code int, msg string) {
	s.send(w, code, []byte(`{"error": `+jsonString(msg)+`}`), "application/json")
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.error(w, http.StatusBadRequest, err.Error())
		return
	}
	precision := r.URL.Query().Get("precision")
	if precision == "" {
		precision = "ns"
	}
	if _, err := Ingest(s.eng, string(body), precision); err != nil {
		s.error(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" && r.Method == http.MethodPost {
		// InfluxDB accepts the query in the URL *or* the POST body.
		body, err := io.ReadAll(r.Body)
		if err == nil && len(body) > 0 {
			if form, err := url.ParseQuery(string(body)); err == nil {
				q = form.Get("q")
			}
		}
	}
	if q == "" {
		s.error(w, http.StatusBadRequest, "missing query parameter 'q'")
		return
	}
	rs, err := s.executeSQL(q)
	if err != nil {
		if isSyntaxError(err) {
			s.error(w, http.StatusBadRequest, err.Error())
		} else {
			s.error(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		s.send(w, http.StatusOK, []byte(rs.ToJSON()), "application/json")
		return
	}
	s.send(w, http.StatusOK, []byte(rs.ToCSV()), "text/csv; charset=utf-8")
}

// executeSQL runs one statement under the engine's read lock. Measurements
// are read through Index() to avoid a nested RLock.
func (s *Server) executeSQL(q string) (*influxql.ResultSet, error) {
	s.eng.RLock()
	defer s.eng.RUnlock()
	ex := &influxql.Executor{
		QE:             s.eng.QueryEngine(),
		MeasurementsFn: s.eng.Index().Measurements,
	}
	return ex.Execute(q)
}

func isSyntaxError(err error) bool {
	var se *influxql.Error
	var le *lineproto.Error
	return errors.As(err, &se) || errors.As(err, &le)
}

// jsonString encodes s as a JSON string literal.
func jsonString(s string) string {
	enc, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(enc)
}
