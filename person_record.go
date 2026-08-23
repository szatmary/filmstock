package filmstock

// PersonRecord is a person as an entity in its own right, rather than a row
// synthesized while indexing. It exists only where there is a stated identity —
// a Q-id, or failing that the wiki article the credit links to. A bare name with
// no link is NOT a person: keying one by its display string merges strangers.
type PersonRecord struct {
	// PageID is the person's identity, the same enwiki page_id every other kind
	// of record is keyed on. Uniform identity across all four kinds means one
	// rule to reason about instead of a special case for people.
	//
	// A Q-id would serve equally well for anyone who has one — Wikidata items
	// are independent of enwiki titles, so both survive a page rename — but only
	// 63.5% of credited people have one, and using it here made person identity
	// depend on Wikidata for no gain over the page_id already in the dump.
	//
	// Zero for a credit whose link target has no article at all. Those people
	// have no canonical identity of any kind; see the count reported by extract.
	PageID  int    `json:"page_id,omitempty"`
	QID     int64  `json:"qid,omitempty"`
	Wiki    string `json:"wiki,omitempty"`
	Name    string `json:"name"`
	WikiURL string `json:"wikipedia_url,omitempty"`

	// Everything below comes from the person's OWN article, and is absent for
	// anyone whose credit links to a page that is not a biography — a redlink,
	// a disambiguation page, or a production company mistaken for a person.
	*PersonBio
}

// PersonBio is what a person's own Wikipedia article adds beyond their name and
// identity. Separate from PersonRecord so "we have a biography" is a single nil
// check rather than a guess at which fields happen to be populated.
type PersonBio struct {
	BirthName   string   `json:"birth_name,omitempty"`
	BirthDate   string   `json:"birth_date,omitempty"`
	BirthPlace  string   `json:"birth_place,omitempty"`
	DeathDate   string   `json:"death_date,omitempty"`
	DeathPlace  string   `json:"death_place,omitempty"`
	Occupation  []string `json:"occupation,omitempty"`
	YearsActive string   `json:"years_active,omitempty"`
	Nationality string   `json:"nationality,omitempty"`
	Image       string   `json:"image,omitempty"`
	Overview    string   `json:"overview,omitempty"`
}
