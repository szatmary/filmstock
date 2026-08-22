package wikitext

import (
	"testing"

	"github.com/szatmary/filmstock"
)

// names flattens a []Person to its display names for comparison.
func names(ps []filmstock.Person) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Every case here is real wikitext that the parser previously mangled. The
// failure mode was silent — a dropped field looks identical to a film with no
// cast — so these exist to make the next regression loud.
func TestSplitPeople(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{{
		// {{Plain list}} is a redirect to {{Plainlist}}; missing the spaced
		// spelling returned an empty payload and deleted the whole cast.
		// This is Mystery Men, which lost all 13 names.
		name: "plain list with a space",
		in:   "{{Plain list|\n* [[Hank Azaria]]\n* [[Claire Forlani]]\n* [[Janeane Garofalo]]\n}}",
		want: []string{"Hank Azaria", "Claire Forlani", "Janeane Garofalo"},
	}, {
		name: "plainlist unspaced still works",
		in:   "{{Plainlist|\n* [[Hank Azaria]]\n* [[Claire Forlani]]\n}}",
		want: []string{"Hank Azaria", "Claire Forlani"},
	}, {
		name: "underscore alias",
		in:   "{{unbulleted_list|[[Alice Smith]]|[[Bob Jones]]}}",
		want: []string{"Alice Smith", "Bob Jones"},
	}, {
		// Little Miss Sunshine: a directing duo sharing one article, with both
		// names in the link's display text. The <br>->\n pass used to tear the
		// markup, yielding "[[Jonathan Dayton and Valerie Faris|Jonathan Dayton"
		// and "Valerie Faris]]" as two separate "people".
		name: "br inside a link",
		in:   "[[Jonathan Dayton and Valerie Faris|Jonathan Dayton<br />Valerie Faris]]",
		want: []string{"Jonathan Dayton", "Valerie Faris"},
	}, {
		name: "br inside a link nested in a list template",
		in:   "{{Plainlist|\n* [[Adil El Arbi and Bilall Fallah|Adil El Arbi<br>Bilall Fallah]]\n}}",
		want: []string{"Adil El Arbi", "Bilall Fallah"},
	}, {
		// {{ill}} marks a person with no English article; returning "" dropped
		// the credit entirely.
		name: "interlanguage link",
		in:   "{{ill|Sergio Corbucci|it|Sergio Corbucci (regista)}}",
		want: []string{"Sergio Corbucci"},
	}, {
		name: "ampersand split outside links",
		in:   "[[Alice Smith]] & [[Bob Jones]]",
		want: []string{"Alice Smith", "Bob Jones"},
	}, {
		// "and" appears inside the link target here and must NOT split.
		name: "and inside a link target does not split",
		in:   "[[Alice and Bob (duo)|Alice and Bob]]",
		want: []string{"Alice and Bob"},
	}, {
		name: "role label prefix stripped",
		in:   "Story & Screenplay: [[Alice Smith]]",
		want: []string{"Alice Smith"},
	}, {
		name: "br separated plain names",
		in:   "Alice Smith<br />Bob Jones",
		want: []string{"Alice Smith", "Bob Jones"},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := names(SplitPeople(c.in))
			if !eq(got, c.want) {
				t.Errorf("SplitPeople(%q)\n  got  %q\n  want %q", c.in, got, c.want)
			}
		})
	}
}

// A shared article means a shared identity: both halves of a duo must keep the
// same wiki target, so they resolve to one Q-id rather than two name-keyed rows.
func TestSplitPeopleSharedLinkTarget(t *testing.T) {
	ps := SplitPeople("[[Jonathan Dayton and Valerie Faris|Jonathan Dayton<br />Valerie Faris]]")
	if len(ps) != 2 {
		t.Fatalf("want 2 people, got %d: %q", len(ps), names(ps))
	}
	for _, p := range ps {
		if p.Wiki != "Jonathan Dayton and Valerie Faris" {
			t.Errorf("%s: wiki = %q, want the shared article title", p.Name, p.Wiki)
		}
	}
}

// No credit may carry raw wikitext into the database; 182 person rows did.
func TestSplitPeopleNeverLeaksWikitext(t *testing.T) {
	inputs := []string{
		"[[Brothers Quay|Stephen Quay<br />Timothy Quay]]",
		"{{Plainlist|\n* [[Danny Bensi and Saunder Jurriaans|Danny Bensi<br>Saunder Jurriaans]]\n}}",
		"[[Alice Smith]]<br />[[Bob Jones]]",
	}
	for _, in := range inputs {
		for _, p := range SplitPeople(in) {
			for _, bad := range []string{"[[", "]]", "|", "<br"} {
				if contains(p.Name, bad) {
					t.Errorf("SplitPeople(%q) leaked %q in name %q", in, bad, p.Name)
				}
			}
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// A linked credit's display name comes from inside the link. Text that happens
// to sit beside it is not part of the person's name — "[[Chris Hemsworth]] (5)"
// produced a person literally named "Chris Hemsworth (5)", which then could not
// be found by name. 6,670 people were mangled this way.
func TestNameComesFromInsideTheLink(t *testing.T) {
	cases := []struct{ in, wantName, wantWiki string }{
		{"[[Chris Hemsworth]] (5)", "Chris Hemsworth", "Chris Hemsworth"},
		{"[[Winston Sharples]] (uncredited)", "Winston Sharples", "Winston Sharples"},
		{"[[Olive Schreiner]] (novel)", "Olive Schreiner", "Olive Schreiner"},
		{"[[Rick Green (comedian)|Rick Green]]", "Rick Green", "Rick Green (comedian)"},
		// Only the first is linked; the trailing names are not this person's.
		{"[[Charlize Theron]], Juan Carlos Saizarbitoria", "Charlize Theron", "Charlize Theron"},
		// Unlinked credits still take the whole piece.
		{"Juan Carlos Saizarbitoria", "Juan Carlos Saizarbitoria", ""},
	}
	for _, c := range cases {
		got := parseOnePerson(c.in)
		if got.Name != c.wantName || got.Wiki != c.wantWiki {
			t.Errorf("parseOnePerson(%q) = {%q,%q}, want {%q,%q}",
				c.in, got.Name, got.Wiki, c.wantName, c.wantWiki)
		}
	}
}
