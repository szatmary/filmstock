package record

import "testing"

// The parenthetical is the only thing telling 31,934 films apart, and it lives
// only in the URL once the display title has been cleaned.
func TestWikiTitleFromURL(t *testing.T) {
	for u, want := range map[string]string{
		"https://en.wikipedia.org/wiki/Vertigo_(1958_film)":      "Vertigo (1958 film)",
		"https://en.wikipedia.org/wiki/Heat_(1995_film)":         "Heat (1995 film)",
		"https://en.wikipedia.org/wiki/Casablanca_(film)":        "Casablanca (film)",
		"https://en.wikipedia.org/wiki/The_Godfather":            "The Godfather",
		"https://en.wikipedia.org/wiki/Am%C3%A9lie":              "Amélie",
		"https://en.wikipedia.org/wiki/Friends_(2002_TV_series)": "Friends (2002 TV series)",
		"":                          "",
		"http://example.com/wiki/X": "",
	} {
		if got := WikiTitleFromURL(u); got != want {
			t.Errorf("WikiTitleFromURL(%q) = %q, want %q", u, got, want)
		}
	}
}
