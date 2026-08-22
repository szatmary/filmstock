package main

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/szatmary/filmstock"
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
	s.Genre = splitList(ib["genre"])
	s.Creator = mergePeople(ib["creator"], ib["developer"])
	s.Starring = splitPeople(ib["starring"])
	s.Network = mergeLists(ib["network"], ib["channel"], ib["first_run"])
	s.Country = splitList(ib["country"])
	s.Composer = mergePeople(ib["composer"], ib["theme_music_composer"])
	s.NumSeasons = cleanText(ib["num_seasons"])
	s.NumEpisodes = cleanText(firstNonEmpty(ib["num_episodes"], ib["episodes"]))
	s.Runtime = cleanText(ib["runtime"])
	if d := parseReleaseDates(ib["first_aired"]); len(d) > 0 {
		s.FirstAired = d[0]
	}
	if d := parseReleaseDates(ib["last_aired"]); len(d) > 0 {
		s.LastAired = d[0]
	}
	if img := ib["image"]; img != "" {
		s.CoverImageFile, s.CoverImageURL = filmstock.CoverImageURL(cleanBareFilename(img))
	}
	if le := cleanText(ib["list_episodes"]); le != "" {
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
var reSeasonHeading = regexp.MustCompile(`(?im)^=+\s*(?:filmstock.Season|Series)\s+(\d+)`)

// parseEpisodeRow builds an Episode from one {{Episode list}} parameter map.
func parseEpisodeRow(ib map[string]string) *filmstock.Episode {
	title := cleanText(firstNonEmpty(ib["title"], ib["rtitle"], ib["englishtitle"]))
	if title == "" {
		return nil
	}
	e := &filmstock.Episode{
		Title:      title,
		DirectedBy: splitPeople(ib["directedby"]),
		WrittenBy:  splitPeople(ib["writtenby"]),
	}
	e.NumberOverall = atoiSafe(ib["episodenumber"])
	e.NumberInSeason = atoiSafe(ib["episodenumber2"])
	if d := parseReleaseDates(ib["originalairdate"]); len(d) > 0 {
		e.AirDate = d[0]
	}
	e.Summary = trimLen(cleanText(ib["shortsummary"]), 1500)
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
		body, next := balancedBody(text, start, len(target))
		if next < 0 {
			break
		}
		idx = next
		if e := parseEpisodeRow(parseInfobox(body)); e != nil {
			out = append(out, taggedEpisode{e, seasonAt(start)})
		}
	}
	return out
}

type taggedEpisode struct {
	ep     *filmstock.Episode
	season int
}

// balancedBody returns the inner body after `nameLen` bytes past `start` and the
// index just past the closing "}}" (or -1). Mirrors findTemplate's brace walk.
func balancedBody(text string, start, nameLen int) (string, int) {
	depth, i := 0, start
	for i < len(text)-1 {
		if text[i] == '{' && text[i+1] == '{' {
			depth++
			i += 2
			continue
		}
		if text[i] == '}' && text[i+1] == '}' {
			depth--
			i += 2
			if depth == 0 {
				return text[start+nameLen : i-2], i
			}
			continue
		}
		i++
	}
	return "", -1
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
