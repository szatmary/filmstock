package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// cmdIndexRecords builds the derived search database from the record hierarchy.
//
//	filmstock index -records OUTDIR
//
// It reads nothing but records: no dump, no resolver cache, no ordering the
// caller has to know. Deleting search.db and re-running restores it exactly,
// which is the point of keeping the records as the source of truth.
func cmdIndexRecords(args []string) {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	records := fs.String("records", "out", "record hierarchy produced by `extract`")
	dbPath := fs.String("db", "", "output database (default <records>/search.db)")
	workers := fs.Int("workers", 16, "reader workers")
	fs.Parse(args)

	out := *dbPath
	if out == "" {
		out = filepath.Join(*records, "search.db")
	}
	start := time.Now()

	// Films first: `index` recreates the database from scratch, so it must run
	// before index-television adds the television tables to the same file.
	fmt.Fprintf(os.Stderr, "[1/3] films -> %s\n", out)
	cmdIndex([]string{
		"-movies", filepath.Join(*records, kindMovie),
		"-records", *records,
		"-db", out,
		"-workers", fmt.Sprint(*workers),
	})

	fmt.Fprintf(os.Stderr, "[2/3] television -> %s\n", out)
	cmdIndexTelevision([]string{
		"-television", filepath.Join(*records, kindTelevision),
		"-records", *records,
		"-db", out,
		"-workers", fmt.Sprint(*workers),
	})

	fmt.Fprintf(os.Stderr, "[3/3] events -> %s\n", out)
	cmdIndexEvents([]string{"-records", *records, "-db", out})

	fmt.Fprintf(os.Stderr, "index complete in %.1f min\n", time.Since(start).Minutes())
	_ = os.Stderr
}
