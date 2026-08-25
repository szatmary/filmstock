package wikitext

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reason this code exists: a cell's position in the source is not its
// position on screen once spans are involved, and in a schedule that difference
// is what hour a show aired.
func TestRowspanShiftsLaterCells(t *testing.T) {
	src := `{| class="wikitable"
|-
! Network !! 8:00 !! 8:30 !! 9:00
|-
! rowspan="2"|ABC
| A800
| A830
| A900
|-
| B800
| B830
| B900
|}`
	tabs := FindTables(src)
	if len(tabs) != 1 {
		t.Fatalf("found %d tables, want 1", len(tabs))
	}
	tb := tabs[0]
	// Row 2's first cell must land in column 1, not column 0: column 0 is still
	// occupied by ABC's rowspan. Getting this wrong shifts every show one slot.
	c, ok := tb.At(2, 1)
	if !ok || c.Text != "B800" {
		t.Errorf("row 2 col 1 = %q (found=%v), want B800", c.Text, ok)
	}
	if c, _ := tb.At(2, 0); c.Text != "ABC" {
		t.Errorf("row 2 col 0 = %q, want the spanning ABC header", c.Text)
	}
	if c, _ := tb.At(2, 3); c.Text != "B900" {
		t.Errorf("row 2 col 3 = %q, want B900", c.Text)
	}
}

// colspan is how long a show is: two half-hour columns is an hour.
func TestColspanGivesDuration(t *testing.T) {
	src := `{| class="wikitable"
|-
! Net !! 8:00 !! 8:30 !! 9:00 !! 9:30
|-
! NBC
| colspan="2"|''[[Friends]]''
| colspan="2"|''[[ER (TV series)|ER]]''
|}`
	tb := FindTables(src)[0]
	c, _ := tb.At(1, 1)
	if c.ColSpan != 2 || !strings.Contains(c.Text, "Friends") {
		t.Errorf("8:00 cell = %q colspan=%d, want Friends spanning 2", c.Text, c.ColSpan)
	}
	// The same cell must answer for 8:30, since it covers that slot.
	if c2, _ := tb.At(1, 2); c2.Text != c.Text {
		t.Errorf("8:30 should still be Friends, got %q", c2.Text)
	}
	if c3, _ := tb.At(1, 3); !strings.Contains(c3.Text, "ER") {
		t.Errorf("9:00 = %q, want ER", c3.Text)
	}
}

// A "|" inside a wikilink is a display-text separator, not a cell divider.
func TestPipeInsideLinkDoesNotSplitCells(t *testing.T) {
	src := `{| class="wikitable"
|-
| [[Murder One (TV series)|Murder One]] || [[ER (TV series)|ER]]
|}`
	tb := FindTables(src)[0]
	if got := len(tb.Cells); got != 2 {
		for _, c := range tb.Cells {
			t.Logf("  cell %q", c.Text)
		}
		t.Fatalf("split into %d cells, want 2", got)
	}
	if c, _ := tb.At(0, 0); !strings.Contains(c.Text, "Murder One") {
		t.Errorf("first cell = %q", c.Text)
	}
}

func TestAttributesAreStripped(t *testing.T) {
	src := `{| class="wikitable"
|-
| style="background:#6699CC;"|''[[Turning Point (TV program)|Turning Point]]''
|}`
	tb := FindTables(src)[0]
	c, _ := tb.At(0, 0)
	if strings.Contains(c.Text, "background") {
		t.Errorf("styling leaked into the content: %q", c.Text)
	}
	if !strings.Contains(c.Text, "Turning Point") {
		t.Errorf("content lost: %q", c.Text)
	}
}

// The real articles, across 68 years. Shape, not content: every one must resolve
// to a grid with a plausible number of columns and no cell off the edge.
func TestRealScheduleFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/schedule-*-thursday.txt")
	if err != nil || len(files) == 0 {
		t.Skip("no fixtures")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		tabs := FindTables(string(b))
		if len(tabs) == 0 {
			t.Errorf("%s: no tables found", filepath.Base(f))
			continue
		}
		tb := tabs[0]
		if tb.Cols < 3 || tb.Rows < 2 {
			t.Errorf("%s: grid is %dx%d, too small to be a schedule", filepath.Base(f), tb.Rows, tb.Cols)
		}
		var linked int
		for _, c := range tb.Cells {
			if c.Col+c.ColSpan > tb.Cols || c.Row+c.RowSpan > tb.Rows {
				t.Errorf("%s: cell %q extends past the grid", filepath.Base(f), c.Text)
			}
			if strings.Contains(c.Text, "[[") {
				linked++
			}
		}
		t.Logf("  %-34s %2dx%-2d cells=%-4d linked=%d",
			filepath.Base(f), tb.Rows, tb.Cols, len(tb.Cells), linked)
		if linked == 0 {
			t.Errorf("%s: no linked shows — the join to series records would be empty",
				filepath.Base(f))
		}
	}
}
