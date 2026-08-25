package build

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/dump"
	"github.com/szatmary/filmstock/internal/wikitext"
)

// Turning a network television schedule article into slots.
//
// The article is one table per night. Reading it means three things that all
// have to be taken from the table rather than assumed:
//
//   - which column is 8:00. The tables lead with header columns — the network,
//     and usually a season sub-column — and how many varies by year. An assumed
//     offset reports every programme an hour out.
//   - how long a programme runs. That is how many columns its cell spans, not
//     anything written in it.
//   - which arrangement it is. Networks reshuffle at midseason, so one slot has
//     a Fall occupant and a Winter one, carried as separate rows under the same
//     network.

var (
	reScheduleTitle = regexp.MustCompile(`(?i)network television schedule`)
	reSeason        = regexp.MustCompile(`^(\d{4}[–-]\d{2,4})`)
	// Case-insensitive: the 1950s articles write "7:00 PM", later ones
	// "7:00 p.m.". A case-sensitive match silently found no time columns at
	// all in the earliest decades, so those articles produced nothing.
	reTimeHeader = regexp.MustCompile(`(?i)^(\d{1,2}):(\d{2})\s*([ap])\.?m`)
	reDayHeading = regexp.MustCompile(`(?m)^==\s*(Sunday|Monday|Tuesday|Wednesday|Thursday|Friday|Saturday)\s*==\s*$`)
	// A {{small|...}} block, then the parenthesised groups inside it. Matching
	// "(...)}}" directly missed every cell that carries more than one note —
	// {{small|(4/16.9)<br/>(Tied with ''[[3rd Rock from the Sun]]'')}} silently
	// yielded no rating at all, which is how Friends came out unranked in a
	// season it placed 4th.
	reSmall    = regexp.MustCompile(`\{\{\s*small\s*\|(.*?)\}\}`)
	reParen    = regexp.MustCompile(`\(([^()]*)\)`)
	reNielsen  = regexp.MustCompile(`^(\d+)/(\d+(?:\.\d+)?)$`)
	reLink     = regexp.MustCompile(`\[\[([^\]|]+)(?:\|([^\]]+))?\]\]`)
	reTemplate = regexp.MustCompile(`\{\{[^{}]*\}\}`)
	reTag      = regexp.MustCompile(`<[^>]+>`)
)

// isScheduleArticle reports whether a page is one of these grids.
func isScheduleArticle(title string) bool { return reScheduleTitle.MatchString(title) }

