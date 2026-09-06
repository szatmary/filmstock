package build

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/szatmary/filmstock/internal/sqldrv"
)

// pageSetDB writes a resolver-shaped cache holding exactly these page_ids —
// the dump's page set, as build-qidmap would leave it.
func pageSetDB(t *testing.T, path string, ids ...int64) {
	t.Helper()
	db, err := sql.Open(sqldrv.Name, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(
		`CREATE TABLE wiki_qid(title TEXT PRIMARY KEY, qid INTEGER, page_id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := db.Exec(`INSERT INTO wiki_qid VALUES(?,?,?)`,
			fmt.Sprintf("P%d", id), id, id); err != nil {
			t.Fatal(err)
		}
	}
}

// A record that disappears for a page the dump still carries is the shape of
// every failure this guard exists for: a rewind, a parser regression that
// stops recognising a template, a truncated import.
func TestBridgeRefusesRemovingLivePages(t *testing.T) {
	dir := t.TempDir()
	tinyBuild(t, filepath.Join(dir, "tip"), map[int]string{1: "Heat", 2: "Solaris", 3: "Stalker"})
	tinyBuild(t, filepath.Join(dir, "new"), map[int]string{1: "Heat", 2: "Solaris"})
	set := filepath.Join(dir, "pages.db")
	pageSetDB(t, set, 1, 2, 3) // Stalker's article still exists

	err := guardRemovals(
		filepath.Join(dir, "tip", "filmstock.db"),
		filepath.Join(dir, "new", "filmstock.db"), set)
	if err == nil {
		t.Fatal("published a bridge that drops a film whose page still exists")
	}
	for _, want := range []string{"movies", "page_id 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name the record (%q): %v", want, err)
		}
	}
}

// The legitimate case: the page really is gone from Wikipedia, so the record
// going with it is the full doing its job. This is the half a statement-count
// threshold can never distinguish from the case above.
func TestBridgeAllowsRemovingDeletedPages(t *testing.T) {
	dir := t.TempDir()
	tinyBuild(t, filepath.Join(dir, "tip"), map[int]string{1: "Heat", 2: "Solaris", 3: "Stalker"})
	tinyBuild(t, filepath.Join(dir, "new"), map[int]string{1: "Heat", 2: "Solaris"})
	set := filepath.Join(dir, "pages.db")
	pageSetDB(t, set, 1, 2) // page 3 was deleted between the dumps

	if err := guardRemovals(
		filepath.Join(dir, "tip", "filmstock.db"),
		filepath.Join(dir, "new", "filmstock.db"), set); err != nil {
		t.Fatalf("refused a legitimate deletion: %v", err)
	}
}

// Additions and changes are what a full is for, however many there are. A
// bridge that only repairs must never be blocked — this is the sixteen
// aged-out days of 20260802–20260817 in miniature.
func TestBridgeAllowsUnlimitedRepair(t *testing.T) {
	dir := t.TempDir()
	tip := map[int]string{1: "Heat"}
	fresh := map[int]string{1: "Heat, restored"}
	for i := 2; i <= 400; i++ {
		fresh[i] = fmt.Sprintf("Repaired %d", i)
	}
	tinyBuild(t, filepath.Join(dir, "tip"), tip)
	tinyBuild(t, filepath.Join(dir, "new"), fresh)
	set := filepath.Join(dir, "pages.db")
	var ids []int64
	for i := 1; i <= 400; i++ {
		ids = append(ids, int64(i))
	}
	pageSetDB(t, set, ids...)

	if err := guardRemovals(
		filepath.Join(dir, "tip", "filmstock.db"),
		filepath.Join(dir, "new", "filmstock.db"), set); err != nil {
		t.Fatalf("a bridge of 400 repairs was refused: %v", err)
	}
}

// A page set that is not a resolver cache must fail loudly rather than pass
// everything: a guard that silently checks nothing is worse than no guard.
func TestBridgeRefusesAnUnusablePageSet(t *testing.T) {
	dir := t.TempDir()
	tinyBuild(t, filepath.Join(dir, "tip"), map[int]string{1: "Heat", 2: "Solaris"})
	tinyBuild(t, filepath.Join(dir, "new"), map[int]string{1: "Heat"})
	empty := filepath.Join(dir, "empty.db")
	db, err := sql.Open(sqldrv.Name, empty)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated(x)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if err := guardRemovals(
		filepath.Join(dir, "tip", "filmstock.db"),
		filepath.Join(dir, "new", "filmstock.db"), empty); err == nil {
		t.Fatal("a page set with no wiki_qid table was accepted")
	}
}
