package wikitext

import "testing"

func TestSplitLinksKeepsTargets(t *testing.T) {
	got := SplitLinks("[[Warner Bros.]] (worldwide)<br>[[Shaw Brothers Studio|Shaw Brothers]] (Hong Kong)")
	if len(got) != 2 {
		t.Fatalf("got %d entries: %+v", len(got), got)
	}
	if got[0].Name != "Warner Bros." || got[0].Wiki != "Warner Bros." || got[0].Note != "worldwide" {
		t.Errorf("entry 0 = %+v", got[0])
	}
	// The display text is what a reader sees; the TARGET is what identifies the
	// company. A piped link must keep both, and they differ here.
	if got[1].Name != "Shaw Brothers" || got[1].Wiki != "Shaw Brothers Studio" {
		t.Errorf("entry 1 = %+v — piped link lost its target", got[1])
	}
}

// An unlinked entry states a name and no identity. Inventing one by matching the
// string is the failure this type exists to prevent.
func TestUnlinkedEntryHasNoTarget(t *testing.T) {
	got := SplitLinks("Universal Pictures")
	if len(got) != 1 || got[0].Name != "Universal Pictures" || got[0].Wiki != "" {
		t.Fatalf("got %+v", got)
	}
}

// A qualifier that is itself a link must not become a separate company.
func TestTrailingLinksAreNotSeparateEntries(t *testing.T) {
	got := SplitLinks("[[Warner Bros.]] in [[North America]]")
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(got), got)
	}
	if got[0].Wiki != "Warner Bros." {
		t.Errorf("identity should be the first link: %+v", got[0])
	}
}

func TestBulletAndPlainlistForms(t *testing.T) {
	for _, in := range []string{
		"* [[A Studio]]\n* [[B Studio]]",
		"{{plainlist|\n* [[A Studio]]\n* [[B Studio]]\n}}",
		"[[A Studio]]<br />[[B Studio]]",
	} {
		got := SplitLinks(in)
		if len(got) != 2 || got[0].Wiki != "A Studio" || got[1].Wiki != "B Studio" {
			t.Errorf("%q -> %+v", in, got)
		}
	}
}
