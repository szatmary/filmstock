package filmstock

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// fakeRelease lays out one tiny but real build the way the bucket will:
// builds.json at the root, manifest and files under the build id.
func fakeRelease(t *testing.T, root, id, title string) {
	t.Helper()
	dir := filepath.Join(root, id)
	os.MkdirAll(dir, 0o777)
	core := filepath.Join(dir, "filmstock.db")
	os.Remove(core)
	h, err := sql.Open("sqlite", core)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE movies(id INTEGER PRIMARY KEY, title TEXT, year INTEGER,
		  release_date TEXT, director TEXT, producer TEXT, writer TEXT, starring TEXT,
		  music TEXT, distributor TEXT, country TEXT, language TEXT, genre TEXT,
		  runtime TEXT, budget TEXT, gross TEXT, wikipedia_url TEXT,
		  cover_image_url TEXT, cover_image_file TEXT, wiki_title TEXT)`,
		`INSERT INTO movies(id,title,year,starring,director,cover_image_file)
		   VALUES (1,'` + title + `',1982,'','','')`,
		`CREATE VIRTUAL TABLE movies_fts USING fts5(title, starring, director,
		  content='movies', content_rowid='id', tokenize='trigram')`,
	} {
		if _, err := h.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	content, _, err := ContentHash(h)
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	sha, err := fileSHA256(core)
	if err != nil {
		t.Fatal(err)
	}
	man := map[string]any{"dump": id, "files": map[string]any{
		"filmstock.db": map[string]any{"sha256": sha, "content_hash": content},
	}}
	mb, _ := json.Marshal(man)
	os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644)
	cat := map[string]any{"latest_full": id,
		"builds": []map[string]any{{"id": id, "kind": "full"}}}
	cb, _ := json.Marshal(cat)
	os.WriteFile(filepath.Join(root, "builds.json"), cb, 0o644)
}

// The whole loop, over HTTP: fresh install fetches, verifies, rebuilds FTS and
// commits; a second run is a no-op; a NEWER release is taken and the swap
// hands back the old handle still open.
func TestUpdaterFullCycle(t *testing.T) {
	bucket := t.TempDir()
	fakeRelease(t, bucket, "20260801", "Blade Runner")
	srv := httptest.NewServer(http.FileServer(http.Dir(bucket)))
	defer srv.Close()

	u := NewUpdater(srv.URL, t.TempDir())
	ctx := context.Background()

	core, build, changed, err := u.Update(ctx)
	if err != nil || !changed || build != "20260801" {
		t.Fatalf("first update: %v changed=%v build=%s", err, changed, build)
	}
	db, err := Open(core, nil)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(db)
	if r, _ := live.DB().SearchFilms(ctx, "blade", "", 5); len(r) != 1 {
		t.Fatalf("search after install: %d results", len(r))
	}

	if _, _, changed, err = u.Update(ctx); err != nil || changed {
		t.Fatalf("second update should be a no-op: %v changed=%v", err, changed)
	}

	fakeRelease(t, bucket, "20260820", "Blade Runner 2049")
	old, changed, err := u.UpdateAndSwap(ctx, live, nil)
	if err != nil || !changed || old == nil {
		t.Fatalf("swap: %v changed=%v old=%v", err, changed, old)
	}
	if r, _ := live.DB().SearchFilms(ctx, "2049", "", 5); len(r) != 1 {
		t.Fatalf("search after swap: %d results", len(r))
	}
	// The old handle still answers until the caller closes it.
	if r, _ := old.SearchFilms(ctx, "blade", "", 5); len(r) != 1 {
		t.Fatal("old handle died before being closed")
	}
	old.Close()
	if u.Current() != "20260820" {
		t.Fatalf("state says %s", u.Current())
	}
}

// A corrupted download never gets a real filename and never becomes current.
func TestUpdaterRefusesBadBytes(t *testing.T) {
	bucket := t.TempDir()
	fakeRelease(t, bucket, "20260801", "X")
	// Corrupt the served file AFTER the manifest recorded its hash.
	f, _ := os.OpenFile(filepath.Join(bucket, "20260801", "filmstock.db"), os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprint(f, "tampered")
	f.Close()

	u := NewUpdater(bucket, t.TempDir()) // local-directory mode, same code path
	if _, _, _, err := u.Update(context.Background()); err == nil {
		t.Fatal("accepted a download whose sha256 does not match the manifest")
	}
	if u.Current() != "" {
		t.Fatal("a refused build became current")
	}
}

// Local-directory mode: "fake it until the bucket exists" — a directory today,
// one string swapped for the URL later.
func TestUpdaterLocalDirectory(t *testing.T) {
	bucket := t.TempDir()
	fakeRelease(t, bucket, "20260801", "Local")
	u := NewUpdater(bucket, t.TempDir())
	_, build, changed, err := u.Update(context.Background())
	if err != nil || !changed || build != "20260801" {
		t.Fatalf("%v changed=%v build=%s", err, changed, build)
	}
}
