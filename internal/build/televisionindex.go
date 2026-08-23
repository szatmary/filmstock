package build

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"

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
  cover_image_file TEXT, wikipedia_url TEXT
);
CREATE INDEX idx_television_title ON television_series(title);
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
	dbPath := fs.String("db", "index.db", "the index (shared with movies)")
	records := fs.String("records", "", "record hierarchy from `extract` (supplies people identities)")
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
	p2q, err := loadPeopleIdentities(*records)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  %d person identities\n", len(p2q))

	fmt.Fprintf(os.Stderr, "indexing television from %s into %s...\n", *records, *dbPath)

	type item struct {
		s *filmstock.TelevisionSeries
	}
	items := make(chan item, 2048)
	go func() {
		defer close(items)
		if err := filmstock.WalkStore(*records, filmstock.KindTelevision, func(r filmstock.StoredRecord) error {
			var ser filmstock.TelevisionSeries
			if err := json.Unmarshal(r.Data, &ser); err != nil {
				fmt.Fprintf(os.Stderr, "record %s: %v\n", r.Key, err)
				return nil
			}
			items <- item{&ser}
			return nil
		}); err != nil {
			fatal(err)
		}
	}()

	tx, _ := db.Begin()
	stmt, _ := tx.Prepare(`INSERT OR REPLACE INTO television_series
		(id,title,year,first_aired,last_aired,genre,creator,starring,network,
		 num_seasons,num_episodes,seasons_count,episodes_count,cover_image_file,wikipedia_url)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
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
			join(s.Genre), joinP(s.Creator), joinP(s.Starring), join(filmstock.Names(s.Network)),
			s.NumSeasons, s.NumEpisodes, len(s.Seasons), epCount,
			s.CoverImageFile, s.WikiURL)

		seen := map[string]bool{}
		pb.credit(seen, s.Creator, s.PageID, "television", "Creator")
		pb.credit(seen, s.Starring, s.PageID, "television", "Cast")
		pb.credit(seen, s.Composer, s.PageID, "television", "Composer")
		// Series-level crew. Without these a television director existed only
		// per-episode, so a person's page showed episodes they directed but
		// never the series they ran.
		pb.credit(seen, s.Director, s.PageID, "television", "Director")
		pb.credit(seen, s.Producer, s.PageID, "television", "Producer")
		pb.credit(seen, s.ExecutiveProducer, s.PageID, "television", "Executive Producer")
		pb.credit(seen, s.Writer, s.PageID, "television", "Writer")
		pb.credit(seen, s.Editor, s.PageID, "television", "Editor")
		pb.credit(seen, s.Cinematography, s.PageID, "television", "Cinematographer")
		pb.credit(seen, s.Presenter, s.PageID, "television", "Presenter")
		pb.credit(seen, s.Narrator, s.PageID, "television", "Narrator")
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
