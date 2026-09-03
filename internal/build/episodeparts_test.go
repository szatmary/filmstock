package build

import (
	"os"
	"testing"

	"github.com/szatmary/filmstock/internal/dump"
	"github.com/szatmary/filmstock/internal/record"
)

// A multi-part episode is ONE {{Episode list}} row spanning several episodes,
// with NumParts saying how many and the differing fields suffixed _1, _2.
// Reading only the unsuffixed names finds no number, no air date and no
// viewership, and collapses the lot into a single entry: Star Trek's "The
// Menagerie" came out as episode 0 of season 0.
func TestMultiPartEpisodeBecomesItsParts(t *testing.T) {
	b, err := os.ReadFile("testdata/season-star-trek-tos-1.wikitext")
	if err != nil {
		t.Fatal(err)
	}
	msgs := parseTelevisionPage(dump.Page{ID: 1,
		Title: "Star Trek: The Original Series season 1", Text: string(b)})
	var eps []*episodeLike
	for _, m := range msgs {
		for _, e := range m.eps {
			eps = append(eps, &episodeLike{e.NumberOverall, e.NumberInSeason, e.Title,
				e.AirDate, e.Viewers, personNames(e.DirectedBy)})
		}
	}
	// The season has 29 episodes; counting the two-parter once gives 28.
	if len(eps) != 29 {
		t.Errorf("season 1 has %d episodes, want 29", len(eps))
	}
	byNum := map[int]*episodeLike{}
	for _, e := range eps {
		byNum[e.inSeason] = e
	}
	for _, want := range []struct {
		num      int
		air      string
		viewers  float64
		director string
	}{
		{11, "1966-11-17", 9.50, "Marc Daniels"},
		{12, "1966-11-24", 10.21, "Robert Butler"},
	} {
		got := byNum[want.num]
		if got == nil {
			t.Errorf("no episode %d — the two-parter did not expand", want.num)
			continue
		}
		if got.title != "The Menagerie" {
			t.Errorf("episode %d: title %q", want.num, got.title)
		}
		if got.air != want.air {
			t.Errorf("episode %d: air %q, want %q", want.num, got.air, want.air)
		}
		if got.viewers != want.viewers {
			t.Errorf("episode %d: viewers %v, want %v", want.num, got.viewers, want.viewers)
		}
		// Each part has its own director; sharing one would attribute the wrong
		// director to half the multi-part episodes in the corpus.
		if len(got.directors) != 1 || got.directors[0] != want.director {
			t.Errorf("episode %d: directors %v, want [%s]", want.num, got.directors, want.director)
		}
	}
}

// Fields the row states once apply to every part.
func TestSharedFieldsReachEveryPart(t *testing.T) {
	got := parseEpisodeRows(map[string]string{
		"numparts":         "2",
		"title":            "The Menagerie",
		"writtenby":        "[[Gene Roddenberry]]",
		"episodenumber_1":  "11",
		"episodenumber_2":  "12",
		"episodenumber2_1": "11",
		"episodenumber2_2": "12",
	})
	if len(got) != 2 {
		t.Fatalf("got %d episodes, want 2", len(got))
	}
	for i, e := range got {
		if e.Title != "The Menagerie" {
			t.Errorf("part %d: title %q", i+1, e.Title)
		}
		if n := personNames(e.WrittenBy); len(n) != 1 || n[0] != "Gene Roddenberry" {
			t.Errorf("part %d: writers %v, want the shared value", i+1, n)
		}
	}
}

// One part's fields must never be read as another's.
func TestPartFieldsDoNotLeakBetweenParts(t *testing.T) {
	got := parseEpisodeRows(map[string]string{
		"numparts":          "2",
		"title":             "Two Parter",
		"episodenumber2_1":  "5",
		"episodenumber2_2":  "6",
		"originalairdate_1": "{{Start date|2001|1|1}}",
		"originalairdate_2": "{{Start date|2001|1|8}}",
	})
	if len(got) != 2 {
		t.Fatalf("got %d episodes, want 2", len(got))
	}
	if got[0].AirDate != "2001-01-01" || got[1].AirDate != "2001-01-08" {
		t.Errorf("air dates %q and %q, want 2001-01-01 and 2001-01-08",
			got[0].AirDate, got[1].AirDate)
	}
	if got[0].NumberInSeason != 5 || got[1].NumberInSeason != 6 {
		t.Errorf("numbers %d and %d, want 5 and 6", got[0].NumberInSeason, got[1].NumberInSeason)
	}
}

// An ordinary row is unaffected.
func TestSinglePartRowIsUnchanged(t *testing.T) {
	got := parseEpisodeRows(map[string]string{
		"title": "Balance of Terror", "episodenumber2": "14",
		"originalairdate": "{{Start date|1966|12|15}}",
	})
	if len(got) != 1 || got[0].NumberInSeason != 14 || got[0].AirDate != "1966-12-15" {
		t.Errorf("got %+v", got)
	}
}

