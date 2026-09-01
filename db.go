package filmstock

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/szatmary/filmstock/internal/sqldrv"
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
// below is answered from the database alone and touches no network.
type DB struct {
	sql *sql.DB
}

// Open opens a search database.
//
//	db, err := filmstock.Open("filmstock.db")
func Open(path string) (*DB, error) {
	// Read-only: nothing in this package writes, and saying so lets several
	// readers share one file without fighting over the write lock.
	h, err := sql.Open(sqldrv.Name, sqldrv.DSN(path, true))
	if err != nil {
		return nil, err
	}
	if err := h.Ping(); err != nil {
		h.Close()
		// A pure-Go build has no zstd VFS, so a compressed database is just
		// bytes it cannot parse. Say which problem it is.
		if sqldrv.PureGo {
			if c, cerr := IsCompressed(path); cerr == nil && c {
				return nil, fmt.Errorf("filmstock: %s is a compressed database, "+
					"which needs the cgo build (this binary was built without it)", path)
			}
		}
		return nil, fmt.Errorf("filmstock: open %s: %w", path, err)
	}
	return &DB{sql: h}, nil
}

// FromSQL wraps an already-open handle, for callers who manage their own pool.
func FromSQL(h *sql.DB) *DB { return &DB{sql: h} }

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

// Person returns the full record for a person: their identity, and — when their
// credit links to an article that is actually a biography — birth and death,
// occupation, nationality and the lead of their article.
//
// Not every person has one. A credit's link target may be a redlink, a
// disambiguation page, or something that is not a person at all; those return a
// record with identity and no PersonBio, which is the honest answer rather than
// an error.
func (db *DB) Person(ctx context.Context, id int) (*PersonRecord, error) {
	var qid, pageID sql.NullInt64
	var name, wiki string
	err := db.sql.QueryRowContext(ctx,
		`SELECT page_id, qid, name, COALESCE(wiki,'') FROM people WHERE id = ?`,
		id).Scan(&pageID, &qid, &name, &wiki)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no person with id %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	rec := &PersonRecord{PageID: int(pageID.Int64), QID: qid.Int64, Wiki: wiki, Name: name}
	return rec, nil
}
