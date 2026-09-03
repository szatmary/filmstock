package build

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/szatmary/filmstock/internal/dump"
	"github.com/szatmary/filmstock/internal/record"
	"github.com/szatmary/filmstock/internal/wikitext"
)

// Both spellings occur; MediaWiki template names are case-insensitive in their
// first letter and Cannes uses "Infobox Film festival".
var eventTemplates = []struct {
	name string
	kind string
}{
	{"Infobox film awards", record.EventAwardCeremony},
	{"Infobox Film festival", record.EventFilmFestival},
	{"Infobox film festival", record.EventFilmFestival},
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

// networkLinks splits the broadcaster field, keeping link targets and dropping
// the delivery labels. The label says HOW the ceremony reached viewers, not WHO
// carried it, and leaving it inside the value makes the field unqueryable — a
// search for "ABC" can never match "Broadcast: ABC". The raw field is passed in
// rather than a cleaned one because the split needs the <br> separators that
// cleaning removes.
func networkLinks(raws ...string) []record.Link {
	out := []record.Link{}
	for _, l := range mergeLinks(raws...) {
		l.Name = strings.TrimSpace(reDeliveryLabel.ReplaceAllString(l.Name, ""))
		if l.Name == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// buildEvent returns the event record for a page, or nil if the page is not one.
func buildEvent(p dump.Page) *record.Event {
	if p.NS != 0 {
		return nil
	}
	for _, t := range eventTemplates {
		body, ok := wikitext.FindTemplateExact(p.Text, t.name)
		if !ok {
			continue
		}
		return newEvent(p, t.kind, wikitext.ParseInfobox(body))
	}
	return nil
}

func newEvent(p dump.Page, kind string, ib map[string]string) *record.Event {
	e := &record.Event{
		Title:      p.Title,
		PageID:     p.ID,
		WikiURL:    "https://en.wikipedia.org/wiki/" + strings.ReplaceAll(url.PathEscape(p.Title), "%20", "_"),
		Kind:       kind,
		RawInfobox: ib,
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := wikitext.CleanText(ib[k]); v != "" {
				return v
			}
		}
		return ""
	}

	e.Award = get("award", "award_link", "name", "awards")
	e.Date = get("date")
	e.Venue = get("site", "venue")
	e.Location = get("location", "country")
	e.Network = networkLinks(ib["network"], ib["broadcaster"])
	e.Previous = mergeLinks(ib["previous"], ib["last"])
	e.Next = mergeLinks(ib["next"])
	e.BestFilm = mergeLinks(ib["film"], ib["best_film"])
	e.MostWins = mergeLinks(ib["most_wins"])
	e.MostNominations = mergeLinks(ib["most_nominations"])
	e.OpeningFilm = mergeLinks(ib["opening"])
	e.ClosingFilm = mergeLinks(ib["closing"])
	e.MainCompetition = mergeLinks(ib["main"])
	e.Founded = get("founded")
	e.CoverImageFile = get("image")
	host := ib["host"]
	if host == "" {
		host = ib["hosts"]
	}
	if kind == record.EventAwardCeremony {
		e.Hosts = wikitext.SplitPeople(host)
	} else {
		e.Organizer = wikitext.CleanText(wikitext.ReComment.ReplaceAllString(host, ""))
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

	e.Overview, _ = wikitext.ExtractLeadAndPlot(p.Text)
	return e
}
