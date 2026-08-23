package filmstock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A store carries the dictionary it was written with. Opening one without it
// must say so plainly: substituting a different dictionary does not fail
// cleanly, it surfaces later as unreadable records.
func TestOpeningAStoreWithNoDictionaryIsAnError(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, KindMovie), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := LoadDictionary(root, KindMovie)
	if err == nil {
		t.Fatal("want an error for a store with no dictionary")
	}
	if !strings.Contains(err.Error(), DictionaryName(KindMovie)) {
		t.Errorf("the error should name the missing file: %v", err)
	}
}

// Seeding writes what this build embeds, so a new store is self-describing from
// the moment it is created.
func TestWriteDictionariesSeedsANewStore(t *testing.T) {
	root := t.TempDir()
	if err := WriteDictionaries(root); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{KindMovie, KindTelevision, KindPerson, KindEvent} {
		got, err := LoadDictionary(root, kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if len(got) == 0 {
			t.Errorf("%s: empty dictionary", kind)
		}
	}
}

// Seeding must never overwrite what a store was already built with — those
// records can only be read with that dictionary.
func TestWriteDictionariesNeverOverwrites(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, DictionaryName(KindMovie))
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("an older dictionary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteDictionaries(root); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "an older dictionary" {
		t.Fatal("seeding overwrote a dictionary a store was built with")
	}
}
