package record

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

// Disambiguators Wikipedia actually writes that the first pass of this regex
// missed — accents, initials, decades, year ranges, plurals and telefilm —
// alongside the parentheses that are part of a NAME and must survive.
func TestCleanTitleDisambiguatorVariants(t *testing.T) {
	strip := map[string]string{
		"The Passenger (2005 François Rotger film)": "The Passenger",
		"Carmen (1915 Cecil B. DeMille film)":       "Carmen",
		"Sati Savitri (C. Pullayya film)":           "Sati Savitri",
		"Sati Savitri (H. M. Reddy film)":           "Sati Savitri",
		"Die Nibelungen (1966–67 film)":             "Die Nibelungen",
		"Superman (1940s animated film series)":     "Superman",
		"Sherlock Holmes (Éclair film series)":      "Sherlock Holmes",
		"Cards on the Table (Vietnamese telefilm)":  "Cards on the Table",
		"The Spirit of Christmas (short films)":     "The Spirit of Christmas",
		"Batman (1989 film)":                        "Batman",
		"Blue Velvet (film)":                        "Blue Velvet",
	}
	for in, want := range strip {
		if got := CleanTitle(in); got != want {
			t.Errorf("CleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
	// A parenthetical that is part of the title is not a disambiguator.
	keep := []string{
		"I Am Curious (Yellow)",
		"I Am Curious (Blue)",
		"19(1)(a)",
		"(Dis)Honesty: The Truth About Lies",
		"Gigantic (A Tale of Two Johns)",
		"Susan Lenox (Her Fall and Rise)",
		"La Commune (Paris, 1871)",
		"Bon Voyage, Charlie Brown (and Don't Come Back!!)",
		"Everything You Always Wanted to Know About Sex* (*But Were Afraid to Ask)",
	}
	for _, in := range keep {
		if got := CleanTitle(in); got != in {
			t.Errorf("CleanTitle(%q) = %q, want it unchanged", in, got)
		}
	}
}
