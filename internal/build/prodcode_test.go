package build

import (
	"os"
	"testing"

	"github.com/szatmary/filmstock/internal/dump"
)

// Futurama season 5 is the case that showed both bugs at once. Wikipedia lists
// the four direct-to-video films split into four episodes each, so sixteen rows
// share four titles and four dates; before ProdCode was read, nothing told part
// 1 of "Bender's Big Score" from part 4. And each row states its DVD date first
// and the Comedy Central broadcast second, so every episode was stamped with a
// date the season record itself contradicted.
func TestFuturamaFilmPartsAreDistinguishable(t *testing.T) {
	b, err := os.ReadFile("testdata/season-futurama-5.wikitext")
	if err != nil {
		t.Fatal(err)
	}
	msgs := parseTelevisionPage(dump.Page{ID: 25385651,
		Title: "Futurama season 5", Text: string(b)})
	byNum := map[int]struct{ title, air, prod string }{}
	for _, m := range msgs {
		for _, e := range m.eps {
			byNum[e.NumberInSeason] = struct{ title, air, prod string }{
				e.Title, e.AirDate, e.ProdCode}
		}
	}
	if len(byNum) != 16 {
		t.Fatalf("season 5 has %d episodes, want 16", len(byNum))
	}
	for _, want := range []struct {
		num              int
		title, air, prod string
	}{
		// The air date is the Comedy Central broadcast, NOT the DVD release the
		// row states first. The season itself reports first_aired 2008-03-23.
		{1, "Bender's Big Score", "2008-03-23", "5ACV01"},
		{4, "Bender's Big Score", "2008-03-23", "5ACV04"},
		{5, "The Beast with a Billion Backs", "2008-10-19", "5ACV05"},
		{9, "Bender's Game", "2009-04-26", "5ACV09"},
		{16, "Into the Wild Green Yonder", "2009-08-30", "5ACV16"},
	} {
		got, ok := byNum[want.num]
		if !ok {
			t.Errorf("no episode %d", want.num)
			continue
		}
		if got.title != want.title {
			t.Errorf("episode %d: title %q, want %q", want.num, got.title, want.title)
		}
		if got.air != want.air {
			t.Errorf("episode %d: air %q, want %q — the DVD date is not an air date",
				want.num, got.air, want.air)
		}
		if got.prod != want.prod {
			t.Errorf("episode %d: prod code %q, want %q", want.num, got.prod, want.prod)
		}
	}
	// The whole point: every episode is now separable from its siblings.
	seen := map[string]int{}
	for n, e := range byNum {
		if e.prod == "" {
			t.Errorf("episode %d has no production code", n)
			continue
		}
		if prev, dup := seen[e.prod]; dup {
			t.Errorf("episodes %d and %d share production code %q", prev, n, e.prod)
		}
		seen[e.prod] = n
	}
}

