package build

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/szatmary/filmstock"
)

// ER's overview, in the shape the article writes it.
const erOverview = `
{{Series overview
| color1 = #1F3D5C
| link1 = List of ER episodes#Season 1
| episodes1 = 25
| start1 = {{Start date|1994|9|19}}
| end1 = {{End date|1995|5|18}}
| infoA = Rank
| infoB = Rating
| infoC = Viewers<br />(millions)
| infoA1 = 2
| infoB1 = 20.0
| infoC1 = 30.1
| episodes2 = 22
| start2 = {{Start date|1995|9|21}}
| end2 = {{Start date|1996|5|16}}
| infoA2 = 1
| infoB2 = 22.0
| infoC2 = 34.9
}}
`

func TestSeriesOverviewReadsEverySeason(t *testing.T) {
	got := parseSeriesOverview(erOverview)
	if len(got) != 2 {
		t.Fatalf("got %d seasons, want 2", len(got))
	}
	if got[0].Season != 1 || got[1].Season != 2 {
		t.Fatalf("seasons out of order: %d, %d", got[0].Season, got[1].Season)
	}
	s := got[0]
	if s.NumEpisodes != 25 {
		t.Errorf("episodes %d, want 25", s.NumEpisodes)
	}
	if s.FirstAired != "1994-09-19" {
		t.Errorf("first aired %q, want 1994-09-19", s.FirstAired)
	}
	if s.LastAired != "1995-05-18" {
		t.Errorf("last aired %q, want 1995-05-18", s.LastAired)
	}
	if s.Rank != 2 || s.Rating != 20.0 || s.Viewers != 30.1 {
		t.Errorf("rank=%d rating=%v viewers=%v, want 2/20/30.1", s.Rank, s.Rating, s.Viewers)
	}
}

// The columns are self-labelling and shows order them differently. Reading
// infoA as Rank because it usually is would record viewership as rank, wrongly
// and undetectably.
func TestSeriesOverviewColumnsAreReadByTheirHeadings(t *testing.T) {
	swapped := `{{Series overview
| infoA = Viewers (millions)
| infoB = Rank
| episodes1 = 10
| infoA1 = 30.1
| infoB1 = 2
}}`
	got := parseSeriesOverview(swapped)
	if len(got) != 1 {
		t.Fatalf("got %d seasons", len(got))
	}
	if got[0].Viewers != 30.1 {
		t.Errorf("viewers %v, want 30.1 — column read by position, not heading", got[0].Viewers)
	}
	if got[0].Rank != 2 {
		t.Errorf("rank %d, want 2", got[0].Rank)
	}
}

// "Nielsen ratings rank" names both. It is a rank column.
func TestHeadingNamingBothIsARank(t *testing.T) {
	if r := overviewRole("Nielsen ratings rank"); r != "rank" {
		t.Errorf("got %q, want rank", r)
	}
	if r := overviewRole("Household rating"); r != "rating" {
		t.Errorf("got %q, want rating", r)
	}
	if r := overviewRole("Avg. viewers<br />(millions)"); r != "viewers" {
		t.Errorf("got %q, want viewers", r)
	}
}

// Cells carry footnotes and references; parsing the whole cell yields zero.
func TestNumbersSurviveTheirFootnotes(t *testing.T) {
	got := parseSeriesOverview(`{{Series overview
| infoA = Rank
| infoBtitle = Viewers
| episodes1 = 25<ref name="abc" />
| infoA1 = 2{{efn|Tied with ''Seinfeld''.}}
| infoB1 = 30.1<ref>x</ref>
}}`)
	if len(got) != 1 {
		t.Fatalf("got %d seasons", len(got))
	}
	if got[0].NumEpisodes != 25 || got[0].Rank != 2 || got[0].Viewers != 30.1 {
		t.Errorf("episodes=%d rank=%d viewers=%v, want 25/2/30.1",
			got[0].NumEpisodes, got[0].Rank, got[0].Viewers)
	}
}

