package filmstock

// UnifiedResult is a type-tagged hit for the combined home-page search.
type UnifiedResult struct {
	Type     string // movie | television | episode | person | event
	Title    string
	Subtitle string
	Link     string
	Cover    string
	Score    float64
}
