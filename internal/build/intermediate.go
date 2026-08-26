package build

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/szatmary/filmstock/internal/dump"
)

// The intermediate store: everything the dump said, before anything decides what
// it means.
//
// Reading the dump and shaping records are different jobs that change at
// different rates, and fusing them means paying a 41-minute re-parse every time a
// question about record shape is asked. This holds the parse so the shaping can
// be re-run in minutes.
//
// Three tiers:
//
//	wikitext   the source of every page we recognise   — a parser fix re-parses this
//	entities   what the parsers made of it             — a shape change re-exports this
//	records    what gets published
//
// It is build-time infrastructure, like the resolver cache. It is never
// published and can be rebuilt from a dump at any time.
//
// It keeps EVERY person, not the credited ones. A daily update cannot resolve a
// new film's cast today precisely because the cast's articles were not edited
// that day and we discarded them on the last full pass — 721,211 person articles
// seen and dropped because, at that moment, nothing had asked for them.
type Inter struct {
	db   *sql.DB
	path string

	insPage *sql.Stmt
	insLink *sql.Stmt
	tx      *sql.Tx
	n       int
}

const interSchema = `
PRAGMA journal_mode=OFF;
PRAGMA synchronous=OFF;

-- One row per page any recogniser claimed.
--
-- kind is what claimed it, and a page can be claimed by more than one thing: an
-- article may carry both a film infobox and a person infobox, and deciding which
-- wins is export's problem, not import's.
CREATE TABLE IF NOT EXISTS pages(
    page_id   INTEGER NOT NULL,
    kind      TEXT    NOT NULL,
    title     TEXT    NOT NULL,
    wikitext  BLOB,              -- gzipped source, so a parser fix never needs the dump
    infobox   TEXT,              -- the complete parameter map, values untouched
    lead      TEXT,
    plot      TEXT,
    parsed    TEXT,              -- what the parser made of it, as JSON
    PRIMARY KEY (page_id, kind)
);
CREATE INDEX IF NOT EXISTS idx_pages_title ON pages(title);
CREATE INDEX IF NOT EXISTS idx_pages_kind  ON pages(kind);

-- Every link a page states, with the field it came from.
--
-- This is the reference graph, and it is what makes incremental export possible:
-- adding a film has to mark its cast for re-export, and without recorded
-- references there is no way to know what went stale. Getting this wrong emits a
-- stale record silently, which is this codebase's characteristic failure.
CREATE TABLE IF NOT EXISTS links(
    from_id INTEGER NOT NULL,
    field   TEXT    NOT NULL,   -- director, starring, list_episodes, next_season…
    target  TEXT    NOT NULL,   -- the link target, unresolved
    PRIMARY KEY (from_id, field, target)
);
CREATE INDEX IF NOT EXISTS idx_links_target ON links(target);

-- What this intermediate was built from, so a stale one is detectable rather
-- than silently mixed with a newer dump.
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT);
`

// rd is what reads must go through.
//
// The store holds ONE connection, because the PRAGMAs are per-connection and a
// pooled second one would quietly run with synchronous=FULL. That makes a read
// issued while a write transaction is open a deadlock rather than a slow query:
// the transaction holds the only connection and the read waits for it forever.
// The daily update reads and writes interleaved, page by page, so this is not a
// corner case — it is the normal path.
func (in *Inter) rd() interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
} {
	if in.tx != nil {
		return in.tx
	}
	return in.db
}

// OpenInter opens or creates an intermediate store.
func OpenInter(path string) (*Inter, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("intermediate %s: %w", path, err)
	}
	// One connection: the PRAGMAs above are per-connection, and a pooled second
	// one would quietly run with synchronous=FULL.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(interSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("intermediate %s: %w", path, err)
	}
	return &Inter{db: db, path: path}, nil
}

func (in *Inter) Close() error {
	if in.tx != nil {
		if err := in.commit(); err != nil {
			in.db.Close()
			return err
		}
	}
	return in.db.Close()
}

// SetSource records which dump this was built from.
func (in *Inter) SetSource(dump string) error {
	_, err := in.db.Exec(
		`INSERT OR REPLACE INTO meta VALUES('source',?),('built',?)`,
		dump, time.Now().UTC().Format(time.RFC3339))
	return err
}

// Source reports the dump this intermediate was built from.
func (in *Inter) Source() (string, error) {
	var s string
	err := in.rd().QueryRow(`SELECT value FROM meta WHERE key='source'`).Scan(&s)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return s, err
}