// A mistyped or vandalised NumParts must not turn one row into thousands.
func TestAbsurdNumPartsIsBounded(t *testing.T) {
	got := parseEpisodeRows(map[string]string{"numparts": "100000", "title": "X"})
	if len(got) > maxEpisodeParts {
		t.Errorf("produced %d episodes from one row", len(got))
	}
}

type episodeLike struct {
	overall, inSeason int
	title, air        string
	viewers           float64
	directors         []string
}

func personNames(ps []record.Person) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

// NumParts alone does not mean several episodes. The Simpsons' "Treehouse of
// Horror VII" is NumParts=3 for its three SEGMENTS: one EpisodeNumber, one air
// date, one title, and a writer per segment. Splitting it invents two episodes
// that never aired — the season reported 29 instead of 25.
func TestSegmentedEpisodeStaysOneEpisode(t *testing.T) {
	b, err := os.ReadFile("testdata/season-the-simpsons-season-8.wikitext")
	if err != nil {
		t.Fatal(err)
	}
	msgs := parseTelevisionPage(dump.Page{ID: 1,
		Title: "The Simpsons season 8", Text: string(b)})
	var all []*record.Episode
	for _, m := range msgs {
		all = append(all, m.eps...)
	}
	if len(all) != 25 {
		t.Errorf("season 8 has %d episodes, want 25", len(all))
	}
	var th *record.Episode
	for _, e := range all {
		if e.Title == "Treehouse of Horror VII" {
			if th != nil {
				t.Fatal("Treehouse of Horror VII appears more than once")
			}
			th = e
		}
	}
	if th == nil {
		t.Fatal("Treehouse of Horror VII missing")
	}
	if th.NumberOverall != 154 || th.NumberInSeason != 1 {
		t.Errorf("numbered #%d s%d, want #154 s1", th.NumberOverall, th.NumberInSeason)
	}
	if th.AirDate != "1996-10-27" {
		t.Errorf("air date %q, want 1996-10-27", th.AirDate)
	}
	// One writer per segment, all writers of the one episode. There is no
	// unsuffixed WrittenBy, so reading only the plain name credits nobody.
	want := []string{"Ken Keeler", "Dan Greaney", "David X. Cohen"}
	got := personNames(th.WrittenBy)
	if len(got) != len(want) {
		t.Fatalf("writers %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("writer %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// An unsuffixed value states something about the whole episode and wins over
// the per-segment ones.
func TestWholeEpisodeValueBeatsSegmentValues(t *testing.T) {
	got := parseEpisodeRows(map[string]string{
		"numparts": "3", "title": "X", "episodenumber2": "1",
		"directedby":   "[[Mike B. Anderson]]",
		"directedby_1": "[[Someone Else]]",
	})
	if len(got) != 1 {
		t.Fatalf("got %d episodes, want 1", len(got))
	}
	if n := personNames(got[0].DirectedBy); len(n) != 1 || n[0] != "Mike B. Anderson" {
		t.Errorf("directors %v, want the episode-wide value", n)
	}
}

// An anthology broadcast is one episode of several named segments, and the
// article names them separately: Rugrats' second episode is "Barbecue Story"
// and "Waiter, There's a Baby in My Soup". With no unsuffixed Title the row
// produced no episode at all — 6,630 of them across the corpus. Joined without
// a separator the two names run together into one wrong title.
func TestSegmentTitlesAreJoinedReadably(t *testing.T) {
	got := parseEpisodeRows(map[string]string{
		"numparts": "2", "episodenumber2": "2",
		"title_1": "[[Barbecue Story]]",
		"title_2": "[[Waiter, There's a Baby in My Soup]]",
	})
	if len(got) != 1 {
		t.Fatalf("got %d episodes, want 1 — an anthology is one broadcast", len(got))
	}
	if want := "Barbecue Story / Waiter, There's a Baby in My Soup"; got[0].Title != want {
		t.Errorf("title %q, want %q", got[0].Title, want)
	}
}

// A segment whose title is only markup is not a segment. Joining it anyway
// manufactures a title of "/ /" and with it an episode the article never listed.
func TestEmptySegmentTitlesDoNotMakeAnEpisode(t *testing.T) {
	got := parseEpisodeRows(map[string]string{
		"numparts": "3", "episodenumber2": "5",
		"title_1": "<!-- -->", "title_2": "{{n/a}}", "title_3": "  ",
	})
	if len(got) != 0 {
		t.Errorf("got %d episodes with titles %q, want none", len(got), got[0].Title)
	}
}

// One real segment among empty ones still names the episode.
func TestOneNamedSegmentIsEnough(t *testing.T) {
	got := parseEpisodeRows(map[string]string{
		"numparts": "2", "episodenumber2": "5",
		"title_1": "[[Real Segment]]", "title_2": "<!-- -->",
	})
	if len(got) != 1 || got[0].Title != "Real Segment" {
		t.Fatalf("got %+v", got)
	}
}
