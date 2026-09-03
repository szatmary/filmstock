package record

// A Schedule is one season of a network television grid: what aired on each
// network, each night, in each half-hour slot.
//
// This is the only source in the corpus for what time a programme aired and what
// it aired against. An episode's air_date gives the day and nothing else — no
// clock time, and no ordering within an evening — so "what was opposite Friends"
// and "when did a show move slots" are answerable from here and nowhere else.
//
// The data comes from the "<year> United States network television schedule"
// articles, of which there are 288 running from 1946–47 to the present, plus
// daytime and late-night variants.
type Schedule struct {
	PageID  int    `json:"page_id"`
	Title   string `json:"title"`
	Season  string `json:"season,omitempty"`  // "1996–97"
	Daypart string `json:"daypart,omitempty"` // prime time, daytime, late night
	WikiURL string `json:"wikipedia_url,omitempty"`

	Entries []ScheduleEntry `json:"entries,omitempty"`
}

// A ScheduleEntry is one programme in one slot.
//
// A season is not one grid but several: networks reshuffle at midseason, so the
// same slot has a different occupant in Fall, Winter and Spring. Part carries
// which, and is empty when the article gives only one arrangement.
type ScheduleEntry struct {
	Day     string `json:"day"`               // Monday … Sunday
	Network string `json:"network"`           // as written: ABC, CBS, NBC, Fox …
	Start   string `json:"start"`             // "20:00", 24-hour
	End     string `json:"end"`               // "20:30"; from how many columns it spans
	Part    string `json:"part,omitempty"`    // Fall, Winter, Spring, Summer
	Title   string `json:"title"`             // as written, links resolved to text
	ShowID  int    `json:"show_id,omitempty"` // page_id when the cell links a show

	// Rerun is a slot the article marks (R): the network filled it with a
	// repeat rather than an original.
	Rerun bool `json:"rerun,omitempty"`

	// Rank and Rating are the Nielsen season rank and household rating carried
	// in some cells as {{small|(8/14.1)}} — rank 8, rating 14.1. Absent for most
	// entries and for every year the article does not annotate.
	Rank   int     `json:"rank,omitempty"`
	Rating float64 `json:"rating,omitempty"`

	// LinkTarget is the wikilink destination, used during extraction to resolve
	// ShowID and never stored: the recorded answer is the page_id, and a title
	// kept beside it would be a second identity for the same thing. Exported
	// only because the builder lives in another package.
	LinkTarget string `json:"-"`
}

// Slots returns every entry for one network on one night, in time order.
func (s *Schedule) Slots(day, network string) []ScheduleEntry {
	var out []ScheduleEntry
	for _, e := range s.Entries {
		if e.Day == day && e.Network == network {
			out = append(out, e)
		}
	}
	return out
}

// Opposite returns everything that aired against showID: entries overlapping it
// in time on the same night, on other networks.
func (s *Schedule) Opposite(showID int) []ScheduleEntry {
	var target []ScheduleEntry
	for _, e := range s.Entries {
		if e.ShowID == showID && showID != 0 {
			target = append(target, e)
		}
	}
	var out []ScheduleEntry
	for _, t := range target {
		for _, e := range s.Entries {
			if e.Network == t.Network || e.Day != t.Day || e.Part != t.Part {
				continue
			}
			if e.Start < t.End && e.End > t.Start { // overlapping half-open ranges
				out = append(out, e)
			}
		}
	}
	return out
}
