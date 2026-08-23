package build

import (
	"testing"

	"github.com/szatmary/filmstock/internal/dump"
)

const ridley = `{{Infobox person
| name        = Ridley Scott
| image       = Ridley Scott by Gage Skidmore.jpg
| birth_name  = Ridley Scott
| birth_date  = {{Birth date and age|df=yes|1937|11|30}}
| birth_place = [[South Shields]], County Durham, England
| occupation  = {{hlist|Film director|film producer}}
| years_active = 1965–present
| nationality = British
}}
'''Sir Ridley Scott''' (born 30 November 1937) is an English [[filmmaker]]. His work
includes science fiction films.

== Early life ==
Scott was born in [[South Shields]].`

const deceased = `{{Infobox person
| name       = Stanley Kubrick
| birth_date = {{Birth date|1928|7|26}}
| death_date = {{Death date and age|1999|3|7|1928|7|26}}
| occupation = Film director
}}
'''Stanley Kubrick''' was an American film director.`

func TestBiographyFromInfoboxPerson(t *testing.T) {
	b := buildBiography(dump.Page{NS: 0, Title: "Ridley Scott", Text: ridley})
	if b == nil {
		t.Fatal("no biography extracted")
	}
	if b.BirthDate != "1937-11-30" {
		t.Errorf("birth_date = %q, want 1937-11-30", b.BirthDate)
	}
	if b.Nationality != "British" {
		t.Errorf("nationality = %q", b.Nationality)
	}
	if b.Overview == "" {
		t.Error("overview is empty — the lead was not captured")
	}
	if got := b.BirthPlace; got == "" || got[0] == '[' {
		t.Errorf("birth_place = %q — wikitext leaked through", got)
	}
}

// A death-date template carries the death date first and the birth date after
// it. Reading past the first three numbers would silently return a birth date
// where a death date belongs.
func TestDeathDateDoesNotPickUpTheTrailingBirthDate(t *testing.T) {
	b := buildBiography(dump.Page{NS: 0, Title: "Stanley Kubrick", Text: deceased})
	if b == nil {
		t.Fatal("no biography extracted")
	}
	if b.BirthDate != "1928-07-26" {
		t.Errorf("birth_date = %q, want 1928-07-26", b.BirthDate)
	}
	if b.DeathDate != "1999-03-07" {
		t.Errorf("death_date = %q, want 1999-03-07", b.DeathDate)
	}
}

// Everything that is not a biography must cost nothing and return nil, because
// every page in the dump is handed to this.
func TestNonBiographiesAreRejected(t *testing.T) {
	for name, page := range map[string]dump.Page{
		"a film":      {NS: 0, Title: "Blade Runner", Text: "{{Infobox film\n| name = Blade Runner\n}}\nA film."},
		"a talk page": {NS: 1, Title: "Talk:Ridley Scott", Text: ridley},
		"plain prose": {NS: 0, Title: "Cinema", Text: "Cinema is an art form."},
		"empty box":   {NS: 0, Title: "Nobody", Text: "{{Infobox person\n| name = Nobody\n}}"},
	} {
		if b := buildBiography(page); b != nil {
			t.Errorf("%s: expected nil, got %+v", name, b)
		}
	}
}

func TestParseDateTemplate(t *testing.T) {
	for in, want := range map[string]string{
		"{{Birth date and age|df=yes|1937|11|30}}": "1937-11-30",
		"{{Birth date|1928|7|26}}":                 "1928-07-26",
		"{{birth date|1900}}":                      "1900",
		"born sometime in the 1930s":               "",
		"{{Birth date|unknown}}":                   "",
	} {
		if got := parseDateTemplate(in); got != want {
			t.Errorf("parseDateTemplate(%q) = %q, want %q", in, got, want)
		}
	}
}

// Season headings are what tell an inline episode list which season it belongs
// to. When this regex stopped matching, every episode in every series article
// fell into season 0, the seasons merged, and dedup by episode number silently
// destroyed 164,445 episodes — while the series count, the source count and the
// Wikidata resolution counts all stayed identical, so nothing else looked wrong.
func TestSeasonHeadingsAreRecognised(t *testing.T) {
	for _, h := range []string{
		"===Season 1 (2015)===",
		"== Season 3 ==",
		"=== Series 2 (2009) ===",
		"====Season 12====",
	} {
		if !reSeasonHeading.MatchString(h) {
			t.Errorf("reSeasonHeading does not match %q", h)
		}
	}
}

// The real failure was not "no episodes" but "all episodes in one season", which
// only shows up when a document has more than one.
func TestInlineEpisodesGetTheirOwnSeasons(t *testing.T) {
	const article = `
===Season 1 (2015)===
{{Episode list|EpisodeNumber=1|EpisodeNumber2=1|Title=Pilot}}
{{Episode list|EpisodeNumber=2|EpisodeNumber2=2|Title=Joust Friends}}
===Season 2 (2016)===
{{Episode list|EpisodeNumber=9|EpisodeNumber2=1|Title=A New Season}}
`
	got := extractEpisodesByHeading(article)
	if len(got) != 3 {
		t.Fatalf("got %d episodes, want 3", len(got))
	}
	seasons := map[int]int{}
	for _, e := range got {
		seasons[e.season]++
	}
	if seasons[1] != 2 || seasons[2] != 1 {
		t.Errorf("seasons = %v, want 2 episodes in season 1 and 1 in season 2", seasons)
	}
}
