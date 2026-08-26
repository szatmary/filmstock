package build

import (
	"database/sql"
	"testing"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/dump"
	_ "modernc.org/sqlite"
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
// queries. Empty is fine: these people resolve to nothing either way.
func emptyCache(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/resolver.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE wiki_qid(title TEXT, qid INTEGER, page_id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	return path
}

func newWorkTestWriter(t *testing.T) *recordWriter {
	t.Helper()
	return &recordWriter{
		people:    map[string]*filmstock.PersonRecord{},
		bios:      map[string]*filmstock.PersonBio{},
		bioPage:   map[string]int{},
		workTitle: map[string]bool{},
		store:     newStoreWriter(t.TempDir()),
	}
}

func TestAWorkIsNotAPerson(t *testing.T) {
	w := newWorkTestWriter(t)
	// A series whose own infobox credits itself.
	w.noteWork("Saul of the Mole Men")
	w.notePeople([]filmstock.Person{
		{Name: "Saul of the Mole Men", Wiki: "Saul of the Mole Men"},
		{Name: "Craig Anton", Wiki: "Craig Anton"},
	})
	_, _, total, _ := w.flushPeople(emptyCache(t))
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
	w.notePeople([]filmstock.Person{{Name: "Ed Wood", Wiki: "Ed Wood"}})
	_, _, total, _ := w.flushPeople(emptyCache(t))
	if total != 1 {
		t.Errorf("kept %d, want 1 — a page with a biography is a person", total)
	}
}
