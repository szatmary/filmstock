package build

import (
	"os"
	"testing"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/dump"
)

// A serial is neither a multi-part episode nor a segmented one. Classic Doctor
// Who writes a four-part story as ONE row: Serial=yes, one EpisodeNumber for
// the story, and OriginalAirDate_1..4 with Viewers_1..4 — four broadcasts a
// week apart. Collapsing it keeps one date and one rating and throws away the
// rest, which is why Doctor Who reported 291 episodes against a real 695.
func TestSerialBecomesItsBroadcasts(t *testing.T) {
	b, err := os.ReadFile("testdata/season-doctor-who-12.wikitext")
	if err != nil {
		t.Fatal(err)
	}
	msgs := parseTelevisionPage(dump.Page{ID: 1, Title: "Doctor Who season 12", Text: string(b)})
	var eps []*epView
	for _, m := range msgs {
		for _, e := range m.eps {
			eps = append(eps, &epView{e.Title, e.AirDate, e.Viewers, e.NumberInSeason})
		}
	}
	// Season 12 is five serials of four parts each: 20 broadcasts.
	if len(eps) != 20 {
		t.Errorf("got %d episodes, want 20 — the serials did not expand", len(eps))
		for _, e := range eps {
			t.Logf("  %q %s %v", e.title, e.air, e.viewers)
		}
		return
	}
	// Robot, the first story: four consecutive Saturdays, each with its own
	// viewership, all sharing the story's position in the season.
	want := []epView{
		{"Robot: Part One", "1974-12-28", 10.8, 1},
		{"Robot: Part Two", "1975-01-04", 10.7, 1},
	}
	for i, w := range want {
		got := eps[i]
		if got.title != w.title {
			t.Errorf("episode %d: title %q, want %q", i, got.title, w.title)
		}
		if got.air != w.air {
			t.Errorf("episode %d: air %q, want %q", i, got.air, w.air)
		}
		if got.viewers != w.viewers {
			t.Errorf("episode %d: viewers %v, want %v", i, got.viewers, w.viewers)
		}
		if got.num != w.num {
			t.Errorf("episode %d: number in season %d, want %d", i, got.num, w.num)
		}
	}
}

// The parts share the story's numbers, so without distinct titles they dedup
// against each other and three of four vanish by another route.
func TestSerialPartsSurviveDeduplication(t *testing.T) {
	c := newTelevisionCollector()
	c.add(televisionMsg{series: &filmstock.TelevisionSeries{PageID: 1, Title: "Doctor Who"}})
	b, err := os.ReadFile("testdata/season-doctor-who-12.wikitext")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range parseTelevisionPage(dump.Page{ID: 50, Title: "Doctor Who season 12", Text: string(b)}) {
		if m.series == nil {
			c.add(m)
		}
	}
	series, _ := c.finish(map[int]int{50: 1}, nil)
	if len(series) != 1 {
		t.Fatalf("got %d series", len(series))
	}
	var n int
	for _, s := range series[0].Seasons {
		n += len(s.Episodes)
	}
	if n != 20 {
		t.Errorf("%d episodes survived deduplication, want 20", n)
	}
}

// An ordinary row must not be read as a serial.
func TestSerialFlagIsRequired(t *testing.T) {
	got := parseEpisodeRows(map[string]string{
		"numparts": "2", "title": "Anthology", "episodenumber2": "3",
		"title_1": "[[A]]", "title_2": "[[B]]",
	})
	if len(got) != 1 {
		t.Errorf("got %d episodes, want 1 — no Serial=yes was stated", len(got))
	}
}

type epView struct {
	title, air string
	viewers    float64
	num        int
}
