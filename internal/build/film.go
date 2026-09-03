package build

import (
	"fmt"
	"os"
	"regexp"

	"github.com/szatmary/filmstock/internal/dump"
	"github.com/szatmary/filmstock/internal/record"
	"github.com/szatmary/filmstock/internal/wikitext"
)

var reInfoboxFilm = regexp.MustCompile(`(?i)\{\{\s*infobox film(\b|\s|\||\})`)

// buildFilm returns the film record for a page, or nil if the page is not a
// film. This is the single definition of "what counts as a film and how it is
// parsed" — both the legacy writer and `extract` call it, so they cannot drift
// apart and start disagreeing about what a given page is.
func buildFilm(p dump.Page) *record.Movie {
	if p.NS != 0 {
		return nil
	}
	if !reInfoboxFilm.MatchString(p.Text) {
		return nil
	}
	// EXACT, not prefix: "{{Infobox film awards}}" and "{{Infobox Film
	// festival}}" both start with "Infobox film", and matching them by prefix
	// put 2,418 awards ceremonies and 1,647 festivals into the film index with
	// year 0 and no cast. They are real media and get their own record type —
	// see event.go. This is the same collision findTemplateExact was introduced
	// to fix on the television side.
	body, ok := wikitext.FindTemplateExact(p.Text, "Infobox film")
	if !ok {
		return nil
	}
	m := buildMovie(p.Title, p.ID, wikitext.ParseInfobox(body))
	m.Overview, m.Plot = wikitext.ExtractLeadAndPlot(p.Text)
	m.Genre = wikitext.ExtractGenres(p.Text)
	return m
}

var reSlug = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(1)
}
