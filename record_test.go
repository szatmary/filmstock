package filmstock

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szatmary/gitdb"
)

// fixture builds a miniature but structurally real index and record store: two
// films written to a gitdb store, with the index carrying the mapping from
// page_id to the store's own record id.
func fixture(t *testing.T) (dbPath, root string) {
	t.Helper()
	root = t.TempDir()

	store, err := gitdb.Open(filepath.Join(root, KindMovie), storeOptions(KindMovie)...)
	if err != nil {
		t.Fatal(err)
	}
	films := []struct {
		pageID int
		title  string
	}{
		{3746, "Blade Runner"},
		{1234, "Solaris"},
	}
	ids := map[int]uint64{}
	for _, f := range films {
		raw, err := json.Marshal(Movie{Title: f.title, PageID: f.pageID, Plot: "plot of " + f.title})
		if err != nil {
			t.Fatal(err)
		}
		id, err := store.Insert(raw)
		if err != nil {
			t.Fatal(err)
		}
		ids[f.pageID] = id
	}

	dbPath = filepath.Join(root, "index.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE movies(id INTEGER PRIMARY KEY, title TEXT, gitdb_id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for _, f := range films {
		if _, err := db.Exec(`INSERT INTO movies VALUES(?,?,?)`, f.pageID, f.title, ids[f.pageID]); err != nil {
			t.Fatal(err)
		}
	}
	return dbPath, root
}

func TestStoreRoundTrip(t *testing.T) {
	dbPath, root := fixture(t)
	db, err := Open(dbPath, Store(root))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := db.Film(context.Background(), 3746)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "Blade Runner" || m.Plot == "" {
		t.Fatalf("got %+v", m)
	}
}

// The second film must come from its own record. A reader that ignored the
// mapping and returned record 1 would pass a single-record test.
func TestStoreReadsTheRecordTheIndexNames(t *testing.T) {
	dbPath, root := fixture(t)
	db, err := Open(dbPath, Store(root))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := db.Film(context.Background(), 1234)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "Solaris" {
		t.Fatalf("got %q, want Solaris — the page_id -> record mapping was ignored", m.Title)
	}
}

// A page_id that is not in the index is not found; that is distinct from a
// fetch that failed, and callers turn one into a 404 and the other into a 500.
func TestMissingIDIsErrNotFound(t *testing.T) {
	dbPath, root := fixture(t)
	db, err := Open(dbPath, Store(root))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Film(context.Background(), 999999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// An index row pointing at a record id the store never allocated means the two
// are out of step. That has to say so, not return an empty record.
func TestIndexPointingAtAMissingRecordIsAnError(t *testing.T) {
	dbPath, root := fixture(t)
	h, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	h.Exec(`UPDATE movies SET gitdb_id = 99999 WHERE id = 3746`)
	h.Close()

	db, err := Open(dbPath, Store(root))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Film(context.Background(), 3746); err == nil {
		t.Fatal("want an error for an index that names a record the store does not have")
	}
}

// Search-only use is legitimate and must not require a record source; asking
// for a record without one has to explain what is missing.
func TestNilSourceIsUsableForSearchAndExplainsItself(t *testing.T) {
	dbPath, _ := fixture(t)
	db, err := Open(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Film(context.Background(), 3746)
	if err == nil || !strings.Contains(err.Error(), "RecordSource") {
		t.Fatalf("want an error naming RecordSource, got %v", err)
	}
}
