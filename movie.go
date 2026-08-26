package filmstock

import (
	"net/url"
	"strconv"
	"strings"
)

// Person is one credited person: display Name plus the Wiki link target (the
// Wikipedia article title), which is the identity we resolve to a Wikidata Q-id.
// Wiki is "" for unlinked / red-linked names.
type Person struct {
	Name string `json:"name"`
	Wiki string `json:"wiki,omitempty"`
}

// Movie is the structured record we emit as JSON, one file per film.
type Movie struct {
	Title        string   `json:"title"`
	PageID       int      `json:"page_id"`
	WikiURL      string   `json:"wikipedia_url"`
	Overview     string   `json:"overview,omitempty"`
	Plot         string   `json:"plot,omitempty"`
	Genre        []string `json:"genre,omitempty"`
	ReleaseDates []string `json:"release_dates,omitempty"`
	Director     []Person `json:"director,omitempty"`
	Producer     []Person `json:"producer,omitempty"`
	Writer       []Person `json:"writer,omitempty"`
	Starring     []Person `json:"starring,omitempty"`
	Music        []Person `json:"music,omitempty"`
	// Narrator is stated by 4,782 films and was read for television but not for
	// film — Orson Welles on History of the World, Part I, Richard Burton on
	// Zulu, Michael Hordern on Barry Lyndon. A narrator is a performance credit
	// like any other.
	Narrator            []Person `json:"narrator,omitempty"`
	Cinematography      []Person `json:"cinematography,omitempty"`
	Editing             []Person `json:"editing,omitempty"`
	ProductionCompanies []Link   `json:"production_companies,omitempty"`
	Distributor         []Link   `json:"distributor,omitempty"`

	// The work a film adapts. Usually a book or play and often outside this
	// database — but the link target is what could ever resolve, and it was
	// present on a third of films and read by nothing.
	BasedOn        []Link `json:"based_on,omitempty"`
	Country        []Link `json:"country,omitempty"`
	Language       []Link `json:"language,omitempty"`
	Runtime        string `json:"runtime,omitempty"`
	Budget         string `json:"budget,omitempty"`
	Gross          string `json:"gross,omitempty"`
	CoverImageFile string `json:"cover_image_file,omitempty"`
	CoverImageURL  string `json:"cover_image_url,omitempty"`
	// Raw holds the untouched infobox parameters for later expansion.
	Raw map[string]string `json:"raw_infobox,omitempty"`
}

// FilePathURL builds a Special:FilePath URL, which MediaWiki 302-redirects to
// the real file regardless of whether it lives on Wikimedia Commons or is a
// non-free file hosted locally on en.wikipedia. A positive width yields a
// resized thumbnail instead of the full-resolution original.
func FilePathURL(file string, width int) string {
	name := strings.TrimSpace(file)
	for _, p := range []string{"File:", "file:", "Image:", "image:"} {
		name = strings.TrimPrefix(name, p)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	u := "https://en.wikipedia.org/wiki/Special:FilePath/" + url.PathEscape(strings.ReplaceAll(name, " ", "_"))
	if width > 0 {
		u += "?width=" + strconv.Itoa(width)
	}
	return u
}

// CoverImageURL returns the cleaned file name and a resolver URL for it.
func CoverImageURL(filename string) (string, string) {
	name := strings.TrimSpace(filename)
	for _, p := range []string{"File:", "file:", "Image:", "image:"} {
		name = strings.TrimPrefix(name, p)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ""
	}
	return name, FilePathURL(name, 0)
}