// A Page is one entity as imported.
type Page struct {
	PageID   int
	Kind     string
	Title    string
	Wikitext string
	Infobox  map[string]string
	Lead     string
	Plot     string
	Parsed   any        // the parser's output, stored as JSON
	Links    []PageLink // every link the page states, with the field it came from
}

// A PageLink is one stated link. A slice rather than a map keyed by field: a
// film states many starring links and a map would keep one of them.
type PageLink struct {
	Field  string // director, starring, list_episodes, next_season…
	Target string // the link target, unresolved
}

// Put writes one page. Repeated writes of the same (page_id, kind) replace, so a
// daily update is an upsert and re-importing is idempotent.
func (in *Inter) Put(p *Page) error {
	if err := in.begin(); err != nil {
		return err
	}
	var ib, parsed []byte
	var err error
	if len(p.Infobox) > 0 {
		if ib, err = json.Marshal(p.Infobox); err != nil {
			return err
		}
	}
	if p.Parsed != nil {
		if parsed, err = json.Marshal(p.Parsed); err != nil {
			return err
		}
	}
	wt, err := squash(p.Wikitext)
	if err != nil {
		return err
	}
	if _, err := in.insPage.Exec(p.PageID, p.Kind, p.Title,
		wt, string(ib), p.Lead, p.Plot, string(parsed)); err != nil {
		return fmt.Errorf("intermediate: page %d (%s): %w", p.PageID, p.Kind, err)
	}
	for _, l := range p.Links {
		t := strings.TrimSpace(l.Target)
		if t == "" {
			continue
		}
		if _, err := in.insLink.Exec(p.PageID, l.Field, t); err != nil {
			return fmt.Errorf("intermediate: link %d %s: %w", p.PageID, l.Field, err)
		}
	}
	in.n++
	// Commit periodically. One transaction across a million pages would hold the
	// whole import in memory and lose all of it on a failure.
	if in.n%50_000 == 0 {
		return in.commit()
	}
	return nil
}

func (in *Inter) begin() error {
	if in.tx != nil {
		return nil
	}
	tx, err := in.db.Begin()
	if err != nil {
		return err
	}
	in.tx = tx
	if in.insPage, err = tx.Prepare(
		`INSERT OR REPLACE INTO pages(page_id,kind,title,wikitext,infobox,lead,plot,parsed)
		 VALUES(?,?,?,?,?,?,?,?)`); err != nil {
		return err
	}
	in.insLink, err = tx.Prepare(
		`INSERT OR REPLACE INTO links(from_id,field,target) VALUES(?,?,?)`)
	return err
}

func (in *Inter) commit() error {
	if in.tx == nil {
		return nil
	}
	in.insPage.Close()
	in.insLink.Close()
	err := in.tx.Commit()
	in.tx, in.insPage, in.insLink = nil, nil, nil
	return err
}

// Flush commits whatever is pending.
func (in *Inter) Flush() error { return in.commit() }

