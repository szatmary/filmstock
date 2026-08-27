package build

import (
	"database/sql"
	"flag"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/szatmary/filmstock"
	"github.com/szatmary/filmstock/internal/sqldrv"
	"github.com/szatmary/filmstock/internal/wikitext"
)

// Problems in the encyclopaedia, reported so they can be fixed there.
//
// Everything here is upstream: an article that says a series ended before it
// began, a template placeholder nobody filled in, a film listed among its own
// cast. Fixing them on Wikipedia helps everyone reading it and reaches us again
// on the next dump, which is a better trade than working around each one here.
//
// The bar for inclusion is high, deliberately. A report full of our own parsing
// artefacts wastes an editor's time and discredits the entries that are real.
// The first version of the reversed-date check compared "1979-10-01" against
// "1979" as strings and produced 314 findings, 295 of them nonsense — a
// year-only end date is not an error. It compares years now, and finds 19.
//
// So each check states what it believes and why it cannot be us.
type finding struct {
	Kind   string // what sort of problem
	Title  string // the article
	Detail string // what is wrong, in a sentence
	URL    string
}

func dedupeFindings(f []finding) []finding {
	seen := map[string]bool{}
	out := f[:0]
	for _, x := range f {
		k := x.Kind + "\x00" + x.Title + "\x00" + x.Detail
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, x)
	}
	return out
}

func lintURL(title string) string {
	return "https://en.wikipedia.org/wiki/" +
		strings.ReplaceAll(url.PathEscape(title), "%20", "_")
}

// CLint reports likely errors in the source articles.
func CLint(args []string) {
	fs := flag.NewFlagSet("lint", flag.ExitOnError)
	dbPath := fs.String("db", "", "the published database")
	interPath := fs.String("inter", defaultInterPath(), "the intermediate, for infobox text")
	format := fs.String("format", "text", "text, tsv or wiki")
	limit := fs.Int("per-kind", 0, "cap findings per kind (0 = all)")
	fs.Parse(args)
	if *dbPath == "" {
		fatal(fmt.Errorf("lint needs -db FILE"))
	}
	db, err := sql.Open(sqldrv.Name, *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	var out []finding
	out = append(out, lintDates(db)...)
	out = append(out, lintYears(db)...)
	if in, err := OpenInter(*interPath); err == nil {
		defer in.Close()
		out = append(out, lintInfoboxes(in)...)
		out = append(out, lintSelfCredits(in)...)
	} else {
		fmt.Fprintf(os.Stderr, "  (no intermediate at %s; infobox checks skipped)\n", *interPath)
	}

	// One article can trip the same check twice — the reversed-date queries
	// overlap where both dates are full ones — and a finding repeated reads as
	// two problems.
	out = dedupeFindings(out)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Title < out[j].Title
	})
	report(out, *format, *limit)
}

// lintDates: a work cannot end before it begins.
//
// Compared as YEARS where either side is year-only, because a year-only end
// date is normal and is not evidence of anything.
func lintDates(db *sql.DB) []finding {
	var out []finding
	q := func(query, kind, what string) {
		rows, err := db.Query(query)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var title, a, b string
			if rows.Scan(&title, &a, &b) != nil {
				continue
			}
			out = append(out, finding{kind, title,
				fmt.Sprintf("%s %s but ended %s", what, a, b), lintURL(title)})
		}
	}
	q(`SELECT wiki_title, first_aired, last_aired FROM television_series
	   WHERE wiki_title<>'' AND length(first_aired)>=4 AND length(last_aired)>=4
	     AND CAST(substr(first_aired,1,4) AS INT) > CAST(substr(last_aired,1,4) AS INT)`,
		"ends before it begins", "began")
	q(`SELECT wiki_title, first_aired, last_aired FROM television_series
	   WHERE wiki_title<>'' AND length(first_aired)=10 AND length(last_aired)=10
	     AND first_aired > last_aired`,
		"ends before it begins", "began")
	q(`SELECT s.wiki_title, se.first_aired, se.last_aired
	   FROM television_seasons se JOIN television_series s ON s.id=se.series_id
	   WHERE s.wiki_title<>'' AND length(se.first_aired)>=4 AND length(se.last_aired)>=4
	     AND CAST(substr(se.first_aired,1,4) AS INT) > CAST(substr(se.last_aired,1,4) AS INT)`,
		"season ends before it begins", "a season began")
	return out
}

// lintYears: a date outside the medium's existence.
//
// 1878 is Muybridge; 1928 is the first regular television broadcast. Anything
// earlier is a typo or a misplaced field, never a fact.
//
// There is deliberately no upper bound. "100 Years" is dated 2115 and that is
// correct — the film is locked in a safe until then — so a far-future date is a
// curiosity rather than evidence of anything, and flagging it would be wrong in
// the one case anybody would check.
func lintYears(db *sql.DB) []finding {
	var out []finding
	rows, err := db.Query(`
		SELECT wiki_title, year, 'film' FROM movies
		  WHERE wiki_title<>'' AND year>0 AND year<1878
		UNION ALL
		SELECT wiki_title, year, 'series' FROM television_series
		  WHERE wiki_title<>'' AND year>0 AND year<1928`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var title, kind string
		var year int
		if rows.Scan(&title, &year, &kind) != nil {
			continue
		}
		out = append(out, finding{"year outside the medium", title,
			fmt.Sprintf("dated %d, before the %s medium existed", year, kind),
			lintURL(title)})
	}
	return out
}

