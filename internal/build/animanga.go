package build

import (
	"strings"

	"github.com/szatmary/filmstock/internal/wikitext"
)

// Anime articles do not use {{Infobox television}}.
//
// They use {{Infobox animanga}}, split into sub-templates: /Header for the
// franchise, /Print for the manga, and /Video for each adaptation. Since we
// matched only "Infobox television", the article was never claimed at all —
// Naruto, One-Punch Man, Code Geass, Samurai Champloo, Serial Experiments Lain
// and Cardcaptor Sakura are simply absent, and their episode-list articles are
// left with no series to attach to. On a sample of 51 well-known anime we held
// 30.
//
// Where a separate "X (TV series)" article exists we already had the show; this
// is for the franchise articles, which is most of them.
//
// One article carries several /Video blocks — Code Geass has two television
// series and four OVAs — and `type` says which is which. Only the broadcast
// forms are television; an OVA or a film is not a series.
var animangaTVTypes = map[string]bool{
	"tv series": true, "tv": true, "tv anime": true, "anime television series": true,
	"television series": true, "ona": true, "net animation": true,
}

// FindAnimangaSeries returns the first {{Infobox animanga/Video}} block on the
// page that describes a broadcast series, with its parameters translated to the
// names {{Infobox television}} uses so the ordinary builder can read them.
//
// The FIRST such block, because identity here is the article's page_id and an
// article is one record. Code Geass's second television series, R2, is a
// separate season of the same franchise page and is not separately addressable
// — a limitation of keying on the article, not of this reader.
func FindAnimangaSeries(text string) (map[string]string, bool) {
	for _, body := range wikitext.FindAllTemplates(text, "Infobox animanga/Video") {
		ib := wikitext.ParseInfobox(body)
		t := strings.ToLower(strings.TrimSpace(wikitext.CleanText(ib["type"])))
		if !animangaTVTypes[t] {
			continue
		}
		return animangaToTelevision(ib), true
	}
	return nil, false
}

// animangaToTelevision renames the fields. The values are untouched: they are
// the same wikitext in the same forms, and every existing parser — dates,
// people, links — reads them unchanged.
var animangaFields = map[string]string{
	"first":        "first_aired",
	"last":         "last_aired",
	"episodes":     "num_episodes",
	"episode_list": "list_episodes",
	"studio":       "company",
	"licensor":     "distributor",
}

func animangaToTelevision(ib map[string]string) map[string]string {
	out := make(map[string]string, len(ib))
	for k, v := range ib {
		if to, ok := animangaFields[k]; ok {
			// Never over an existing value: a block stating both keeps its own.
			if out[to] == "" {
				out[to] = v
			}
			continue
		}
		if out[k] == "" {
			out[k] = v
		}
	}
	// The franchise title belongs to the article, not to one adaptation block:
	// Code Geass's block is titled "Code Geass: Lelouch of the Rebellion" while
	// the article — and so the page_id, and so the record — is "Code Geass".
	delete(out, "title")
	return out
}
