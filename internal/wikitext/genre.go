package wikitext

import "regexp"

// reCategory captures the name of each [[Category:…]] link.
var reCategory = regexp.MustCompile(`\[\[Category:([^\]|]+)`)

// genrePatterns maps a canonical genre to a word-boundary matcher. Movie
// infoboxes have no genre field, so genres are mined from category names like
// "American science fiction films". Word boundaries avoid false hits (e.g. "war"
// inside "award"). Order here is the display order.
var genrePatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"Science fiction", regexp.MustCompile(`(?i)\b(?:science[ -]fiction|sci-?fi)\b`)},
	{"Fantasy", regexp.MustCompile(`(?i)\bfantasy\b`)},
	{"Superhero", regexp.MustCompile(`(?i)\bsuperhero\b`)},
	{"Action", regexp.MustCompile(`(?i)\baction\b`)},
	{"Adventure", regexp.MustCompile(`(?i)\badventure\b`)},
	{"Animation", regexp.MustCompile(`(?i)\b(?:animated|animation|anime)\b`)},
	{"Comedy", regexp.MustCompile(`(?i)\bcomed(?:y|ies)\b`)},
	{"Romance", regexp.MustCompile(`(?i)\broman(?:ce|tic)\b`)},
	{"Drama", regexp.MustCompile(`(?i)\bdrama\b|\bmelodrama\b`)},
	{"Horror", regexp.MustCompile(`(?i)\bhorror\b`)},
	{"Slasher", regexp.MustCompile(`(?i)\bslasher\b`)},
	{"Thriller", regexp.MustCompile(`(?i)\bthriller\b`)},
	{"Mystery", regexp.MustCompile(`(?i)\bmystery\b`)},
	{"Crime", regexp.MustCompile(`(?i)\bcrime\b`)},
	{"Film noir", regexp.MustCompile(`(?i)\b(?:film noir|neo-noir)\b`)},
	{"War", regexp.MustCompile(`(?i)\bwar\b`)},
	{"Western", regexp.MustCompile(`(?i)\bwestern\b`)},
	{"Musical", regexp.MustCompile(`(?i)\bmusical\b`)},
	{"Documentary", regexp.MustCompile(`(?i)\bdocumentary\b`)},
	{"Biography", regexp.MustCompile(`(?i)\b(?:biographical|biopic)\b`)},
	{"Historical", regexp.MustCompile(`(?i)\b(?:historical|period)\b`)},
	{"Disaster", regexp.MustCompile(`(?i)\bdisaster\b`)},
	{"Spy", regexp.MustCompile(`(?i)\bspy\b`)},
	{"Sports", regexp.MustCompile(`(?i)\bsports?\b`)},
	{"Martial arts", regexp.MustCompile(`(?i)\bmartial arts\b`)},
	{"Coming-of-age", regexp.MustCompile(`(?i)\bcoming[ -]of[ -]age\b`)},
	{"Teen", regexp.MustCompile(`(?i)\bteen\b`)},
	{"Zombie", regexp.MustCompile(`(?i)\bzombie\b`)},
}

// ExtractGenres derives canonical genres from an article's film categories.
func ExtractGenres(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range reCategory.FindAllStringSubmatch(text, -1) {
		cat := m[1]
		if !reFilmWord.MatchString(cat) {
			continue // keep to film categories to avoid stray matches
		}
		for _, g := range genrePatterns {
			if !seen[g.name] && g.re.MatchString(cat) {
				seen[g.name] = true
				out = append(out, g.name)
			}
		}
	}
	return out
}

var reFilmWord = regexp.MustCompile(`(?i)\bfilms?\b`)
