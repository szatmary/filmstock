package build

import (
	"encoding/json"
	"testing"

	"github.com/szatmary/filmstock"
)

// Identity is the enwiki page_id for every kind of record, people included.
// A person keyed on anything else is a special case, and the special case is
// what produced 85,523 people keyed by a hash of their article path.
func TestIdentityIsPageIDForEveryKind(t *testing.T) {
	for _, tc := range []struct {
		kind string
		rec  any
		want int64
	}{
		{filmstock.KindMovie, filmstock.Movie{PageID: 51563, Title: "Heat"}, 51563},
		{filmstock.KindEvent, filmstock.Event{PageID: 771, Title: "68th Academy Awards"}, 771},
		{filmstock.KindTelevision, filmstock.TelevisionSeries{PageID: 9182, Title: "Lost"}, 9182},
		{filmstock.KindPerson, filmstock.PersonRecord{PageID: 1037346, QID: 5272886, Name: "Dick Enberg"}, 1037346},
	} {
		b, err := json.Marshal(tc.rec)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := identityOf(tc.kind, b)
		if !ok || got != tc.want {
			t.Errorf("%s: identity = %d (ok=%v), want %d", tc.kind, got, ok, tc.want)
		}
	}
}

// A Q-id must not be used as the key even when present: two identity schemes for
// one kind of record is how a person ends up stored twice.
func TestQIDIsNotThePersonKey(t *testing.T) {
	b, _ := json.Marshal(filmstock.PersonRecord{PageID: 1037346, QID: 5272886, Name: "Dick Enberg"})
	got, _ := identityOf(filmstock.KindPerson, b)
	if got == 5272886 {
		t.Fatal("keyed by Q-id; the key must be the page_id")
	}
	if got != 1037346 {
		t.Fatalf("identity = %d, want the page_id 1037346", got)
	}
}

// A credit whose link target has no article gets no identity, and so no record.
//
// It used to get one: a hash of the link target, the single non-canonical
// identity in the database. That hash put two unrelated people in the same
// record, and it would have changed the day either article was created. The
// credit itself is not lost — it stands on the work that states it, and the
// index still builds a searchable person row and their credits from there.
func TestPersonWithNoArticleHasNoIdentity(t *testing.T) {
	b, _ := json.Marshal(filmstock.PersonRecord{Wiki: "Some_Uncredited_Extra", Name: "Extra"})
	if got, ok := identityOf(filmstock.KindPerson, b); ok {
		t.Errorf("identity = %d; a link target alone is a display string, not an identity", got)
	}
}

// A person with an article is keyed by its page_id, like every other record.
func TestPersonWithAnArticleIsKeyedByPageID(t *testing.T) {
	b, _ := json.Marshal(filmstock.PersonRecord{PageID: 4242, Wiki: "Real Person", Name: "Real Person"})
	got, ok := identityOf(filmstock.KindPerson, b)
	if !ok {
		t.Fatal("a person with an article must have an identity")
	}
	if got != 4242 {
		t.Errorf("identity = %d, want 4242", got)
	}
}

// No link and no article is not an entity at all.
func TestPersonWithNothingHasNoIdentity(t *testing.T) {
	b, _ := json.Marshal(filmstock.PersonRecord{Name: "Just A Name"})
	if _, ok := identityOf(filmstock.KindPerson, b); ok {
		t.Error("a bare display name must not become an identity")
	}
}
