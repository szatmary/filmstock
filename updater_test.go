package filmstock

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szatmary/filmstock/internal/sqldrv"
)

// fakeRelease lays out one tiny but real build the way the bucket will:
// builds.json at the root, manifest and files under the build id.
func fakeRelease(t *testing.T, root, id, title string) {
	t.Helper()
	dir := filepath.Join(root, id)
	os.MkdirAll(dir, 0o777)
	core := filepath.Join(dir, "filmstock.db")
	os.Remove(core)
	h, err := sql.Open(sqldrv.Name, core)
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
	db, err := Open(core)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(db)
	var title string
	if err := live.DB().SQL().QueryRow(
		`SELECT title FROM movies WHERE id = 1`).Scan(&title); err != nil || title == "" {
		t.Fatalf("reading the installed build: %q %v", title, err)
	}

	if _, _, changed, err = u.Update(ctx); err != nil || changed {
		t.Fatalf("second update should be a no-op: %v changed=%v", err, changed)
	}

	fakeRelease(t, bucket, "20260820", "Blade Runner 2049")
	old, changed, err := u.UpdateAndSwap(ctx, live)
	if err != nil || !changed || old == nil {
		t.Fatalf("swap: %v changed=%v old=%v", err, changed, old)
	}
	var swapped string
	if err := live.DB().SQL().QueryRow(
		`SELECT title FROM movies WHERE id = 1`).Scan(&swapped); err != nil ||
		swapped != "Blade Runner 2049" {
		t.Fatalf("after the swap the live handle reads %q (%v)", swapped, err)
	}
	// The old handle still answers until the caller closes it.
	var previous string
	if err := old.SQL().QueryRow(
		`SELECT title FROM movies WHERE id = 1`).Scan(&previous); err != nil ||
		previous != "Blade Runner" {
		t.Fatalf("the old handle died before being closed: %q (%v)", previous, err)
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

// fakeDaily derives build id from parent by one INSERT, publishing the patch
// beside the full file the way `filmstock publish` does.
func fakeDaily(t *testing.T, root, id, parent, patchSQL string) {
	t.Helper()
	dir := filepath.Join(root, id)
	os.MkdirAll(dir, 0o777)
	core := filepath.Join(dir, "filmstock.db")
	if err := copyLocal(filepath.Join(root, parent, "filmstock.db"), core); err != nil {
		t.Fatal(err)
	}
	if err := applySQL(core, []byte(patchSQL)); err != nil {
		t.Fatal(err)
	}
	h, err := sql.Open(sqldrv.Name, "file:"+core+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	content, _, err := ContentHash(h)
	h.Close()
	if err != nil {
		t.Fatal(err)
	}
	sha, _ := fileSHA256(core)

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(patchSQL))
	zw.Close()
	patchPath := filepath.Join(dir, "filmstock.db.patch.sql.gz")
	os.WriteFile(patchPath, gz.Bytes(), 0o644)
	psha, _ := fileSHA256(patchPath)

	man := map[string]any{"dump": id, "files": map[string]any{
		"filmstock.db":              map[string]any{"sha256": sha, "content_hash": content},
		"filmstock.db.patch.sql.gz": map[string]any{"sha256": psha},
	}}
	mb, _ := json.Marshal(man)
	os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644)

	cat := map[string]any{"latest_full": parent, "latest": id,
		"builds": []map[string]any{
			{"id": parent, "kind": "full"},
			{"id": id, "kind": "daily", "parent": parent},
		}}
	cb, _ := json.Marshal(cat)
	os.WriteFile(filepath.Join(root, "builds.json"), cb, 0o644)
}

const stalkerPatch = `INSERT INTO movies(id,title,year,starring,director,cover_image_file)
  VALUES (2,'Stalker',1979,'','','');`

// The patch road: with the daily's FULL file deleted from the release, only
// the patch can reach the new build — and it does, content-verified.
func TestUpdaterTakesThePatchRoad(t *testing.T) {
	bucket := t.TempDir()
	fakeRelease(t, bucket, "20260801", "Blade Runner")
	u := NewUpdater(bucket, t.TempDir())
	ctx := context.Background()
	if _, _, changed, err := u.Update(ctx); err != nil || !changed {
		t.Fatalf("seed full: changed=%v err=%v", changed, err)
	}

	fakeDaily(t, bucket, "20260802", "20260801", stalkerPatch)
	if err := os.Remove(filepath.Join(bucket, "20260802", "filmstock.db")); err != nil {
		t.Fatal(err)
	}

	core, build, changed, err := u.Update(ctx)
	if err != nil || !changed || build != "20260802" {
		t.Fatalf("patch road: build=%s changed=%v err=%v", build, changed, err)
	}
	h, err := sql.Open(sqldrv.Name, "file:"+core+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var title string
	if err := h.QueryRow(`SELECT title FROM movies WHERE id=2`).Scan(&title); err != nil || title != "Stalker" {
		t.Fatalf("patched build lacks the day's row: %q %v", title, err)
	}
}

// A patch that produces the wrong content must not be trusted — and with
// daily databases unhosted there is nothing to fall back to: a consumer
// already at the parent stays put with an error, and a fresh consumer lands
// on the full, honestly behind the tip, rather than empty-handed.
func TestUpdaterRefusesALyingPatch(t *testing.T) {
	bucket := t.TempDir()
	fakeRelease(t, bucket, "20260801", "Blade Runner")
	fakeDaily(t, bucket, "20260802", "20260801", stalkerPatch)
	// The daily hosts no databases, only its patch and manifest.
	os.Remove(filepath.Join(bucket, "20260802", "filmstock.db"))
	// Replace the published patch with one that inserts the wrong film, and
	// fix its manifest sha so only the CONTENT check can catch it.
	lie := `INSERT INTO movies(id,title,year,starring,director,cover_image_file)
	  VALUES (2,'Not Stalker',1979,'','','');`
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(lie))
	zw.Close()
	pp := filepath.Join(bucket, "20260802", "filmstock.db.patch.sql.gz")
	os.WriteFile(pp, gz.Bytes(), 0o644)
	psha, _ := fileSHA256(pp)
	mp := filepath.Join(bucket, "20260802", "manifest.json")
	var man map[string]any
	mb, _ := os.ReadFile(mp)
	json.Unmarshal(mb, &man)
	man["files"].(map[string]any)["filmstock.db.patch.sql.gz"].(map[string]any)["sha256"] = psha
	mb, _ = json.Marshal(man)
	os.WriteFile(mp, mb, 0o644)
	ctx := context.Background()

	// A fresh consumer takes the full road and lands on the full.
	u := NewUpdater(bucket, t.TempDir())
	_, build, changed, err := u.Update(ctx)
	if err != nil || !changed || build != "20260801" {
		t.Fatalf("fresh consumer: build=%s changed=%v err=%v; want to land on the full", build, changed, err)
	}

	// Now at the parent, the lying patch is the only road forward: refuse
	// loudly and stay put.
	if _, build, _, err := u.Update(ctx); err == nil {
		t.Fatalf("a lying patch was accepted (build=%s)", build)
	}
	if got := u.Current(); got != "20260801" {
		t.Fatalf("current = %s after refusing the patch; must stay at the full", got)
	}
}

// The splitter must cut on statement boundaries only: a semicolon inside a
// string literal is data, and a title containing one is not hypothetical.
func TestSQLBatchesRespectsStringLiterals(t *testing.T) {
	patch := []byte(
		`INSERT INTO movies(id,title) VALUES(1,'Hello; World');` + "\n" +
			`INSERT INTO movies(id,title) VALUES(2,'It''s here; really');` + "\n" +
			`INSERT INTO movies(id,title) VALUES(3,'plain');` + "\n")
	squash := func(s string) string { return strings.Join(strings.Fields(s), "") }
	for _, n := range []int{1, 2, 3, 100} {
		got := sqlBatches(patch, n)
		var joined string
		for _, b := range got {
			joined += b
		}
		if squash(joined) != squash(string(patch)) {
			t.Errorf("n=%d: batches do not reassemble to the patch: %q", n, got)
		}
		// One statement per batch at n=1 proves the two literal semicolons
		// were not mistaken for statement ends.
		if n == 1 && len(got) != 3 {
			t.Errorf("n=1: %d batches, want 3 — a semicolon inside a literal split a statement", len(got))
		}
		if n >= 3 && len(got) != 1 {
			t.Errorf("n=%d: %d batches, want 1", n, len(got))
		}
	}
}
