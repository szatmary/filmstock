package build

import (
	"os"
	"testing"

	"github.com/szatmary/filmstock/internal/dump"
)

// Real season articles. The per-season cast is the whole reason seasons became
// first class, so it is worth asserting against the encyclopaedia rather than
// against a fixture I wrote to match my own parser.
func TestRealSeasonArticles(t *testing.T) {
	cases := []struct {
		file, title    string
		id, num, eps   int
		first, last    string
		network, image string
		starring       []string
	}{{
		file: "season-er-season-1", title: "ER season 1", id: 101, num: 1, eps: 25,
		first: "1994-09-19", last: "1995-05-18", network: "NBC",
		image: "DVD Season 1 Cover (EUA).jpg",
		// Six, not everyone who ever appeared in fifteen years of ER. That
		// distinction is the modelling error this fixes: the series-level cast
		// list asserts a fifteen-season run for all of them.
		starring: []string{"Anthony Edwards", "George Clooney", "Sherry Stringfield",
			"Noah Wyle", "Julianna Margulies", "Eriq La Salle"},
	}, {
		// A split season, whose infobox states the span across both parts.
		file: "season-breaking-bad-season-5", title: "Breaking Bad season 5", id: 102,
		num: 5, eps: 16, first: "2012-07-15", last: "2013-09-29", network: "AMC",
		image: "Breaking Bad season five part i and ii dvd.png",
		starring: []string{"Bryan Cranston", "Anna Gunn", "Aaron Paul", "Dean Norris",
			"Betsy Brandt", "RJ Mitte", "Bob Odenkirk", "Jonathan Banks",
			"Laura Fraser", "Jesse Plemons"},
	}, {
		// No starring parameter at all. Empty is the correct answer here, and
		// saying so keeps a future "fix" from inventing one.
		file: "season-the-simpsons-season-8", title: "The Simpsons season 8", id: 103,
		num: 8, eps: 25, first: "1996-10-27", last: "1997-05-18", network: "Fox",
		image: "The Simpsons season 8.jpg", starring: nil,
	}}

	for _, c := range cases {
		b, err := os.ReadFile("testdata/" + c.file + ".wikitext")
		if err != nil {
			t.Fatal(err)
		}
		msgs := parseTelevisionPage(dump.Page{ID: c.id, Title: c.title, Text: string(b)})
		if len(msgs) != 1 || msgs[0].seasonMeta == nil {
			t.Errorf("%s: got %d messages, want one carrying season metadata", c.title, len(msgs))
			continue
		}
		s := msgs[0].seasonMeta
		if s.PageID != c.id {
			t.Errorf("%s: page_id %d, want %d — a season must be addressable", c.title, s.PageID, c.id)
		}
		if s.Season != c.num {
			t.Errorf("%s: season %d, want %d", c.title, s.Season, c.num)
		}
		if len(msgs[0].eps) != c.eps {
			t.Errorf("%s: %d episodes, want %d", c.title, len(msgs[0].eps), c.eps)
		}
		if s.FirstAired != c.first || s.LastAired != c.last {
			t.Errorf("%s: %s..%s, want %s..%s", c.title, s.FirstAired, s.LastAired, c.first, c.last)
		}
		if s.Network != c.network {
			t.Errorf("%s: network %q, want %q", c.title, s.Network, c.network)
		}
		if s.Image != c.image {
			t.Errorf("%s: image %q, want %q", c.title, s.Image, c.image)
		}
		var got []string
		for _, p := range s.Starring {
			got = append(got, p.Name)
		}
		if len(got) != len(c.starring) {
			t.Errorf("%s: %d starring, want %d: %v", c.title, len(got), len(c.starring), got)
			continue
		}
		for i := range got {
			if got[i] != c.starring[i] {
				t.Errorf("%s: starring[%d] = %q, want %q", c.title, i, got[i], c.starring[i])
			}
		}
	}
}
