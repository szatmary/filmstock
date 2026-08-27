package build

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/szatmary/filmstock"
)

// A dbWriter takes finished records straight into the published database.
//
// The record tree used to sit between the two: export wrote a gitdb store and a
// separate pass walked it to build the database. Since the database is what
// ships, that middle serialisation was a copy of the data on the way past — it
// cost a full write and a full read of every record for nothing but the
// convenience of two commands instead of one.
//
// The one thing that makes this less than mechanical is ORDER. Credits are
// produced with each work, as the pass reads it, but a person's identity is not
// resolved until every work has been seen — a credit is a link target, and
// which page_id it resolves to depends on the whole corpus. So credits cannot
// be written when they are produced.
//
// They are staged in a table rather than held in memory. There are 1.7M of
// them; SQLite handles that without noticing, and a map would not.
type dbWriter struct {
	mu   sync.Mutex
	db   *sql.DB
	tx   *sql.Tx
	err  error
	n    int
	text string // the synopsis database, attached

	insMovie, insSeries, insSeason, insEpisode  *sql.Stmt
	insEvent, insSchedule, insSlot, insPerson   *sql.Stmt
	insMovieText, insSeriesText, insEpisodeText *sql.Stmt
	insCredit                                   *sql.Stmt

	unchanged, updated, inserted int
}

// creditStagingSchema holds credits until people have identities.
//
// wiki is the credit's link target, which is what a person is recognised by
// before anything is resolved. At the end it joins to person_alias, which the
// person pass fills in — so a credit reaches the right person by the same
// stated link the encyclopaedia used, never by a display name.
const creditStagingSchema = `
CREATE TABLE IF NOT EXISTS credit_staging(
  work_id INTEGER NOT NULL, work_type TEXT NOT NULL,
  role TEXT NOT NULL, wiki TEXT NOT NULL, name TEXT NOT NULL
);
`

func newDBWriter(dbPath, textPath string) (*dbWriter, error) {
	if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.Remove(textPath); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // the PRAGMAs below are per-connection
	for _, s := range []string{
		`PRAGMA journal_mode=OFF`, `PRAGMA synchronous=OFF`,
		schema, peopleSchema, televisionSchema, eventSchema, scheduleSchema,
		creditStagingSchema,
	} {
		if _, err := db.Exec(s); err != nil {
			db.Close()
			return nil, fmt.Errorf("schema: %w", err)
		}
	}
	w := &dbWriter{db: db, text: textPath}
	if err := attachText(db, textPath); err != nil {
		db.Close()
		return nil, err
	}
	if err := w.begin(); err != nil {
		db.Close()
		return nil, err
	}
	return w, nil
}

func (w *dbWriter) Err() error { return w.err }

func (w *dbWriter) Counts() (int, int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.unchanged, w.updated, w.inserted
}

// sweep has nothing to do: the database is built fresh each run, so a record
// the run did not produce was never written. The gitdb store needed a sweep
// because it was updated in place.
func (w *dbWriter) sweep() int { return 0 }

func (w *dbWriter) fail(err error) {
	if err != nil && w.err == nil {
		w.err = err
	}
}

