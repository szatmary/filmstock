package build

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/wikitext"
)

// {{Series overview}} — every season of a show in one template, on the
// episode-list page.
//
// It is the only place the corpus states a season's Nielsen standing, and it is
// worth reading for a second reason: it gives episode counts and air-date ranges
// for seasons that have no article of their own, which is most seasons of most
// shows.
//
// The awkward part is that its extra columns are self-labelling. infoA is Rank
// on one show, Viewers on another, and "18–49 demo" on a third:
//
//	| infoA = Rank                  | infoA1 = 2
//	| infoB = Rating                | infoB1 = 20.0
//	| infoC = Viewers (millions)    | infoC1 = 30.1
//
// The heading is the un-numbered parameter and the per-season values carry the
// season number. Nothing distinguishes them but that digit.
//
// A season can also be SPLIT, and then the parts carry a letter as well:
//
//	| episodes5 = 16 | episodes5A = 8 | start5A = … | end5A = …
//	                 | episodes5B = 8 | start5B = … | end5B = …
//
// Breaking Bad's fifth season is written that way, and so is most of modern
// cable and streaming. Requiring the parameter to end in a digit parses the
// total episode count and then silently drops every air date the season has.
//
// So the column has to be identified by what the article calls it, not by its
// position. Reading infoA as Rank because it usually is would record a show's
// viewership as its rank on every series that orders the columns differently —
// wrong, and wrong in a way nothing downstream could detect.
//
// Two ways to get this wrong, both of which produce perfect episode counts and
// air dates alongside no Nielsen figures whatsoever:
//
//   - ParseInfobox lower-cases every key, so the letters must be matched in
//     lower case.
//   - the heading parameter is "infoA", not "infoAtitle". "infoAtitle" is what
//     the template's documentation suggests and what I assumed; across real
//     articles it does not occur once. Both are accepted because accepting the
//     documented spelling costs nothing, but the un-numbered form is the one
//     reality uses.
var (
	reOverviewParam = regexp.MustCompile(`^(episodes|start|end|link|info([a-z]))(\d+)([a-z]?)$`)
	reInfoTitle     = regexp.MustCompile(`^info([a-z])(?:title)?$`)
	reFirstNumber   = regexp.MustCompile(`\d+(?:\.\d+)?`)
)

// parseSeriesOverview reads every {{Series overview}} on a page.
//
// Templates carrying "multiseries" are skipped, deliberately. Shows that share
// an episode list nest one overview per series inside an outer one:
//
//	{{Series overview | infoA = Rank | multiseries =
//	  {{Series overview | series = ''[[I Love Lucy]]''            | episodes1 = 35 …}}
//	  {{Series overview | series = ''[[The Lucy–Desi Comedy Hour]]'' | episodes1 = 13 …}}
//	}}
//
// Both blocks number their seasons from one, and this function returns a flat
// list that the caller keys by season number alone. Reading them without
// routing each block to the series it names would merge two shows' seasons —
// giving I Love Lucy's rank to a Lucy–Desi season and vice versa. Absent data
// is recoverable; data attributed to the wrong show is not, and nothing
// downstream could tell.
//
// Doing this properly means carrying the "series" parameter through to the
// collector so each block attaches to the series it names. Until then the outer
// template yields nothing, which is what it did before this file existed.
func parseSeriesOverview(text string) []*filmstock.Season {
	var out []*filmstock.Season
	for _, body := range wikitext.FindAllTemplates(text, "Series overview") {
		ib := wikitext.ParseInfobox(body)
		if strings.TrimSpace(ib["multiseries"]) != "" {
			continue
		}
		out = append(out, overviewSeasons(ib)...)
	}
	return out
}

// overviewSeasons turns one template's parameters into seasons.
func overviewSeasons(ib map[string]string) []*filmstock.Season {
	// What each lettered column means, taken from the article's own headings.
	role := map[string]string{}
	for k, v := range ib {
		if m := reInfoTitle.FindStringSubmatch(k); m != nil {
			if r := overviewRole(v); r != "" {
				role[m[1]] = r
			}
		}
	}

	byNum := map[int]*filmstock.Season{}
	season := func(n int) *filmstock.Season {
		s, ok := byNum[n]
		if !ok {
			s = &filmstock.Season{Season: n}
			byNum[n] = s
		}
		return s
	}
	// Episode counts stated only per part, summed. Used only where the season
	// does not state its own total, which it usually does.
	partEps := map[int]int{}
	for k, v := range ib {
		m := reOverviewParam.FindStringSubmatch(k)
		if m == nil || strings.TrimSpace(v) == "" {
			continue
		}
		n, err := strconv.Atoi(m[3])
		if err != nil || n <= 0 {
			continue
		}
		split := m[4] != "" // a part of a split season: 5A, 5B
		switch m[1] {
		case "episodes":
			c := firstInt(v)
			if c <= 0 {
				continue
			}
			if split {
				partEps[n] += c
			} else {
				season(n).NumEpisodes = c
			}
		case "start":
			// A split season starts when its FIRST part does.
			if d := parseReleaseDates(v); len(d) > 0 {
				if cur := season(n).FirstAired; cur == "" || d[0] < cur {
					season(n).FirstAired = d[0]
				}
			}
		case "end":
			// …and ends when its LAST part does.
			if d := parseReleaseDates(v); len(d) > 0 {
				if cur := season(n).LastAired; cur == "" || d[0] > cur {
					season(n).LastAired = d[0]
				}
			}
		case "link":
			// Recorded only as a join hint; the identity is the page_id the
			// link resolves to, never the title it was written with.
		default: // infoA1, infoB2, …
			switch role[m[2]] {
			case "rank":
				season(n).Rank = firstInt(v)
			case "rating":
				season(n).Rating = firstFloat(v)
			case "viewers":
				season(n).Viewers = firstFloat(v)
			}
		}
	}

	var out []*filmstock.Season
	for n, s := range byNum {
		if s.NumEpisodes == 0 {
			s.NumEpisodes = partEps[n]
		}
		out = append(out, s)
	}
	sortSeasons(out)
	return out
}

// overviewRole maps a column heading to what it holds.
//
// Order matters: "Nielsen ratings rank" contains both "rating" and "rank", and
// a show whose rank column is headed that way would otherwise record its rank
// as a rating. Rank is tested first because a heading naming both is a rank
// column — the rating columns are headed "Rating" or "Household rating".
func overviewRole(title string) string {
	t := strings.ToLower(wikitext.CleanText(title))
	switch {
	case strings.Contains(t, "rank"):
		return "rank"
	case strings.Contains(t, "viewer"), strings.Contains(t, "audience"):
		return "viewers"
	case strings.Contains(t, "rating"), strings.Contains(t, "share"):
		return "rating"
	}
	return ""
}

// firstInt and firstFloat take the leading number out of a cell. The values
// carry footnotes, references and thousands separators — "2{{efn|tied}}",
// "30.1<ref name=x/>" — and parsing the whole cell yields zero for all of them.
func firstInt(s string) int {
	n, _ := strconv.Atoi(strings.SplitN(reFirstNumber.FindString(clean(s)), ".", 2)[0])
	return n
}

func firstFloat(s string) float64 {
	f, _ := strconv.ParseFloat(reFirstNumber.FindString(clean(s)), 64)
	return f
}

func clean(s string) string {
	return strings.ReplaceAll(wikitext.CleanText(s), ",", "")
}

func sortSeasons(s []*filmstock.Season) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Season < s[j-1].Season; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
