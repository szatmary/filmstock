package build

import (
	"strings"

	"github.com/szatmary/filmstock"
)

// The published database schema, one DDL block per subject area. The
// recognisers emit records and dbwriter is the recordSink that lands them
// here; these blocks are all that survives of the per-kind index builders.

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
  wikipedia_url TEXT, cover_image_url TEXT, cover_image_file TEXT,
  -- The article title, disambiguator and all.
  --
  -- title is the DISPLAY title with Wikipedia's parenthetical removed, because
  -- "(1985 film)" is namespacing rather than part of the name. But 11,925 titles
  -- are shared by 31,934 films: six Vertigos, five Heats, two Godfathers — the
  -- 175-minute cut and the 539-minute television version. Stripped, they are one
  -- string repeated, and the only thing that ever told them apart was the
  -- parenthetical.
  --
  -- Nothing keys on this. Identity is the page_id, as everywhere else.
  wiki_title TEXT
);
CREATE INDEX idx_movies_wiki_title ON movies(wiki_title);
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

const peopleSchema = `
DROP TABLE IF EXISTS people;
DROP TABLE IF EXISTS credits;
DROP TABLE IF EXISTS person_alias;
DROP TABLE IF EXISTS people_fts;
-- image_url resolves through Special:FilePath, exactly as film posters do.
-- 83% of biographies state a portrait filename; a credit-only person has none.
CREATE TABLE people(id INTEGER PRIMARY KEY, page_id INTEGER, qid INTEGER,
  name TEXT NOT NULL, wiki TEXT, image_url TEXT);
CREATE INDEX idx_people_qid ON people(qid);
CREATE INDEX idx_people_name ON people(name);
CREATE TABLE credits(person_id INTEGER, work_id INTEGER, work_type TEXT, role TEXT);
CREATE INDEX idx_credits_person ON credits(person_id);
CREATE INDEX idx_credits_work ON credits(work_id, work_type);
CREATE TABLE person_alias(wiki TEXT PRIMARY KEY, person_id INTEGER);
CREATE VIRTUAL TABLE people_fts USING fts5(name, content='people', content_rowid='id', tokenize='trigram');`

const televisionSchema = `
DROP TABLE IF EXISTS television_series;
DROP TABLE IF EXISTS television_fts;
DROP TABLE IF EXISTS television_episodes;
DROP TABLE IF EXISTS television_episodes_fts;
DROP TABLE IF EXISTS television_seasons;
DELETE FROM credits WHERE work_type='television';
CREATE TABLE television_series(
  id INTEGER PRIMARY KEY, title TEXT NOT NULL, year INTEGER,
  first_aired TEXT, last_aired TEXT, genre TEXT, creator TEXT, starring TEXT,
  network TEXT, num_seasons TEXT, num_episodes TEXT,
  seasons_count INTEGER, episodes_count INTEGER,
  cover_image_file TEXT, cover_image_url TEXT, wikipedia_url TEXT,
  -- As with films: 2,208 titles are shared by 5,215 series. Five different
  -- shows are called Friends, five are called The Office.
  wiki_title TEXT
);
CREATE INDEX idx_television_wiki_title ON television_series(wiki_title);
CREATE INDEX idx_television_title ON television_series(title);
CREATE VIRTUAL TABLE television_fts USING fts5(
  title, starring, creator,
  content='television_series', content_rowid='id', tokenize='trigram'
);
-- Seasons, as their own rows. A season is a real article with a real page_id,
-- and it carries what only it knows: the cast for that run, the network it was
-- on, and its Nielsen standing. The series-level cast cannot express any of it
-- — one flat Starring for a fifteen-season show asserts everyone was in it
-- throughout.
--
-- page_id is 0 for a season with no article of its own, which is most seasons
-- of most shows: they come from the series overview table on the episode-list
-- page, and exist here only because of it.
CREATE TABLE television_seasons(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  series_id INTEGER NOT NULL, season INTEGER NOT NULL, page_id INTEGER,
  num_episodes INTEGER, first_aired TEXT, last_aired TEXT,
  network TEXT, starring TEXT, image TEXT,
  rank INTEGER, rating REAL, viewers REAL
);
CREATE INDEX idx_television_seasons_series ON television_seasons(series_id, season);
CREATE TABLE television_episodes(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  series_id INTEGER, season INTEGER,
  number_in_season INTEGER, number_overall INTEGER, title TEXT, air_date TEXT,
  viewers REAL
);
CREATE INDEX idx_television_ep_series ON television_episodes(series_id);
CREATE VIRTUAL TABLE television_episodes_fts USING fts5(
  title, content='television_episodes', content_rowid='id', tokenize='trigram'
);`

const eventSchema = `
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS events_fts;
DELETE FROM credits WHERE work_type='event';
CREATE TABLE events(
  id INTEGER PRIMARY KEY, title TEXT NOT NULL, kind TEXT NOT NULL,
  award TEXT, edition INTEGER, date TEXT, year INTEGER,
  hosts TEXT, organizer TEXT, venue TEXT, location TEXT, network TEXT,
  best_film TEXT, most_wins TEXT, opening_film TEXT, closing_film TEXT,
  cover_image_file TEXT, wikipedia_url TEXT
);
CREATE INDEX idx_events_year ON events(year);
CREATE INDEX idx_events_kind ON events(kind);
CREATE VIRTUAL TABLE events_fts USING fts5(
  title, award, hosts,
  content='events', content_rowid='id', tokenize='trigram'
);`

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
		{"Narrator", m.Narrator},
	}
}

func join(v []string) string { return strings.Join(v, " · ") }

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

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

func joinP(v []filmstock.Person) string {
	names := make([]string, len(v))
	for i, p := range v {
		names[i] = p.Name
	}
	return strings.Join(names, " · ")
}
