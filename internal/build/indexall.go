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
	cache := fs.String("cache", defaultCachePath(), "resolver cache, for the Wikidata identifiers")
	fs.Parse(args)

	out := *dbPath
	if out == "" {
		out = "index.db"
	}
	start := time.Now()

	// The person identity map is read from the store ONCE and shared. Both the
	// film and the television indexer need it, and each used to rebuild it —
	// ~220k records inflated against the dictionary twice for the same answer.
	// That was nearly free when records were loose files on a warm page cache.
	p2q, err := loadPeopleQIDs(*records)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  %d person identities from the store\n", len(p2q))
	sharedIdentities = p2q

	// Films first: `index` recreates the database from scratch, so it must run
	// before index-television adds the television tables to the same file.
	fmt.Fprintf(os.Stderr, "[1/6] films -> %s\n", out)
	CIndex([]string{
		"-records", *records,
		"-db", out,
	})

	fmt.Fprintf(os.Stderr, "[2/6] television -> %s\n", out)
	CIndexTelevision([]string{
		"-records", *records,
		"-db", out,
	})

	fmt.Fprintf(os.Stderr, "[3/6] events -> %s\n", out)
	CIndexEvents([]string{"-records", *records, "-db", out})

	fmt.Fprintf(os.Stderr, "[4/6] schedules -> %s\n", out)
	CIndexSchedules([]string{"-records", *records, "-db", out})

	fmt.Fprintf(os.Stderr, "[5/6] external identifiers -> %s\n", out)
	CIndexExternalIDs([]string{"-db", out, "-cache", *cache})

	fmt.Fprintf(os.Stderr, "[6/6] franchises and sequels -> %s\n", out)
	CIndexSeries([]string{"-db", out, "-cache", *cache})

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
