package filmstock

// Award ceremonies and film festivals as a first-class record type.
//
// These arrived in the database by accident: `{{Infobox film awards}}` and
// `{{Infobox Film festival}}` both satisfy a PREFIX test for `{{Infobox film}}`,
// so 2,418 awards ceremonies and 1,647 festivals were indexed as films — with
// year 0, no cast and no director, sitting inert in film search results. It is
// the same collision that put standalone episode articles into television search, fixed
// there with findTemplateExact and assumed harmless here.
//
// The fix is not to drop them. A ceremony is broadcast on a network and a
// festival streams its programme: they are media in their own right, with their
// own natural key (page_id), their own schema, and their own answerable
// questions — "who hosted the 53rd NAACP Image Awards", "what opened Cannes in
// 2017". They were being thrown away as a side effect of being mistyped.
//
// What the infoboxes actually carry, which is why this is worth typing properly:
//
//	film awards     award, number, date, host, site, network, most_wins,
//	                most_nominations, film (the best-picture winner), previous/next
//	film festival   name, number, date, location, host, opening, closing, main,
//	                awards, founded, previous/next
//
// `previous`/`next` make each ceremony an ordered series — the same structure
// television seasons have, and the reason `number` is parsed rather than kept as
// prose.

type Event struct {
	Title   string `json:"title"`
	PageID  int    `json:"page_id"`
	WikiURL string `json:"wikipedia_url"`

	// Kind separates the two, because they answer different questions and a
	// single "event" bucket would make both unqueryable.
	Kind    string `json:"kind"` // award_ceremony | film_festival
	Award   string `json:"award,omitempty"`
	Edition int    `json:"edition,omitempty"` // 53rd -> 53
	Date    string `json:"date,omitempty"`
	Year    int    `json:"year,omitempty"`

	// Only ceremonies have a human host. In {{Infobox Film festival}} the same
	// parameter names the ORGANISING BODY — "Toronto International Film Festival
	// Group", "Istanbul Foundation for Culture and Arts" — so parsing it as a
	// Person put 109 organisations into the people table. Kept as a plain string
	// rather than guessed at: a few festivals really do list a human compère
	// (Cannes 2004: Laura Morante), and there is no stated fact distinguishing
	// them, so we record what the field says instead of inventing a type for it.
	Hosts     []Person `json:"hosts,omitempty"`
	Organizer string   `json:"organizer,omitempty"`
	Venue     string   `json:"venue,omitempty"`
	Location  string   `json:"location,omitempty"`
	Network   []string `json:"network,omitempty"` // televised

	Previous string `json:"previous,omitempty"`
	Next     string `json:"next,omitempty"`

	BestFilm        string `json:"best_film,omitempty"`
	MostWins        string `json:"most_wins,omitempty"`
	MostNominations string `json:"most_nominations,omitempty"`
	OpeningFilm     string `json:"opening_film,omitempty"`
	ClosingFilm     string `json:"closing_film,omitempty"`
	MainCompetition string `json:"main_competition,omitempty"`
	Founded         string `json:"founded,omitempty"`

	Overview       string            `json:"overview,omitempty"`
	CoverImageFile string            `json:"cover_image_file,omitempty"`
	RawInfobox     map[string]string `json:"raw_infobox,omitempty"`
}

const (
	EventAwardCeremony = "award_ceremony"
	EventFilmFestival  = "film_festival"
)
