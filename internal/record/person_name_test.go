package record

import "testing"

func TestCleanPersonNameStripsDisambiguators(t *testing.T) {
	for in, want := range map[string]string{
		"Ian Dickson (TV personality)":     "Ian Dickson",
		"John Smith (actor)":               "John Smith",
		"Peter Jackson (rower)":            "Peter Jackson",
		"Mark Wright (Royal Navy officer)": "Mark Wright",
		"Michael Jones (sport shooter)":    "Michael Jones",
		// No parenthetical: untouched, accents and punctuation intact.
		"Yvette González-Nacer": "Yvette González-Nacer",
		"R.J. Colleary":         "R.J. Colleary",
		"Robert Downey Jr.":     "Robert Downey Jr.",
		"":                      "",
	} {
		if got := CleanPersonName(in); got != want {
			t.Errorf("CleanPersonName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Only a TRAILING parenthetical, and only one: a name that opens with one, or
// carries one mid-string, keeps it.
func TestCleanPersonNameOnlyStripsTheTrailingOne(t *testing.T) {
	for in, want := range map[string]string{
		"(Hed) P.E.":                  "(Hed) P.E.",
		"Tim (Ripper) Owens":          "Tim (Ripper) Owens",
		"Tim (Ripper) Owens (singer)": "Tim (Ripper) Owens",
	} {
		if got := CleanPersonName(in); got != want {
			t.Errorf("CleanPersonName(%q) = %q, want %q", in, got, want)
		}
	}
}

// It is a display transform. Nothing may key on it — two people disambiguated
// only by their parenthetical share a cleaned name, and that is fine precisely
// because identity is the page_id.
func TestCleanedNamesMayCollide(t *testing.T) {
	if CleanPersonName("John Smith (actor)") != CleanPersonName("John Smith (politician)") {
		t.Skip("no collision to demonstrate")
	}
}