// An unlabelled extra column is left alone rather than guessed at.
func TestUnlabelledColumnIsIgnored(t *testing.T) {
	got := parseSeriesOverview(`{{Series overview
| infoA = Timeslot
| episodes1 = 10
| infoA1 = 22
}}`)
	if len(got) != 1 {
		t.Fatalf("got %d seasons", len(got))
	}
	if got[0].Rank != 0 || got[0].Rating != 0 || got[0].Viewers != 0 {
		t.Errorf("a Timeslot column was read as a Nielsen figure: %+v", got[0])
	}
}

// A season stated only by {{Series overview}} must still become a season.
// Enumerating seasons from episode lists alone means a show whose overview
// gives twenty seasons of counts, dates and ratings, but whose episode rows do
// not parse, produces no seasons at all.
func TestSeasonFromOverviewAloneSurvives(t *testing.T) {
	c := newTelevisionCollector()
	c.add(televisionMsg{series: &filmstock.TelevisionSeries{PageID: 100, Title: "ER"}})
	for _, m := range overviewMsgs(erOverview, 200, "List of ER episodes", 100) {
		c.add(m)
	}
	series, _ := c.finish(map[int]int{200: 100}, nil)
	if len(series) != 1 {
		t.Fatalf("got %d series", len(series))
	}
	got := series[0].Seasons
	if len(got) != 2 {
		t.Fatalf("got %d seasons, want 2 — overview-only seasons were dropped", len(got))
	}
	if got[0].NumEpisodes != 25 || got[0].Rank != 2 || got[0].Viewers != 30.1 {
		t.Errorf("season 1: %+v", got[0])
	}
}

// The season article and the overview know different things. Taking the first
// source that mentions a season keeps one and discards the other.
func TestSeasonMergesBothSources(t *testing.T) {
	c := newTelevisionCollector()
	c.add(televisionMsg{series: &filmstock.TelevisionSeries{PageID: 100, Title: "ER"}})
	// The overview, on the episode-list page: Nielsen figures.
	for _, m := range overviewMsgs(erOverview, 200, "List of ER episodes", 100) {
		c.add(m)
	}
	// The season's own article: cast, network, page_id.
	c.add(televisionMsg{srcID: 300, srcTitle: "ER season 1", season: 1, auth: true,
		seasonMeta: &filmstock.Season{Season: 1, PageID: 300, Network: "NBC",
			Starring: []filmstock.Person{{Name: "George Clooney"}}}})
	series, _ := c.finish(map[int]int{200: 100, 300: 100}, nil)
	if len(series) != 1 {
		t.Fatalf("got %d series", len(series))
	}
	var s1 *filmstock.Season
	for _, s := range series[0].Seasons {
		if s.Season == 1 {
			s1 = s
		}
	}
	if s1 == nil {
		t.Fatal("no season 1")
	}
	if s1.Rank != 2 || s1.Viewers != 30.1 {
		t.Errorf("lost the overview's Nielsen figures: rank=%d viewers=%v", s1.Rank, s1.Viewers)
	}
	if s1.PageID != 300 || s1.Network != "NBC" || len(s1.Starring) != 1 {
		t.Errorf("lost the season article's own data: %+v", s1)
	}
}

// mergeSeason adds; it must never overwrite what is already known.
func TestMergeSeasonNeverOverwrites(t *testing.T) {
	dst := &filmstock.Season{Season: 1, PageID: 300, Network: "NBC", Rank: 2}
	mergeSeason(dst, &filmstock.Season{Season: 1, PageID: 999, Network: "CBS", Rank: 9, Viewers: 30.1})
	if dst.PageID != 300 || dst.Network != "NBC" || dst.Rank != 2 {
		t.Errorf("overwrote known values: %+v", dst)
	}
	if dst.Viewers != 30.1 {
		t.Errorf("did not fill the empty one: %+v", dst)
	}
}

