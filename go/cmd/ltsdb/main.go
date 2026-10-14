// Command ltsdb is a single-file time-series database engine.
//
// Subcommands:
//
//	serve   start the InfluxDB-compatible HTTP API
//	query   run an InfluxQL statement and print the result (CSV/JSON)
//	exec    run an InfluxQL statement, print only the row count
//	import  ingest line protocol from a file or stdin
//	bench   run the standard 100k-point performance workload
package main

import (
	"fmt"
	"os"
)

const (
	exitOK      = 0
	exitUsage   = 1 // usage / syntax / parse errors
	exitRuntime = 2 // I/O, corruption, internal errors
)

func usage(w *os.File) {
	fmt.Fprintf(w, `ltsdb - single-file time-series database

Usage:
  ltsdb serve   --db PATH [--addr 127.0.0.1:8086]
  ltsdb query   --db PATH [--json] ("<SQL>" | --file Q.sql)
  ltsdb exec    --db PATH ("<SQL>" | --file Q.sql)
  ltsdb import  --db PATH (--file lines.lp | -) [--precision ns|s|m|u] [--commit-every N]
  ltsdb bench   --db PATH [--points 100000] [--hosts 8]
`)
}

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(exitUsage)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "query":
		err = cmdQuery(os.Args[2:])
	case "exec":
		err = cmdExec(os.Args[2:])
	case "import":
		err = cmdImport(os.Args[2:])
	case "bench":
		err = cmdBench(os.Args[2:])
	case "help", "-h", "--help":
		usage(os.Stdout)
		os.Exit(exitOK)
	default:
		fmt.Fprintf(os.Stderr, "ltsdb: unknown subcommand %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(exitUsage)
	}
	if err != nil {
		os.Exit(exitFromError(err))
	}
	os.Exit(exitOK)
}

// exitFromError maps error categories to process exit codes.
func exitFromError(err error) int {
	msg := err.Error()
	for _, usageErr := range []string{"syntax", "parse", "unknown", "malformed", "invalid", "unsupported"} {
		if contains(msg, usageErr) {
			return exitUsage
		}
	}
	return exitRuntime
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
