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
	Season      int    `json:"season"`
	NumEpisodes int    `json:"num_episodes"`
	FirstAired  string `json:"first_aired,omitempty"`
	LastAired   string `json:"last_aired,omitempty"`

	// PageID is the season's own article, when it has one. Seasons are real
	// pages with real ids, so a season is addressable like everything else here
	// rather than being reachable only as an index into its series.
	PageID int `json:"page_id,omitempty"`

	// Starring is this season's cast, which is the thing a series-level cast
	// list cannot express. TelevisionSeries.Starring is one flat list for a
	// show's whole run, so a fifteen-season series asserts that everyone who
	// ever appeared was in it throughout — Clooney was in ER for five seasons of
	// fifteen. That is not missing coverage, it is a modelling error recorded as
	// fact.
	Starring []Person `json:"starring,omitempty"`

	// Network can change mid-run: a show cancelled by one network and picked up
	// by another keeps its title and its series record.
	Network string `json:"network,omitempty"`
	Image   string `json:"image,omitempty"`

	// Nielsen figures for the season as a whole, from {{Series overview}}.
	// Per SEASON, deliberately: an episode inherits these by membership rather
	// than carrying a copy, because attaching a season's rank to each of its
	// episodes would invent precision the source does not have.
	Rank    int     `json:"rank,omitempty"`
	Rating  float64 `json:"rating,omitempty"`
	Viewers float64 `json:"viewers,omitempty"`

	Episodes []*Episode `json:"episodes,omitempty"`
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

	// ProdCode is the production code exactly as the row states it, and is an
	// OPAQUE per-series label — never a number, never comparable across series.
	// Real values include 5ACV01, 4F02, 456601 and 4A. Sorting by it recovers
	// production order within some series (Doctor Who season 12 states 4A, 4C,
	// 4B, 4E, 4D in air order) but the codes are not fixed-width everywhere, so
	// filmstock states the label and derives no ordering from it.
	//
	// Where the row names more than one code — a rerun under a second code, an
	// episode held over from the previous production block — they are joined
	// with " / ", the same way a two-segment title is.
	ProdCode string `json:"prod_code,omitempty"`

	// Viewers is US viewership in millions, as the episode list states it.
	//
	// Per EPISODE, unlike the Nielsen rank and rating carried on a schedule
	// grid, which are per SEASON — a season's rank stamped onto each of its
	// episodes would invent precision the source does not have.
	Viewers float64 `json:"viewers,omitempty"`
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
