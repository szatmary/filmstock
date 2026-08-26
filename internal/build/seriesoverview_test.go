package build

import (
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
| infoAtitle = Rank
| infoBtitle = Rating
| infoCtitle = Viewers<br />(millions)
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
| infoAtitle = Viewers (millions)
| infoBtitle = Rank
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
| infoAtitle = Rank
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
| infoAtitle = Timeslot
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
