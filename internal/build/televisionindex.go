package build

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/szatmary/filmstock"
	_ "modernc.org/sqlite"
)

// televisionSchema deliberately stores no prose. Episode summaries and series
// overview/plot are not indexed by FTS (which covers title/starring/creator
// only) and are already in the record .json.gz that the detail page loads — so
// in the database they were 219 MB of duplication, 34% of the whole file,
// carried by everyone who downloads it. The film side never had them; this is
// television catching up, not a new policy. See docs/TODO.md §D1.
//
// series_title went with them: it was a denormalised copy of
// television_series.title across 551,174 rows, reachable by join.
const televisionSchema = `
DROP TABLE IF EXISTS television_series;
DROP TABLE IF EXISTS television_fts;
DROP TABLE IF EXISTS television_episodes;
DROP TABLE IF EXISTS television_episodes_fts;
DELETE FROM credits WHERE work_type='television';
CREATE TABLE television_series(
  id INTEGER PRIMARY KEY, title TEXT NOT NULL, year INTEGER,
  first_aired TEXT, last_aired TEXT, genre TEXT, creator TEXT, starring TEXT,
  network TEXT, num_seasons TEXT, num_episodes TEXT,
  seasons_count INTEGER, episodes_count INTEGER,
  cover_image_file TEXT, wikipedia_url TEXT, path TEXT NOT NULL
);
CREATE VIRTUAL TABLE television_fts USING fts5(
  title, starring, creator,
  content='television_series', content_rowid='id', tokenize='trigram'
);
CREATE TABLE television_episodes(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  series_id INTEGER, season INTEGER,
  number_in_season INTEGER, number_overall INTEGER, title TEXT, air_date TEXT
);
CREATE INDEX idx_television_ep_series ON television_episodes(series_id);
CREATE VIRTUAL TABLE television_episodes_fts USING fts5(
  title, content='television_episodes', content_rowid='id', tokenize='trigram'
);`

// CIndexTelevision adds television_series + television_fts tables to an existing database, indexing the
// per-series JSON.gz files. Does not touch the movies tables.
func CIndexTelevision(args []string) {
	fs := flag.NewFlagSet("index-television", flag.ExitOnError)
	televisionDir := fs.String("television", "out/television", "directory of per-series JSON.gz files")
	dbPath := fs.String("db", "out/search.db", "SQLite database (shared with movies)")
	records := fs.String("records", "", "record hierarchy from `extract` (supplies people identities)")
	workers := fs.Int("workers", 16, "reader workers")
	fs.Parse(args)

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(televisionSchema); err != nil {
		fatal(err)
	}
	// Identities come from the person records, not from a dump or resolver.
	p2q, err := loadPeopleQIDs(*records)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  %d person identities from records\n", len(p2q))

	var files []string
	filepath.WalkDir(*televisionDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json.gz") {
			files = append(files, p)
		}
		return nil
	})
	fmt.Fprintf(os.Stderr, "indexing %d television series into %s...\n", len(files), *dbPath)

	type item struct {
		s    *filmstock.TelevisionSeries
		path string
	}
	paths := make(chan string, 2048)
	items := make(chan item, 2048)
	var rwg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for p := range paths {
				s, err := filmstock.ReadTelevisionSeriesGz(p)
				if err != nil {
					continue
				}
				rel, _ := filepath.Rel(*televisionDir, p)
				items <- item{s, rel}
			}
		}()
	}
	go func() {
		for _, p := range files {
			paths <- p
		}
		close(paths)
		rwg.Wait()
		close(items)
	}()

	tx, _ := db.Begin()
	stmt, _ := tx.Prepare(`INSERT OR REPLACE INTO television_series
		(id,title,year,first_aired,last_aired,genre,creator,starring,network,
		 num_seasons,num_episodes,seasons_count,episodes_count,cover_image_file,wikipedia_url,path)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	epStmt, _ := tx.Prepare(`INSERT INTO television_episodes
		(series_id,season,number_in_season,number_overall,title,air_date) VALUES(?,?,?,?,?,?)`)
	// Reuses movie-pass person ids (loads existing people); resolves Q-ids.
	pb, err := newPeopleBuilder(tx, p2q)
	if err != nil {
		fatal(err)
	}

	n, nEp := 0, 0
	for it := range items {
		s := it.s
		year := 0
		if len(s.FirstAired) >= 4 {
			fmt.Sscanf(s.FirstAired[:4], "%d", &year)
		}
		epCount := 0
		for _, se := range s.Seasons {
			epCount += len(se.Episodes)
		}
		cleanName := filmstock.CleanTelevisionTitle(s.Title)
		stmt.Exec(s.PageID, cleanName, year, s.FirstAired, s.LastAired,
			join(s.Genre), joinP(s.Creator), joinP(s.Starring), join(s.Network),
			s.NumSeasons, s.NumEpisodes, len(s.Seasons), epCount,
			s.CoverImageFile, s.WikiURL, it.path)

		seen := map[string]bool{}
		pb.credit(seen, s.Creator, s.PageID, "television", "Creator")
		pb.credit(seen, s.Starring, s.PageID, "television", "Cast")
		pb.credit(seen, s.Composer, s.PageID, "television", "Composer")
		for _, se := range s.Seasons {
			for _, e := range se.Episodes {
				pb.credit(seen, e.DirectedBy, s.PageID, "television", "Director")
				pb.credit(seen, e.WrittenBy, s.PageID, "television", "Writer")
				epStmt.Exec(s.PageID, se.Season, e.NumberInSeason, e.NumberOverall, e.Title, e.AirDate)
				nEp++
			}
		}
		n++
	}
	stmt.Close()
	epStmt.Close()
	tx.Commit()
	fmt.Fprintf(os.Stderr, "  inserted %d series, %d episodes. building FTS...\n", n, nEp)
	for _, t := range []string{"television_fts", "television_episodes_fts", "people_fts"} {
		if _, err := db.Exec(`INSERT INTO ` + t + `(` + t + `) VALUES('rebuild')`); err != nil {
			fatal(err)
		}
	}
	fmt.Fprintln(os.Stderr, "done.")
}