// buildSchedule extracts every slot from a schedule article.
func buildSchedule(p dump.Page) *filmstock.Schedule {
	if p.NS != 0 || !isScheduleArticle(p.Title) {
		return nil
	}
	s := &filmstock.Schedule{
		PageID:  int(p.ID),
		Title:   p.Title,
		Daypart: daypartOf(p.Title),
	}
	if m := reSeason.FindStringSubmatch(p.Title); m != nil {
		s.Season = m[1]
	}

	// Split into day sections. Only the per-night grids are read: articles also
	// carry a "By network" section that re-presents the same programmes in a
	// different shape, and reading both would double every entry.
	locs := reDayHeading.FindAllStringSubmatchIndex(p.Text, -1)
	for i, loc := range locs {
		day := p.Text[loc[2]:loc[3]]
		end := len(p.Text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if next := strings.Index(p.Text[loc[1]:end], "\n=="); next >= 0 {
			end = loc[1] + next
		}
		for _, tb := range wikitext.FindTables(p.Text[loc[1]:end]) {
			s.Entries = append(s.Entries, readGrid(tb, day)...)
		}
	}
	if len(s.Entries) == 0 {
		return nil
	}
	return s
}

func daypartOf(title string) string {
	switch {
	case strings.Contains(title, "(daytime)"):
		return "daytime"
	case strings.Contains(title, "(late night)"):
		return "late night"
	case strings.Contains(title, "(Saturday morning)"):
		return "Saturday morning"
	default:
		return "prime time"
	}
}

// readGrid turns one night's table into entries.
func readGrid(tb *wikitext.Table, day string) []filmstock.ScheduleEntry {
	// Which column is which time, read from the header row.
	times := map[int]string{}
	firstTime := -1
	for _, c := range tb.Cells {
		if !c.Header {
			continue
		}
		m := reTimeHeader.FindStringSubmatch(cleanCell(c.Text))
		if m == nil {
			continue
		}
		h, _ := strconv.Atoi(m[1])
		if strings.EqualFold(m[3], "p") && h != 12 {
			h += 12
		} else if strings.EqualFold(m[3], "a") && h == 12 {
			h = 0
		}
		for col := c.Col; col < c.Col+c.ColSpan; col++ {
			times[col] = pad2(h) + ":" + m[2]
		}
		if firstTime < 0 || c.Col < firstTime {
			firstTime = c.Col
		}
	}
	if firstTime < 1 {
		return nil // no time columns, or no room for a network label
	}
	slotMinutes := gridStep(times)

	var out []filmstock.ScheduleEntry
	for row := range tb.Rows {
		// Everything left of the first time column labels the row. The leftmost
		// is the network; a second, when present, is which part of the season
		// this arrangement belongs to.
		network, part := "", ""
		for col := range firstTime {
			c, ok := tb.At(row, col)
			if !ok {
				continue
			}
			txt := cleanCell(c.Text)
			if txt == "" {
				continue
			}
			if col == 0 {
				network = txt
			} else if part == "" {
				part = txt
			}
		}
		if network == "" {
			continue // the header row itself
		}
		for col := firstTime; col < tb.Cols; col++ {
			start, ok := times[col]
			if !ok {
				continue
			}
			c, found := tb.At(row, col)
			if !found || c.Header || c.Col != col || c.Row != row {
				continue // empty, a label, or a cell already reported at its origin
			}
			e := filmstock.ScheduleEntry{
				Day: day, Network: network, Part: part, Start: start,
				End: addMinutes(start, slotMinutes*c.ColSpan),
			}
			e.Title, e.ShowID, e.Rerun, e.Rank, e.Rating = readSlot(c.Text)
			if e.Title == "" {
				continue
			}
			out = append(out, e)
		}
	}
	return out
}

// readSlot pulls a programme out of one cell.
func readSlot(raw string) (title string, showID int, rerun bool, rank int, rating float64) {
	for _, blk := range reSmall.FindAllStringSubmatch(raw, -1) {
		for _, g := range reParen.FindAllStringSubmatch(blk[1], -1) {
			inner := strings.TrimSpace(g[1])
			if strings.EqualFold(inner, "R") {
				rerun = true
				continue
			}
			if n := reNielsen.FindStringSubmatch(inner); n != nil {
				rank, _ = strconv.Atoi(n[1])
				rating, _ = strconv.ParseFloat(n[2], 64)
			}
		}
	}
	// The first wikilink is the programme; later ones are footnotes or networks.
	if m := reLink.FindStringSubmatch(raw); m != nil {
		title = m[1]
		if m[2] != "" {
			title = m[2]
		}
	}
	if title == "" {
		title = cleanCell(raw)
	} else {
		title = cleanCell(title)
	}
	return title, 0, rerun, rank, rating
}

// cleanCell reduces wikitext to the text a reader sees.
func cleanCell(s string) string {
	s = reLink.ReplaceAllStringFunc(s, func(m string) string {
		g := reLink.FindStringSubmatch(m)
		if g[2] != "" {
			return g[2]
		}
		return g[1]
	})
	for range 3 { // templates nest
		s = reTemplate.ReplaceAllString(s, "")
	}
	s = reTag.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "'''", "")
	s = strings.ReplaceAll(s, "''", "")
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "'"))
}

// gridStep is how many minutes one column covers, taken from the gap between
// consecutive time headers. Most years are half-hour grids; the earliest are
// fifteen-minute ones, and assuming thirty would double every duration.
func gridStep(times map[int]string) int {
	cols := make([]int, 0, len(times))
	for c := range times {
		cols = append(cols, c)
	}
	if len(cols) < 2 {
		return 30
	}
	best := 0
	for _, a := range cols {
		for _, b := range cols {
			if b != a+1 {
				continue
			}
			if d := minutesBetween(times[a], times[b]); d > 0 && (best == 0 || d < best) {
				best = d
			}
		}
	}
	if best == 0 {
		return 30
	}
	return best
}

func minutesBetween(a, b string) int {
	ha, ma := hhmm(a)
	hb, mb := hhmm(b)
	d := (hb*60 + mb) - (ha*60 + ma)
	if d < 0 {
		d += 24 * 60
	}
	return d
}

func hhmm(s string) (int, int) {
	p := strings.SplitN(s, ":", 2)
	if len(p) != 2 {
		return 0, 0
	}
	h, _ := strconv.Atoi(p[0])
	m, _ := strconv.Atoi(p[1])
	return h, m
}

func addMinutes(t string, mins int) string {
	h, m := hhmm(t)
	total := (h*60 + m + mins) % (24 * 60)
	return pad2(total/60) + ":" + pad2(total%60)
}
