package filmstock

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/szatmary/filmstock/internal/sqldrv"
)

// DB is an open filmstock database, safe for concurrent use.
//
// It is a handle and nothing more. This package's job is the FILES — building
// them, publishing them, fetching them, verifying them, keeping them current —
// and a consumer's questions are its own, so they are asked in SQL against a
// documented schema (docs/schema.md) rather than through methods here.
type DB struct {
	sql *sql.DB
}

// An Attach names another database file to make visible on the same
// connections, so one query can join across them.
//
//	db, _ := filmstock.Open("filmstock.db",
//	    filmstock.Attach{Schema: "text", Path: "filmstock-text.db"})
//	db.SQL().Query(`SELECT m.title, t.plot FROM movies m
//	                JOIN text.movie_text t ON t.id = m.id WHERE m.year = ?`, 1954)
//
// Attachments are declared at Open rather than added later because ATTACH is
// per-connection state: a statement run on one pooled connection leaves every
// other one unable to see the schema. Declaring them up front lets the pool
// grow and shrink with every connection carrying the same schemas.
type Attach struct {
	Schema string // the name the database answers to in SQL
	Path   string // the file
}

// Open opens a published filmstock database read-only, optionally attaching
// others beside it.
//
// What comes back is a handle and a documented schema. This package manages
// the databases — fetching them, verifying them, keeping them current — and
// deliberately does not wrap them in an API: a consumer's questions are its
// own, and SQL against attached databases answers more of them than any set
// of methods would.
//
//	db, err := filmstock.Open("filmstock.db")
func Open(path string, attach ...Attach) (*DB, error) {
	var init []string
	for _, a := range attach {
		if a.Schema == "" || a.Path == "" {
			return nil, fmt.Errorf("filmstock: attach needs both a schema and a path, got %+v", a)
		}
		if _, err := os.Stat(a.Path); err != nil {
			return nil, fmt.Errorf("filmstock: attaching %s: %w", a.Schema, err)
		}
		// Read-only for the same reason the main database is, and through the
		// same DSN builder, so an attached database may be compressed too.
		init = append(init, fmt.Sprintf("ATTACH DATABASE %s AS %s",
			quoteSQL(sqldrv.DSN(a.Path, true)), quoteIdent(a.Schema)))
	}
	// Read-only: nothing in this package writes, and saying so lets several
	// readers share one file without fighting over the write lock.
	c, err := sqldrv.Connector(path, true, init)
	if err != nil {
		return nil, err
	}
	h := sql.OpenDB(c)
	if err := h.Ping(); err != nil {
		h.Close()
		// A pure-Go build has no zstd VFS, so a compressed database is just
		// bytes it cannot parse. Say which problem it is.
		if sqldrv.PureGo {
			if c, cerr := isCompressed(path); cerr == nil && c {
				return nil, fmt.Errorf("filmstock: %s is a compressed database, "+
					"which needs the cgo build (this binary was built without it)", path)
			}
		}
		return nil, fmt.Errorf("filmstock: open %s: %w", path, err)
	}
	return &DB{sql: h}, nil
}

// quoteSQL renders a string as a SQL literal; quoteIdent as an identifier.
// ATTACH takes both and neither can be a bound parameter.
func quoteSQL(s string) string   { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// FromSQL wraps an already-open handle, for callers who manage their own pool.
func FromSQL(h *sql.DB) *DB { return &DB{sql: h} }

// SQL exposes the underlying handle. Everything this package does is a plain
// query against a documented schema, so dropping to SQL is expected rather than
// a workaround — use it for joins and aggregates the API does not cover.
func (db *DB) SQL() *sql.DB { return db.sql }

func (db *DB) Close() error { return db.sql.Close() }
