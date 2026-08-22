package main

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Award ceremonies and film festivals as a first-class record type.
//
// These arrived in the database by accident: `{{Infobox film awards}}` and
// `{{Infobox Film festival}}` both satisfy a PREFIX test for `{{Infobox film}}`,
// so 2,418 awards ceremonies and 1,647 festivals were indexed as films — with
// year 0, no cast and no director, sitting inert in film search results. It is
// the same collision that put standalone episode articles into television search, fixed
// there with findTemplateExact and assumed harmless here.
//
// The fix is not to drop them. A ceremony is broadcast on a network and a
// festival streams its programme: they are media in their own right, with their
// own natural key (page_id), their own schema, and their own answerable
// questions — "who hosted the 53rd NAACP Image Awards", "what opened Cannes in
// 2017". They were being thrown away as a side effect of being mistyped.
//
// What the infoboxes actually carry, which is why this is worth typing properly:
//
//	film awards     award, number, date, host, site, network, most_wins,
//	                most_nominations, film (the best-picture winner), previous/next
//	film festival   name, number, date, location, host, opening, closing, main,
//	                awards, founded, previous/next
//
// `previous`/`next` make each ceremony an ordered series — the same structure
// television seasons have, and the reason `number` is parsed rather than kept as
// prose.

type Event struct {
	Title   string `json:"title"`
	PageID  int    `json:"page_id"`
	WikiURL string `json:"wikipedia_url"`

	// Kind separates the two, because they answer different questions and a
	// single "event" bucket would make both unqueryable.
	Kind    string `json:"kind"` // award_ceremony | film_festival
	Award   string `json:"award,omitempty"`
	Edition int    `json:"edition,omitempty"` // 53rd -> 53
	Date    string `json:"date,omitempty"`
	Year    int    `json:"year,omitempty"`

	// Only ceremonies have a human host. In {{Infobox Film festival}} the same
	// parameter names the ORGANISING BODY — "Toronto International Film Festival
	// Group", "Istanbul Foundation for Culture and Arts" — so parsing it as a
	// Person put 109 organisations into the people table. Kept as a plain string
	// rather than guessed at: a few festivals really do list a human compère
	// (Cannes 2004: Laura Morante), and there is no stated fact distinguishing
	// them, so we record what the field says instead of inventing a type for it.
	Hosts     []Person `json:"hosts,omitempty"`
	Organizer string   `json:"organizer,omitempty"`
	Venue     string   `json:"venue,omitempty"`
	Location  string   `json:"location,omitempty"`
	Network   []string `json:"network,omitempty"` // televised

	Previous string `json:"previous,omitempty"`
	Next     string `json:"next,omitempty"`

	BestFilm        string `json:"best_film,omitempty"`
	MostWins        string `json:"most_wins,omitempty"`
	MostNominations string `json:"most_nominations,omitempty"`
	OpeningFilm     string `json:"opening_film,omitempty"`
	ClosingFilm     string `json:"closing_film,omitempty"`
	MainCompetition string `json:"main_competition,omitempty"`
	Founded         string `json:"founded,omitempty"`

	Overview       string            `json:"overview,omitempty"`
	CoverImageFile string            `json:"cover_image_file,omitempty"`
	RawInfobox     map[string]string `json:"raw_infobox,omitempty"`
}

const (
	eventAwardCeremony = "award_ceremony"
	eventFilmFestival  = "film_festival"
)

// Both spellings occur; MediaWiki template names are case-insensitive in their
// first letter and Cannes uses "Infobox Film festival".
var eventTemplates = []struct {
	name string
	kind string
}{
	{"Infobox film awards", eventAwardCeremony},
	{"Infobox Film festival", eventFilmFestival},
	{"Infobox film festival", eventFilmFestival},
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
func buildEvent(p Page) *Event {
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

func newEvent(p Page, kind string, ib map[string]string) *Event {
	e := &Event{
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
	if kind == eventAwardCeremony {
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
