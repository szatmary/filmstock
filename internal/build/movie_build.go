package build

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/wikitext"
)

var reFilmDate = regexp.MustCompile(`\{\{[Ff]ilm date\|([^}]*)\}\}`)
var reStartDate = regexp.MustCompile(`\{\{[Ss]tart date\|([^}]*)\}\}`)
var reEndDate = regexp.MustCompile(`\{\{[Ee]nd date\|([^}]*)\}\}`)
var reBareDate = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)

var months = map[string]string{
	"january": "01", "february": "02", "march": "03", "april": "04",
	"may": "05", "june": "06", "july": "07", "august": "08",
	"september": "09", "october": "10", "november": "11", "december": "12",
}

// parseReleaseDates extracts ISO-ish dates from a release field.
func parseReleaseDates(raw string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	dateFromTemplateArgs := func(args string) {
		fields := strings.Split(args, "|")
		var nums []string
		for _, f := range fields {
			f = strings.TrimSpace(f)
			if strings.Contains(f, "=") { // df=y, mf=y
				continue
			}
			if regexp.MustCompile(`^\d+$`).MatchString(f) {
				nums = append(nums, f)
			}
		}
		if len(nums) >= 3 && len(nums[0]) == 4 {
			add(nums[0] + "-" + pad(nums[1]) + "-" + pad(nums[2]))
		} else if len(nums) >= 1 && len(nums[0]) == 4 {
			add(nums[0])
		}
	}
	for _, m := range reFilmDate.FindAllStringSubmatch(raw, -1) {
		dateFromTemplateArgs(m[1])
	}
	for _, m := range reStartDate.FindAllStringSubmatch(raw, -1) {
		dateFromTemplateArgs(m[1])
	}
	for _, m := range reEndDate.FindAllStringSubmatch(raw, -1) {
		dateFromTemplateArgs(m[1])
	}
	for _, m := range reBareDate.FindAllStringSubmatch(raw, -1) {
		add(m[1] + "-" + m[2] + "-" + m[3])
	}
	// Fallback: "Month DD, YYYY" in cleaned text.
	if len(out) == 0 {
		txt := wikitext.CleanText(raw)
		re := regexp.MustCompile(`(?i)([A-Za-z]+)\s+(\d{1,2}),\s*(\d{4})`)
		for _, m := range re.FindAllStringSubmatch(txt, -1) {
			if mm, ok := months[strings.ToLower(m[1])]; ok {
				add(m[3] + "-" + mm + "-" + pad(m[2]))
			}
		}
		// year only
		if len(out) == 0 {
			re2 := regexp.MustCompile(`\b(1[89]\d{2}|20\d{2})\b`)
			if m := re2.FindStringSubmatch(txt); m != nil {
				add(m[1])
			}
		}
	}
	return out
}

func pad(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

// buildMovie constructs a Movie from a page title/id and its infobox params.
func buildMovie(title string, pageID int, ib map[string]string) *filmstock.Movie {
	m := &filmstock.Movie{
		Title:   title,
		PageID:  pageID,
		WikiURL: "https://en.wikipedia.org/wiki/" + strings.ReplaceAll(url.PathEscape(title), "%20", "_"),
		Raw:     ib,
	}
	m.ReleaseDates = parseReleaseDates(ib["released"])
	m.Director = wikitext.SplitPeople(ib["director"])
	m.Producer = wikitext.SplitPeople(ib["producer"])
	m.Writer = mergePeople(ib["writer"], ib["screenplay"], ib["story"])
	m.Starring = wikitext.SplitPeople(ib["starring"])
	m.Music = wikitext.SplitPeople(ib["music"])
	m.Cinematography = wikitext.SplitPeople(ib["cinematography"])
	m.Editing = wikitext.SplitPeople(ib["editing"])
	m.ProductionCompanies = mergeLists(ib["production_companies"], ib["studio"])
	m.Distributor = wikitext.SplitList(ib["distributor"])
	m.Country = wikitext.SplitList(ib["country"])
	m.Language = wikitext.SplitList(ib["language"])
	m.Runtime = wikitext.CleanText(ib["runtime"])
	m.Budget = wikitext.CleanText(ib["budget"])
	m.Gross = wikitext.CleanText(ib["gross"])
	if img := ib["image"]; img != "" {
		m.CoverImageFile, m.CoverImageURL = filmstock.CoverImageURL(cleanBareFilename(img))
	}
	return m
}

func mergeLists(vals ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vals {
		for _, item := range wikitext.SplitList(v) {
			if !seen[item] {
				seen[item] = true
				out = append(out, item)
			}
		}
	}
	return out
}

// mergePeople merges several person fields, de-duped by identity (Wiki else Name).
func mergePeople(vals ...string) []filmstock.Person {
	seen := map[string]bool{}
	var out []filmstock.Person
	for _, v := range vals {
		for _, p := range wikitext.SplitPeople(v) {
			key := p.Wiki
			if key == "" {
				key = p.Name
			}
			if !seen[key] {
				seen[key] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// cleanBareFilename removes wiki/HTML noise from an image parameter value.
func cleanBareFilename(v string) string {
	v = wikitext.ReComment.ReplaceAllString(v, "")
	v = strings.TrimSpace(v)
	// Sometimes wrapped as [[File:Foo.jpg|...]]
	if strings.HasPrefix(v, "[[") {
		inner := strings.TrimPrefix(v, "[[")
		inner = strings.TrimSuffix(inner, "]]")
		if bar := strings.Index(inner, "|"); bar >= 0 {
			inner = inner[:bar]
		}
		v = inner
	}
	return strings.TrimSpace(v)
}
