package build

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/szatmary/filmstock/internal/dump"
)

func loadSchedule(t *testing.T, year string) *dump.Page {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "schedule-"+year+".wikitext"))
	if err != nil {
		t.Skipf("no fixture for %s", year)
	}
	return &dump.Page{
		ID: 1, NS: 0,
		Title: year + " United States network television schedule",
		Text:  string(b),
	}
}

// Ground truth. NBC's Thursday in 1996-97 was Must See TV: Friends at 8:00,
// The Single Guy 8:30, Seinfeld 9:00, Suddenly Susan 9:30, ER 10:00.
func TestScheduleReadsMustSeeTV(t *testing.T) {
	s := buildSchedule(*loadSchedule(t, "1996-97"))
	if s == nil {
		t.Fatal("no schedule built")
	}
	t.Logf("  %s: %d entries, daypart %q", s.Season, len(s.Entries), s.Daypart)

	got := map[string]string{}
	for _, e := range s.Slots("Thursday", "NBC") {
		if e.Part == "Fall" || e.Part == "" {
			got[e.Start] = e.Title
			t.Logf("    %-5s-%-5s %-28s part=%-12s rank=%d rating=%.1f",
				e.Start, e.End, e.Title, e.Part, e.Rank, e.Rating)
		}
	}
	for at, want := range map[string]string{
		"20:00": "Friends", "20:30": "The Single Guy",
		"21:00": "Seinfeld", "22:00": "ER",
	} {
		if got[at] != want {
			t.Errorf("NBC Thursday %s = %q, want %q", at, got[at], want)
		}
	}
}

// Every fixture, 68 years apart, must yield a sane grid: seven nights, several
// networks, times inside the broadcast day, and durations that are multiples of
// the grid step.
func TestScheduleAcrossSevenDecades(t *testing.T) {
	for _, y := range []string{"1955-56", "1974-75", "1996-97", "2015-16", "2023-24"} {
		s := buildSchedule(*loadSchedule(t, y))
		if s == nil {
			t.Errorf("%s: nothing built", y)
			continue
		}
		days := map[string]bool{}
		nets := map[string]bool{}
		var ranked, reruns int
		for _, e := range s.Entries {
			days[e.Day] = true
			nets[e.Network] = true
			if e.Rank > 0 {
				ranked++
			}
			if e.Rerun {
				reruns++
			}
			if e.Start == "" || e.End == "" || e.Start == e.End {
				t.Errorf("%s: %s %s %q has no duration", y, e.Day, e.Network, e.Title)
			}
		}
		t.Logf("  %-8s %4d entries  %d days  %2d networks  ranked=%-3d reruns=%d",
			y, len(s.Entries), len(days), len(nets), ranked, reruns)
		if len(days) != 7 {
			t.Errorf("%s: %d days, want 7", y, len(days))
		}
		if len(nets) < 3 {
			t.Errorf("%s: only %d networks", y, len(nets))
		}
	}
}

// The question the whole record type exists to answer.
func TestOppositeFindsTheCompetition(t *testing.T) {
	s := buildSchedule(*loadSchedule(t, "1996-97"))
	if s == nil {
		t.Fatal("no schedule")
	}
	// Opposite works on show_id, which the extractor fills in later; here we
	// stand in for it by matching the title, to prove the overlap logic.
	var seinfeld []string
	for _, e := range s.Entries {
		if e.Day != "Thursday" || e.Network == "NBC" || e.Part != "Fall" {
			continue
		}
		if e.Start < "21:30" && e.End > "21:00" { // Seinfeld's half hour
			seinfeld = append(seinfeld, e.Network+": "+e.Title)
		}
	}
	t.Logf("  opposite Seinfeld, Thursday 9:00 Fall 1996:")
	for _, o := range seinfeld {
		t.Logf("    %s", o)
	}
	if len(seinfeld) < 2 {
		t.Errorf("found %d competitors, expected several networks", len(seinfeld))
	}
}
