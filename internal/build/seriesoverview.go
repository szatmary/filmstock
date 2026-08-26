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
//	| infoAtitle = Rank            | infoA1 = 2
//	| infoBtitle = Rating          | infoB1 = 20.0
//	| infoCtitle = Viewers<br />(millions)  | infoC1 = 30.1
//
// So the column has to be identified by what the article calls it, not by its
// position. Reading infoA as Rank because it usually is would record a show's
// viewership as its rank on every series that orders the columns differently —
// wrong, and wrong in a way nothing downstream could detect.
//
// The letters are matched in lower case because ParseInfobox lower-cases every
// key, so "infoAtitle" arrives as "infoatitle". Matching [A-Z] here reads the
// episode counts and air dates perfectly and silently finds no Nielsen figures
// at all, which is the shape of failure this file exists to avoid.
var (
	reOverviewParam = regexp.MustCompile(`^(episodes|start|end|link|info([a-z]))(\d+)$`)
	reInfoTitle     = regexp.MustCompile(`^info([a-z])title$`)
	reFirstNumber   = regexp.MustCompile(`\d+(?:\.\d+)?`)
)

// parseSeriesOverview reads every {{Series overview}} on a page.
func parseSeriesOverview(text string) []*filmstock.Season {
	var out []*filmstock.Season
	for _, body := range wikitext.FindAllTemplates(text, "Series overview") {
		out = append(out, overviewSeasons(wikitext.ParseInfobox(body))...)
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
	for k, v := range ib {
		m := reOverviewParam.FindStringSubmatch(k)
		if m == nil || strings.TrimSpace(v) == "" {
			continue
		}
		n, err := strconv.Atoi(m[3])
		if err != nil || n <= 0 {
			continue
		}
		switch m[1] {
		case "episodes":
			if c := firstInt(v); c > 0 {
				season(n).NumEpisodes = c
			}
		case "start":
			if d := parseReleaseDates(v); len(d) > 0 {
				season(n).FirstAired = d[0]
			}
		case "end":
			if d := parseReleaseDates(v); len(d) > 0 {
				season(n).LastAired = d[0]
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
	for _, s := range byNum {
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
