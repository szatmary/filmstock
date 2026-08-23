package filmstock

import (
	"regexp"
	"strings"
)

// TelevisionSeries is the structured record for one television series, with nested
// seasons and episodes.
type TelevisionSeries struct {
	Title    string   `json:"title"`
	PageID   int      `json:"page_id"`
	Type     string   `json:"type"` // always "television"
	WikiURL  string   `json:"wikipedia_url"`
	Overview string   `json:"overview,omitempty"`
	Plot     string   `json:"plot,omitempty"`
	Genre    []string `json:"genre,omitempty"`
	Creator  []Person `json:"creator,omitempty"`
	Starring []Person `json:"starring,omitempty"`
	Network  []Link   `json:"network,omitempty"`
	Country  []Link   `json:"country,omitempty"`
	Composer []Person `json:"composer,omitempty"`

	// Series-level crew. Films have carried these from the beginning;
	// television did not, so "who directed this series" had no answer while the
	// same question about a film did. They were present on a majority of series
	// infoboxes and read by nothing, which silently shrank the people graph.
	Director            []Person `json:"director,omitempty"`
	Producer            []Person `json:"producer,omitempty"`
	ExecutiveProducer   []Person `json:"executive_producer,omitempty"`
	Writer              []Person `json:"writer,omitempty"`
	Editor              []Person `json:"editor,omitempty"`
	Cinematography      []Person `json:"cinematography,omitempty"`
	Presenter           []Person `json:"presenter,omitempty"`
	Narrator            []Person `json:"narrator,omitempty"`
	ProductionCompanies []Link   `json:"production_companies,omitempty"`

	// References to other works. BasedOn is usually a book or play and often
	// outside this database; Related points at other series, which usually are
	// in it. Both keep the link target, because the target is what could ever
	// resolve to a record and the display string never can.
	BasedOn        []Link            `json:"based_on,omitempty"`
	Related        []Link            `json:"related,omitempty"`
	NumSeasons     string            `json:"num_seasons,omitempty"`
	NumEpisodes    string            `json:"num_episodes,omitempty"`
	FirstAired     string            `json:"first_aired,omitempty"`
	LastAired      string            `json:"last_aired,omitempty"`
	Runtime        string            `json:"runtime,omitempty"`
	CoverImageFile string            `json:"cover_image_file,omitempty"`
	CoverImageURL  string            `json:"cover_image_url,omitempty"`
	ListEpisodes   string            `json:"list_episodes,omitempty"` // linked episode-list article
	Seasons        []*Season         `json:"seasons,omitempty"`
	Raw            map[string]string `json:"raw_infobox,omitempty"`
}

// Season groups episodes; SeasonNum is the season/series number.
type Season struct {
	Season      int        `json:"season"`
	NumEpisodes int        `json:"num_episodes"`
	FirstAired  string     `json:"first_aired,omitempty"`
	LastAired   string     `json:"last_aired,omitempty"`
	Episodes    []*Episode `json:"episodes,omitempty"`
}

// Episode is one episode row parsed from an {{Episode list}} template.
type Episode struct {
	NumberOverall  int      `json:"number_overall,omitempty"`
	NumberInSeason int      `json:"number_in_season,omitempty"`
	Title          string   `json:"title"`
	AirDate        string   `json:"air_date,omitempty"`
	DirectedBy     []Person `json:"directed_by,omitempty"`
	WrittenBy      []Person `json:"written_by,omitempty"`
	Summary        string   `json:"summary,omitempty"`
}

// reTelevisionDisambig strips a trailing TV disambiguation parenthetical.
//
// This is a DISPLAY transform only. It was once also used to build the key that
// joined a series to its episodes, which meant two shows differing only by
// disambiguator collapsed into one and the loser was silently dropped. The
// disambiguator is the only thing telling those shows apart, so nothing derived
// from a stripped title may ever be used as an identity or join key — see
// loadSeasonOf, which gets the season->series edge from Wikidata instead.
var reTelevisionDisambig = regexp.MustCompile(`(?i)\s*\([^()]*\b(?:TV series|TV programme|TV program|TV serial|miniseries|anime|web series|franchise)\b[^()]*\)\s*$`)

// CleanTelevisionTitle strips a trailing "(… TV series)" disambiguator for display and
// ranking (the analog of CleanTitle for films).
func CleanTelevisionTitle(title string) string {
	return strings.TrimSpace(reTelevisionDisambig.ReplaceAllString(title, ""))
}
