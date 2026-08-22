package main

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Person is one credited person: display Name plus the Wiki link target (the
// Wikipedia article title), which is the identity we resolve to a Wikidata Q-id.
// Wiki is "" for unlinked / red-linked names.
type Person struct {
	Name string `json:"name"`
	Wiki string `json:"wiki,omitempty"`
}

// Movie is the structured record we emit as JSON, one file per film.
type Movie struct {
	Title               string   `json:"title"`
	PageID              int      `json:"page_id"`
	WikiURL             string   `json:"wikipedia_url"`
	Overview            string   `json:"overview,omitempty"`
	Plot                string   `json:"plot,omitempty"`
	Genre               []string `json:"genre,omitempty"`
	ReleaseDates        []string `json:"release_dates,omitempty"`
	Director            []Person `json:"director,omitempty"`
	Producer            []Person `json:"producer,omitempty"`
	Writer              []Person `json:"writer,omitempty"`
	Starring            []Person `json:"starring,omitempty"`
	Music               []Person `json:"music,omitempty"`
	Cinematography      []Person `json:"cinematography,omitempty"`
	Editing             []Person `json:"editing,omitempty"`
	ProductionCompanies []string `json:"production_companies,omitempty"`
	Distributor         []string `json:"distributor,omitempty"`
	Country             []string `json:"country,omitempty"`
	Language            []string `json:"language,omitempty"`
	Runtime             string   `json:"runtime,omitempty"`
	Budget              string   `json:"budget,omitempty"`
	Gross               string   `json:"gross,omitempty"`
	CoverImageFile      string   `json:"cover_image_file,omitempty"`
	CoverImageURL       string   `json:"cover_image_url,omitempty"`
	// Raw holds the untouched infobox parameters for later expansion.
	Raw map[string]string `json:"raw_infobox,omitempty"`
}

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
		txt := cleanText(raw)
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

// filePathURL builds a Special:FilePath URL, which MediaWiki 302-redirects to
// the real file regardless of whether it lives on Wikimedia Commons or is a
// non-free file hosted locally on en.wikipedia. A positive width yields a
// resized thumbnail instead of the full-resolution original.
func filePathURL(file string, width int) string {
	name := strings.TrimSpace(file)
	for _, p := range []string{"File:", "file:", "Image:", "image:"} {
		name = strings.TrimPrefix(name, p)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	u := "https://en.wikipedia.org/wiki/Special:FilePath/" + url.PathEscape(strings.ReplaceAll(name, " ", "_"))
	if width > 0 {
		u += "?width=" + strconv.Itoa(width)
	}
	return u
}

// coverImageURL returns the cleaned file name and a resolver URL for it.
func coverImageURL(filename string) (string, string) {
	name := strings.TrimSpace(filename)
	for _, p := range []string{"File:", "file:", "Image:", "image:"} {
		name = strings.TrimPrefix(name, p)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ""
	}
	return name, filePathURL(name, 0)
}

// buildMovie constructs a Movie from a page title/id and its infobox params.
func buildMovie(title string, pageID int, ib map[string]string) *Movie {
	m := &Movie{
		Title:   title,
		PageID:  pageID,
		WikiURL: "https://en.wikipedia.org/wiki/" + strings.ReplaceAll(url.PathEscape(title), "%20", "_"),
		Raw:     ib,
	}
	m.ReleaseDates = parseReleaseDates(ib["released"])
	m.Director = splitPeople(ib["director"])
	m.Producer = splitPeople(ib["producer"])
	m.Writer = mergePeople(ib["writer"], ib["screenplay"], ib["story"])
	m.Starring = splitPeople(ib["starring"])
	m.Music = splitPeople(ib["music"])
	m.Cinematography = splitPeople(ib["cinematography"])
	m.Editing = splitPeople(ib["editing"])
	m.ProductionCompanies = mergeLists(ib["production_companies"], ib["studio"])
	m.Distributor = splitList(ib["distributor"])
	m.Country = splitList(ib["country"])
	m.Language = splitList(ib["language"])
	m.Runtime = cleanText(ib["runtime"])
	m.Budget = cleanText(ib["budget"])
	m.Gross = cleanText(ib["gross"])
	if img := ib["image"]; img != "" {
		m.CoverImageFile, m.CoverImageURL = coverImageURL(cleanBareFilename(img))
	}
	return m
}

func mergeLists(vals ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vals {
		for _, item := range splitList(v) {
			if !seen[item] {
				seen[item] = true
				out = append(out, item)
			}
		}
	}
	return out
}

// mergePeople merges several person fields, de-duped by identity (Wiki else Name).
func mergePeople(vals ...string) []Person {
	seen := map[string]bool{}
	var out []Person
	for _, v := range vals {
		for _, p := range splitPeople(v) {
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
	v = reComment.ReplaceAllString(v, "")
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
