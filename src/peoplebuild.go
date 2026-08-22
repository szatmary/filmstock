package main

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// openWikidata opens the title→Q-id map read-only. Returns nil (people fall back
// to name identity) with a warning if the map is missing.
func openWikidata(path string) *sql.DB {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(os.Stderr, "warning: wikidata map %s not found — people keyed by name (no Q-id)\n", path)
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: open wikidata:", err)
		return nil
	}
	return db
}

// peopleSchema (re)creates the people, credits, alias and FTS tables. People are
// keyed by Wikidata Q-id when resolvable, so same-name individuals stay distinct.
const peopleSchema = `
DROP TABLE IF EXISTS people;
DROP TABLE IF EXISTS credits;
DROP TABLE IF EXISTS person_alias;
DROP TABLE IF EXISTS people_fts;
CREATE TABLE people(id INTEGER PRIMARY KEY, qid INTEGER, name TEXT NOT NULL, wiki TEXT);
CREATE INDEX idx_people_qid ON people(qid);
CREATE TABLE credits(person_id INTEGER, work_id INTEGER, work_type TEXT, role TEXT);
CREATE INDEX idx_credits_person ON credits(person_id);
CREATE INDEX idx_credits_work ON credits(work_id, work_type);
CREATE TABLE person_alias(wiki TEXT PRIMARY KEY, person_id INTEGER);
CREATE VIRTUAL TABLE people_fts USING fts5(name, content='people', content_rowid='id', tokenize='trigram');`

// personKey is the dedup identity, and it has exactly two tiers:
//
//	Q-id            — Wikidata resolved the linked article
//	wiki link target — the credit links to an article we could not resolve to a
//	                   Q-id (red link, or missing from wiki_qid). Still a STATED
//	                   identity: the editor named that exact article, and it
//	                   upgrades to a Q-id for free on a later extract.
//
// There is deliberately no third tier. A bare name with no link has no identity,
// and keying one by the normalized display name merges strangers — that is what
// made the two John Williamses one person. Such credits keep their name as a
// string on the work record and simply are not entities.
func personKey(qid int64, wiki string) (string, bool) {
	if qid > 0 {
		return "q" + strconv.FormatInt(qid, 10), true
	}
	if wiki != "" {
		return "w:" + normalize(wiki), true
	}
	return "", false
}

// peopleBuilder resolves Person → a canonical person row (by Q-id) and records
// credits + wiki aliases. Shared by the movie and television indexers so a person who is
// in a film AND a series collapses to one entity.
type peopleBuilder struct {
	title2qid map[string]int64 // full wiki_qid map, loaded once (in-memory)
	byKey     map[string]int64 // dedup key -> person id
	insPerson *sql.Stmt
	insCredit *sql.Stmt
	insAlias  *sql.Stmt
}

// loadPeopleQIDs builds the link-target → Q-id map from the PERSON RECORDS
// rather than from wikidata.db.
//
// This is what makes `index` pure: extract already resolved every identity and
// baked it into the records, so indexing needs no dump, no resolver cache, and
// no ordering constraint. It is also far smaller — one entry per person actually
// credited (~250k) instead of the entire 10M-row wiki_qid table.
func loadPeopleQIDs(recordsDir string) (map[string]int64, error) {
	m := map[string]int64{}
	err := walkRecords(recordsDir, kindPerson, func(p string) error {
		var pr PersonRecord
		if readRecordJSON(p, &pr) == nil && pr.Wiki != "" {
			m[pr.Wiki] = pr.QID
		}
		return nil
	})
	return m, err
}

func newPeopleBuilder(tx *sql.Tx, title2qid map[string]int64) (*peopleBuilder, error) {
	b := &peopleBuilder{byKey: map[string]int64{}, title2qid: title2qid}
	if b.title2qid == nil {
		b.title2qid = map[string]int64{}
	}
	var err error
	// Load any existing people (so the television pass reuses movie-pass person ids).
	rows, err := tx.Query(`SELECT id, qid, name, wiki FROM people`)
	if err == nil {
		for rows.Next() {
			var id, qid sql.NullInt64
			var name, wiki sql.NullString
			if rows.Scan(&id, &qid, &name, &wiki) == nil {
				if k, ok := personKey(qid.Int64, wiki.String); ok {
					b.byKey[k] = id.Int64
				}
			}
		}
		rows.Close()
	}
	if b.insPerson, err = tx.Prepare(`INSERT INTO people(qid,name,wiki) VALUES(?,?,?)`); err != nil {
		return nil, err
	}
	if b.insCredit, err = tx.Prepare(`INSERT INTO credits(person_id,work_id,work_type,role) VALUES(?,?,?,?)`); err != nil {
		return nil, err
	}
	if b.insAlias, err = tx.Prepare(`INSERT OR IGNORE INTO person_alias(wiki,person_id) VALUES(?,?)`); err != nil {
		return nil, err
	}
	return b, nil
}

// qidOf resolves a wiki title to a Q-id (0 if none) via the in-memory map.
func (b *peopleBuilder) qidOf(title string) int64 {
	if title == "" {
		return 0
	}
	return b.title2qid[title]
}

// person resolves a Person to a canonical person id (creating the row if new).
func (b *peopleBuilder) person(p Person) (int64, bool) {
	name := strings.TrimSpace(p.Name)
	if name == "" || len(name) > 120 {
		return 0, false
	}
	// Resolve ONLY through the link target. Looking the bare name up as an article
	// title would attach every unlinked "John Smith" to whichever person happens to
	// hold that title — an invented identity, and silently wrong.
	qid := b.qidOf(p.Wiki)
	key, ok := personKey(qid, p.Wiki)
	if !ok {
		return 0, false
	}
	id, ok := b.byKey[key]
	if !ok {
		var q interface{}
		if qid > 0 {
			q = qid
		}
		var w interface{}
		if p.Wiki != "" {
			w = p.Wiki
		}
		res, err := b.insPerson.Exec(q, name, w)
		if err != nil {
			return 0, false
		}
		id, _ = res.LastInsertId()
		b.byKey[key] = id
	}
	if p.Wiki != "" {
		b.insAlias.Exec(p.Wiki, id)
	}
	return id, true
}

// credit records one credited role for a work, deduped per (person,role,work).
func (b *peopleBuilder) credit(seen map[string]bool, people []Person, workID int, workType, role string) {
	for _, p := range people {
		id, ok := b.person(p)
		if !ok {
			continue
		}
		k := role + "\x00" + strconv.FormatInt(id, 10)
		if seen[k] {
			continue
		}
		seen[k] = true
		b.insCredit.Exec(id, workID, workType, role)
	}
}

// joinP joins person display names with " · " for the flat display columns.
func joinP(v []Person) string {
	names := make([]string, len(v))
	for i, p := range v {
		names[i] = p.Name
	}
	return strings.Join(names, " · ")
}
