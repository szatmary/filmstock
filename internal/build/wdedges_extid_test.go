package build

import (
	"encoding/json"
	"testing"
)

// An external identifier's datavalue is a plain string, not the entity
// reference a relation claim carries, so it needs its own decoder.
func TestClaimStringsReadsIdentifiers(t *testing.T) {
	raw := json.RawMessage(`[
	  {"rank":"normal","mainsnak":{"snaktype":"value","datavalue":{"value":"tt0068646"}}},
	  {"rank":"deprecated","mainsnak":{"snaktype":"value","datavalue":{"value":"tt9999999"}}},
	  {"rank":"normal","mainsnak":{"snaktype":"novalue"}},
	  {"rank":"preferred","mainsnak":{"snaktype":"value","datavalue":{"value":"  tt0071562 "}}}
	]`)
	got := claimStrings(raw)
	want := []string{"tt0068646", "tt0071562"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// A relation claim must not be read as an identifier, nor the reverse: their
// datavalues have different shapes and confusing them yields silent nonsense.
func TestRelationClaimsAreNotIdentifiers(t *testing.T) {
	relation := json.RawMessage(`[{"rank":"normal","mainsnak":{"snaktype":"value",
	  "datavalue":{"value":{"entity-type":"item","numeric-id":582972,"id":"Q582972"}}}}]`)
	if got := claimStrings(relation); len(got) != 0 {
		t.Errorf("read a relation as an identifier: %v", got)
	}
	if got := claimTargets(relation); len(got) != 1 || got[0] != 582972 {
		t.Errorf("relation targets = %v, want [582972]", got)
	}
}

// Every property we extract must have a name, since the name is the column
// value consumers join on.
func TestEveryExternalPropertyIsNamed(t *testing.T) {
	for p, name := range externalIDProps {
		if name == "" {
			t.Errorf("property %s has no source name", p)
		}
		if p == "" || p[0] != 'P' {
			t.Errorf("%q is not a property id", p)
		}
	}
	if externalIDProps["P345"] != "imdb" {
		t.Errorf("P345 is IMDb ID; got %q", externalIDProps["P345"])
	}
}