// NumEpisodes must agree with Episodes when rows parsed, or the record
// contradicts itself; the stated count fills in only when none did.
func TestNumEpisodesAgreesWithEpisodes(t *testing.T) {
	c := newTelevisionCollector()
	c.add(televisionMsg{series: &filmstock.TelevisionSeries{PageID: 100, Title: "ER"}})
	for _, m := range overviewMsgs(erOverview, 200, "List of ER episodes", 100) {
		c.add(m) // says 25
	}
	c.add(televisionMsg{srcID: 200, season: 1, auth: true, eps: []*filmstock.Episode{
		{Title: "24 Hours", NumberInSeason: 1}, {Title: "Day One", NumberInSeason: 2}}})
	series, _ := c.finish(map[int]int{200: 100}, nil)
	for _, s := range series[0].Seasons {
		if s.Season == 1 && s.NumEpisodes != len(s.Episodes) {
			t.Errorf("num_episodes=%d but %d episodes carried", s.NumEpisodes, len(s.Episodes))
		}
	}
}

// Real articles, because the synthetic fixture above encoded my own assumption
// about the template and so agreed with a parser that read no real Nielsen
// figure at all. These are the wikitext of five episode-list articles as
// Wikipedia serves it.
func TestRealSeriesOverviews(t *testing.T) {
	type want struct {
		seasons             int
		s1Eps               int
		s1First, s1Last     string
		s1Rank              int
		s1Rating, s1Viewers float64
	}
	cases := map[string]want{
		// Fifteen seasons, almost none of which have an article of their own —
		// which is the point: without the overview they do not exist at all.
		"er": {15, 25, "1994-09-19", "1995-05-18", 2, 20.0, 30.1},
		// No info columns whatsoever. Zeros here are the correct answer, not a
		// parse failure, and the test says so.
		"breaking-bad": {5, 7, "2008-01-20", "2008-03-09", 0, 0, 0},
		"cheers":       {11, 22, "1982-09-30", "1983-03-31", 74, 13.1, 10.9},
		"the-simpsons": {38, 13, "1989-12-17", "1990-05-13", 30, 14.5, 13.4},
	}
	for name, w := range cases {
		b, err := os.ReadFile("testdata/overview-" + name + ".wikitext")
		if err != nil {
			t.Fatal(err)
		}
		got := parseSeriesOverview(string(b))
		if len(got) != w.seasons {
			t.Errorf("%s: %d seasons, want %d", name, len(got), w.seasons)
			continue
		}
		s := got[0]
		if s.Season != 1 {
			t.Errorf("%s: first season is %d", name, s.Season)
		}
		if s.NumEpisodes != w.s1Eps {
			t.Errorf("%s s1: %d episodes, want %d", name, s.NumEpisodes, w.s1Eps)
		}
		if s.FirstAired != w.s1First || s.LastAired != w.s1Last {
			t.Errorf("%s s1: %s..%s, want %s..%s", name, s.FirstAired, s.LastAired, w.s1First, w.s1Last)
		}
		if s.Rank != w.s1Rank || s.Rating != w.s1Rating || s.Viewers != w.s1Viewers {
			t.Errorf("%s s1: rank=%d rating=%v viewers=%v, want %d/%v/%v",
				name, s.Rank, s.Rating, s.Viewers, w.s1Rank, w.s1Rating, w.s1Viewers)
		}
	}
}

// Breaking Bad's fifth season is split: episodes5A/5B with their own dates and
// no start5 or end5 at all. Requiring the parameter to end in a digit parses
// the total episode count and silently drops every date the season has.
func TestSplitSeasonSpansItsParts(t *testing.T) {
	b, err := os.ReadFile("testdata/overview-breaking-bad.wikitext")
	if err != nil {
		t.Fatal(err)
	}
	var s5 *filmstock.Season
	for _, s := range parseSeriesOverview(string(b)) {
		if s.Season == 5 {
			s5 = s
		}
	}
	if s5 == nil {
		t.Fatal("no season 5")
	}
	if s5.NumEpisodes != 16 {
		t.Errorf("episodes %d, want 16 (the stated total, not a part)", s5.NumEpisodes)
	}
	if s5.FirstAired != "2012-07-15" {
		t.Errorf("first aired %q, want 2012-07-15 — the earlier part", s5.FirstAired)
	}
	if s5.LastAired != "2013-09-29" {
		t.Errorf("last aired %q, want 2013-09-29 — the later part", s5.LastAired)
	}
}

