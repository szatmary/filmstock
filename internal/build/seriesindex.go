package build

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/szatmary/filmstock/internal/sqldrv"
)

// Franchises and sequel order.
//
// Wikipedia's film infobox does not carry this. The followed_by/preceded_by
// parameters were deprecated and 225 of 165,740 films mention them, two with a
// value — so the article says which films exist and nothing about how they
// follow one another.
//
// Wikidata does, in two shapes that answer different questions:
//
//	P179  part of the series   membership: Prometheus is an Alien film
//	P156  followed by          order: Aliens comes after Alien
//
// Membership is what "more from this franchise" needs and it groups films whose
// titles do not give them away — Prometheus and Covenant are Alien films.
// Ordering is what "watch next" needs. Neither substitutes for the other.
const seriesSchema = `
DROP TABLE IF EXISTS franchises;
DROP TABLE IF EXISTS franchise_members;
DROP TABLE IF EXISTS sequels;

-- A franchise, keyed by its own article's page_id like everything else here.
-- qid is kept because that is what the membership edges are stated against.
CREATE TABLE franchises(
  id INTEGER PRIMARY KEY, qid INTEGER NOT NULL, title TEXT NOT NULL
);
CREATE INDEX idx_franchises_qid ON franchises(qid);

CREATE TABLE franchise_members(
  franchise_id INTEGER NOT NULL, id INTEGER NOT NULL, kind TEXT NOT NULL,
  PRIMARY KEY (franchise_id, id, kind)
) WITHOUT ROWID;
CREATE INDEX idx_franchise_members_id ON franchise_members(id);

-- One row per stated ordering: id is followed by next_id.
CREATE TABLE sequels(
  id INTEGER NOT NULL, kind TEXT NOT NULL, next_id INTEGER NOT NULL,
  PRIMARY KEY (id, kind, next_id)
) WITHOUT ROWID;
CREATE INDEX idx_sequels_next ON sequels(next_id);
`

// CIndexSeries publishes franchise membership and sequel order.
func CIndexSeries(args []string) {
	fs := flag.NewFlagSet("index-series", flag.ExitOnError)
	dbPath := fs.String("db", "index.db", "the database to add franchises to")
	cache := fs.String("cache", defaultCachePath(), "resolver cache holding the Wikidata edges")
	fs.Parse(args)

	db, err := sql.Open(sqldrv.Name, *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	// One connection, and a page cache worth having.
	//
	// This pass joins the 165k-row work tables against a 10.3M-row Q-id map in
	// an attached 1.2 GB cache, which is tens of millions of random b-tree
	// probes. On SQLite's 2 MB default cache almost every one of them misses:
	// measured at 2h26m for work the sqlite3 CLI does in 30s. It is also the
	// reason ATTACH must not be left to a pooled connection — the attachment
	// is per-connection, so a second connection would not see `wd` at all.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		`PRAGMA cache_size=-1048576`, // 1 GB, not 2 MB
		`PRAGMA temp_store=MEMORY`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			fatal(fmt.Errorf("%s: %w", pragma, err))
		}
	}
	if !tableExists(*cache, "wd_part_of_series", "") {
		fmt.Fprintf(os.Stderr, "  no wd_part_of_series in %s; run `filmstock build-wd-edges`\n", *cache)
		return
	}
	abs, _ := filepath.Abs(*cache)
	if _, err := db.Exec(`ATTACH DATABASE '` +
		strings.ReplaceAll(abs, "'", "''") + `' AS wd`); err != nil {
		fatal(err)
	}
	if _, err := db.Exec(seriesSchema); err != nil {
		fatal(err)
	}

	// The franchises themselves: every series item some work of ours belongs to,
	// that has an article of its own to be keyed by.
	t0 := time.Now()
	if _, err := db.Exec(`
		INSERT OR IGNORE INTO franchises(id,qid,title)
		SELECT fq.page_id, s.series_qid, fq.title
		FROM wd.wd_part_of_series s
		JOIN wd.wiki_qid fq ON fq.qid = s.series_qid
		WHERE s.item_qid IN (
		    -- UNION ALL, not UNION, and the difference is 2h26m against 9s.
		    --
		    -- IN does its own de-duplication, so the UNION's was pure waste --
		    -- but it was not free waste. To de-duplicate by merging, both arms
		    -- have to arrive sorted by qid, and SQLite 3.53's planner obliges by
		    -- reading wiki_qid through idx_wiki_qid: a traversal of a 10.3M-row
		    -- index with a random rowid probe into the work table for every
		    -- entry, 20.6M probes across the two arms. UNION ALL removes the
		    -- ordering requirement and the same query becomes two sequential
		    -- scans.
		    --
		    -- Worth knowing that this is version-dependent: the system sqlite3
		    -- (3.45) chose UNION USING TEMP B-TREE here and ran in 9s, so the
		    -- pathology only appears against the vendored 3.53.4 that ships in
		    -- the binary. A query timed against the CLI has not been timed.
		    SELECT q.qid FROM movies m JOIN wd.wiki_qid q ON q.page_id = m.id
		    UNION ALL SELECT q.qid FROM television_series t JOIN wd.wiki_qid q ON q.page_id = t.id
		)`); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  franchises   %6.1fs\n", time.Since(t0).Seconds())

	for _, m := range []struct{ kind, table string }{
		{"movies", "movies"}, {"television", "television_series"},
	} {
		res, err := db.Exec(`
			INSERT OR IGNORE INTO franchise_members(franchise_id,id,kind)
			SELECT f.id, w.id, ?
			FROM `+m.table+` w
			JOIN wd.wiki_qid q ON q.page_id = w.id
			JOIN wd.wd_part_of_series s ON s.item_qid = q.qid
			JOIN franchises f ON f.qid = s.series_qid`, m.kind)
		if err != nil {
			fatal(err)
		}
		n, _ := res.RowsAffected()
		fmt.Fprintf(os.Stderr, "  %-12s %7d franchise members  %6.1fs\n", m.kind, n, time.Since(t0).Seconds())
		t0 = time.Now()
	}

	if !tableExists(*cache, "wd_sequel", "") {
		fmt.Fprintln(os.Stderr, "  no wd_sequel in the cache yet; sequel order will "+
			"appear after the next `build-wd-edges` run")
	} else {
		for _, m := range []struct{ kind, table string }{
			{"movies", "movies"}, {"television", "television_series"},
		} {
			// Both ends must be works we hold: an ordering edge to something
			// outside the database is not a link, it is a dangling id.
			res, err := db.Exec(`
				INSERT OR IGNORE INTO sequels(id,kind,next_id)
				SELECT a.id, ?, b.id
				FROM `+m.table+` a
				JOIN wd.wiki_qid qa ON qa.page_id = a.id
				JOIN wd.wd_sequel s ON s.item_qid = qa.qid
				JOIN wd.wiki_qid qb ON qb.qid = s.next_qid
				JOIN `+m.table+` b ON b.id = qb.page_id`, m.kind)
			if err != nil {
				fatal(err)
			}
			n, _ := res.RowsAffected()
			fmt.Fprintf(os.Stderr, "  %-12s %7d sequel edges\n", m.kind, n)
		}
	}
	var nf int
	db.QueryRow(`SELECT COUNT(*) FROM franchises`).Scan(&nf)
	fmt.Fprintf(os.Stderr, "  %d franchises\n", nf)
}
