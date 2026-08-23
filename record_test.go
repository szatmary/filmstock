package filmstock

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gzipJSON encodes v the way the extractor writes records.
func gzipJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	return buf.Bytes()
}

// fixture builds a miniature but structurally real index and record tree.
func fixture(t *testing.T) (dbPath, root string) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, KindMovie, "a2"), 0o755); err != nil {
		t.Fatal(err)
	}

	films := []struct {
		id    int
		title string
		path  string
	}{
		{3746, "Blade Runner", "a2/3746.json.gz"},
		{1234, "Solaris", "a2/1234.json.gz"},
	}

	for _, f := range films {
		b := gzipJSON(t, Movie{Title: f.title, PageID: f.id, Plot: "plot of " + f.title})
		if err := os.WriteFile(filepath.Join(root, KindMovie, f.path), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dbPath = filepath.Join(root, "search.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE movies(id INTEGER PRIMARY KEY, title TEXT, path TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, f := range films {
		if _, err := db.Exec(`INSERT INTO movies VALUES(?,?,?)`, f.id, f.title, f.path); err != nil {
			t.Fatal(err)
		}
	}
	return dbPath, root
}

// Search-only use is legitimate; it must not require a RecordSource, and asking
// for a record without one must explain what to pass.
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

// A missing id must be distinguishable from a failed fetch: one is a 404, the
// other a 500, and collapsing them makes a broken record source look like a
// missing record.
func TestMissingIDIsErrNotFound(t *testing.T) {
	dbPath, root := fixture(t)
	db, err := Open(dbPath, Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Film(context.Background(), 999999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// A real id whose record file is missing is NOT "not found" — the row exists.
	os.Remove(filepath.Join(root, KindMovie, "a2/3746.json.gz"))
	if _, err := db.Film(context.Background(), 3746); errors.Is(err, ErrNotFound) {
		t.Error("a present row with an unreadable record must not report ErrNotFound")
	}
}

// Dir must read the record the index points at, not merely the first one.
func TestDirReadsTheRecordTheIndexNames(t *testing.T) {
	dbPath, root := fixture(t)
	db, err := Open(dbPath, Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := db.Film(context.Background(), 1234)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "Solaris" {
		t.Fatalf("got %q, want Solaris", m.Title)
	}
}
