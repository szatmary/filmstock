package build

import (
	"encoding/json"
	"testing"
)

// Encoded JSON key order is not guaranteed stable, so comparing bytes alone
// would report every record as changed and the tool would be useless.
func TestReorderedFieldsAreNotADifference(t *testing.T) {
	a := []byte(`{"title":"Heat","year":1995,"cast":["Pacino","De Niro"]}`)
	b := []byte(`{"year":1995,"cast":["Pacino","De Niro"],"title":"Heat"}`)
	if got := differingFields(a, b); len(got) != 0 {
		t.Errorf("reported %v for a reordering", got)
	}
}

// Nested maps too — a nested reordering is the common case, since records carry
// maps of infobox parameters.
func TestNestedReorderingIsNotADifference(t *testing.T) {
	a := []byte(`{"raw":{"director":"Mann","starring":"Pacino"}}`)
	b := []byte(`{"raw":{"starring":"Pacino","director":"Mann"}}`)
	if got := differingFields(a, b); len(got) != 0 {
		t.Errorf("reported %v for a nested reordering", got)
	}
}

// Array order IS meaningful: cast order is billing order.
func TestArrayOrderIsADifference(t *testing.T) {
	a := []byte(`{"cast":["Pacino","De Niro"]}`)
	b := []byte(`{"cast":["De Niro","Pacino"]}`)
	if got := differingFields(a, b); len(got) != 1 || got[0] != "cast" {
		t.Errorf("got %v, want [cast] — billing order is meaningful", got)
	}
}

func TestNamesTheFieldsThatDiffer(t *testing.T) {
	a := []byte(`{"title":"Heat","year":1995,"gone":1}`)
	b := []byte(`{"title":"Heat","year":1996,"new":2}`)
	got := differingFields(a, b)
	want := map[string]bool{"year": true, "gone (removed)": true, "new (added)": true}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected %q in %v", g, got)
		}
	}
}

// Numbers that differ only in encoding are not a change: 1995 and 1995.0 decode
// to the same float, and a record round-tripped through a different marshaller
// would otherwise show as differing everywhere.
func TestEquivalentNumbersAreNotADifference(t *testing.T) {
	var a, b json.RawMessage = []byte(`{"v":1995}`), []byte(`{"v":1995.0}`)
	if got := differingFields(a, b); len(got) != 0 {
		t.Errorf("reported %v", got)
	}
}