// Counts reports how many pages of each kind were imported.
func (in *Inter) Counts() (map[string]int, error) {
	if err := in.commit(); err != nil {
		return nil, err
	}
	rows, err := in.db.Query(`SELECT kind, COUNT(*) FROM pages GROUP BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// ByTitle resolves a title to the page_id of a given kind, which is how export
// turns a stated link into an identity without ever comparing display strings.
func (in *Inter) ByTitle(title, kind string) (int, bool) {
	var id int
	err := in.rd().QueryRow(
		`SELECT page_id FROM pages WHERE title = ? AND kind = ?`, title, kind).Scan(&id)
	return id, err == nil
}

// Referrers returns the pages that link to a title. Incremental export uses this
// to find what a change made stale.
func (in *Inter) Referrers(target string) ([]int, error) {
	rows, err := in.rd().Query(`SELECT DISTINCT from_id FROM links WHERE target = ?`, target)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Each visits every page of a kind. Wikitext is skipped unless asked for: it is
// the bulk of the store and most passes do not need it.
func (in *Inter) Each(kind string, withWikitext bool, fn func(*Page) error) error {
	if err := in.commit(); err != nil {
		return err
	}
	cols := `page_id,title,infobox,lead,plot,parsed,''`
	if withWikitext {
		cols = `page_id,title,infobox,lead,plot,parsed,wikitext`
	}
	rows, err := in.db.Query(`SELECT `+cols+` FROM pages WHERE kind = ?`, kind)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p Page
		var ib, parsed, wt sql.NullString
		var err error
		if err := rows.Scan(&p.PageID, &p.Title, &ib, &p.Lead, &p.Plot, &parsed, &wt); err != nil {
			return err
		}
		p.Kind = kind
		if p.Wikitext, err = unsquash([]byte(wt.String)); err != nil {
			return err
		}
		if ib.String != "" {
			json.Unmarshal([]byte(ib.String), &p.Infobox)
		}
		if parsed.String != "" {
			p.Parsed = json.RawMessage(parsed.String)
		}
		if err := fn(&p); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Wikitext is 93% of this store's bytes and compresses about 3.5:1, which is the
// difference between an intermediate that fits beside everything else on the
// NVMe and one that does not. gzip because the rest of the project already uses
// it, and because its magic bytes make the blob self-describing: a store written
// before this change still reads.
var gzPool = sync.Pool{New: func() any { return gzip.NewWriter(nil) }}

func squash(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	var buf bytes.Buffer
	buf.Grow(len(s) / 3)
	zw := gzPool.Get().(*gzip.Writer)
	defer gzPool.Put(zw)
	zw.Reset(&buf)
	if _, err := io.WriteString(zw, s); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func unsquash(b []byte) (string, error) {
	if len(b) == 0 {
		return "", nil
	}
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		return string(b), nil // written before wikitext was compressed
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	return string(out), err
}

// EachPage replays every stored page as the dump presented it.
//
// Distinct by page_id, not one row per kind: an article claimed as both a film
// and a person is one page, and handing it to the recognisers twice would count
// it twice. The wikitext is what makes this a replay rather than an
// approximation — export runs the same parsers over the same bytes the dump
// carried, so a record built from the intermediate is the record extract would
// have built.
func (in *Inter) EachPage(fn func(dump.Page) error) error {
	if err := in.commit(); err != nil {
		return err
	}
	rows, err := in.db.Query(
		`SELECT page_id, title, MAX(wikitext) FROM pages GROUP BY page_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p dump.Page
		var wt []byte
		if err := rows.Scan(&p.ID, &p.Title, &wt); err != nil {
			return err
		}
		if p.Text, err = unsquash(wt); err != nil {
			return err
		}
		if err := fn(p); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Pages reports how many distinct pages EachPage will visit.
func (in *Inter) Pages() (int, error) {
	if err := in.commit(); err != nil {
		return 0, err
	}
	var n int
	err := in.rd().QueryRow(`SELECT COUNT(DISTINCT page_id) FROM pages`).Scan(&n)
	return n, err
}

// Replace makes the store's view of one page match the claims given, which is
// what a daily update needs and Put alone cannot do.
//
// Two things Put would get wrong:
//
//   - a page that stops qualifying. An infobox is removed, so a film is no
//     longer a film, and recognise claims nothing for it. Put writes nothing
//     and the old row survives — the store then states as fact something the
//     encyclopaedia no longer says.
//   - links that go away. A cast member removed from an infobox leaves a row in
//     the reference graph pointing at a credit that no longer exists, and the
//     graph is what export uses to decide what went stale.
//
// So the page's rows are reconciled rather than added to: kinds no longer
// claimed are dropped, and its links are rewritten wholesale.
func (in *Inter) Replace(pageID int, claims []*Page) error {
	if err := in.begin(); err != nil {
		return err
	}
	if _, err := in.tx.Exec(`DELETE FROM links WHERE from_id = ?`, pageID); err != nil {
		return fmt.Errorf("intermediate: clearing links for %d: %w", pageID, err)
	}
	kinds := make([]any, 0, len(claims)+1)
	kinds = append(kinds, pageID)
	holes := ""
	for _, c := range claims {
		if holes != "" {
			holes += ","
		}
		holes += "?"
		kinds = append(kinds, c.Kind)
	}
	q := `DELETE FROM pages WHERE page_id = ?`
	if holes != "" {
		q += ` AND kind NOT IN (` + holes + `)`
	}
	if _, err := in.tx.Exec(q, kinds...); err != nil {
		return fmt.Errorf("intermediate: dropping unclaimed kinds for %d: %w", pageID, err)
	}
	for _, c := range claims {
		if err := in.Put(c); err != nil {
			return err
		}
	}
	return nil
}

// Kinds reports which kinds a page is currently stored under.
func (in *Inter) Kinds(pageID int) ([]string, error) {
	rows, err := in.rd().Query(`SELECT kind FROM pages WHERE page_id = ?`, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
