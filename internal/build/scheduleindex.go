package build

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/szatmary/filmstock"
)

// Network television schedules, as tables.
//
// This is the only source in the corpus for what TIME a programme aired and
// what it aired against. An episode's air_date gives the day and nothing else,
// so "what was opposite Friends" and "when did a show move slots" are
// answerable from here and nowhere else.
const scheduleSchema = `
DROP TABLE IF EXISTS schedules;
DROP TABLE IF EXISTS schedule_slots;
CREATE TABLE schedules(
  id INTEGER PRIMARY KEY, title TEXT NOT NULL,
  season TEXT, daypart TEXT, wikipedia_url TEXT
);
CREATE INDEX idx_schedules_season ON schedules(season);

-- One programme in one half-hour slot on one night.
--
-- show_id is the series page_id where the cell linked an article we hold, and
-- 0 where it did not — about 29% of slots, mostly specials, films and shows
-- with no article. The title is kept either way because it is what the grid
-- says, but only show_id may be joined on.
CREATE TABLE schedule_slots(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  schedule_id INTEGER NOT NULL,
  day TEXT NOT NULL, network TEXT NOT NULL,
  start TEXT NOT NULL, end TEXT NOT NULL,
  part TEXT, title TEXT NOT NULL, show_id INTEGER,
  rerun INTEGER, rank INTEGER, rating REAL
);
CREATE INDEX idx_slots_schedule ON schedule_slots(schedule_id);
CREATE INDEX idx_slots_show ON schedule_slots(show_id);
-- "What else was on at 8pm that Thursday" is the question this table exists
-- for, and it is a scan without this.
CREATE INDEX idx_slots_when ON schedule_slots(day, start);
`

// CIndexSchedules adds the schedule tables to an existing database.
func CIndexSchedules(args []string) {
	fs := flag.NewFlagSet("index-schedules", flag.ExitOnError)
	dbPath := fs.String("db", "index.db", "the database to add schedules to")
	records := fs.String("records", "filmstock-data", "record tree from `filmstock export`")
	fs.Parse(args)

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(scheduleSchema); err != nil {
		fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		fatal(err)
	}
	insS, err := tx.Prepare(`INSERT OR REPLACE INTO schedules
		(id,title,season,daypart,wikipedia_url) VALUES(?,?,?,?,?)`)
	if err != nil {
		fatal(err)
	}
	insSlot, err := tx.Prepare(`INSERT INTO schedule_slots
		(schedule_id,day,network,start,end,part,title,show_id,rerun,rank,rating)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		fatal(err)
	}

	var nGrid, nSlot, nLinked int
	err = filmstock.WalkStore(*records, filmstock.KindSchedule, func(r filmstock.StoredRecord) error {
		var s filmstock.Schedule
		if err := json.Unmarshal(r.Data, &s); err != nil {
			fmt.Fprintf(os.Stderr, "schedule %s: %v\n", r.Key, err)
			return nil
		}
		if _, err := insS.Exec(s.PageID, s.Title, s.Season, s.Daypart, s.WikiURL); err != nil {
			return err
		}
		nGrid++
		for _, e := range s.Entries {
			rerun := 0
			if e.Rerun {
				rerun = 1
			}
			if _, err := insSlot.Exec(s.PageID, e.Day, e.Network, e.Start, e.End,
				e.Part, e.Title, e.ShowID, rerun, e.Rank, e.Rating); err != nil {
				return err
			}
			nSlot++
			if e.ShowID != 0 {
				nLinked++
			}
		}
		return nil
	})
	if err != nil {
		fatal(err)
	}
	insS.Close()
	insSlot.Close()
	if err := tx.Commit(); err != nil {
		fatal(err)
	}
	pct := 0
	if nSlot > 0 {
		pct = 100 * nLinked / nSlot
	}
	fmt.Fprintf(os.Stderr, "  inserted %d schedules, %d slots (%d linked to a series, %d%%)\n",
		nGrid, nSlot, nLinked, pct)
}
