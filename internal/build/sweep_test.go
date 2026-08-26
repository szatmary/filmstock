package build

import (
	"testing"

	"github.com/szatmary/filmstock"
)

// An export derives the complete set of records, so a key left in the store
// without being written is one the encyclopaedia no longer supports. Without
// the sweep an export into an EXISTING store could only ever add: a fresh
// export into an empty directory looked correct because the stale records
// simply were not written, and running the same export over yesterday's store
// kept every one of them.
func TestSweepRemovesWhatWasNotWritten(t *testing.T) {
	dir := t.TempDir()
	if err := filmstock.WriteDictionaries(dir); err != nil {
		t.Fatal(err)
	}

	// Yesterday: two films.
	w := newStoreWriter(dir)
	w.wrote = map[string]map[string]bool{}
	w.put(filmstock.KindMovie, 1, map[string]any{"title": "Kept"})
	w.put(filmstock.KindMovie, 2, map[string]any{"title": "Gone Tomorrow"})
	if n := w.sweep(); n != 0 {
		t.Fatalf("swept %d on a run that wrote everything", n)
	}
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}

	// Today: only the first still qualifies.
	w2 := newStoreWriter(dir)
	w2.wrote = map[string]map[string]bool{}
	w2.put(filmstock.KindMovie, 1, map[string]any{"title": "Kept"})
	removed := w2.sweep()
	if removed != 1 {
		t.Errorf("swept %d records, want 1", removed)
	}
	if err := w2.Err(); err != nil {
		t.Fatal(err)
	}

	db, err := filmstock.OpenStore(dir, filmstock.KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	if !db.Has("1") {
		t.Error("swept a record that was written this run")
	}
	if db.Has("2") {
		t.Error("a record the run did not produce survived")
	}
}

// A partial run has not reached most records. Sweeping then would delete the
// rest of the store, so it must be off unless the export is complete.
func TestPartialRunNeverSweeps(t *testing.T) {
	dir := t.TempDir()
	if err := filmstock.WriteDictionaries(dir); err != nil {
		t.Fatal(err)
	}
	w := newStoreWriter(dir)
	w.wrote = map[string]map[string]bool{}
	w.put(filmstock.KindMovie, 1, map[string]any{"title": "A"})
	w.put(filmstock.KindMovie, 2, map[string]any{"title": "B"})
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}

	partial := newStoreWriter(dir) // wrote left nil: sweep disabled
	partial.put(filmstock.KindMovie, 1, map[string]any{"title": "A"})
	if n := partial.sweep(); n != 0 {
		t.Errorf("a partial run swept %d records", n)
	}
	if err := partial.Err(); err != nil {
		t.Fatal(err)
	}

	db, _ := filmstock.OpenStore(dir, filmstock.KindMovie)
	if !db.Has("2") {
		t.Error("a partial run deleted a record it simply had not reached")
	}
}