// begin opens a transaction and prepares every statement against it. Statements
// belong to the transaction they were prepared on, so they are remade each time
// the batch commits.
func (w *dbWriter) begin() error {
	tx, err := w.db.Begin()
	if err != nil {
		return err
	}
	w.tx = tx
	p := func(q string) *sql.Stmt {
		s, e := tx.Prepare(q)
		if e != nil && err == nil {
			err = fmt.Errorf("%s: %w", strings.SplitN(q, "(", 2)[0], e)
		}
		return s
	}
	w.insMovie = p(`INSERT OR REPLACE INTO movies
		(id,title,year,release_date,director,producer,writer,starring,music,
		 distributor,country,language,genre,runtime,budget,gross,wikipedia_url,
		 cover_image_url,cover_image_file,wiki_title)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	w.insSeries = p(`INSERT OR REPLACE INTO television_series
		(id,title,year,first_aired,last_aired,genre,creator,starring,network,
		 num_seasons,num_episodes,seasons_count,episodes_count,cover_image_file,
		 wikipedia_url,wiki_title) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	w.insSeason = p(`INSERT INTO television_seasons
		(series_id,season,page_id,num_episodes,first_aired,last_aired,
		 network,starring,image,rank,rating,viewers) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`)
	w.insEpisode = p(`INSERT INTO television_episodes
		(series_id,season,number_in_season,number_overall,title,air_date,viewers)
		VALUES(?,?,?,?,?,?,?)`)
	w.insEvent = p(`INSERT OR REPLACE INTO events
		(id,title,kind,award,edition,date,year,hosts,organizer,venue,location,network,
		 best_film,most_wins,opening_film,closing_film,cover_image_file,wikipedia_url)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	w.insSchedule = p(`INSERT OR REPLACE INTO schedules
		(id,title,season,daypart,wikipedia_url) VALUES(?,?,?,?,?)`)
	w.insSlot = p(`INSERT INTO schedule_slots
		(schedule_id,day,network,start,end,part,title,show_id,rerun,rank,rating)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
	w.insPerson = p(`INSERT INTO people(page_id,qid,name,wiki,image_url) VALUES(?,?,?,?,?)`)
	w.insMovieText = p(`INSERT OR REPLACE INTO synopsis.movie_text(id,overview,plot) VALUES(?,?,?)`)
	w.insSeriesText = p(`INSERT OR REPLACE INTO synopsis.television_text(id,overview,plot) VALUES(?,?,?)`)
	w.insEpisodeText = p(`INSERT OR REPLACE INTO synopsis.episode_text(id,series_id,summary) VALUES(?,?,?)`)
	w.insCredit = p(`INSERT INTO credit_staging(work_id,work_type,role,wiki,name) VALUES(?,?,?,?,?)`)
	return err
}

// commit closes the batch. Called periodically so a long run is not one
// transaction holding every row it has written.
func (w *dbWriter) commit() error {
	if w.tx == nil {
		return nil
	}
	for _, s := range []*sql.Stmt{w.insMovie, w.insSeries, w.insSeason, w.insEpisode,
		w.insEvent, w.insSchedule, w.insSlot, w.insPerson,
		w.insMovieText, w.insSeriesText, w.insEpisodeText, w.insCredit} {
		if s != nil {
			s.Close()
		}
	}
	err := w.tx.Commit()
	w.tx = nil
	return err
}

// credit stages one person's link target against a work.
func (w *dbWriter) credit(people []filmstock.Person, workID int64, workType, role string) {
	for _, p := range people {
		if p.Wiki == "" {
			continue // a bare name is not an identity, and never has been
		}
		if _, err := w.insCredit.Exec(workID, workType, role, p.Wiki, p.Name); err != nil {
			w.fail(err)
			return
		}
	}
}

// put writes one record. The identity is always an enwiki page_id.
func (w *dbWriter) put(kind string, identity int64, v any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return
	}
	switch kind {
	case filmstock.KindMovie:
		w.putMovie(identity, v.(*filmstock.Movie))
	case filmstock.KindTelevision:
		w.putSeries(identity, v.(*filmstock.TelevisionSeries))
	case filmstock.KindEvent:
		w.putEvent(identity, v.(*filmstock.Event))
	case filmstock.KindSchedule:
		w.putSchedule(identity, v.(*filmstock.Schedule))
	case filmstock.KindPerson:
		w.putPerson(identity, v.(*filmstock.PersonRecord))
	default:
		w.fail(fmt.Errorf("dbwriter: unknown kind %q", kind))
		return
	}
	w.inserted++
	// Commit periodically. One transaction across 400k records would hold every
	// row it had written in memory until the very end.
	if w.n++; w.n%50_000 == 0 {
		if err := w.commit(); err != nil {
			w.fail(err)
			return
		}
		w.fail(w.begin())
	}
}

func (w *dbWriter) putMovie(id int64, m *filmstock.Movie) {
	if _, err := w.insMovie.Exec(id, m.Title, yearOf(m), first(m.ReleaseDates),
		joinP(m.Director), joinP(m.Producer), joinP(m.Writer), joinP(m.Starring),
		joinP(m.Music), join(filmstock.Names(m.Distributor)),
		join(filmstock.Names(m.Country)), join(filmstock.Names(m.Language)),
		join(m.Genre), m.Runtime, m.Budget, m.Gross, m.WikiURL,
		m.CoverImageURL, m.CoverImageFile, filmstock.WikiTitleFromURL(m.WikiURL)); err != nil {
		w.fail(err)
		return
	}
	if m.Overview != "" || m.Plot != "" {
		if _, err := w.insMovieText.Exec(id, m.Overview, m.Plot); err != nil {
			w.fail(err)
			return
		}
	}
	for _, rc := range roleCredits(m) {
		w.credit(rc.people, id, "movie", rc.role)
	}
}

func (w *dbWriter) putSeries(id int64, s *filmstock.TelevisionSeries) {
	year := 0
	if len(s.FirstAired) >= 4 {
		fmt.Sscanf(s.FirstAired[:4], "%d", &year)
	}
	eps := 0
	for _, se := range s.Seasons {
		eps += len(se.Episodes)
	}
	if _, err := w.insSeries.Exec(id, filmstock.CleanTelevisionTitle(s.Title), year,
		s.FirstAired, s.LastAired, join(s.Genre), joinP(s.Creator), joinP(s.Starring),
		join(filmstock.Names(s.Network)), s.NumSeasons, s.NumEpisodes,
		len(s.Seasons), eps, s.CoverImageFile, s.WikiURL,
		filmstock.WikiTitleFromURL(s.WikiURL)); err != nil {
		w.fail(err)
		return
	}
	if s.Overview != "" || s.Plot != "" {
		w.insSeriesText.Exec(id, s.Overview, s.Plot)
	}
	for _, c := range []struct {
		role   string
		people []filmstock.Person
	}{
		{"Creator", s.Creator}, {"Cast", s.Starring}, {"Composer", s.Composer},
		{"Director", s.Director}, {"Producer", s.Producer},
		{"Executive Producer", s.ExecutiveProducer}, {"Writer", s.Writer},
		{"Editor", s.Editor}, {"Cinematographer", s.Cinematography},
		{"Presenter", s.Presenter}, {"Narrator", s.Narrator},
	} {
		w.credit(c.people, id, "television", c.role)
	}
	for _, se := range s.Seasons {
		if _, err := w.insSeason.Exec(id, se.Season, se.PageID, se.NumEpisodes,
			se.FirstAired, se.LastAired, se.Network, joinP(se.Starring), se.Image,
			se.Rank, se.Rating, se.Viewers); err != nil {
			w.fail(err)
			return
		}
		w.credit(se.Starring, id, "television", "Cast")
		for _, e := range se.Episodes {
			res, err := w.insEpisode.Exec(id, se.Season, e.NumberInSeason,
				e.NumberOverall, e.Title, e.AirDate, e.Viewers)
			if err != nil {
				w.fail(err)
				return
			}
			if e.Summary != "" {
				if eid, err := res.LastInsertId(); err == nil {
					w.insEpisodeText.Exec(eid, id, e.Summary)
				}
			}
			w.credit(e.DirectedBy, id, "television", "Director")
			w.credit(e.WrittenBy, id, "television", "Writer")
		}
	}
}

func (w *dbWriter) putEvent(id int64, e *filmstock.Event) {
	names := make([]string, 0, len(e.Hosts))
	for _, h := range e.Hosts {
		names = append(names, h.Name)
	}
	if _, err := w.insEvent.Exec(id, e.Title, e.Kind, e.Award, e.Edition, e.Date,
		e.Year, strings.Join(names, ", "), e.Organizer, e.Venue, e.Location,
		join(filmstock.Names(e.Network)), join(filmstock.Names(e.BestFilm)),
		join(filmstock.Names(e.MostWins)), join(filmstock.Names(e.OpeningFilm)),
		join(filmstock.Names(e.ClosingFilm)), e.CoverImageFile, e.WikiURL); err != nil {
		w.fail(err)
		return
	}
	w.credit(e.Hosts, id, "event", "Host")
}

func (w *dbWriter) putSchedule(id int64, s *filmstock.Schedule) {
	if _, err := w.insSchedule.Exec(id, s.Title, s.Season, s.Daypart, s.WikiURL); err != nil {
		w.fail(err)
		return
	}
	for _, e := range s.Entries {
		rerun := 0
		if e.Rerun {
			rerun = 1
		}
		if _, err := w.insSlot.Exec(id, e.Day, e.Network, e.Start, e.End, e.Part,
			e.Title, e.ShowID, rerun, e.Rank, e.Rating); err != nil {
			w.fail(err)
			return
		}
	}
}

func (w *dbWriter) putPerson(id int64, p *filmstock.PersonRecord) {
	// The biography states a bare filename; the published column is a URL,
	// resolved through Special:FilePath exactly as film posters are.
	imageURL := ""
	if p.PersonBio != nil && p.Image != "" {
		_, imageURL = filmstock.CoverImageURL(p.Image)
	}
	if _, err := w.insPerson.Exec(id, p.QID, p.Name, p.Wiki, imageURL); err != nil {
		w.fail(err)
	}
}

// finish resolves the staged credits and builds the search indexes.
//
// This is where the ordering is paid off: every work has been seen, every
// person has an identity, and a credit's link target can finally be turned into
// a person id.
func (w *dbWriter) finish() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if err := w.commit(); err != nil {
		return err
	}
	exec := func(q string) {
		if w.err != nil {
			return
		}
		if _, err := w.db.Exec(q); err != nil {
			w.err = fmt.Errorf("%s: %w", strings.TrimSpace(strings.SplitN(q, "\n", 2)[0]), err)
		}
	}

	// A credited person with no article gets no RECORD, but stays searchable and
	// keeps their credits: the works that state them are the evidence they
	// exist. 77,743 of them, and dropping their rows here would quietly undo
	// what the record decision deliberately preserved.
	//
	// Keyed by the link target, which is what the encyclopaedia stated, and
	// named by it too — the credit's display name varies between films while the
	// target does not.
	exec(`INSERT INTO people(page_id,qid,name,wiki)
	      SELECT 0, 0, MIN(s.name), s.wiki FROM credit_staging s
	      WHERE s.wiki NOT IN (SELECT wiki FROM people WHERE wiki <> '')
	      GROUP BY s.wiki`)

	// The alias table is what a credit joins through: link target -> person.
	exec(`INSERT OR IGNORE INTO person_alias(wiki,person_id)
	      SELECT wiki, id FROM people WHERE wiki <> ''`)

	// DISTINCT because one person can be stated twice in the same role on the
	// same work — two starring parameters, a season cast repeating the series
	// cast — and that is one credit, not two.
	exec(`INSERT INTO credits(person_id,work_id,work_type,role)
	      SELECT DISTINCT a.person_id, s.work_id, s.work_type, s.role
	      FROM credit_staging s JOIN person_alias a ON a.wiki = s.wiki`)
	exec(`DROP TABLE credit_staging`)

	for _, t := range []string{"movies_fts", "television_fts",
		"television_episodes_fts", "events_fts", "people_fts"} {
		exec(`INSERT INTO ` + t + `(` + t + `) VALUES('rebuild')`)
	}
	return w.err
}

// Close finishes and releases the database.
func (w *dbWriter) Close() error {
	err := w.finish()
	if cerr := w.db.Close(); err == nil {
		err = cerr
	}
	return err
}
