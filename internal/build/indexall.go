package build

import (
	"flag"
	"fmt"
	"os"
	"time"
)

// CIndexRecords builds the derived search database from the record hierarchy.
//
//	filmstock index -records OUTDIR
//
// It reads nothing but records: no dump, no resolver cache, no ordering the
// caller has to know. Deleting index.db and re-running restores it exactly,
// which is the point of keeping the records as the source of truth.
func CIndexRecords(args []string) {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	records := fs.String("records", "filmstock-data", "the record tree, from `filmstock extract`")
	dbPath := fs.String("db", "", "the index to write")
	fs.Parse(args)

	out := *dbPath
	if out == "" {
		out = "index.db"
	}
	start := time.Now()

	// Films first: `index` recreates the database from scratch, so it must run
	// before index-television adds the television tables to the same file.
	fmt.Fprintf(os.Stderr, "[1/3] films -> %s\n", out)
	CIndex([]string{
		"-records", *records,
		"-db", out,
	})

	fmt.Fprintf(os.Stderr, "[2/3] television -> %s\n", out)
	CIndexTelevision([]string{
		"-records", *records,
		"-db", out,
	})

	fmt.Fprintf(os.Stderr, "[3/3] events -> %s\n", out)
	CIndexEvents([]string{"-records", *records, "-db", out})

	fmt.Fprintf(os.Stderr, "index complete in %.1f min\n", time.Since(start).Minutes())
	_ = os.Stderr
}
