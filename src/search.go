package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// SearchResult is one ranked hit, shared by the CLI and the web server.
type SearchResult struct {
	ID       int
	Title    string
	Year     int
	Starring string
	Director string
	Cover    string
	Path     string
	Score    float64
}

// searchMovies runs the fuzzy search: FTS5 trigram retrieval of candidates that
// share at least one query trigram, then a precise Sørensen–Dice re-rank in Go
// against the chosen field (title|starring|director).
func searchMovies(ctx context.Context, db *sql.DB, query, field string, limit int) ([]SearchResult, error) {
	qgrams := trigrams(normalize(query))
	if len(qgrams) == 0 {
		return nil, nil
	}
	qTokens := strings.Fields(normalize(query))
	parts := make([]string, 0, len(qgrams))
	for g := range qgrams {
		parts = append(parts, `"`+strings.ReplaceAll(g, `"`, `""`)+`"`)
	}
	match := strings.Join(parts, " OR ")

	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.title, m.year, m.starring, m.director, m.cover_image_file, m.path
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
		if err := rows.Scan(&r.ID, &r.Title, &year, &r.Starring, &r.Director, &coverFile, &r.Path); err != nil {
			continue
		}
		r.Year = int(year.Int64)
		r.Cover = filePathURL(coverFile, 0) // resolves Commons-or-en automatically
		target := cleanTitle(r.Title)
		switch field {
		case "starring":
			target = r.Starring
		case "director":
			target = r.Director
		}
		r.Score = fuzzyScore(qgrams, qTokens, target)
		r.Title = cleanTitle(r.Title) // display without the (film) disambiguator
		results = append(results, r)
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func cmdSearch(args []string) {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	dbPath := fs.String("db", "../movies.db", "SQLite database")
	n := fs.Int("n", 20, "max results")
	field := fs.String("field", "title", "field to rank against: title|starring|director")
	fs.Parse(args)
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		fmt.Fprintln(os.Stderr, "usage: mediadb search [-n N] [-field title|starring|director] QUERY")
		os.Exit(2)
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	results, err := searchMovies(context.Background(), db, query, *field, *n)
	if err != nil {
		fatal(err)
	}
	if len(results) == 0 {
		fmt.Println("no matches.")
		return
	}
	for _, r := range results {
		yr := ""
		if r.Year > 0 {
			yr = fmt.Sprintf(" (%d)", r.Year)
		}
		fmt.Printf("%.2f  %s%s\n", r.Score, r.Title, yr)
		if r.Director != "" {
			fmt.Printf("        dir: %s\n", r.Director)
		}
		if r.Starring != "" {
			fmt.Printf("        cast: %s\n", truncate(r.Starring, 80))
		}
	}
}

// reDisambig strips a trailing Wikipedia disambiguation parenthetical that is a
// year and/or "…film" (e.g. "(film)", "(1975 film)", "(2017 American film)",
// "(1975)"), which otherwise dilutes trigram overlap for short title queries.
var reDisambig = regexp.MustCompile(`(?i)\s*\((?:\d{4}(?: [a-z]+)*(?: film)?|(?:[a-z]+ )*film)\)\s*$`)

// cleanTitle removes a trailing film disambiguator for ranking purposes.
func cleanTitle(t string) string {
	return reDisambig.ReplaceAllString(t, "")
}

// normalize lowercases and keeps only [a-z0-9 ], collapsing whitespace.
func normalize(s string) string {
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
	nt := normalize(target)
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

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
