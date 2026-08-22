package filmstock

import (
	"regexp"
	"strings"
)

// TelevisionSeries is the structured record for one television series, with nested
// seasons and episodes.
type TelevisionSeries struct {
	Title          string            `json:"title"`
	PageID         int               `json:"page_id"`
	Type           string            `json:"type"` // always "television"
	WikiURL        string            `json:"wikipedia_url"`
	Overview       string            `json:"overview,omitempty"`
	Plot           string            `json:"plot,omitempty"`
	Genre          []string          `json:"genre,omitempty"`
	Creator        []Person          `json:"creator,omitempty"`
	Starring       []Person          `json:"starring,omitempty"`
	Network        []string          `json:"network,omitempty"`
	Country        []string          `json:"country,omitempty"`
	Composer       []Person          `json:"composer,omitempty"`
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
