package wikitext

import (
	"strings"
	"testing"
)

// Wikipedia converted {{Episode list}} to a Lua module. The parameters are
// unchanged; only the opening differs. Matching just the template form skipped
// every converted article — ER, fifteen seasons and 331 episodes, had none.
func TestFindAllTemplatesMatchesModuleInvocations(t *testing.T) {
	text := `
{{Episode table |overall=5 |season=5}}
{{#invoke:Episode list|sublist|ER season 1
|EpisodeNumber=1
|Title=[[24 Hours (ER)|24 Hours]]
|DirectedBy=[[Rod Holcomb]]
|OriginalAirDate={{Start date|1994|9|19}}
}}
{{#invoke:Episode list|sublist|ER season 1
|EpisodeNumber=2
|Title=Day One
}}
{{Episode list
|EpisodeNumber=3
|Title=Old Style
}}`
	got := FindAllTemplates(text, "Episode list")
	if len(got) != 3 {
		for i, g := range got {
			t.Logf("  [%d] %.60s", i, g)
		}
		t.Fatalf("found %d episode entries, want 3 (two module, one template)", len(got))
	}
	for _, want := range []string{"24 Hours", "Day One", "Old Style"} {
		var seen bool
		for _, g := range got {
			if strings.Contains(g, want) {
				seen = true
			}
		}
		if !seen {
			t.Errorf("%q missing from the results", want)
		}
	}
}
