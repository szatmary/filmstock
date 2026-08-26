package filmstock

import "testing"

// A third of film titles carry Wikipedia's disambiguator. It is namespacing, not
// part of the name, and it reaches a UI as "Alice in Wonderland (1985 film)".
func TestCleanTitleStripsDisambiguators(t *testing.T) {
	for _, c := range [][2]string{
		{"Heat (film)", "Heat"},
		{"Alice in Wonderland (1985 film)", "Alice in Wonderland"},
		{"The Thing (2017 American film)", "The Thing"},
		{"Nosferatu (1922)", "Nosferatu"},
		{"Rocky (film series)", "Rocky"},
		{"Flash Gordon (serial)", "Flash Gordon"},
		{"MonsterVerse (franchise)", "MonsterVerse"},
		{"Les Misérables (1998 French-language film)", "Les Misérables"},
		{"Roots (1977 miniseries)", "Roots"},
	} {
		if got := CleanTitle(c[0]); got != c[1] {
			t.Errorf("CleanTitle(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

// Only a TRAILING parenthetical that names a form is a disambiguator. Anything
// else is part of the title, and stripping it would rename the film.
func TestCleanTitleLeavesRealTitlesAlone(t *testing.T) {
	for _, s := range []string{
		"(500) Days of Summer",
		"Good Night, and Good Luck",
		"Amélie",
		"Fantastic Mr. Fox",
		"8½",
		"Der Untergang (Downfall)", // a parenthetical that is not a form word
		"Three Colours: Blue",
	} {
		if got := CleanTitle(s); got != s {
			t.Errorf("CleanTitle(%q) changed it to %q", s, got)
		}
	}
}
