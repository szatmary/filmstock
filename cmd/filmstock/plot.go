package main

import (
	"regexp"
	"strings"
)

const (
	maxOverview = 2000
	maxPlot     = 6000
)

// reFirstHeading matches the first section heading (level 2+), which ends the lead.
var reFirstHeading = regexp.MustCompile(`(?m)^==`)

// rePlotHeading matches a Plot/Synopsis-style section heading.
var rePlotHeading = regexp.MustCompile(`(?im)^=+\s*(?:plot(?:\s+summary)?|synopsis|plot\s+synopsis|premise|storyline)\s*=+\s*$`)

// reLevel2Heading matches the next level-2 heading (== X ==), used as a section
// boundary. Level-3+ subheadings (=== … ===) do NOT match, so subsections stay
// inside the captured section.
var reLevel2Heading = regexp.MustCompile(`(?m)^==[^=]`)

// extractLeadAndPlot returns a cleaned overview (the article lead, before the
// first heading) and the Plot/Synopsis section text, both plain-text and length
// capped. Either may be "".
func extractLeadAndPlot(text string) (overview, plot string) {
	lead := text
	if loc := reFirstHeading.FindStringIndex(text); loc != nil {
		lead = text[:loc[0]]
	}
	overview = trimLen(cleanText(lead), maxOverview)

	if loc := rePlotHeading.FindStringIndex(text); loc != nil {
		rest := text[loc[1]:]
		if b := reLevel2Heading.FindStringIndex(rest); b != nil {
			rest = rest[:b[0]]
		}
		plot = trimLen(cleanText(rest), maxPlot)
	}
	return overview, plot
}

// fullPlainText cleans an entire article to prose for the full-text corpus:
// refs/templates/markup stripped, section headings and paragraphs preserved.
// This is the raw-source prose — kept on disk, NOT in the search record.
func fullPlainText(text string) string {
	return trimLen(cleanText(text), 120000)
}

// trimLen truncates s to at most n bytes, backing up to the last space so words
// aren't split, and appends an ellipsis when truncated.
func trimLen(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "…"
}