// {{n/a}} is not a number. Seinfeld's first season has a Viewers figure but
// writes {{n/a}} for rank and rating, and a parser that took the first digits
// out of the raw cell would invent them.
func TestNotApplicableIsNotAFigure(t *testing.T) {
	b, err := os.ReadFile("testdata/overview-seinfeld.wikitext")
	if err != nil {
		t.Fatal(err)
	}
	got := parseSeriesOverview(string(b))
	if len(got) != 9 {
		t.Fatalf("%d seasons, want 9", len(got))
	}
	s1 := got[0]
	if s1.Rank != 0 || s1.Rating != 0 {
		t.Errorf("season 1: rank=%d rating=%v, want both absent — the article says {{n/a}}",
			s1.Rank, s1.Rating)
	}
	if s1.Viewers != 19.2 {
		t.Errorf("season 1: viewers %v, want 19.2 — the one figure it does state", s1.Viewers)
	}
	// And the season that does have figures still has them.
	s9 := got[8]
	if s9.Rank != 1 || s9.Rating != 22.0 || s9.Viewers != 38.1 {
		t.Errorf("season 9: rank=%d rating=%v viewers=%v, want 1/22/38.1",
			s9.Rank, s9.Rating, s9.Viewers)
	}
}

// Two series can name the same episode-list article — 51 lists in the corpus
// are claimed by 530 series between them. Resolving that by map iteration order
// changed the owner between runs over identical input, which is how I Love
// Lucy's six seasons attached to it on one run and to The Lucy–Desi Comedy Hour
// on the next, with nothing in either output saying so.
func TestWholeArticleClaimBeatsSectionClaim(t *testing.T) {
	whole := listClaim{series: 4319219, fragment: false}
	section := listClaim{series: 144832, fragment: true}
	if !betterClaim(whole, section) {
		t.Error("a section claim won against a whole-article claim")
	}
	if betterClaim(section, whole) {
		t.Error("a section claim beat a whole-article claim")
	}
}

// Among equal claims the answer must be the same on every run, and it must be
// decided by page_id rather than by anything displayed.
func TestEqualClaimsResolveDeterministically(t *testing.T) {
	lo := listClaim{series: 100, fragment: false}
	hi := listClaim{series: 999, fragment: false}
	if !betterClaim(lo, hi) || betterClaim(hi, lo) {
		t.Error("equal claims did not resolve to the lower page_id")
	}
	both := listClaim{series: 100, fragment: true}
	other := listClaim{series: 999, fragment: true}
	if !betterClaim(both, other) || betterClaim(other, both) {
		t.Error("equal section claims did not resolve to the lower page_id")
	}
}

// A multiseries overview must yield nothing rather than merging two shows.
// Both nested blocks number their seasons from one, so reading them into a list
// keyed by season number alone gives one show's Nielsen rank to the other's
// season. Absent data is recoverable; misattributed data is not.
func TestMultiseriesOverviewIsSkippedNotMerged(t *testing.T) {
	b, err := os.ReadFile("testdata/overview-i-love-lucy.wikitext")
	if err != nil {
		t.Skip("fixture not present")
	}
	if got := parseSeriesOverview(string(b)); len(got) != 0 {
		t.Errorf("got %d seasons from a multiseries overview; two shows would be merged", len(got))
	}
}

// Sources are merged in page_id order. Built by ranging over a map, they arrive
// in a different order on every run, and where two sources describe the same
// season the first one seen wins — so the record changed between runs over
// identical input.
func TestSeasonSourcesMergeInAFixedOrder(t *testing.T) {
	build := func() *filmstock.Season {
		c := newTelevisionCollector()
		c.add(televisionMsg{series: &filmstock.TelevisionSeries{PageID: 1, Title: "X"}})
		// Two sources describing season 1 with different episode counts, and no
		// episode rows, so the count can only come from the metadata.
		c.add(televisionMsg{srcID: 500, season: 1,
			seasonMeta: &filmstock.Season{Season: 1, NumEpisodes: 14}})
		c.add(televisionMsg{srcID: 200, season: 1,
			seasonMeta: &filmstock.Season{Season: 1, NumEpisodes: 13}})
		series, _ := c.finish(map[int]int{500: 1, 200: 1}, nil)
		if len(series) != 1 || len(series[0].Seasons) != 1 {
			t.Fatalf("unexpected shape: %d series", len(series))
		}
		return series[0].Seasons[0]
	}
	first := build().NumEpisodes
	if first != 13 {
		t.Errorf("num_episodes %d, want 13 — the lower page_id (200) should win", first)
	}
	for range 20 {
		if got := build().NumEpisodes; got != first {
			t.Fatalf("num_episodes varies between runs: %d then %d", first, got)
		}
	}
}

