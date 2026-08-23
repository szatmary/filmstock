package build

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/dump"
	"github.com/szatmary/filmstock/internal/wikitext"
)

// personInfoboxes are the biography infoboxes worth reading. Wikipedia has no
// single one: {{Infobox person}} is the general case, and the rest are separate
// templates with overlapping-but-different parameter sets, not redirects to it.
// Matched EXACTLY, for the same reason films are — "Infobox person" is a prefix
// of nothing useful, but "Infobox artist" is a prefix of "Infobox artist
// discography", and prefix matching is what put 2,418 award ceremonies into the
// film index.
var personInfoboxes = []string{
	"Infobox person",
	"Infobox actor",
	"Infobox musical artist",
	"Infobox writer",
	"Infobox artist",
	"Infobox comedian",
	"Infobox model",
	"Infobox architect",
	"Infobox academic",
	"Infobox officeholder",
	"Infobox military person",
	"Infobox sportsperson",
}

// reDateTemplate pulls a date out of {{Birth date|1937|11|30}} and its many
// cousins — birth date and age, death date and age, dda, bda. The named
// parameters (df=yes, mf=y) are skipped by taking only bare numeric arguments.
var reDateTemplate = regexp.MustCompile(`(?i)\{\{\s*(?:birth[ _-]?date(?:[ _-]and[ _-]age)?|death[ _-]?date(?:[ _-]and[ _-]age)?|bda|dda)\s*\|([^}]*)\}\}`)

// parseDateTemplate returns YYYY-MM-DD, or YYYY when that is all the article
// states. A death-date template carries the death date first and the birth date
// after it, so only the leading run of numbers is taken.
func parseDateTemplate(s string) string {
	m := reDateTemplate.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	var nums []int
	for _, part := range strings.Split(m[1], "|") {
		part = strings.TrimSpace(part)
		if part == "" || strings.ContainsRune(part, '=') {
			continue // df=yes, mf=y and friends
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			break // stop at the first non-numeric: the rest is not this date
		}
		nums = append(nums, n)
		if len(nums) == 3 {
			break
		}
	}
	switch {
	case len(nums) >= 3 && nums[0] > 1000:
		return pad4(nums[0]) + "-" + pad2(nums[1]) + "-" + pad2(nums[2])
	case len(nums) >= 1 && nums[0] > 1000:
		return pad4(nums[0])
	}
	return ""
}

func pad4(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// buildBiography reads a person's own article. It returns nil for any page that
// is not a biography — which is most of the dump — so the caller can hand it
// every page cheaply.
//
// The result is keyed by article title, NOT by name: the title is what a credit
// links to, and it is the only thing that ties this article to the people
// discovered from film and television infoboxes.
func buildBiography(p dump.Page) *filmstock.PersonBio {
	if p.NS != 0 {
		return nil
	}
	var ib map[string]string
	for _, name := range personInfoboxes {
		if body, ok := wikitext.FindTemplateExact(p.Text, name); ok {
			ib = wikitext.ParseInfobox(body)
			break
		}
	}
	if ib == nil {
		return nil
	}

	b := &filmstock.PersonBio{
		BirthName:   wikitext.CleanText(ib["birth_name"]),
		BirthPlace:  wikitext.CleanText(ib["birth_place"]),
		DeathPlace:  wikitext.CleanText(ib["death_place"]),
		YearsActive: wikitext.CleanText(ib["years_active"]),
		Nationality: wikitext.CleanText(ib["nationality"]),
		Image:       wikitext.CleanText(ib["image"]),
		Occupation:  wikitext.SplitList(ib["occupation"]),
	}
	b.BirthDate = parseDateTemplate(ib["birth_date"])
	if b.BirthDate == "" {
		b.BirthDate = wikitext.CleanText(ib["birth_date"])
	}
	b.DeathDate = parseDateTemplate(ib["death_date"])
	if b.DeathDate == "" {
		b.DeathDate = wikitext.CleanText(ib["death_date"])
	}
	// The lead is the encyclopaedic summary; the rest of a biography article is
	// career narrative that belongs in the corpus, not in a record.
	lead, _ := wikitext.ExtractLeadAndPlot(p.Text)
	b.Overview = wikitext.TrimLen(lead, 1500)

	// An infobox with nothing in it is not evidence of a biography.
	if b.BirthDate == "" && b.DeathDate == "" && b.Overview == "" &&
		len(b.Occupation) == 0 && b.BirthPlace == "" {
		return nil
	}
	return b
}
