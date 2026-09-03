package record

import (
	"regexp"
	"strings"
)

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
