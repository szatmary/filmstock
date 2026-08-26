package build

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
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
	if _, err := db.Exec(`
		INSERT OR IGNORE INTO franchises(id,qid,title)
		SELECT fq.page_id, s.series_qid, fq.title
		FROM wd.wd_part_of_series s
		JOIN wd.wiki_qid fq ON fq.qid = s.series_qid
		WHERE s.item_qid IN (
		    SELECT q.qid FROM movies m JOIN wd.wiki_qid q ON q.page_id = m.id
		    UNION SELECT q.qid FROM television_series t JOIN wd.wiki_qid q ON q.page_id = t.id
		)`); err != nil {
		fatal(err)
	}

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
		fmt.Fprintf(os.Stderr, "  %-12s %7d franchise members\n", m.kind, n)
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
