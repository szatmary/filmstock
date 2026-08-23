package build

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/szatmary/filmstock"
	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=OFF;
PRAGMA synchronous=OFF;
CREATE TABLE movies(
  id INTEGER PRIMARY KEY,
  title TEXT NOT NULL,
  year INTEGER,
  release_date TEXT,
  director TEXT, producer TEXT, writer TEXT, starring TEXT,
  music TEXT, distributor TEXT, country TEXT, language TEXT, genre TEXT,
  runtime TEXT, budget TEXT, gross TEXT,
  wikipedia_url TEXT, cover_image_url TEXT, cover_image_file TEXT
);
-- Lookup by exact title, and by name on the people side, are the two access
-- paths with no index at all. Locally that hid behind the page cache; over a
-- remote VFS a title lookup became a 20,120-request, 80 MB table scan, because
-- SQLite had nothing to seek with.
CREATE INDEX idx_movies_title ON movies(title);
CREATE INDEX idx_movies_year ON movies(year);
CREATE VIRTUAL TABLE movies_fts USING fts5(
  title, starring, director,
  content='movies', content_rowid='id', tokenize='trigram'
);
`

// roleCredits maps a display role label to the Movie field holding those people.
func roleCredits(m *filmstock.Movie) []struct {
	role   string
	people []filmstock.Person
} {
	return []struct {
		role   string
		people []filmstock.Person
	}{
		{"Director", m.Director},
		{"Writer", m.Writer},
		{"Producer", m.Producer},
		{"Cast", m.Starring},
		{"Composer", m.Music},
		{"Cinematographer", m.Cinematography},
		{"Editor", m.Editing},
	}
}

// indexItem carries a parsed movie plus its store-relative path to the DB writer.
type indexItem struct {
	m *filmstock.Movie
}

func CIndex(args []string) {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	dbPath := fs.String("db", "index.db", "the index to write")
	records := fs.String("records", "", "record hierarchy from `extract` (supplies people identities)")
	fs.Parse(args)

	if err := os.Remove(*dbPath); err != nil && !os.IsNotExist(err) {
		fatal(err)
	}
	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		fatal(err)
	}
	if _, err := db.Exec(peopleSchema); err != nil {
		fatal(err)
	}
	// Identities come from the person records, not from a dump or resolver.
	p2q, err := loadPeopleIdentities(*records)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  %d person identities\n", len(p2q))

	fmt.Fprintf(os.Stderr, "indexing movies from %s into %s...\n", *records, *dbPath)

	items := make(chan indexItem, 2048)
	go func() {
		defer close(items)
		// One reader: the store is a sequential file and JSON decoding 165k
		// small records is seconds, so a worker pool would buy nothing and
		// would have to preserve store order anyway.
		if err := filmstock.WalkStore(*records, filmstock.KindMovie, func(r filmstock.StoredRecord) error {
			var m filmstock.Movie
			if err := json.Unmarshal(r.Data, &m); err != nil {
				fmt.Fprintf(os.Stderr, "record %s: %v\n", r.Key, err)
				return nil
			}
			items <- indexItem{&m}
			return nil
		}); err != nil {
			fatal(err)
		}
	}()

	// single writer: one big transaction
	tx, err := db.Begin()
	if err != nil {
		fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO movies
		(id,title,year,release_date,director,producer,writer,starring,music,
		 distributor,country,language,genre,runtime,budget,gross,wikipedia_url,cover_image_url,cover_image_file)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		fatal(err)
	}
	pb, err := newPeopleBuilder(tx, p2q)
	if err != nil {
		fatal(err)
	}

	n, credits := 0, 0
	for it := range items {
		m := it.m
		if _, err := stmt.Exec(
			m.PageID, m.Title, yearOf(m), first(m.ReleaseDates),
			joinP(m.Director), joinP(m.Producer), joinP(m.Writer), joinP(m.Starring),
			joinP(m.Music), join(filmstock.Names(m.Distributor)), join(filmstock.Names(m.Country)), join(filmstock.Names(m.Language)), join(m.Genre),
			m.Runtime, m.Budget, m.Gross, m.WikiURL, m.CoverImageURL, m.CoverImageFile,
		); err != nil {
			fmt.Fprintln(os.Stderr, "insert error:", m.Title, err)
			continue
		}
		seen := map[string]bool{}
		for _, rc := range roleCredits(m) {
			pb.credit(seen, rc.people, m.PageID, "movie", rc.role)
			credits += len(rc.people)
		}
		n++
		if n%20000 == 0 {
			fmt.Fprintf(os.Stderr, "\r  inserted %d movies, %d credits...", n, credits)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "\r  inserted %d movies, %d credits (%d people). building FTS indexes...\n",
		n, credits, len(pb.byKey))
	if _, err := db.Exec(`INSERT INTO movies_fts(movies_fts) VALUES('rebuild')`); err != nil {
		fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO people_fts(people_fts) VALUES('rebuild')`); err != nil {
		fatal(err)
	}
	fmt.Fprintln(os.Stderr, "done.")
}

func join(v []string) string { return strings.Join(v, " · ") }

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// yearOf extracts a 4-digit year from the first release date, or 0.
func yearOf(m *filmstock.Movie) int {
	s := first(m.ReleaseDates)
	if len(s) >= 4 {
		y := 0
		for i := 0; i < 4; i++ {
			if s[i] < '0' || s[i] > '9' {
				return 0
			}
			y = y*10 + int(s[i]-'0')
		}
		return y
	}
	return 0
}
