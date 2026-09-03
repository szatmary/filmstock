package record

// A Link is one entry in a list field, with its Wikipedia link target kept.
//
// The link target is an identity; the display text is not. "Warner Bros.
// (worldwide)" and "Warner Bros." are the same company rendered two ways, and
// keying on the rendering would leave them unconnected — the same mistake that
// fifty differently-disambiguated "Big Brother" articles punish on the
// television side.
//
// Wiki is empty when the source did not link, which is ordinary and is not
// guessed at: an unlinked "Warner Bros." states a name and no identity, and
// inventing one by matching the string is exactly what this type exists to
// avoid.
type Link struct {
	Name string `json:"name"`
	Wiki string `json:"wiki,omitempty"`
	Note string `json:"note,omitempty"` // parenthetical qualifier, e.g. "worldwide"
}

// Names returns just the display strings, for callers that only render.
func Names(ls []Link) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.Name
	}
	return out
}
