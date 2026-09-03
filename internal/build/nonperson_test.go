package build

import (
	"database/sql"
	"testing"

	"github.com/szatmary/filmstock/internal/dump"
	"github.com/szatmary/filmstock/internal/record"
	"github.com/szatmary/filmstock/internal/sqldrv"
)

// A work is not a person. Infoboxes link works into credit fields — "Saul of
// the Mole Men" lists itself under starring, and 60 Minutes, The Flintstones
// and Annie Hall are all credited somewhere as people. 469 such credit targets
// across the corpus.
//
// Only a pass that has seen the whole encyclopaedia can tell: recognising that
// a credit target is itself a film needs the film's article, which a streaming
// pass has no reason to be holding when it reads the credit.
// flushPeople reads the resolver cache, so the test needs one with the table it
// queries, holding whichever people are supposed to have articles. Anyone absent
// has no page_id and therefore no record, which is the point.
func cacheWith(t *testing.T, people map[string]int) string {
	t.Helper()
	path := t.TempDir() + "/resolver.db"
	db, err := sql.Open(sqldrv.Name, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE wiki_qid(title TEXT, qid INTEGER, page_id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for title, pid := range people {
		if _, err := db.Exec(`INSERT INTO wiki_qid VALUES(?,?,?)`, title, pid*10, pid); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func newWorkTestWriter(t *testing.T) *recordWriter {
	t.Helper()
	return &recordWriter{
		people:    map[string]*record.PersonRecord{},
		bios:      map[string]*record.PersonBio{},
		bioPage:   map[string]int{},
		workTitle: map[string]bool{},
		store:     newMemSink(),
	}
}

func TestAWorkIsNotAPerson(t *testing.T) {
	w := newWorkTestWriter(t)
	// A series whose own infobox credits itself.
	w.noteWork("Saul of the Mole Men")
	w.notePeople([]record.Person{
		{Name: "Saul of the Mole Men", Wiki: "Saul of the Mole Men"},
		{Name: "Craig Anton", Wiki: "Craig Anton"},
	})
	cache := cacheWith(t, map[string]int{
		"Craig Anton":          700,
		"Saul of the Mole Men": 800, // it HAS an article; it is just not a person
	})
	_, _, total, _ := w.flushPeople(cache)
	if total != 1 {
		t.Errorf("kept %d people, want 1 — the series was recorded as a person", total)
	}
	if _, ok := w.people["Craig Anton"]; !ok {
		t.Error("dropped the real person")
	}
}

// A page claimed as both a film and a biography is a person the film parser
// mistook for a film. The biography is the better evidence.
func TestAPageThatIsAlsoABiographyStaysAPerson(t *testing.T) {
	w := newWorkTestWriter(t)
	w.noteWork("Ed Wood")
	w.handlePerson(dump.Page{ID: 1, Title: "Ed Wood", Text: `{{Infobox person
| name = Ed Wood
| birth_date = {{birth date|1924|10|10}}
| occupation = Filmmaker
}}`})
	w.notePeople([]record.Person{{Name: "Ed Wood", Wiki: "Ed Wood"}})
	_, _, total, _ := w.flushPeople(cacheWith(t, map[string]int{"Ed Wood": 900}))
	if total != 1 {
		t.Errorf("kept %d, want 1 — a page with a biography is a person", total)
	}
}

// A credit whose link target has no article gets no record. The credit is not
// lost: it stands on the work that states it, which already carries the name
// and the link target, and the index builds a searchable person row from there.
func TestRedlinkedCreditGetsNoRecord(t *testing.T) {
	w := newWorkTestWriter(t)
	w.notePeople([]record.Person{
		{Name: "Real Person", Wiki: "Real Person"},
		{Name: "Nobody Wrote This One", Wiki: "Nobody Wrote This One"},
	})
	_, _, total, noIdentity := w.flushPeople(cacheWith(t, map[string]int{"Real Person": 500}))
	if total != 1 {
		t.Errorf("wrote %d records, want 1", total)
	}
	if noIdentity != 1 {
		t.Errorf("counted %d credits without an article, want 1", noIdentity)
	}
}

// And it relinks by itself: the credit stores the link TARGET, so once the
// article exists its page_id resolves straight from the dump — no rekeying, no
// migration, nothing to reconcile.
func TestCreditGainsARecordWhenTheArticleAppears(t *testing.T) {
	const wiki = "Later Somebody Wrote It"
	before := newWorkTestWriter(t)
	before.notePeople([]record.Person{{Name: "X", Wiki: wiki}})
	if _, _, total, _ := before.flushPeople(cacheWith(t, nil)); total != 0 {
		t.Fatalf("wrote %d records before the article existed", total)
	}

	after := newWorkTestWriter(t)
	after.notePeople([]record.Person{{Name: "X", Wiki: wiki}})
	// The article now exists; its page_id comes from the dump, via bioPage.
	after.bioPage[wiki] = 6161
	_, _, total, noIdentity := after.flushPeople(cacheWith(t, nil))
	if total != 1 || noIdentity != 0 {
		t.Errorf("wrote %d records (%d without an article), want 1 and 0", total, noIdentity)
	}
	if got := after.people[wiki].PageID; got != 6161 {
		t.Errorf("page_id %d, want 6161 — taken from the dump, not a lookup", got)
	}
}