func TestParseProdCode(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"bare", "4F02", "4F02"},
		{"padded", "  5ACV01  ", "5ACV01"},
		// A citation on the code is common — 2,557 values carry one.
		{"citation", `40510-480<ref name=viewers11>{{cite web|title=x|url=http://e/}}</ref>`, "40510-480"},
		{"self closing citation", `40510-479<ref name=viewers11/>`, "40510-479"},
		// An explanatory footnote is not part of the code.
		{"footnote", `201{{efn|name="reg error"|The U.S. copyright registrations disagree.}}`, "201"},
		// "No production code" is written as markup and must not become the
		// literal string "N/A", which would read as a real code.
		{"placeholder", "{{small|N/A}}", ""},
		{"empty", "", ""},
		// Two codes for one episode. Joined with " / " because CleanText turns
		// the <br /> into a space, which would read as one nonsense code.
		{"two codes", "2-25<br />2-26", "2-25 / 2-26"},
		{"two codes with citation", `3F25<ref>{{cite web|url=http://e/}}</ref><br>3G01`, "3F25 / 3G01"},
		{"held over", "MAC510<br />MAC414{{efn|name=s4}}", "MAC510 / MAC414"},
		// A <br /> INSIDE the citation is not a second code. Splitting there
		// severed the ref and left its prose behind as one.
		{"line break inside citation",
			`227223<ref>{{cite web|url=http://cocatalog.loc.gov|title=West Wing : no. 227223, ` +
				`The special episode / directed by William Couturie|work=[[Copyright Catalog]]}}` +
				`<br />Search for the title</ref>`, "227223"},
		// The parameter is commented out, so the row states no code at all.
		// ParseInfobox hands the value over anyway, trailing the closer.
		{"commented out parameter", "103 -->", ""},
		{"comment closer alone", "-->", ""},
		// An opener with no closer hides the rest of the field.
		{"dangling comment opener", "60272\n<!-- Hiding this for now", "60272"},
		{"balanced comment", "4F02<!-- checked against the DVD -->", "4F02"},
		// splitParams ends the parameter at the pipe inside this citation, so
		// the value arrives with an unclosed <ref> and the citation's title
		// trailing it. The code is what precedes the citation.
		{"citation truncated at a pipe",
			`101<ref>[https://tv.azpm.org/schedules/episode/270825/ Episode of Rosie's Rules`,
			"101"},
		{"bare external link", `113 [https://example.org/x Episode listing]`, "113"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := parseProdCode(c.in); got != c.want {
				t.Errorf("parseProdCode(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestEpisodeAirDate(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"single", "{{Start date|2007|11|27}}", "2007-11-27"},
		{"none", "TBA", ""},
		// 4,267 of the 4,445 multi-date rows are a two-country release and the
		// first date is the original airing. This must not change.
		{"two countries", `{{Start date|df=yes|1999|3|19}} (US)<br />{{Start date|df=yes|1999|11|29}} (UK)`,
			"1999-03-19"},
		{"two networks", `{{Start date|2013|1|7}} (DirectTV)<br />{{Start date|2013|8|2}} (NBC)`,
			"2013-01-07"},
		// The 178 that are a home-video release listed before the broadcast.
		{"dvd then broadcast", `{{Start date|2007|11|27}} (DVD)<br>{{Start date|2008|3|23}} ([[Comedy Central]])`,
			"2008-03-23"},
		{"home video then television", `{{Start date|1996|09|02|df=y}} (Home video)<br>{{Start date|1997|04|03|df=y}} (Television)`,
			"1997-04-03"},
		// No <br /> at all, just two templates side by side.
		{"space separated", `{{Start date|2006|2|21}} (DVD) {{Start date|2006|7|21}} ([[Cartoon Network]])`,
			"2006-07-21"},
		{"complete series dvd", `{{Start date|2014|10|13}} (The Complete Series DVD) {{Start date|2020|11|10}} ([[Paramount+|Paramount Plus]])`,
			"2020-11-10"},
		// Broadcast first, home video second: already right, left alone.
		{"broadcast then dvd", `{{Start date|2008|3|23}} (Comedy Central)<br>{{Start date|2007|11|27}} (DVD)`,
			"2008-03-23"},
		// Where every date is a home-video date, that is what the row says the
		// release was. Skipping them all would lose the only date there is.
		{"all home video", `{{Start date|2007|11|27}} (DVD)<br>{{Start date|2008|1|15}} (Blu-ray)`,
			"2007-11-27"},
		// "Discovery" is a network, not a disc. The annotation match is on whole
		// words for exactly this reason.
		{"discovery is a network", `{{Start date|2005|4|3}} (Discovery)<br />{{Start date|2005|9|1}} (UK)`,
			"2005-04-03"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := episodeAirDate(c.in); got != c.want {
				t.Errorf("episodeAirDate(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The Simpsons' season 8 mixes holdover 3F codes among the 4F ones, which is
// what makes the production code worth keeping: it is the only alternate
// ordering the article states. It also carries a citation and a second code on
// the same field, so it exercises the cleaning on real wikitext.
func TestSimpsonsProdCodesSurviveTheirCitations(t *testing.T) {
	b, err := os.ReadFile("testdata/season-the-simpsons-season-8.wikitext")
	if err != nil {
		t.Fatal(err)
	}
	msgs := parseTelevisionPage(dump.Page{ID: 2,
		Title: "The Simpsons season 8", Text: string(b)})
	byNum := map[int]string{}
	for _, m := range msgs {
		for _, e := range m.eps {
			byNum[e.NumberInSeason] = e.ProdCode
		}
	}
	for num, want := range map[int]string{
		1:  "4F02",
		2:  "3F23",
		3:  "4F03",
		9:  "3F24",
		10: "3F25 / 3G01", // states two codes, the first with a citation
	} {
		if got := byNum[num]; got != want {
			t.Errorf("episode %d: prod code %q, want %q", num, got, want)
		}
	}
}
