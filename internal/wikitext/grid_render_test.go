package wikitext

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

var wlink = regexp.MustCompile(`\[\[([^\]|]+)(?:\|([^\]]+))?\]\]`)

func plain(s string) string {
	s = wlink.ReplaceAllStringFunc(s, func(m string) string {
		g := wlink.FindStringSubmatch(m)
		if g[2] != "" {
			return g[2]
		}
		return g[1]
	})
	s = strings.ReplaceAll(s, "''", "")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// Ground truth: NBC's Thursday in 1996–97 was Friends 8:00, The Single Guy 8:30,
// Seinfeld 9:00, Suddenly Susan 9:30, ER 10:00 — the "Must See TV" block. If the
// grid puts those in the right columns, the span resolution is right.
func TestNBC1996ThursdayMatchesReality(t *testing.T) {
	b, err := os.ReadFile("testdata/schedule-1996-97-thursday.txt")
	if err != nil {
		t.Skip("fixture missing")
	}
	tb := FindTables(string(b))[0]

	// Render it, so a failure shows the whole grid rather than one assertion.
	for r := range tb.Rows {
		var out []string
		for c := range tb.Cols {
			cell, ok := tb.At(r, c)
			s := ""
			if ok && cell.Row == r && cell.Col == c {
				s = plain(cell.Text)
			} else if ok {
				s = "·"
			}
			if len(s) > 16 {
				s = s[:15] + "…"
			}
			out = append(out, fmt.Sprintf("%-17s", s))
		}
		t.Logf("  %s", strings.Join(out, ""))
	}

	// Which column is 8:00 must be READ, never assumed. This table has two
	// leading header columns — the network, then the season (Fall/Winter) — so
	// times begin at column 2. Other years lead differently, and an assumed
	// offset silently reports every show an hour early or late.
	slot := map[string]int{}
	for _, c := range tb.Cells {
		if !c.Header {
			continue
		}
		if m := timeHdr.FindStringSubmatch(plain(c.Text)); m != nil {
			slot[m[1]] = c.Col
		}
	}
	if len(slot) < 4 {
		t.Fatalf("only found %d time columns: %v", len(slot), slot)
	}
	t.Logf("  time columns: %v", slot)

	nbcRow := -1
	for _, c := range tb.Cells {
		if plain(c.Text) == "NBC" {
			nbcRow = c.Row
			break
		}
	}
	if nbcRow < 0 {
		t.Fatal("no NBC row found")
	}
	for _, w := range []struct{ at, show string }{
		{"8:00", "Friends"}, {"8:30", "The Single Guy"},
		{"9:00", "Seinfeld"}, {"10:00", "ER"},
	} {
		col, ok := slot[w.at]
		if !ok {
			t.Errorf("no %s column", w.at)
			continue
		}
		c, ok := tb.At(nbcRow, col)
		if !ok {
			t.Errorf("NBC at %s: nothing there", w.at)
			continue
		}
		if got := plain(c.Text); !strings.Contains(got, w.show) {
			t.Errorf("NBC at %s = %q, want %q", w.at, got, w.show)
		}
	}
}

var timeHdr = regexp.MustCompile(`^(\d{1,2}:\d{2})\s*[ap]\.?m`)
