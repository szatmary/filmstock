package filmstock

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/szatmary/filmstock/internal/sqldrv"
)

func makeDB(t *testing.T, path, ddl string) {
	t.Helper()
	h, err := sql.Open(sqldrv.Name, path)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Exec(ddl); err != nil {
		t.Fatal(err)
	}
}

// The point of the whole API: open the database, attach the others, and let
// the consumer join across them in one query.
func TestOpenAttachJoinsAcrossDatabases(t *testing.T) {
	dir := t.TempDir()
	core := filepath.Join(dir, "filmstock.db")
	text := filepath.Join(dir, "filmstock-text.db")
	makeDB(t, core, `CREATE TABLE movies(id INTEGER PRIMARY KEY, title TEXT, year INTEGER);
	                 INSERT INTO movies VALUES(31371,'Seven Samurai',1954);`)
	makeDB(t, text, `CREATE TABLE movie_text(id INTEGER PRIMARY KEY, plot TEXT);
	                 INSERT INTO movie_text VALUES(31371,'Farmers hire seven samurai.');`)

	db, err := Open(core, Attach{Schema: "text", Path: text})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var title, plot string
	err = db.SQL().QueryRow(`SELECT m.title, t.plot FROM movies m
	                         JOIN text.movie_text t ON t.id = m.id`).Scan(&title, &plot)
	if err != nil {
		t.Fatalf("cross-database join: %v", err)
	}
	if title != "Seven Samurai" || plot == "" {
		t.Errorf("got %q / %q", title, plot)
	}
}

// ATTACH is per-connection, so the schema has to survive the pool opening new
// ones — this is the bug the connect hook exists to prevent.
func TestAttachSurvivesEveryPooledConnection(t *testing.T) {
	dir := t.TempDir()
	core := filepath.Join(dir, "filmstock.db")
	text := filepath.Join(dir, "filmstock-text.db")
	makeDB(t, core, `CREATE TABLE movies(id INTEGER PRIMARY KEY, title TEXT);
	                 INSERT INTO movies VALUES(1,'A');`)
	makeDB(t, text, `CREATE TABLE movie_text(id INTEGER PRIMARY KEY, plot TEXT);
	                 INSERT INTO movie_text VALUES(1,'p');`)

	db, err := Open(core, Attach{Schema: "text", Path: text})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SQL().SetMaxOpenConns(8)

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var n int
			if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM text.movie_text`).Scan(&n); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a pooled connection could not see the attached schema: %v", err)
	}
}

func TestAttachRejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	core := filepath.Join(dir, "filmstock.db")
	makeDB(t, core, `CREATE TABLE movies(id INTEGER PRIMARY KEY);`)
	if _, err := Open(core, Attach{Schema: "text", Path: filepath.Join(dir, "nope.db")}); err == nil {
		t.Fatal("attaching a file that does not exist should fail at Open")
	}
	if _, err := Open(core, Attach{Schema: "", Path: core}); err == nil {
		t.Fatal("an attach without a schema name should fail")
	}
	_ = os.Remove
}
