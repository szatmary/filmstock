package filmstock

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// fixture builds a miniature but structurally real database: two films whose
// records exist both as loose files and inside a pack, with the offsets stored
// exactly as `filmstock pack` stores them.
func fixture(t *testing.T) (dbPath, root, packDir string) {
	t.Helper()
	root = t.TempDir()
	packDir = filepath.Join(root, "packs")
	if err := os.MkdirAll(filepath.Join(root, KindMovie, "a2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(packDir, 0o755); err != nil {
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

	var pack bytes.Buffer
	type loc struct{ off, length int }
	locs := map[int]loc{}
	for _, f := range films {
		b := gzipJSON(t, Movie{Title: f.title, PageID: f.id, Plot: "plot of " + f.title})
		if err := os.WriteFile(filepath.Join(root, KindMovie, f.path), b, 0o644); err != nil {
			t.Fatal(err)
		}
		locs[f.id] = loc{pack.Len(), len(b)}
		pack.Write(b)
	}
	if err := os.WriteFile(filepath.Join(packDir, KindMovie+".pack"), pack.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath = filepath.Join(root, "search.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE movies(id INTEGER PRIMARY KEY, title TEXT, path TEXT,
		pack_offset INTEGER, pack_length INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for _, f := range films {
		l := locs[f.id]
		if _, err := db.Exec(`INSERT INTO movies VALUES(?,?,?,?,?)`,
			f.id, f.title, f.path, l.off, l.length); err != nil {
			t.Fatal(err)
		}
	}
	return dbPath, root, packDir
}

// Dir and Remote must be interchangeable. If they ever disagree, a consumer's
// results depend on where they got their records, which defeats the point.
func TestDirAndRemoteAgree(t *testing.T) {
	dbPath, root, packDir := fixture(t)
	srv := httptest.NewServer(http.FileServer(http.Dir(packDir)))
	defer srv.Close()

	local, err := Open(dbPath, Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	remote, err := Open(dbPath, Remote(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	for _, id := range []int{3746, 1234} {
		a, err := local.Film(context.Background(), id)
		if err != nil {
			t.Fatalf("Dir film %d: %v", id, err)
		}
		b, err := remote.Film(context.Background(), id)
		if err != nil {
			t.Fatalf("Remote film %d: %v", id, err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("id %d: Dir gave %+v, Remote gave %+v", id, *a, *b)
		}
		if a.Plot == "" {
			t.Errorf("id %d: plot missing — the record was not decoded", id)
		}
	}
}

// The second record must come from its own byte range, not from offset 0. A
// pack reader that ignores the offset still passes a one-record test.
func TestRemoteReadsTheRightRange(t *testing.T) {
	dbPath, _, packDir := fixture(t)
	srv := httptest.NewServer(http.FileServer(http.Dir(packDir)))
	defer srv.Close()

	db, err := Open(dbPath, Remote(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := db.Film(context.Background(), 1234)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "Solaris" {
		t.Fatalf("got %q, want Solaris — offset was ignored", m.Title)
	}
}

// A host that ignores Range answers 200 with the whole pack. Streaming a
// gigabyte to answer a 5 KB lookup is worse than failing, so it must fail.
func TestRemoteRejectsHostThatIgnoresRange(t *testing.T) {
	dbPath, _, packDir := fixture(t)
	whole, err := os.ReadFile(filepath.Join(packDir, KindMovie+".pack"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // deliberately ignores r.Header["Range"]
		w.Write(whole)
	}))
	defer srv.Close()

	db, err := Open(dbPath, Remote(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Film(context.Background(), 3746); err == nil {
		t.Fatal("want an error when the host ignores Range, got none")
	} else if !strings.Contains(err.Error(), "206") {
		t.Errorf("error should name the missing 206: %v", err)
	}
}

// An unpacked database must say so, rather than issuing a nonsense range.
func TestRemoteOnUnpackedDatabaseExplainsItself(t *testing.T) {
	dbPath, _, packDir := fixture(t)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`UPDATE movies SET pack_offset = NULL, pack_length = NULL`)
	db.Close()

	srv := httptest.NewServer(http.FileServer(http.Dir(packDir)))
	defer srv.Close()
	h, err := Open(dbPath, Remote(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	_, err = h.Film(context.Background(), 3746)
	if err == nil || !strings.Contains(err.Error(), "filmstock pack") {
		t.Fatalf("want an error naming `filmstock pack`, got %v", err)
	}
}

// Search-only use is legitimate; it must not require a RecordSource, and asking
// for a record without one must explain what to pass.
func TestNilSourceIsUsableForSearchAndExplainsItself(t *testing.T) {
	dbPath, _, _ := fixture(t)
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
	dbPath, root, _ := fixture(t)
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