// lintInfoboxes: a template placeholder nobody replaced.
//
// {{Infobox television}} ships with commented hints, and a fair number of
// articles keep them: "<!-- name of list of episodes article goes here -->"
// sits in list_episodes on 57 series. It renders as nothing, so a reader never
// sees it and nobody notices — which is exactly why it survives.
//
// Also an anchor that names no section. Pointing list_episodes at a section of
// the same article is a normal convention where the episodes are listed inline,
// and 727 articles do it correctly — so the anchor EXISTING is not the problem
// and reporting them all would be 87% noise. What is reported is an anchor with
// no matching heading: The Bugaloos links "#List of episodes" where the section
// is "Episodes", so the link silently goes nowhere.
func lintInfoboxes(in *Inter) []finding {
	var out []finding
	in.Each(filmstock.KindTelevision, true, func(p *Page) error {
		t := strings.TrimSpace(p.Infobox["list_episodes"])
		switch {
		case t == "":
		case strings.Contains(t, "<!--") &&
			strings.TrimSpace(wikitext.ReComment.ReplaceAllString(t, "")) == "":
			out = append(out, finding{"unfilled template placeholder", p.Title,
				"list_episodes still holds the template's own comment and names no article",
				lintURL(p.Title)})
		case strings.HasPrefix(t, "#"):
			want := strings.ToLower(strings.TrimSpace(t[1:]))
			if want == "" || hasHeading(p.Wikitext, want) {
				return nil // the anchor resolves; this is the normal convention
			}
			out = append(out, finding{"anchor names no section", p.Title,
				fmt.Sprintf("list_episodes points at %q and the article has no such "+
					"heading, so the link goes nowhere", t), lintURL(p.Title)})
		}
		return nil
	})
	return out
}

// reHeading matches a section heading of any level.
var reHeading = regexp.MustCompile(`(?m)^=+\s*(.+?)\s*=+\s*$`)

func hasHeading(text, want string) bool {
	for _, m := range reHeading.FindAllStringSubmatch(text, -1) {
		if strings.EqualFold(strings.TrimSpace(m[1]), want) {
			return true
		}
	}
	return false
}

// lintSelfCredits: a work listed among its own cast.
//
// "Saul of the Mole Men" appears in its own starring parameter. It renders as a
// link back to the page you are reading, so it looks harmless, and every
// database built from the infobox gains a person who is a television series.
func lintSelfCredits(in *Inter) []finding {
	var out []finding
	seen := map[string]bool{}
	rows, err := in.db.Query(`
		SELECT p.title, l.field, l.target
		FROM links l JOIN pages p ON p.page_id = l.from_id
		WHERE l.target = p.title
		  AND l.field IN ('starring','director','producer','writer','creator',
		                  'composer','narrator','presenter','music')`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var title, field, target string
		if rows.Scan(&title, &field, &target) != nil {
			continue
		}
		if seen[title+field] {
			continue
		}
		seen[title+field] = true
		out = append(out, finding{"a work credited as a person", title,
			fmt.Sprintf("%s lists the article itself, so every database built from "+
				"this infobox gains a person who is a work", field),
			lintURL(title)})
	}
	return out
}

// report writes the findings.
func report(f []finding, format string, perKind int) {
	counts := map[string]int{}
	var kept []finding
	for _, x := range f {
		counts[x.Kind]++
		if perKind == 0 || counts[x.Kind] <= perKind {
			kept = append(kept, x)
		}
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	switch format {
	case "tsv":
		for _, x := range kept {
			fmt.Printf("%s\t%s\t%s\t%s\n", x.Kind, x.Title, x.Detail, x.URL)
		}
	case "wiki":
		for _, k := range kinds {
			fmt.Printf("\n== %s (%d) ==\n", k, counts[k])
			for _, x := range kept {
				if x.Kind == k {
					fmt.Printf("* [[%s]] — %s\n", x.Title, x.Detail)
				}
			}
		}
	default:
		last := ""
		for _, x := range kept {
			if x.Kind != last {
				fmt.Printf("\n%s (%d)\n%s\n", x.Kind, counts[x.Kind],
					strings.Repeat("-", len(x.Kind)+8))
				last = x.Kind
			}
			fmt.Printf("  %-46s %s\n      %s\n", x.Title, x.Detail, x.URL)
		}
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	fmt.Fprintf(os.Stderr, "\n%d findings across %d kinds\n", total, len(counts))
}
