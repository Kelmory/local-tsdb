package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Kelmory/local-tsdb/internal/bench"
	"github.com/Kelmory/local-tsdb/internal/engine"
	"github.com/Kelmory/local-tsdb/internal/httpapi"
	"github.com/Kelmory/local-tsdb/internal/influxql"
)

func openDB(path string) (*engine.Engine, error) {
	if path == "" {
		return nil, errors.New("--db is required")
	}
	return engine.Open(path)
}

// executeSQL runs one statement against the engine under its read lock.
func executeSQL(eng *engine.Engine, q string) (*influxql.ResultSet, error) {
	eng.RLock()
	defer eng.RUnlock()
	ex := &influxql.Executor{
		QE:             eng.QueryEngine(),
		MeasurementsFn: eng.Index().Measurements,
	}
	return ex.Execute(q)
}

// sqlFromArgs resolves the statement from --file, --q or the positional arg.
func sqlFromArgs(fs *flag.FlagSet, q, qfile string) (string, error) {
	if qfile != "" {
		data, err := os.ReadFile(qfile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}
	if q != "" {
		return q, nil
	}
	if fs.NArg() > 0 {
		return strings.Join(fs.Args(), " "), nil
	}
	return "", errors.New("no query given (pass SQL, --q or --file)")
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dbPath := fs.String("db", "", "database file path (required)")
	addr := fs.String("addr", "127.0.0.1:8086", "listen address")
	fs.Parse(args)
	eng, err := openDB(*dbPath)
	if err != nil {
		return err
	}
	defer eng.Close()
	srv := httpapi.New(eng, *addr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	fmt.Fprintf(os.Stderr, "ltsdb: serving %s at http://%s\n", *dbPath, *addr)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func cmdQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	dbPath := fs.String("db", "", "database file path (required)")
	jsonOut := fs.Bool("json", false, "output JSON instead of CSV")
	q := fs.String("q", "", "SQL statement (or pass as positional arg)")
	qfile := fs.String("file", "", "read SQL from file")
	fs.Parse(args)
	sql, err := sqlFromArgs(fs, *q, *qfile)
	if err != nil {
		return err
	}
	eng, err := openDB(*dbPath)
	if err != nil {
		return err
	}
	defer eng.Close()
	rs, err := executeSQL(eng, sql)
	if err != nil {
		return err
	}
	if *jsonOut {
		fmt.Println(rs.ToJSON())
	} else {
		fmt.Print(rs.ToCSV())
	}
	return nil
}

func cmdExec(args []string) error {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	dbPath := fs.String("db", "", "database file path (required)")
	q := fs.String("q", "", "SQL statement (or pass as positional arg)")
	qfile := fs.String("file", "", "read SQL from file")
	fs.Parse(args)
	sql, err := sqlFromArgs(fs, *q, *qfile)
	if err != nil {
		return err
	}
	eng, err := openDB(*dbPath)
	if err != nil {
		return err
	}
	defer eng.Close()
	rs, err := executeSQL(eng, sql)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d\n", rs.NRows())
	return nil
}

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dbPath := fs.String("db", "", "database file path (required)")
	file := fs.String("file", "", "line protocol file, '-' for stdin (required)")
	precision := fs.String("precision", "ns", "timestamp precision: ns|s|m|u")
	commitEvery := fs.Int("commit-every", 0, "commit every N lines (0 = single commit)")
	fs.Parse(args)
	eng, err := openDB(*dbPath)
	if err != nil {
		return err
	}
	defer eng.Close()
	var rd io.Reader = os.Stdin
	switch {
	case *file == "-":
	case *file != "":
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		defer f.Close()
		rd = f
	default:
		return errors.New("import: --file is required ('-' for stdin)")
	}
	total := 0
	if *commitEvery <= 0 {
		data, err := io.ReadAll(rd)
		if err != nil {
			return err
		}
		n, err := httpapi.Ingest(eng, string(data), *precision)
		if err != nil {
			return err
		}
		total = n
	} else {
		sc := bufio.NewScanner(rd)
		sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
		var buf []string
		flush := func() error {
			if len(buf) == 0 {
				return nil
			}
			n, err := httpapi.Ingest(eng, strings.Join(buf, "\n"), *precision)
			if err != nil {
				return err
			}
			total += n
			buf = buf[:0]
			return nil
		}
		for sc.Scan() {
			buf = append(buf, sc.Text())
			if len(buf) >= *commitEvery {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if err := sc.Err(); err != nil {
			return err
		}
		if err := flush(); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "imported %d points\n", total)
	return nil
}

func cmdBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	dbPath := fs.String("db", "", "database file path (required)")
	points := fs.Int("points", 100000, "total points")
	hosts := fs.Int("hosts", 8, "number of series")
	fs.Parse(args)
	if *dbPath == "" {
		return errors.New("bench: --db is required")
	}
	res, err := bench.Run(bench.Options{DBPath: *dbPath, Points: *points, Hosts: *hosts})
	if err != nil {
		return err
	}
	fmt.Printf("[bench] wrote %d points in %.3fs -> %.0f points/s\n",
		res.NPoints, res.WriteSeconds, float64(res.NPoints)/math.Max(res.WriteSeconds, 1e-9))
	fmt.Printf("[bench] file size: %d bytes (%.2f bytes/point)\n",
		res.FileSize, float64(res.FileSize)/math.Max(float64(res.NPoints), 1))
	fmt.Printf("[bench] full scan of %d rows: %.3fs\n", res.FullScanRows, res.FullScanSeconds)
	fmt.Printf("[bench] pruned range (8 hosts x 1001 rows): %.4fs for %d rows\n",
		res.PruneSeconds, res.PruneRows)
	fmt.Printf("[bench] mean over %d points: %.3fs (mean=%.6f)\n",
		res.NPoints, res.MeanSeconds, res.Mean)
	return nil
}
