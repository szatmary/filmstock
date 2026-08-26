package build

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// External identifiers, published as join keys.
//
// These are CC0 statements on Wikidata about a work — not content taken from
// the services they name. That distinction is what lets them ship at all: IMDb's
// datasets are licensed for personal and non-commercial use and could never be
// redistributed here, but anyone entitled to a copy can attach it and join on
// imdb_id in one statement. filmstock supplies the key and never the data, so
// everything it publishes stays under Wikipedia's and Wikidata's own licences.
//
//	ATTACH 'title.ratings.db' AS imdb;
//	SELECT m.title, r.averageRating
//	  FROM movies m
//	  JOIN external_ids x ON x.id = m.id AND x.kind = 'movies' AND x.source = 'imdb'
//	  JOIN imdb.ratings r ON r.tconst = x.value;
const externalIDSchema = `
DROP TABLE IF EXISTS external_ids;
CREATE TABLE external_ids(
  id     INTEGER NOT NULL,   -- our identity: the enwiki page_id
  kind   TEXT    NOT NULL,   -- movies | television | people
  source TEXT    NOT NULL,   -- imdb | tmdb_movie | tmdb_tv | tvdb | rotten_tomatoes
  value  TEXT    NOT NULL,
  PRIMARY KEY (id, kind, source, value)
) WITHOUT ROWID;
CREATE INDEX idx_external_source ON external_ids(source, value);
`

// CIndexExternalIDs joins Wikidata's identifiers onto the records by Q-id.
func CIndexExternalIDs(args []string) {
	fs := flag.NewFlagSet("index-external-ids", flag.ExitOnError)
	dbPath := fs.String("db", "index.db", "the database to add identifiers to")
	cache := fs.String("cache", defaultCachePath(), "resolver cache holding wd_external_id")
	fs.Parse(args)

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	if !tableExists(*cache, "wd_external_id", "") {
		fmt.Fprintf(os.Stderr,
			"  no wd_external_id in %s; run `filmstock build-wd-edges` to collect them\n", *cache)
		return
	}
	abs, err := filepath.Abs(*cache)
	if err != nil {
		fatal(err)
	}
	if _, err := db.Exec(`ATTACH DATABASE '` +
		strings.ReplaceAll(abs, "'", "''") + `' AS wd`); err != nil {
		fatal(err)
	}
	if _, err := db.Exec(externalIDSchema); err != nil {
		fatal(err)
	}

	// Works reach their Q-id through wiki_qid, keyed on the page_id they are
	// already stored under. People carry theirs on the record, because the
	// person pass resolves it — 99% of them have one.
	stmts := []struct {
		kind, sql string
	}{
		{"movies", `INSERT OR IGNORE INTO external_ids(id,kind,source,value)
			SELECT m.id, 'movies', x.source, x.value FROM movies m
			JOIN wd.wiki_qid q ON q.page_id = m.id
			JOIN wd.wd_external_id x ON x.item_qid = q.qid`},
		{"television", `INSERT OR IGNORE INTO external_ids(id,kind,source,value)
			SELECT t.id, 'television', x.source, x.value FROM television_series t
			JOIN wd.wiki_qid q ON q.page_id = t.id
			JOIN wd.wd_external_id x ON x.item_qid = q.qid`},
		{"people", `INSERT OR IGNORE INTO external_ids(id,kind,source,value)
			SELECT p.page_id, 'people', x.source, x.value FROM people p
			JOIN wd.wd_external_id x ON x.item_qid = p.qid
			WHERE p.qid > 0 AND p.page_id > 0`},
	}
	for _, s := range stmts {
		res, err := db.Exec(s.sql)
		if err != nil {
			fatal(fmt.Errorf("%s: %w", s.kind, err))
		}
		n, _ := res.RowsAffected()
		fmt.Fprintf(os.Stderr, "  %-12s %8d identifiers\n", s.kind, n)
	}
	var bySource string
	rows, err := db.Query(`SELECT source, COUNT(*) FROM external_ids GROUP BY source ORDER BY 2 DESC`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var s string
			var n int
			rows.Scan(&s, &n)
			bySource += fmt.Sprintf("    %-18s %8d\n", s, n)
		}
	}
	fmt.Fprint(os.Stderr, bySource)
}
