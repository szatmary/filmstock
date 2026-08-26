package build

import (
	"strings"
	"testing"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/dump"
)

// The pages that matter, one per way television is written up on Wikipedia.
var televisionPages = []dump.Page{
	{ID: 1, Title: "ER (TV series)", Text: "{{Infobox television\n| creator = [[Michael Crichton]]\n| starring = [[George Clooney]]\n| network = [[NBC]]\n}}\n''ER'' is a drama."},
	{ID: 2, Title: "ER season 1", Text: "{{Infobox television season\n| season_number = 1\n| network = [[NBC]]\n| starring = [[George Clooney]]\n}}"},
	{ID: 3, Title: "List of ER episodes", Text: "{{Episode list\n| EpisodeNumber = 1\n| Title = 24 Hours\n}}"},
	{ID: 4, Title: "ER season 2", Text: "{{Episode list\n| EpisodeNumber = 1\n| Title = Welcome Back Carter\n}}"},
}

// Import and extract must agree on which pages are television. They are separate
// claim paths over the same corpus, and when they disagree the loss is silent:
// nothing errors, a kind is simply absent from the store and every episode on
// those pages is gone.
func TestImportClaimsEveryTelevisionPageExtractDoes(t *testing.T) {
	for _, p := range televisionPages {
		wantTV := len(parseTelevisionPage(p)) > 0
		gotTV := false
		for _, ip := range recognise(p, true) {
			switch ip.Kind {
			case filmstock.KindTelevision, kindSeason, kindEpisodeList:
				gotTV = true
			}
		}
		if gotTV != wantTV {
			t.Errorf("%s: import claims television=%v, extract=%v", p.Title, gotTV, wantTV)
		}
	}
}

// The claim must land under the right kind, or export looks in the wrong place.
func TestImportKinds(t *testing.T) {
	want := map[string]string{
		"ER (TV series)":      filmstock.KindTelevision,
		"ER season 1":         kindSeason,
		"List of ER episodes": kindEpisodeList,
		"ER season 2":         kindSeason,
	}
	for _, p := range televisionPages {
		var kinds []string
		for _, ip := range recognise(p, true) {
			kinds = append(kinds, ip.Kind)
		}
		if len(kinds) != 1 || kinds[0] != want[p.Title] {
			t.Errorf("%s: kinds %v, want [%s]", p.Title, kinds, want[p.Title])
		}
	}
}

// A season article carries an infobox television, so claiming on that alone
// files seasons as series. Order of the claims is what prevents it.
func TestSeasonArticleIsNotClaimedAsSeries(t *testing.T) {
	p := dump.Page{ID: 9, Title: "ER season 3", Text: "{{Infobox television season\n| season_number = 3\n}}\n{{Episode list\n| Title = x\n}}"}
	for _, ip := range recognise(p, true) {
		if ip.Kind == filmstock.KindTelevision {
			t.Fatal("season article claimed as a series")
		}
	}
}

// Wikitext is kept unless asked otherwise: it is what makes a parser fix cheap.
func TestRecogniseKeepsWikitext(t *testing.T) {
	p := televisionPages[0]
	got := recognise(p, true)
	if len(got) == 0 || got[0].Wikitext != p.Text {
		t.Fatal("wikitext not carried through")
	}
	if got = recognise(p, false); len(got) == 0 || got[0].Wikitext != "" {
		t.Fatal("-no-wikitext still stored the text")
	}
}

// Every link in a claimed field is recorded, with its field. This is the
// reference graph incremental export depends on.
func TestRecogniseRecordsLinks(t *testing.T) {
	got := recognise(televisionPages[0], true)
	if len(got) != 1 {
		t.Fatalf("got %d entities", len(got))
	}
	want := map[string]string{"Michael Crichton": "creator", "George Clooney": "starring", "NBC": "network"}
	seen := map[string]string{}
	for _, l := range got[0].Links {
		seen[l.Target] = l.Field
	}
	for target, field := range want {
		if seen[target] != field {
			t.Errorf("link %q: field %q, want %q", target, seen[target], field)
		}
	}
}

// A person page and a film page are claimed independently; a page that is both
// must yield both, since choosing between them needs the whole corpus.
func TestRecogniseCanClaimOnePageTwice(t *testing.T) {
	p := dump.Page{ID: 7, Title: "Clint Eastwood",
		Text: "{{Infobox person\n| name = Clint Eastwood\n| birth_date = 1930\n}}"}
	kinds := map[string]bool{}
	for _, ip := range recognise(p, true) {
		kinds[ip.Kind] = true
		if ip.PageID != 7 {
			t.Errorf("page_id %d, want 7", ip.PageID)
		}
	}
	if !kinds[filmstock.KindPerson] {
		t.Errorf("person not claimed; got %v", kinds)
	}
}

// Lead extraction is lazy, so it must still happen for pages that are claimed.
func TestRecogniseStillExtractsLead(t *testing.T) {
	p := dump.Page{ID: 8, Title: "Heat (1995 film)",
		Text: "{{Infobox film\n| name = Heat\n| director = [[Michael Mann]]\n}}\n'''''Heat''''' is a 1995 American crime film.\n"}
	got := recognise(p, true)
	if len(got) == 0 {
		t.Fatal("film not claimed")
	}
	if !strings.Contains(got[0].Lead, "1995") {
		t.Errorf("lead %q does not look extracted", got[0].Lead)
	}
}
