package main

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/szatmary/filmstock"
)

// Both spellings occur; MediaWiki template names are case-insensitive in their
// first letter and Cannes uses "Infobox Film festival".
var eventTemplates = []struct {
	name string
	kind string
}{
	{"Infobox film awards", filmstock.EventAwardCeremony},
	{"Infobox Film festival", filmstock.EventFilmFestival},
	{"Infobox film festival", filmstock.EventFilmFestival},
}

// reEdition pulls the ordinal out of a title like "53rd NAACP Image Awards" or
// "1st Golden Raspberry Awards". The infobox `number` field is preferred; this
// is the fallback for the many articles that omit it.
var reEdition = regexp.MustCompile(`^(\d+)(?:st|nd|rd|th)\b`)

// reYearIn finds a four-digit year anywhere in a date or title.
var reYearIn = regexp.MustCompile(`\b(1[89]\d{2}|20\d{2})\b`)

// reDeliveryLabel matches the "how it was carried" prefixes ceremonies put in
// their network field: "Broadcast: ABC<br>Streaming: Hulu".
var reDeliveryLabel = regexp.MustCompile(`(?i)^(broadcast|streaming|radio|television|tv|online|simulcast|worldwide)\s*:\s*`)

// networkList splits the broadcaster field and drops those labels. The label
// says HOW the ceremony reached viewers, not WHO carried it, and leaving it
// inside the value makes the field unqueryable — a search for "ABC" can never
// match the string "Broadcast: ABC". The raw field is passed in rather than a
// cleaned one because splitList needs the <br> separators cleanText removes.
func networkList(raw string) []string {
	out := []string{}
	for _, v := range splitList(raw) {
		v = strings.TrimSpace(reDeliveryLabel.ReplaceAllString(v, ""))
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// buildEvent returns the event record for a page, or nil if the page is not one.
func buildEvent(p Page) *filmstock.Event {
	if p.NS != 0 {
		return nil
	}
	for _, t := range eventTemplates {
		body, ok := findTemplateExact(p.Text, t.name)
		if !ok {
			continue
		}
		return newEvent(p, t.kind, parseInfobox(body))
	}
	return nil
}

func newEvent(p Page, kind string, ib map[string]string) *filmstock.Event {
	e := &filmstock.Event{
		Title:      p.Title,
		PageID:     p.ID,
		WikiURL:    "https://en.wikipedia.org/wiki/" + strings.ReplaceAll(url.PathEscape(p.Title), "%20", "_"),
		Kind:       kind,
		RawInfobox: ib,
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := cleanText(ib[k]); v != "" {
				return v
			}
		}
		return ""
	}

	e.Award = get("award", "award_link", "name", "awards")
	e.Date = get("date")
	e.Venue = get("site", "venue")
	e.Location = get("location", "country")
	e.Network = networkList(ib["network"] + "\n" + ib["broadcaster"])
	e.Previous = get("previous", "last")
	e.Next = get("next")
	e.BestFilm = get("film", "best_film")
	e.MostWins = get("most_wins")
	e.MostNominations = get("most_nominations")
	e.OpeningFilm = get("opening")
	e.ClosingFilm = get("closing")
	e.MainCompetition = get("main")
	e.Founded = get("founded")
	e.CoverImageFile = get("image")
	host := ib["host"]
	if host == "" {
		host = ib["hosts"]
	}
	if kind == filmstock.EventAwardCeremony {
		e.Hosts = splitPeople(host)
	} else {
		e.Organizer = cleanText(reComment.ReplaceAllString(host, ""))
	}

	if n, err := strconv.Atoi(strings.TrimSpace(ib["number"])); err == nil && n > 0 {
		e.Edition = n
	} else if m := reEdition.FindStringSubmatch(strings.TrimSpace(p.Title)); m != nil {
		e.Edition, _ = strconv.Atoi(m[1])
	}
	// Year from the date, else from the title ("2017 Cannes Film Festival").
	if m := reYearIn.FindStringSubmatch(e.Date); m != nil {
		e.Year, _ = strconv.Atoi(m[1])
	} else if m := reYearIn.FindStringSubmatch(p.Title); m != nil {
		e.Year, _ = strconv.Atoi(m[1])
	}

	e.Overview, _ = extractLeadAndPlot(p.Text)
	return e
}
