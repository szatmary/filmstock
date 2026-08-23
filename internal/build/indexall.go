package build

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/szatmary/filmstock"
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

	// Stamp the store state this index was built from, so a later `git pull`
	// that leaves the index behind can be detected instead of silently
	// answering correct-looking queries against yesterday's corpus.
	if fp, err := filmstock.StoreFingerprint(*records); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not fingerprint the store: %v\n", err)
	} else {
		db, err := sql.Open("sqlite", out)
		if err != nil {
			fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT);
			INSERT OR REPLACE INTO meta VALUES('store_fingerprint', ?)`, fp); err != nil {
			fatal(err)
		}
		db.Close()
		fmt.Fprintf(os.Stderr, "  store fingerprint %s\n", fp[:12])
	}

	fmt.Fprintf(os.Stderr, "index complete in %.1f min\n", time.Since(start).Minutes())
	_ = os.Stderr
}