// Vera states "infoA14 = 6.24" for season 14 and "infoA14S = 3.11" for its
// specials. Both matched the parameter pattern and both wrote the season's
// viewership, so which survived was decided by Go's map iteration order — the
// same text parsed twice in one process gave different answers.
func TestPartFiguresDoNotOverwriteTheSeason(t *testing.T) {
	b, err := os.ReadFile("testdata/overview-vera.wikitext")
	if err != nil {
		t.Fatal(err)
	}
	var first []*filmstock.Season
	for run := range 25 {
		got := parseSeriesOverview(string(b))
		if run == 0 {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("season count varies: %d then %d", len(first), len(got))
		}
		for i := range got {
			a, _ := json.Marshal(got[i])
			b, _ := json.Marshal(first[i])
			if !bytes.Equal(a, b) {
				t.Fatalf("season %d varies between parses:\n  %s\n  %s",
					got[i].Season, b, a)
			}
		}
	}
	var s14 *filmstock.Season
	for _, s := range first {
		if s.Season == 14 {
			s14 = s
		}
	}
	if s14 == nil {
		t.Fatal("no season 14")
	}
	if s14.Viewers != 6.24 {
		t.Errorf("season 14 viewers %v, want 6.24 — the season's own figure, not the specials'", s14.Viewers)
	}
}

// Articles carry two columns measuring the same thing. Charmed has "Rank" and
// "Network Rank"; Once Upon a Time has "Viewers rank" and "18-49 rank". Both
// matched, so both wrote Season.Rank and the later one silently replaced the
// primary figure with a qualified one — and which was "later" was map order.
func TestPrimaryColumnKeepsItsRole(t *testing.T) {
	cases := []struct {
		file   string
		season int
		rank   int
		note   string
	}{
		// infoB = Rank (117), infoC = Network Rank (2). 117 is the answer.
		{"overview-charmed", 3, 117, "Network Rank must not replace Rank"},
		// infoB = Viewers rank (50), infoD = 18-49 rank (17). The overall
		// viewership rank is the season's rank; the 18-49 demo rank is a
		// different measure this record does not model.
		{"overview-once-upon-a-time", 4, 50, "the demo rank must not replace the overall rank"},
	}
	for _, c := range cases {
		b, err := os.ReadFile("testdata/" + c.file + ".wikitext")
		if err != nil {
			t.Fatal(err)
		}
		var got *filmstock.Season
		for _, s := range parseSeriesOverview(string(b)) {
			if s.Season == c.season {
				got = s
			}
		}
		if got == nil {
			t.Errorf("%s: no season %d", c.file, c.season)
			continue
		}
		if got.Rank != c.rank {
			t.Errorf("%s season %d: rank %d, want %d — %s",
				c.file, c.season, got.Rank, c.rank, c.note)
		}
	}
}

// And it must be the same answer every time, which one parse cannot show.
func TestOverviewParsingIsRepeatable(t *testing.T) {
	for _, f := range []string{"overview-charmed", "overview-once-upon-a-time",
		"overview-vera", "overview-er", "overview-the-simpsons"} {
		b, err := os.ReadFile("testdata/" + f + ".wikitext")
		if err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(parseSeriesOverview(string(b)))
		for range 25 {
			got, _ := json.Marshal(parseSeriesOverview(string(b)))
			if !bytes.Equal(got, want) {
				t.Fatalf("%s parses differently between runs:\n  %s\n  %s", f, want, got)
			}
		}
	}
}
