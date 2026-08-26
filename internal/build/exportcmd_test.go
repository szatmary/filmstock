package build

import (
	"testing"

	"github.com/szatmary/filmstock/internal/dump"
)

// Import and export are only worth splitting if the page export replays is the
// page import saw. Everything downstream — the parsers, the collector, the
// record writer — is shared, so this round trip is the whole of what could
// differ between a cheap export and a full extract.
func TestPageSurvivesTheRoundTrip(t *testing.T) {
	in := openInter(t)
	for _, p := range televisionPages {
		for _, ip := range recognise(p, true) {
			if err := in.Put(ip); err != nil {
				t.Fatal(err)
			}
		}
	}
	in.Flush()

	back := map[int]dump.Page{}
	if err := in.EachPage(func(p dump.Page) error { back[p.ID] = p; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, want := range televisionPages {
		got, ok := back[want.ID]
		if !ok {
			t.Errorf("%s: not replayed", want.Title)
			continue
		}
		if got.Title != want.Title {
			t.Errorf("page %d: title %q, want %q", want.ID, got.Title, want.Title)
		}
		if got.Text != want.Text {
			t.Errorf("page %d (%s): wikitext differs — export would parse different bytes",
				want.ID, want.Title)
		}
		// Only main-namespace pages are ever stored, so a replayed page is
		// namespace 0 by construction. The handlers drop anything else, and a
		// non-zero value here would silently discard the entire corpus.
		if got.NS != 0 {
			t.Errorf("page %d: NS %d — handlers would drop it", want.ID, got.NS)
		}
	}
}

// A page claimed under two kinds is one page. Replaying it once per kind would
// hand it to the recognisers twice and double every credit it states.
func TestRoundTripVisitsEachPageOnce(t *testing.T) {
	in := openInter(t)
	p := dump.Page{ID: 42, Title: "Clint Eastwood", Text: "{{Infobox person\n| name = x\n}}"}
	for _, kind := range []string{"people", "movies"} {
		if err := in.Put(&Page{PageID: p.ID, Kind: kind, Title: p.Title, Wikitext: p.Text}); err != nil {
			t.Fatal(err)
		}
	}
	in.Flush()
	var n int
	in.EachPage(func(dump.Page) error { n++; return nil })
	if n != 1 {
		t.Errorf("visited %d times, want 1 — credits would be counted twice", n)
	}
}
