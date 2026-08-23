package build

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/wikitext"
)

// buildTelevisionSeries constructs a series record from an {{Infobox television}} map.
func buildTelevisionSeries(title string, pageID int, ib map[string]string) *filmstock.TelevisionSeries {
	s := &filmstock.TelevisionSeries{
		Title:   title,
		PageID:  pageID,
		Type:    "television",
		WikiURL: "https://en.wikipedia.org/wiki/" + strings.ReplaceAll(url.PathEscape(title), "%20", "_"),
		Raw:     ib,
	}
	s.Genre = wikitext.SplitList(ib["genre"])
	s.Creator = mergePeople(ib["creator"], ib["developer"])
	s.Starring = wikitext.SplitPeople(ib["starring"])
	s.Network = mergeLinks(ib["network"], ib["channel"], ib["first_run"])
	s.Country = wikitext.SplitLinks(ib["country"])
	s.Composer = mergePeople(ib["composer"], ib["theme_music_composer"], ib["music"])
	s.Director = mergePeople(ib["director"], ib["directed_by"])
	s.Producer = mergePeople(ib["producer"], ib["produced_by"])
	s.ExecutiveProducer = mergePeople(ib["executive_producer"], ib["executive_producers"])
	s.Writer = mergePeople(ib["writer"], ib["written_by"], ib["screenplay"])
	s.Editor = mergePeople(ib["editor"], ib["edited_by"])
	s.Cinematography = mergePeople(ib["cinematography"], ib["cinematographer"])
	s.Presenter = mergePeople(ib["presenter"], ib["presented_by"], ib["host"], ib["starring_host"])
	s.Narrator = mergePeople(ib["narrator"], ib["narrated_by"], ib["voices"])
	s.ProductionCompanies = mergeLinks(ib["company"], ib["production_company"], ib["production_companies"], ib["studio"])
	s.BasedOn = wikitext.SplitLinks(ib["based_on"])
	s.Related = wikitext.SplitLinks(ib["related"])
	s.NumSeasons = wikitext.CleanText(ib["num_seasons"])
	s.NumEpisodes = wikitext.CleanText(firstNonEmpty(ib["num_episodes"], ib["episodes"]))
	s.Runtime = wikitext.CleanText(ib["runtime"])
	if d := parseReleaseDates(ib["first_aired"]); len(d) > 0 {
		s.FirstAired = d[0]
	}
	if d := parseReleaseDates(ib["last_aired"]); len(d) > 0 {
		s.LastAired = d[0]
	}
	if img := ib["image"]; img != "" {
		s.CoverImageFile, s.CoverImageURL = filmstock.CoverImageURL(cleanBareFilename(img))
	}
	if le := wikitext.CleanText(ib["list_episodes"]); le != "" {
		s.ListEpisodes = le
	}
	return s
}

// reArticleSeason matches season-article titles: "Foo season 3",
// "Foo (season 3)", "Foo (series 3)". Captures the series title and number.
var reArticleSeason = regexp.MustCompile(`(?i)^(.*?)\s+(?:\(\s*)?(?:season|series)\s+(\d+)\s*\)?$`)

// articleSeason splits a season-article title into (series, seasonNumber).
func articleSeason(title string) (string, int, bool) {
	m := reArticleSeason.FindStringSubmatch(title)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, false
	}
	return strings.TrimSpace(m[1]), n, true
}

// reListEpisodes matches "List of Foo episodes" -> "Foo".
var reListEpisodes = regexp.MustCompile(`(?i)^List of (.+) episodes$`)

// reSeasonHeading matches "== Season 3 ==" / "=== Series 2 (2009) ===".
var reSeasonHeading = regexp.MustCompile(`(?im)^=+\s*(?:Season|Series)\s+(\d+)`)

// parseEpisodeRow builds an Episode from one {{Episode list}} parameter map.
func parseEpisodeRow(ib map[string]string) *filmstock.Episode {
	title := wikitext.CleanText(firstNonEmpty(ib["title"], ib["rtitle"], ib["englishtitle"]))
	if title == "" {
		return nil
	}
	e := &filmstock.Episode{
		Title:      title,
		DirectedBy: wikitext.SplitPeople(ib["directedby"]),
		WrittenBy:  wikitext.SplitPeople(ib["writtenby"]),
	}
	e.NumberOverall = atoiSafe(ib["episodenumber"])
	e.NumberInSeason = atoiSafe(ib["episodenumber2"])
	if d := parseReleaseDates(ib["originalairdate"]); len(d) > 0 {
		e.AirDate = d[0]
	}
	e.Summary = wikitext.TrimLen(wikitext.CleanText(ib["shortsummary"]), 1500)
	return e
}

// extractEpisodesByHeading pulls every {{Episode list}} row from an article,
// tagging each with the season implied by the nearest preceding season heading
// (0 if none). Returns episodes paired with their heading-derived season.
func extractEpisodesByHeading(text string) []taggedEpisode {
	// Precompute heading positions -> season number.
	type hd struct {
		pos    int
		season int
	}
	var heads []hd
	for _, m := range reSeasonHeading.FindAllStringSubmatchIndex(text, -1) {
		n, _ := strconv.Atoi(text[m[2]:m[3]])
		heads = append(heads, hd{m[0], n})
	}
	seasonAt := func(pos int) int {
		s := 0
		for _, h := range heads {
			if h.pos < pos {
				s = h.season
			} else {
				break
			}
		}
		return s
	}

	var out []taggedEpisode
	// Walk each Episode list occurrence, using its byte position for the heading.
	lower := strings.ToLower(text)
	target := "{{episode list"
	idx := 0
	for {
		rel := strings.Index(lower[idx:], target)
		if rel < 0 {
			break
		}
		start := idx + rel
		body, next := wikitext.BalancedBody(text, start, len(target))
		if next < 0 {
			break
		}
		idx = next
		if e := parseEpisodeRow(wikitext.ParseInfobox(body)); e != nil {
			out = append(out, taggedEpisode{e, seasonAt(start)})
		}
	}
	return out
}

type taggedEpisode struct {
	ep     *filmstock.Episode
	season int
}

func atoiSafe(s string) int {
	s = strings.TrimSpace(s)
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
