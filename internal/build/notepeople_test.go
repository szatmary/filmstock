package build

import (
	"reflect"
	"testing"

	"github.com/szatmary/filmstock/internal/record"
)

// Every []Person field on a record type must reach notePeople.
//
// Missing one does not lose those people visibly — the index still builds
// credits straight from the record, so they appear in the database as normal.
// What they lose is Q-id resolution, because that happens only for people who
// were noted. The result is an index row with a null qid and no record in the
// store, and a person keyed by a hash of their article path instead of a stable
// identity.
//
// That is not hypothetical: handleSeries listed Creator, Starring and Composer
// while TelevisionSeries had grown eight more person fields, and 8,407 people
// ended up mis-keyed with their Q-ids sitting unread in the resolver cache.
// Counting fields by reflection means adding a twelfth field fails here rather
// than silently repeating it.
func TestEveryPersonFieldIsNoted(t *testing.T) {
	for _, tc := range []struct {
		name string
		// build a value with a uniquely-named person in every []Person field
		build func() (any, int)
		// run the REAL handler, not a restatement of it
		note func(*recordWriter, any)
	}{
		{
			name: "Movie",
			build: func() (any, int) {
				var m record.Movie
				n := fillPeople(reflect.ValueOf(&m).Elem())
				return &m, n
			},
			note: func(w *recordWriter, v any) { w.handleFilmRecord(v.(*record.Movie), 1) },
		},
		{
			name: "TelevisionSeries",
			build: func() (any, int) {
				var s record.TelevisionSeries
				n := fillPeople(reflect.ValueOf(&s).Elem())
				return &s, n
			},
			note: func(w *recordWriter, v any) { w.handleSeries(v.(*record.TelevisionSeries)) },
		},
		{
			name: "Event",
			build: func() (any, int) {
				var e record.Event
				n := fillPeople(reflect.ValueOf(&e).Elem())
				return &e, n
			},
			note: func(w *recordWriter, v any) { w.handleEventRecord(v.(*record.Event)) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, want := tc.build()
			if want == 0 {
				t.Fatalf("%s has no []Person fields; the test is not exercising anything", tc.name)
			}
			w := &recordWriter{
				people: map[string]*record.PersonRecord{},
				store:  newMemSink(),
			}
			tc.note(w, rec)
			if got := len(w.people); got != want {
				missing := missingFields(reflect.ValueOf(rec).Elem(), w.people)
				t.Errorf("%s: noted %d of %d []Person fields; not reaching notePeople: %v",
					tc.name, got, want, missing)
			}
		})
	}
}

// fillPeople puts one person, named after the field, into every []Person field.
// Returns how many it filled.
func fillPeople(v reflect.Value) int {
	t := v.Type()
	var n int
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Type != reflect.TypeOf([]record.Person(nil)) {
			continue
		}
		v.Field(i).Set(reflect.ValueOf([]record.Person{{
			Name: f.Name,
			Wiki: "Wiki_" + f.Name, // Wiki must be non-empty or notePeople skips it
		}}))
		n++
	}
	return n
}

func missingFields(v reflect.Value, noted map[string]*record.PersonRecord) []string {
	t := v.Type()
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Type != reflect.TypeOf([]record.Person(nil)) {
			continue
		}
		if _, ok := noted["Wiki_"+f.Name]; !ok {
			out = append(out, f.Name)
		}
	}
	return out
}

// A person with no link has no stated identity and must not become an entity:
// attaching a bare "John Smith" to whoever holds that article title is an
// invented identity.
func TestUnlinkedPeopleAreNotNoted(t *testing.T) {
	w := &recordWriter{people: map[string]*record.PersonRecord{}}
	w.notePeople([]record.Person{
		{Name: "Linked", Wiki: "Linked_Person"},
		{Name: "Unlinked"},
	})
	if len(w.people) != 1 {
		t.Fatalf("noted %d people, want 1", len(w.people))
	}
	if _, ok := w.people["Linked_Person"]; !ok {
		t.Error("the linked person was not noted")
	}
}

func TestNotePeopleDeduplicatesByLink(t *testing.T) {
	w := &recordWriter{people: map[string]*record.PersonRecord{}}
	for range 3 {
		w.notePeople([]record.Person{{Name: "Same", Wiki: "Same_Person"}})
	}
	if len(w.people) != 1 {
		t.Errorf("noted %d, want 1 — the same link is one person", len(w.people))
	}
}
