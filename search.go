package filmstock

import (
	"context"
	"database/sql"
	"regexp"
	"sort"
	"strings"
)

// SearchResult is one ranked hit, shared by the CLI and the web server.
type SearchResult struct {
	ID       int
	Title    string
	Year     int
	Starring string
	Director string
	Cover    string
	Score    float64
}

// SearchMovies runs the fuzzy search: FTS5 trigram retrieval of candidates that
// share at least one query trigram, then a precise Sørensen–Dice re-rank in Go
// against the chosen field (title|starring|director).
func SearchMovies(ctx context.Context, db *sql.DB, query, field string, limit int) ([]SearchResult, error) {
	qgrams := trigrams(Normalize(query))
	if len(qgrams) == 0 {
		return nil, nil
	}
	qTokens := strings.Fields(Normalize(query))
	parts := make([]string, 0, len(qgrams))
	for g := range qgrams {
		parts = append(parts, `"`+strings.ReplaceAll(g, `"`, `""`)+`"`)
	}
	match := strings.Join(parts, " OR ")

	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.title, m.year, m.starring, m.director, m.cover_image_file
		FROM movies_fts f JOIN movies m ON m.id = f.rowid
		WHERE movies_fts MATCH ?
		ORDER BY bm25(movies_fts, 10.0, 2.0, 2.0)
		LIMIT 500`, match)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var year sql.NullInt64
		var coverFile string
		if err := rows.Scan(&r.ID, &r.Title, &year, &r.Starring, &r.Director, &coverFile); err != nil {
			continue
		}
		r.Year = int(year.Int64)
		r.Cover = FilePathURL(coverFile, 0) // resolves Commons-or-en automatically
		target := CleanTitle(r.Title)
		switch field {
		case "starring":
			target = r.Starring
		case "director":
			target = r.Director
		}
		r.Score = fuzzyScore(qgrams, qTokens, target)
		r.Title = CleanTitle(r.Title) // display without the (film) disambiguator
		results = append(results, r)
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// reDisambig strips a trailing Wikipedia disambiguation parenthetical: a year
// and/or a form word — "(film)", "(1975 film)", "(2017 American film)",
// "(1975)", "(film series)", "(serial)", "(franchise)".
//
// Measured against the corpus, 57,284 film titles carry a parenthetical and this
// matches 56,679 of them. The remainder are mostly "(film series)", "(serial)"
// and "(franchise)", now included, plus hyphenated language forms like
// "(1998 French-language film)".
//
// It matches only a TRAILING parenthetical containing a known form word, so a
// title that genuinely opens with one — "(500) Days of Summer" — is untouched.
var reDisambig = regexp.MustCompile(`(?i)\s*\((?:` +
	// Year-led: "1985 film", "1966–67 film", "1940s animated film series",
	// "2005 François Rotger film". The year may be a range (en dash or
	// hyphen) or a decade, and any words between it and the keyword may
	// carry accents or initials — "Éclair", "C. Pullayya".
	`\d{4}(?:[–-]\d{2,4})?s?(?:[ -][\p{L}.]+)*(?: (?:` + disambigKind + `))?` +
	// Word-led: "film", "short films", "Vietnamese telefilm",
	// "Éclair film series".
	`|(?:[\p{L}.-]+ )*(?:` + disambigKind + `)` +
	`)\)\s*$`)

// disambigKind is the vocabulary that makes a trailing parenthetical a
// DISAMBIGUATOR rather than part of the name. Requiring one of these is what
// keeps "I Am Curious (Yellow)" and "19(1)(a)" intact, so it is deliberately
// a closed list rather than "any parenthetical".
const disambigKind = `films?|film series|short films?|telefilms?|serials?|franchise|miniseries`

// CleanTitle removes a trailing film disambiguator for ranking purposes.
func CleanTitle(t string) string {
	return reDisambig.ReplaceAllString(t, "")
}

// Normalize lowercases and keeps only [a-z0-9 ], collapsing whitespace.
func Normalize(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevSpace = false
		case r == ' ' || r == '\t' || r == '-' || r == '_' || r == ':':
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// trigrams returns the set of 3-character shingles of s.
func trigrams(s string) map[string]struct{} {
	out := make(map[string]struct{})
	r := []rune(s)
	for i := 0; i+3 <= len(r); i++ {
		out[string(r[i:i+3])] = struct{}{}
	}
	return out
}

// tokenSetScore matches each query word to its best-matching title word (trigram
// Dice, or exact match for <3-char words), length-weighted so meaningful words
// ("matrix") count more than short ones ("the"). Ignores extra title words, so a
// short query scores high against a long title that CONTAINS it, e.g.
// "gummy bears" vs "adventures of the gummi bears".
func tokenSetScore(qTokens, tTokens []string) float64 {
	if len(qTokens) == 0 || len(tTokens) == 0 {
		return 0
	}
	tg := make([]map[string]struct{}, len(tTokens))
	for i, t := range tTokens {
		tg[i] = trigrams(t)
	}
	var tot, wsum float64
	for _, qw := range qTokens {
		best := 0.0
		qgm := trigrams(qw)
		for i, tw := range tTokens {
			var s float64
			if tw == qw {
				s = 1.0
			} else if len(qw) >= 3 {
				s = dice(qgm, tg[i])
			}
			if s > best {
				best = s
			}
		}
		w := float64(len(qw))
		tot += best * w
		wsum += w
	}
	if wsum == 0 {
		return 0
	}
	return tot / wsum
}

// fuzzyScore ranks a candidate as a blend of whole-string Dice (rewards exact,
// short titles) and token-set score (rewards the query appearing as words inside
// a longer title). The blend keeps "The Godfather" above "Long Arm of the
// Godfather" while still surfacing "Adventures of the Gummi Bears" for "gummy
// bears". Shared ranking signal for all searches.
func fuzzyScore(qgrams map[string]struct{}, qTokens []string, target string) float64 {
	nt := Normalize(target)
	d := dice(qgrams, trigrams(nt))
	ts := tokenSetScore(qTokens, strings.Fields(nt))
	return 0.5*d + 0.5*ts
}

// dice computes the Sørensen–Dice coefficient between two trigram sets.
func dice(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for g := range a {
		if _, ok := b[g]; ok {
			inter++
		}
	}
	return 2 * float64(inter) / float64(len(a)+len(b))
}

func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

type UnifiedResult struct {
	Type     string // movie | television | episode | person | event
	Title    string
	Subtitle string
	Link     string
	Cover    string
	Score    float64
}
