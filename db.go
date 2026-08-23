package filmstock

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite" // pure Go: importers of this package need no cgo
)

// ErrNotFound is returned when an id is not in the database. It is a distinct
// error because callers need to tell it apart from a fetch that failed: one is a
// 404, the other is a 500, and collapsing them makes a broken record source look
// like a missing record.
var ErrNotFound = errors.New("filmstock: not found")

// DB is a searchable filmstock database plus the source its full records come
// from. It is safe for concurrent use.
//
// The division of labour is the whole design: every search, ranking and count
// below is answered from index.db alone and touches no network. Only the
// single-record accessors (Film, Series, Event) go to the RecordSource. That is
// what lets a consumer take a 161 MB database and still reach 620k full records.
type DB struct {
	sql *sql.DB
	src RecordSource
}

// Open opens a search database. src may be nil if the caller only searches;
// the record accessors then return an error explaining what is missing rather
// than panicking.
//
//	db, err := filmstock.Open("index.db", filmstock.Dir("out"))
func Open(path string, src RecordSource) (*DB, error) {
	// Read-only: nothing in this package writes, and saying so lets several
	// readers share one file without fighting over the write lock.
	h, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	if err := h.Ping(); err != nil {
		h.Close()
		return nil, fmt.Errorf("filmstock: open %s: %w", path, err)
	}
	return &DB{sql: h, src: src}, nil
}

// FromSQL wraps an already-open handle, for callers who manage their own pool.
func FromSQL(h *sql.DB, src RecordSource) *DB { return &DB{sql: h, src: src} }

// SQL exposes the underlying handle. Everything this package does is a plain
// query against a documented schema, so dropping to SQL is expected rather than
// a workaround — use it for joins and aggregates the API does not cover.
func (db *DB) SQL() *sql.DB { return db.sql }

func (db *DB) Close() error { return db.sql.Close() }

// ── search: database only, no network ──────────────────────────────────────

// SearchFilms ranks films by fuzzy title match. field selects what is matched:
// "title", "starring" or "director"; empty means title.
func (db *DB) SearchFilms(ctx context.Context, q, field string, limit int) ([]SearchResult, error) {
	return SearchMovies(ctx, db.sql, q, field, limit)
}

// SearchSeries ranks television series. field: "title", "starring" or "creator".
func (db *DB) SearchSeries(ctx context.Context, q, field string, limit int) ([]TelevisionSearchResult, error) {
	return SearchTelevision(ctx, db.sql, q, field, limit)
}

func (db *DB) SearchEpisodes(ctx context.Context, q string, limit int) ([]EpisodeSearchResult, error) {
	return SearchEpisodes(ctx, db.sql, q, limit)
}

func (db *DB) SearchPeople(ctx context.Context, q string, limit int) ([]PersonResult, error) {
	return SearchPeople(ctx, db.sql, q, limit)
}

func (db *DB) SearchEvents(ctx context.Context, q string, limit int) ([]UnifiedResult, error) {
	return SearchEvents(ctx, db.sql, q, limit)
}

// Filmography returns everything a person is credited on, grouped by role.
func (db *DB) Filmography(personID int) (*Filmography, error) {
	return PersonFilmography(db.sql, personID)
}

// PersonID resolves a display name to a person id. It returns 0 when the name
// is unknown — names are not identities here, so a miss is ordinary.
func (db *DB) PersonID(name string) int { return PersonIDByName(db.sql, name) }

// ── records: one fetch each ────────────────────────────────────────────────

// Film returns the full film record: everything in the database plus what only
// the record holds — plot, overview, genre, cinematography, editing, production
// companies, release dates, and the unparsed infobox.
func (db *DB) Film(ctx context.Context, id int) (*Movie, error) {
	var m Movie
	if err := db.fetch(ctx, KindMovie, id, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Series returns the full series record, including every season and episode
// with its summary, director and writer. Episode summaries live here and not in
// the database on purpose: they are 150 MB of text that nothing searches.
func (db *DB) Series(ctx context.Context, id int) (*TelevisionSeries, error) {
	var s TelevisionSeries
	if err := db.fetch(ctx, KindTelevision, id, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Event returns the full award-ceremony or festival record.
func (db *DB) Event(ctx context.Context, id int) (*Event, error) {
	var e Event
	if err := db.fetch(ctx, KindEvent, id, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// locate reads where a record lives, from the index.
func (db *DB) locate(ctx context.Context, kind string, id int) (Location, error) {
	var table string
	switch kind {
	case KindMovie:
		table = "movies"
	case KindTelevision:
		table = "television_series"
	case KindEvent:
		table = "events"
	default:
		return Location{}, fmt.Errorf("filmstock: no records of kind %q", kind)
	}
	loc := Location{Kind: kind, ID: id}
	err := db.sql.QueryRowContext(ctx,
		`SELECT gitdb_id FROM `+table+` WHERE id = ?`, id).Scan(&loc.GitdbID)
	if err == sql.ErrNoRows {
		return Location{}, fmt.Errorf("no %s with id %d: %w", kind, id, ErrNotFound)
	}
	return loc, err
}

func (db *DB) fetch(ctx context.Context, kind string, id int, v any) error {
	if db.src == nil {
		return fmt.Errorf("filmstock: no RecordSource configured; " +
			"pass filmstock.Dir(root) to Open")
	}
	loc, err := db.locate(ctx, kind, id)
	if err != nil {
		return err
	}
	b, err := db.src.Fetch(ctx, loc)
	if err != nil {
		return err
	}
	return decodeRecord(b, v)
}

// Person returns the full record for a person: their identity, and — when their
// credit links to an article that is actually a biography — birth and death,
// occupation, nationality and the lead of their article.
//
// Not every person has one. A credit's link target may be a redlink, a
// disambiguation page, or something that is not a person at all; those return a
// record with identity and no PersonBio, which is the honest answer rather than
// an error.
func (db *DB) Person(ctx context.Context, id int) (*PersonRecord, error) {
	var qid sql.NullInt64
	var name, wiki string
	err := db.sql.QueryRowContext(ctx,
		`SELECT qid, name, COALESCE(wiki,'') FROM people WHERE id = ?`, id).Scan(&qid, &name, &wiki)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no person with id %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	rec := &PersonRecord{QID: qid.Int64, Wiki: wiki, Name: name}
	if db.src == nil {
		return rec, nil
	}
	// A person's identity is their Q-id where they have one, and the link target
	// hash where they do not. That identity is what the record carries; where the
	// record lives is the store's business and comes from the index.
	recID := qid.Int64
	if recID == 0 {
		if wiki == "" {
			return rec, nil
		}
		recID = -int64(PersonRecordPathID(wiki))
	}
	var gid uint64
	if err := db.sql.QueryRowContext(ctx,
		`SELECT COALESCE(gitdb_id,0) FROM people WHERE id = ?`, id).Scan(&gid); err != nil || gid == 0 {
		return rec, nil // identity stands even with no record behind it
	}
	loc := Location{Kind: KindPerson, ID: int(recID), GitdbID: gid}
	b, err := db.src.Fetch(ctx, loc)
	if err != nil {
		return rec, nil // no record on disk is not an error; the identity stands
	}
	var full PersonRecord
	if err := decodeRecord(b, &full); err != nil {
		return rec, nil
	}
	return &full, nil
}
