package record

import (
	"regexp"
	"strings"
)

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

	// Aliases are additional link targets that resolve to this same article —
	// a renamed page leaves its old title behind as a redirect, and credits
	// keep linking through it. Identity is the page_id, so those credits
	// belong to this person, not to a second one.
	Aliases []string `json:"aliases,omitempty"`

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

// rePersonDisambig strips a trailing parenthetical from a person's article
// title. Unlike films, whose disambiguators are a small vocabulary — "(1985
// film)", "(film series)" — a person's is an open set of occupations and
// nationalities: 103,370 person articles carry one across 9,362 distinct forms,
// and a list of role words covers barely half of them (rower, sport shooter,
// diplomat, Royal Navy officer, judge). Wikipedia's convention is that the
// parenthetical is never part of the name, so any trailing one goes.
var rePersonDisambig = regexp.MustCompile(`\s*\([^()]*\)\s*$`)

// CleanPersonName is a person's display name, taken from their article title.
//
// The name used to come from whichever credit was recorded first, and with
// parsing spread across workers "first" meant arrival order. The same person
// then got a different name on different runs over identical input — 454 of
// them per run, flipping between "Bob Colleary" and "R.J. Colleary", between
// "Yvette González-Nacer" and "Yvette Gonzalez-Nacer", between "Ian \"Dicko\"
// Dickson" and "Ian Dickson". Identity was never in doubt: page_id and article
// title agreed every time. Only the label moved.
//
// So the label comes from the article title too, which is stable, canonical,
// and by Wikipedia's own naming convention the person's common name. This is a
// DISPLAY transform: nothing keys on it, exactly as with film titles.
func CleanPersonName(articleTitle string) string {
	return strings.TrimSpace(rePersonDisambig.ReplaceAllString(strings.TrimSpace(articleTitle), ""))
}
