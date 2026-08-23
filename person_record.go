package filmstock

// PersonRecord is a person as an entity in its own right, rather than a row
// synthesized while indexing. It exists only where there is a stated identity —
// a Q-id, or failing that the wiki article the credit links to. A bare name with
// no link is NOT a person: keying one by its display string merges strangers.
type PersonRecord struct {
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
