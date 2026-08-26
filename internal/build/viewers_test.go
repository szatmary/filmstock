package build

import "testing"

// Episode lists carry US viewership in millions, in the same parameter map as
// the air date we already read. The value is rarely a bare number.
func TestParseViewers(t *testing.T) {
	for _, c := range []struct {
		in   string
		want float64
	}{
		{`23.8<ref name="1.01-1.02">{{cite news|title=Nielsen}}</ref>`, 23.8},
		{"1.23[2]", 1.23},
		{"9.10&nbsp;million", 9.10},
		{"2.1-2.4", 2.1}, // a range takes its first figure rather than guessing
		{" 14 ", 14},
		{"", 0},
		{"N/A", 0},
		{"TBD", 0},
		{"1234567", 0}, // viewers are millions; this is a parse artefact
	} {
		if got := parseViewers(c.in); got != c.want {
			t.Errorf("parseViewers(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The whole row, as an episode list actually writes it.
func TestEpisodeRowKeepsViewers(t *testing.T) {
	e := parseEpisodeRow(map[string]string{
		"title":           "[[24 Hours (ER)|24 Hours]]",
		"episodenumber":   "1",
		"episodenumber2":  "1",
		"originalairdate": "{{Start date|1994|9|19}}",
		"viewers":         `23.8<ref name="1.01-1.02">x</ref>`,
		"shortsummary":    "Pilot.",
	})
	if e == nil {
		t.Fatal("no episode built")
	}
	if e.Viewers != 23.8 {
		t.Errorf("Viewers = %v, want 23.8", e.Viewers)
	}
	if e.AirDate != "1994-09-19" {
		t.Errorf("AirDate = %q", e.AirDate)
	}
}
