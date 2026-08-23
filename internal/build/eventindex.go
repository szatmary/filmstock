package build

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/szatmary/filmstock"
	_ "modernc.org/sqlite"
)

// Indexing award ceremonies and film festivals.
//
// A separate table rather than a `type` column on movies, for the same reason
// television has its own: the answerable questions differ. A film is found by
// cast and premise; a ceremony is found by its award, its edition, its host and
// the year — and mixing them cost us 4,065 rows of a film index that could never
// match a film query.
//
// Hosts go into the shared people/credits tables with work_type='event', so a
// person page shows "hosted the 53rd NAACP Image Awards" beside their filmography.

const eventSchema = `
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS events_fts;
DELETE FROM credits WHERE work_type='event';
CREATE TABLE events(
  id INTEGER PRIMARY KEY, title TEXT NOT NULL, kind TEXT NOT NULL,
  award TEXT, edition INTEGER, date TEXT, year INTEGER,
  hosts TEXT, organizer TEXT, venue TEXT, location TEXT, network TEXT,
  best_film TEXT, most_wins TEXT, opening_film TEXT, closing_film TEXT,
  cover_image_file TEXT, wikipedia_url TEXT, gitdb_id INTEGER NOT NULL
);
CREATE INDEX idx_events_year ON events(year);
CREATE INDEX idx_events_kind ON events(kind);
CREATE VIRTUAL TABLE events_fts USING fts5(
  title, award, hosts,
  content='events', content_rowid='id', tokenize='trigram'
);`

func CIndexEvents(args []string) {
	fs := flag.NewFlagSet("index-events", flag.ExitOnError)
	records := fs.String("records", "filmstock-data", "the record tree")
	dbPath := fs.String("db", "", "the index to write")
	fs.Parse(args)

	out := *dbPath
	if out == "" {
		out = "index.db"
	}
	dir := filepath.Join(*records, filmstock.KindEvent)
	if _, err := os.Stat(dir); err != nil {
		fatal(fmt.Errorf("no event records at %s — run `extract` first", dir))
	}
	start := time.Now()

	db, err := sql.Open("sqlite", out)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(eventSchema); err != nil {
		fatal(err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=OFF; PRAGMA synchronous=OFF`); err != nil {
		fatal(err)
	}

	// Person ids already exist from the film and television passes; hosts that
	// are not otherwise in the database are skipped rather than invented, so
	// people stay keyed the way peoplebuild.go keyed them.
	people := map[string]int64{}
	rows, err := db.Query(`SELECT id, name FROM people`)
	if err != nil {
		fatal(err)
	}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err == nil {
			people[strings.ToLower(name)] = id
		}
	}
	rows.Close()

	tx, err := db.Begin()
	if err != nil {
		fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO events
      (id,title,kind,award,edition,date,year,hosts,organizer,venue,location,network,
       best_film,most_wins,opening_film,closing_film,cover_image_file,
       wikipedia_url,gitdb_id) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		fatal(err)
	}
	credit, err := tx.Prepare(
		`INSERT INTO credits(person_id, work_id, work_type, role) VALUES (?,?,'event','Host')`)
	if err != nil {
		fatal(err)
	}

	var n, ceremonies, festivals, hosted int
	err = filmstock.WalkStore(*records, filmstock.KindEvent, func(r filmstock.StoredRecord) error {
		var e filmstock.Event
		if err := json.Unmarshal(r.Data, &e); err != nil {
			return nil
		}
		names := make([]string, 0, len(e.Hosts))
		for _, h := range e.Hosts {
			names = append(names, h.Name)
		}
		if _, err := stmt.Exec(e.PageID, e.Title, e.Kind, e.Award, e.Edition, e.Date,
			e.Year, strings.Join(names, ", "), e.Organizer, e.Venue, e.Location,
			strings.Join(e.Network, ", "), e.BestFilm, e.MostWins, e.OpeningFilm,
			e.ClosingFilm, e.CoverImageFile, e.WikiURL, r.GitdbID); err != nil {
			return err
		}
		for _, h := range e.Hosts {
			if id, ok := people[strings.ToLower(h.Name)]; ok {
				if _, err := credit.Exec(id, e.PageID); err != nil {
					return err
				}
				hosted++
			}
		}
		n++
		if e.Kind == filmstock.EventAwardCeremony {
			ceremonies++
		} else {
			festivals++
		}
		return nil
	})
	if err != nil {
		fatal(err)
	}
	if err := tx.Commit(); err != nil {
		fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO events_fts(events_fts) VALUES('rebuild')`); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  events=%d (%d ceremonies, %d festivals)  host credits=%d  in %.1fs\n",
		n, ceremonies, festivals, hosted, time.Since(start).Seconds())
}
