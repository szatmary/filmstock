package build

import (
	"os"
	"testing"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/dump"
)

// Anime articles use {{Infobox animanga}}, not {{Infobox television}}, so the
// franchise article was never claimed and the show was simply absent — Naruto,
// One-Punch Man, Code Geass, Samurai Champloo, Cardcaptor Sakura. 21 of 51
// well-known anime were missing.
func TestAnimeFranchiseArticleIsASeries(t *testing.T) {
	b, err := os.ReadFile("testdata/anime-code-geass.wikitext")
	if err != nil {
		t.Fatal(err)
	}
	msgs := parseTelevisionPage(dump.Page{ID: 5, Title: "Code Geass", Text: string(b)})
	var s *filmstock.TelevisionSeries
	for _, m := range msgs {
		if m.series != nil {
			s = m.series
		}
	}
	if s == nil {
		t.Fatal("the article was not recognised as a series")
	}
	// The record is the ARTICLE — Code Geass — not the adaptation block's own
	// title, because the page_id is the identity.
	if s.Title != "Code Geass" {
		t.Errorf("title %q, want %q", s.Title, "Code Geass")
	}
	if s.PageID != 5 {
		t.Errorf("page_id %d, want 5", s.PageID)
	}
	if len(s.Director) == 0 || s.Director[0].Name != "Gorō Taniguchi" {
		t.Errorf("director %v, want Gorō Taniguchi", personNames(s.Director))
	}
	if n := filmstock.Names(s.Network); len(n) == 0 {
		t.Error("no network")
	}
	if s.Overview == "" {
		t.Error("no overview")
	}
}

// /Video also describes OVAs and films. Those are not broadcast series.
func TestOnlyBroadcastFormsAreSeries(t *testing.T) {
	for _, typ := range []string{"ova", "film", "oav", "movie"} {
		text := "{{Infobox animanga/Video\n| type = " + typ + "\n| title = X\n| studio = Y\n}}"
		if _, ok := FindAnimangaSeries(text); ok {
			t.Errorf("type=%q was read as a television series", typ)
		}
	}
	for _, typ := range []string{"tv series", "TV Series", "ona"} {
		text := "{{Infobox animanga/Video\n| type = " + typ + "\n| title = X\n}}"
		if _, ok := FindAnimangaSeries(text); !ok {
			t.Errorf("type=%q was not read as a television series", typ)
		}
	}
}

// The field names differ; the values do not, so every existing parser reads
// them unchanged.
func TestAnimangaFieldsAreTranslated(t *testing.T) {
	got, ok := FindAnimangaSeries(`{{Infobox animanga/Video
| type = tv series
| first = {{Start date|2006|10|5}}
| last = {{End date|2007|7|29}}
| episodes = 25
| studio = [[Sunrise (company)|Sunrise]]
| episode_list = List of Code Geass episodes
}}`)
	if !ok {
		t.Fatal("not recognised")
	}
	for k, want := range map[string]string{
		"first_aired":   "{{Start date|2006|10|5}}",
		"last_aired":    "{{End date|2007|7|29}}",
		"num_episodes":  "25",
		"list_episodes": "List of Code Geass episodes",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if got["company"] == "" {
		t.Error("studio did not become company")
	}
}

// A page carrying both infoboxes is television: that is the more specific claim.
func TestTelevisionInfoboxWins(t *testing.T) {
	p := dump.Page{ID: 1, Title: "X", Text: `{{Infobox television
| starring = [[Someone]]
}}
{{Infobox animanga/Video
| type = tv series
| studio = Y
}}`}
	var n int
	for _, m := range parseTelevisionPage(p) {
		if m.series != nil {
			n++
		}
	}
	if n != 1 {
		t.Errorf("produced %d series records, want 1", n)
	}
}
