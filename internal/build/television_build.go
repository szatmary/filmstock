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
		// Display title, as for films: "(TV series)" is Wikipedia namespacing,
		// not part of the name. Identity stays the page_id and the page name
		// survives in WikiURL, built below from the raw title.
		//
		// Nothing may key on this. Stripping the disambiguator once WAS used to
		// join series to episodes, and two shows differing only by disambiguator
		// collapsed into one with the loser silently dropped — the BBC's House of
		// Cards, PBS Frontline, every non-US Big Brother.
		Title:   filmstock.CleanTelevisionTitle(title),
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
	// released, where there is no first_aired: a television film or special has
	// one broadcast rather than a run, and {{Infobox television}} takes either.
	// 9,278 pages state released and nothing else — The Day After, Threads, A
	// Charlie Brown Christmas, Night Gallery — and had no date at all, which is
	// 83% of every series in the corpus missing one.
	if d := parseReleaseDates(firstNonEmpty(ib["first_aired"], ib["released"])); len(d) > 0 {
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
// reEpisodePart matches a per-part parameter suffix: EpisodeNumber_1, Viewers_2.
var reEpisodePart = regexp.MustCompile(`_(\d+)$`)

// segmentTitle are the fields naming a segment rather than describing it.
var segmentTitle = map[string]bool{"title": true, "rtitle": true, "englishtitle": true}

// maxEpisodeParts bounds NumParts. Three is the most any real article uses;
// the ceiling is only here so a vandalised or mistyped value cannot make one
// row into thousands of episodes.
const maxEpisodeParts = 12

// parseEpisodeRows turns one {{Episode list}} row into the episodes it covers.
//
// Usually one. But a multi-part episode is written as a SINGLE row spanning
// several, with NumParts saying how many and the differing fields suffixed:
//
//	|NumParts=2
//	|EpisodeNumber_1=11              |EpisodeNumber_2=12
//	|OriginalAirDate_1={{Start date|1966|11|17}}
//	|OriginalAirDate_2={{Start date|1966|11|24}}
//	|DirectedBy_1=Marc Daniels       |DirectedBy_2=Robert Butler
//	|Title=[[The Menagerie...|The Menagerie]]
//	|WrittenBy=[[Gene Roddenberry]]
//
// Reading only the unsuffixed names finds no number, no air date and no
// viewership, and collapses both episodes into one untitled-by-number entry —
// Star Trek's "The Menagerie" came out as episode 0 of season 0, and the season
// reported 28 episodes instead of 29. 10,514 such rows across 908 articles,
// covering upwards of 21,000 episodes.
func parseEpisodeRows(ib map[string]string) []*filmstock.Episode {
	n := atoiSafe(ib["numparts"])
	if n < 2 {
		if e := parseEpisodeRow(ib); e != nil {
			return []*filmstock.Episode{e}
		}
		return nil
	}
	if n > maxEpisodeParts {
		n = maxEpisodeParts
	}
	// NumParts alone does not mean several episodes. The Simpsons' "Treehouse
	// of Horror VII" is NumParts=3 for its three SEGMENTS, and states one
	// EpisodeNumber, one air date and one title — splitting it into three
	// episodes invents two that never aired, and the season reports 29 instead
	// of 25.
	//
	// What separates the two is whether the parts are numbered as episodes.
	// The Menagerie gives EpisodeNumber_1=11 and EpisodeNumber_2=12; Treehouse
	// gives EpisodeNumber=154 once. So: per-part numbers mean per-part
	// episodes, and anything else is one episode made of segments.
	if !hasPerPartNumbers(ib, n) {
		if e := parseEpisodeRow(mergeEpisodeParts(ib, n)); e != nil {
			return []*filmstock.Episode{e}
		}
		return nil
	}
	var out []*filmstock.Episode
	for i := 1; i <= n; i++ {
		if e := parseEpisodeRow(episodePart(ib, i)); e != nil {
			out = append(out, e)
		}
	}
	return out
}

// hasPerPartNumbers reports whether the row numbers its parts as episodes.
func hasPerPartNumbers(ib map[string]string, n int) bool {
	for i := 1; i <= n; i++ {
		sfx := "_" + strconv.Itoa(i)
		if ib["episodenumber"+sfx] != "" || ib["episodenumber2"+sfx] != "" {
			return true
		}
	}
	return false
}

// mergeEpisodeParts folds a segmented episode's per-segment fields into one.
//
// Treehouse of Horror VII credits WrittenBy_1, _2 and _3 — one writer per
// segment — and no unsuffixed WrittenBy at all, so reading only the plain name
// credits nobody. They are all writers of the one episode, so they are joined.
// An unsuffixed value, where the row gives one, is what the row says about the
// episode as a whole and is left alone.
func mergeEpisodeParts(ib map[string]string, n int) map[string]string {
	out := make(map[string]string, len(ib))
	for k, v := range ib {
		if !reEpisodePart.MatchString(k) {
			out[k] = v
		}
	}
	joined := map[string][]string{}
	for i := 1; i <= n; i++ {
		sfx := "_" + strconv.Itoa(i)
		for k, v := range ib {
			if v == "" || !strings.HasSuffix(k, sfx) {
				continue
			}
			base := strings.TrimSuffix(k, sfx)
			if out[base] != "" {
				continue // the row already states this for the whole episode
			}
			joined[base] = append(joined[base], v)
		}
	}
	for base, vs := range joined {
		if segmentTitle[base] {
			// "Barbecue Story / Waiter, There's a Baby in My Soup" — one Rugrats
			// broadcast of two named segments, written the way the article
			// renders it. Joined any other way the two run together into a
			// title that reads as one wrong name.
			//
			// Segments whose title is only markup — an empty template, a
			// comment — are left out rather than joined as nothing, which
			// manufactures a title of "/ /" and with it an episode that the
			// article never listed.
			named := vs[:0]
			for _, v := range vs {
				if wikitext.CleanText(v) != "" {
					named = append(named, v)
				}
			}
			if len(named) > 0 {
				out[base] = strings.Join(named, " / ")
			}
			continue
		}
		// <br /> rather than a comma: SplitPeople deliberately does not split on
		// commas, because names carry them — "Robert Downey, Jr." is one person.
		out[base] = strings.Join(vs, "<br />")
	}
	return out
}

// episodePart presents one part's parameters: the shared unsuffixed fields,
// then this part's suffixed ones over the top. Another part's fields are left
// out entirely, so part 2's air date can never be read as part 1's.
func episodePart(ib map[string]string, i int) map[string]string {
	want := "_" + strconv.Itoa(i)
	out := make(map[string]string, len(ib))
	for k, v := range ib {
		if !reEpisodePart.MatchString(k) {
			out[k] = v
		}
	}
	for k, v := range ib {
		if strings.HasSuffix(k, want) {
			out[strings.TrimSuffix(k, want)] = v
		}
	}
	return out
}

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
	e.Viewers = parseViewers(ib["viewers"])
	return e
}

// reViewers takes the leading number out of a viewers field.
//
// The value is rarely just a number: "23.8<ref name=...>{{cite news|...}}</ref>",
// "1.23[2]", "9.10&nbsp;million". Anything after the first number is citation or
// units, and a range ("2.1-2.4") takes its first figure rather than guessing.
var reViewers = regexp.MustCompile(`^\s*([0-9]+(?:\.[0-9]+)?)`)

func parseViewers(s string) float64 {
	m := reViewers.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil || v <= 0 || v > 1000 { // millions; anything larger is a parse artefact
		return 0
	}
	return v
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
		season := seasonAt(start)
		for _, e := range parseEpisodeRows(wikitext.ParseInfobox(body)) {
			out = append(out, taggedEpisode{e, season})
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
