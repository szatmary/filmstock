package filmstock

import (
	"database/sql"
	"testing"

	"github.com/szatmary/filmstock/internal/sqldrv"
)

func hashDB(t *testing.T, setup []string) (string, map[string]string) {
	t.Helper()
	h, err := sql.Open(sqldrv.Name, "file:"+t.TempDir()+"/x.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	for _, q := range setup {
		if _, err := h.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	total, tables, err := ContentHash(h)
	if err != nil {
		t.Fatal(err)
	}
	return total, tables
}

const epSchema = `CREATE TABLE television_episodes(id INTEGER PRIMARY KEY AUTOINCREMENT,
  series_id INTEGER, season INTEGER, number_in_season INTEGER, number_overall INTEGER,
  title TEXT, air_date TEXT, viewers REAL, prod_code TEXT)`

// The whole point: a patched consumer and a fresh build hold the same facts in
// different storage — different insert order, different AUTOINCREMENT ids,
// vacuumed or not — and must produce the same hash.
func TestSameFactsSameHash(t *testing.T) {
	a, _ := hashDB(t, []string{epSchema,
		`INSERT INTO television_episodes(series_id,season,number_in_season,number_overall,title,air_date,viewers)
		 VALUES (1,1,1,1,'Pilot','1999-01-10',3.5),(1,1,2,2,'Two','1999-01-17',3.1)`,
	})
	b, _ := hashDB(t, []string{epSchema,
		`INSERT INTO television_episodes(id,series_id,season,number_in_season,number_overall,title,air_date,viewers)
		 VALUES (901,1,1,2,2,'Two','1999-01-17',3.1)`, // reversed order, wild ids
		`INSERT INTO television_episodes(id,series_id,season,number_in_season,number_overall,title,air_date,viewers)
		 VALUES (77,1,1,1,1,'Pilot','1999-01-10',3.5)`,
		`VACUUM`,
	})
	if a != b {
		t.Fatalf("same facts, different hash:\n  %s\n  %s", a, b)
	}
}

// One changed fact changes the hash.
func TestChangedFactChangesHash(t *testing.T) {
	a, _ := hashDB(t, []string{epSchema,
		`INSERT INTO television_episodes(series_id,season,number_in_season,number_overall,title,air_date,viewers)
		 VALUES (1,1,1,1,'Pilot','1999-01-10',3.5)`})
	b, _ := hashDB(t, []string{epSchema,
		`INSERT INTO television_episodes(series_id,season,number_in_season,number_overall,title,air_date,viewers)
		 VALUES (1,1,1,1,'Pilot','1999-01-10',3.6)`})
	if a == b {
		t.Fatal("a changed viewership did not change the hash")
	}
}

// The serialiser must keep distinct stored values distinct. (A first version
// of this test inserted '1' into an INTEGER column and asserted it differed
// from 1 — but SQLite's column affinity coerces them to the same stored fact,
// so the same hash was correct and the test was wrong.) NULL, the empty
// string, and a literal "n" are three values no affinity can conflate.
func TestDistinctValuesStayDistinct(t *testing.T) {
	mk := func(title, year string) string {
		a, _ := hashDB(t, []string{
			`CREATE TABLE movies(id INTEGER PRIMARY KEY, title TEXT, year INTEGER,
			  release_date TEXT, director TEXT, producer TEXT, writer TEXT, starring TEXT,
			  music TEXT, distributor TEXT, country TEXT, language TEXT, genre TEXT,
			  runtime TEXT, budget TEXT, gross TEXT, wikipedia_url TEXT,
			  cover_image_url TEXT, cover_image_file TEXT, wiki_title TEXT)`,
			`INSERT INTO movies(id,title,year) VALUES (1,` + title + `,` + year + `)`})
		return a
	}
	hs := map[string]string{
		"null-title": mk("NULL", "1"), "empty-title": mk("''", "1"),
		"n-title": mk("'n'", "1"), "null-year": mk("'x'", "NULL"),
		"zero-year": mk("'x'", "0"),
	}
	seen := map[string]string{}
	for name, h := range hs {
		if prev, dup := seen[h]; dup {
			t.Errorf("%s and %s hashed identically", prev, name)
		}
		seen[h] = name
	}
}

// Credits hash through the person's link target, so the synthetic person row
// id never enters — two builds numbering people differently still agree.
func TestCreditsHashThroughWiki(t *testing.T) {
	peopleSchema := `CREATE TABLE people(id INTEGER PRIMARY KEY, page_id INTEGER,
	  qid INTEGER, name TEXT, wiki TEXT, image_url TEXT)`
	creditSchema := `CREATE TABLE credits(person_id INTEGER, work_id INTEGER,
	  work_type TEXT, role TEXT)`
	a, _ := hashDB(t, []string{peopleSchema, creditSchema,
		`INSERT INTO people VALUES (1,100,0,'A','A',''),(2,200,0,'B','B','')`,
		`INSERT INTO credits VALUES (1,9,'movie','Cast'),(2,9,'movie','Director')`})
	b, _ := hashDB(t, []string{peopleSchema, creditSchema,
		`INSERT INTO people VALUES (55,200,0,'B','B',''),(44,100,0,'A','A','')`,
		`INSERT INTO credits VALUES (55,9,'movie','Director'),(44,9,'movie','Cast')`})
	if a != b {
		t.Fatal("renumbered people changed the credits hash")
	}
}

// An unknown table is an error, never a guess.
func TestUnknownTableRefuses(t *testing.T) {
	h, _ := sql.Open(sqldrv.Name, "file:"+t.TempDir()+"/x.db")
	defer h.Close()
	h.Exec(`CREATE TABLE brand_new_thing(x)`)
	if _, _, err := ContentHash(h); err == nil {
		t.Fatal("hashed a table with no canonical query")
	}
}

// FTS shadow tables and derived tables are invisible: a consumer who rebuilt
// FTS locally must match a database that never had it.
func TestDerivedTablesAreInvisible(t *testing.T) {
	base := []string{epSchema,
		`INSERT INTO television_episodes(series_id,season,number_in_season,number_overall,title)
		 VALUES (1,1,1,1,'Pilot')`}
	a, _ := hashDB(t, base)
	b, _ := hashDB(t, append(append([]string{}, base...),
		`CREATE TABLE person_alias(wiki TEXT PRIMARY KEY, person_id INTEGER)`,
		`INSERT INTO person_alias VALUES ('X',1)`,
		`CREATE VIRTUAL TABLE movies_fts USING fts5(title)`,
		`INSERT INTO movies_fts VALUES ('noise')`))
	if a != b {
		t.Fatal("derived tables leaked into the content hash")
	}
}
